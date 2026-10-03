package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Starts n goroutines and releases them at the same instant.
func fireAll(n int, fn func(i int)) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
}

// One GROUP BY statement = one consistent snapshot (three separate COUNTs can tear mid-burst).
func seatCounts(db *sql.DB, showID int64) (map[string]int, error) {
	out := map[string]int{}
	rows, err := db.Query("Select status, COUNT(*) from show_seats where show_id=? GROUP BY status;", showID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var c int
		rows.Scan(&s, &c)
		out[s] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func mustReserve(t *testing.T, db *sql.DB, user string, showID int64, seats []string, key string, limit int) ReservationData {
	t.Helper()
	data, _, err := Reserve(context.Background(), db, user, showID, seats, key, int64(len(seats))*1000, limit)
	if err != nil {
		t.Fatalf("reserve(%s, %v) failed: %v", user, seats, err)
	}
	return data
}

func seatStatus(t *testing.T, db *sql.DB, showID int64, label string) string {
	t.Helper()
	var s string
	if err := db.QueryRow("SELECT status FROM show_seats WHERE show_id=? AND seat_label=?", showID, label).Scan(&s); err != nil {
		t.Fatalf("seat %s: %v", label, err)
	}
	return s
}

// seats_held for a user; 0 if the row was never created
func heldOf(t *testing.T, db *sql.DB, showID int64, user string) int {
	t.Helper()
	var n int
	err := db.QueryRow("SELECT seats_held FROM user_show WHERE show_id=? AND user_id=?", showID, user).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func countReservations(t *testing.T, db *sql.DB, user, key string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM reservations WHERE user_id=? AND idempotency_key=?", user, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func countAvailable(t *testing.T, db *sql.DB, showID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM show_seats WHERE show_id=? AND status='available'", showID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func mkShow(t *testing.T, db *sql.DB, seatCount, limit int) int64 {
	t.Helper()
	seats := make([]string, seatCount)
	for i := range seats {
		seats[i] = fmt.Sprintf("S%d", i+1)
	}
	id, err := CreateShow(context.Background(), db,
		CreateShowInput{Name: "t", Seats: seats, PricePaise: 1000, PerUserLimit: limit})
	if err != nil {
		t.Fatal(err)
	}
	return int64(id)
}

// The reconciliation invariant, plus cross-checks between the tables.
func assertInvariant(t *testing.T, db *sql.DB, showID int64) {
	t.Helper()
	counts := map[string]int{}
	rows, err := db.Query("SELECT status, COUNT(*) FROM show_seats WHERE show_id=? GROUP BY status", showID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		var c int
		rows.Scan(&s, &c)
		counts[s] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()

	var total int
	db.QueryRow("SELECT total_seats FROM shows WHERE id=?", showID).Scan(&total)
	if got := counts["available"] + counts["held"] + counts["confirmed"]; got != total {
		t.Fatalf("invariant broken: %v != total %d", counts, total)
	}

	var held, resSeats int
	db.QueryRow("SELECT COALESCE(SUM(seats_held),0) FROM user_show WHERE show_id=?", showID).Scan(&held)
	db.QueryRow(`SELECT COALESCE(SUM(JSON_LENGTH(seats)),0) FROM reservations
	             WHERE show_id=? AND status='confirmed'`, showID).Scan(&resSeats)
	if held != counts["confirmed"] || resSeats != counts["confirmed"] {
		t.Fatalf("mismatch: seats_held=%d reservation_seats=%d confirmed=%d", held, resSeats, counts["confirmed"])
	}
}

func TestHappyPath(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	showID := mkShow(t, db, 5, 4)

	data, replayed, err := Reserve(ctx, db, "alice", showID, []string{"S2", "S1"}, "key-1", 2000, 4)
	if err != nil {
		t.Fatalf("reserve failed: %v", err)
	}
	if replayed {
		t.Fatal("first call must not be a replay")
	}
	if data.AmountPaise != 2000 || data.Status != "confirmed" {
		t.Fatalf("unexpected data: %+v", data)
	}

	// the seats belong to this reservation
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM show_seats
	             WHERE show_id=? AND status='confirmed' AND reservation_id=?`,
		showID, data.ReservationID).Scan(&n)
	if n != 2 {
		t.Fatalf("expected 2 confirmed seats for the reservation, got %d", n)
	}

	// the limit counter moved
	var held int
	db.QueryRow("SELECT seats_held FROM user_show WHERE show_id=? AND user_id='alice'", showID).Scan(&held)
	if held != 2 {
		t.Fatalf("seats_held = %d, want 2", held)
	}

	assertInvariant(t, db, showID)
}

func TestPartialRequest(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	showID := mkShow(t, db, 5, 4)

	data, replayed, err := Reserve(ctx, db, "bob", showID, []string{"S1"}, "ky-1", 1000, 4)

	if err != nil {
		t.Fatalf("reserve failed: %v", err)
	}

	if replayed {
		t.Fatal("first call must not be a replay")
	}

	if data.AmountPaise != 1000 || data.Status != "confirmed" {
		t.Fatalf("unexpected data: %+v", data)
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM show_seats WHERE show_id=? AND status='confirmed' AND reservation_id=?`, showID, data.ReservationID).Scan(&n)
	if n != 1 {
		t.Fatalf("expected 1 confirmed seat, got %d", n)
	}
	var held int
	db.QueryRow("SELECT seats_held FROM user_show WHERE show_id=? AND user_id='bob'", showID).Scan(&held)
	if held != 1 {
		t.Fatalf("seats_held = %d, want 1", held)
	}

	assertInvariant(t, db, showID)

	// alice asks for S1 (taken by bob) and S2 (free): must be declined as a whole
	_, replayed2, err2 := Reserve(ctx, db, "alice", showID, []string{"S1", "S2"}, "ky-2", 2000, 4)

	if err2 == nil {
		t.Fatalf("expected ErrSeatNotAvailable, got: %v", err2)
	}

	if replayed2 {
		t.Fatal("declined request cannot be replayed")
	}
	if !errors.Is(err2, ErrSeatsUnavailable) {
		t.Fatalf("expected ErrSeatNotAvailable, got: %v", err2)
	}

	var su *SeatsUnavailableError
	if !errors.As(err2, &su) {
		t.Fatalf("expected SeatsUnavailableError, got %T: %v", err2, err2)
	}

	if len(su.Seats) != 1 || su.Seats[0] != "S1" {
		t.Fatalf("expected unavailable seats [S1], got %+v", su)
	}

	var s2 string
	db.QueryRow("Select status from show_seats where show_id=? and seat_label ='S2'", showID).Scan(&s2)
	if s2 != "available" {
		t.Fatalf("expected S2 to be available, got %s", s2)
	}

	// S1 still belongs to bob's reservation
	var owner int64
	db.QueryRow("select reservation_id from show_seats where show_id=? and seat_label = 'S1'", showID).Scan(&owner)
	if owner != data.ReservationID {
		t.Fatalf("expected owner %d, got %d", data.ReservationID, owner)
	}

	// alice's limit counter was rolled back (her row exists from INSERT IGNORE, at 0)
	var aliceHeld int
	db.QueryRow("SELECT seats_held FROM user_show WHERE show_id=? AND user_id='alice'", showID).Scan(&aliceHeld)
	if aliceHeld != 0 {
		t.Fatalf("alice seats_held = %d, want 0", aliceHeld)
	}

	// no reservation request left for alice key
	var cnt int
	db.QueryRow("Select COUNT(*) from reservations where user_id='alice' and idempotency_key='ky-2'").Scan(&cnt)
	if cnt != 0 {
		t.Fatalf("expected 0 reservations for alice key, got %d", cnt)
	}

	assertInvariant(t, db, showID)

}

func TestOverLimitSingleRequest(t *testing.T) {
	db := testDB(t)
	showID := mkShow(t, db, 10, 4)

	_, replayed, err := Reserve(context.Background(), db, "alice", showID,
		[]string{"S1", "S2", "S3", "S4", "S5"}, "k1", 5000, 4)

	if !errors.Is(err, ErrOverLimit) {
		t.Fatalf("expected ErrOverLimit, got %v", err)
	}
	if replayed {
		t.Fatal("a declined request is not a replay")
	}
	if got := countAvailable(t, db, showID); got != 10 {
		t.Fatalf("available = %d, want 10 (nothing should change)", got)
	}
	if got := countReservations(t, db, "alice", "k1"); got != 0 {
		t.Fatalf("found %d reservation rows for the declined request", got)
	}
	if got := heldOf(t, db, showID, "alice"); got != 0 {
		t.Fatalf("seats_held = %d, want 0", got)
	}
	assertInvariant(t, db, showID)
}

func TestOverLimitCumulative(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	showID := mkShow(t, db, 10, 4)

	mustReserve(t, db, "alice", showID, []string{"S1", "S2", "S3"}, "k1", 4)
	if got := heldOf(t, db, showID, "alice"); got != 3 {
		t.Fatalf("seats_held = %d, want 3", got)
	}

	// 3 + 2 = 5 > 4
	_, _, err := Reserve(ctx, db, "alice", showID, []string{"S4", "S5"}, "k2", 2000, 4)
	if !errors.Is(err, ErrOverLimit) {
		t.Fatalf("expected ErrOverLimit, got %v", err)
	}
	for _, s := range []string{"S4", "S5"} {
		if got := seatStatus(t, db, showID, s); got != "available" {
			t.Fatalf("%s status = %q, want available", s, got)
		}
	}
	if got := heldOf(t, db, showID, "alice"); got != 3 {
		t.Fatalf("seats_held = %d after the declined request, want 3 (the increment must roll back)", got)
	}
	if got := countReservations(t, db, "alice", "k2"); got != 0 {
		t.Fatalf("found %d reservation rows for the declined request", got)
	}

	// the failed attempt must not have leaked capacity: one more seat still fits
	mustReserve(t, db, "alice", showID, []string{"S4"}, "k3", 4)
	if got := heldOf(t, db, showID, "alice"); got != 4 {
		t.Fatalf("seats_held = %d, want 4", got)
	}

	// and now she is at the limit
	_, _, err = Reserve(ctx, db, "alice", showID, []string{"S6"}, "k4", 1000, 4)
	if !errors.Is(err, ErrOverLimit) {
		t.Fatalf("expected ErrOverLimit at the limit, got %v", err)
	}
	if got := seatStatus(t, db, showID, "S6"); got != "available" {
		t.Fatalf("S6 status = %q, want available", got)
	}

	assertInvariant(t, db, showID)
}

func TestLimitIsPerUser(t *testing.T) {
	db := testDB(t)
	showID := mkShow(t, db, 10, 2)

	mustReserve(t, db, "alice", showID, []string{"S1", "S2"}, "ka", 2)
	mustReserve(t, db, "bob", showID, []string{"S3", "S4"}, "kb", 2) // bob has his own counter

	if got := heldOf(t, db, showID, "alice"); got != 2 {
		t.Fatalf("alice seats_held = %d, want 2", got)
	}
	if got := heldOf(t, db, showID, "bob"); got != 2 {
		t.Fatalf("bob seats_held = %d, want 2", got)
	}
	assertInvariant(t, db, showID)
}

func TestLimitIsPerShow(t *testing.T) {
	db := testDB(t)
	show1 := mkShow(t, db, 10, 2)
	show2 := mkShow(t, db, 10, 2)

	// different keys on purpose: reusing one key across shows is an idempotency conflict
	mustReserve(t, db, "alice", show1, []string{"S1", "S2"}, "k-show1", 2)
	mustReserve(t, db, "alice", show2, []string{"S1", "S2"}, "k-show2", 2)

	if got := heldOf(t, db, show1, "alice"); got != 2 {
		t.Fatalf("show1 seats_held = %d, want 2", got)
	}
	if got := heldOf(t, db, show2, "alice"); got != 2 {
		t.Fatalf("show2 seats_held = %d, want 2", got)
	}
	assertInvariant(t, db, show1)
	assertInvariant(t, db, show2)
}

func TestReplay(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	showID := mkShow(t, db, 5, 4)

	first, replayed, err := Reserve(ctx, db, "alice", showID, []string{"S1", "S2"}, "k1", 2000, 4)
	if err != nil || replayed {
		t.Fatalf("first call: err=%v replayed=%v", err, replayed)
	}

	second, replayed2, err := Reserve(ctx, db, "alice", showID, []string{"S1", "S2"}, "k1", 2000, 4)
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if !replayed2 {
		t.Fatal("retry must be flagged as a replay")
	}
	if second.ReservationID != first.ReservationID ||
		second.AmountPaise != first.AmountPaise ||
		!slices.Equal(second.Seats, first.Seats) {
		t.Fatalf("replay returned a different reservation: first=%+v second=%+v", first, second)
	}

	// the retry moved nothing
	if got := countReservations(t, db, "alice", "k1"); got != 1 {
		t.Fatalf("reservation rows = %d, want 1", got)
	}
	if got := heldOf(t, db, showID, "alice"); got != 2 {
		t.Fatalf("seats_held = %d, want 2 (a retry must not count twice)", got)
	}
	if got := countAvailable(t, db, showID); got != 3 {
		t.Fatalf("available = %d, want 3", got)
	}
	assertInvariant(t, db, showID)
}

func TestReplayWithReorderedSeats(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	showID := mkShow(t, db, 5, 4)

	first := mustReserve(t, db, "alice", showID, []string{"S1", "S2"}, "k1", 4)

	second, replayed, err := Reserve(ctx, db, "alice", showID, []string{"S2", "S1"}, "k1", 2000, 4)
	if err != nil {
		t.Fatalf("same seats in another order must be a replay, got %v", err)
	}
	if !replayed || second.ReservationID != first.ReservationID {
		t.Fatalf("replayed=%v ids %d vs %d", replayed, first.ReservationID, second.ReservationID)
	}
	assertInvariant(t, db, showID)
}

func TestSameKeyDifferentSeats(t *testing.T) {
	db := testDB(t)
	showID := mkShow(t, db, 5, 4)

	mustReserve(t, db, "alice", showID, []string{"S1", "S2"}, "k1", 4)

	_, replayed, err := Reserve(context.Background(), db, "alice", showID, []string{"S1", "S3"}, "k1", 2000, 4)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict, got %v", err)
	}
	if replayed {
		t.Fatal("a conflict is not a replay")
	}

	if got := seatStatus(t, db, showID, "S3"); got != "available" {
		t.Fatalf("S3 status = %q, want available", got)
	}
	if got := countReservations(t, db, "alice", "k1"); got != 1 {
		t.Fatalf("reservation rows = %d, want 1", got)
	}
	if got := heldOf(t, db, showID, "alice"); got != 2 {
		t.Fatalf("seats_held = %d, want 2", got)
	}
	assertInvariant(t, db, showID)
}

func TestSameKeyOnAnotherShow(t *testing.T) {
	db := testDB(t)
	show1 := mkShow(t, db, 5, 4)
	show2 := mkShow(t, db, 5, 4)

	mustReserve(t, db, "alice", show1, []string{"S1"}, "k1", 4)

	// same user, same key, same seat labels, different show: the hash includes the show id
	_, _, err := Reserve(context.Background(), db, "alice", show2, []string{"S1"}, "k1", 1000, 4)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict, got %v", err)
	}
	if got := seatStatus(t, db, show2, "S1"); got != "available" {
		t.Fatalf("show2 S1 status = %q, want available", got)
	}
	assertInvariant(t, db, show2)
}

func TestKeyIsPerUser(t *testing.T) {
	db := testDB(t)
	showID := mkShow(t, db, 5, 4)

	a := mustReserve(t, db, "alice", showID, []string{"S1"}, "same-key", 4)
	b := mustReserve(t, db, "bob", showID, []string{"S2"}, "same-key", 4) // bob's key is independent

	if a.ReservationID == b.ReservationID {
		t.Fatal("two users with the same key string must get separate reservations")
	}
	assertInvariant(t, db, showID)
}

func TestUnknownSeat(t *testing.T) {
	db := testDB(t)
	showID := mkShow(t, db, 5, 4)

	_, _, err := Reserve(context.Background(), db, "alice", showID, []string{"S1", "ZZ99"}, "k1", 2000, 4)
	if !errors.Is(err, ErrSeatsUnavailable) {
		t.Fatalf("expected ErrSeatsUnavailable, got %v", err)
	}
	if got := seatStatus(t, db, showID, "S1"); got != "available" {
		t.Fatalf("S1 status = %q, want available (all-or-nothing)", got)
	}
	if got := heldOf(t, db, showID, "alice"); got != 0 {
		t.Fatalf("seats_held = %d, want 0", got)
	}
	assertInvariant(t, db, showID)
}

func TestHotSeat(t *testing.T) {
	db := testDB(t)
	showID := mkShow(t, db, 10, 4)

	const n = 200
	var ok, taken atomic.Int32
	fireAll(n, func(i int) {
		_, _, err := Reserve(context.Background(), db, fmt.Sprintf("u%d", i),
			showID, []string{"S1"}, fmt.Sprintf("k%d", i), 1000, 4)
		switch {
		case err == nil:
			ok.Add(1)
		case errors.Is(err, ErrSeatsUnavailable):
			taken.Add(1)
		default:
			t.Errorf("unexpected error: %v", err)
		}
	})

	if ok.Load() != 1 || taken.Load() != n-1 {
		t.Fatalf("ok=%d taken=%d, want 1 and %d", ok.Load(), taken.Load(), n-1)
	}
	assertInvariant(t, db, showID)
}

func TestSameKeyInParallel(t *testing.T) {
	db := testDB(t)
	showID := mkShow(t, db, 5, 4)

	const n = 20
	ids := make([]int64, n) // each goroutine writes its own index, so no race
	var fresh atomic.Int32
	fireAll(n, func(i int) {
		d, replayed, err := Reserve(context.Background(), db, "alice", showID,
			[]string{"S1", "S2"}, "same-key", 2000, 4)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
			return
		}
		ids[i] = d.ReservationID
		if !replayed {
			fresh.Add(1)
		}
	})

	if fresh.Load() != 1 {
		t.Fatalf("%d calls created a reservation, want exactly 1", fresh.Load())
	}
	for i, id := range ids {
		if id == 0 || id != ids[0] {
			t.Fatalf("call %d returned reservation %d, want %d", i, id, ids[0])
		}
	}
	if got := countReservations(t, db, "alice", "same-key"); got != 1 {
		t.Fatalf("reservation rows = %d, want 1", got)
	}
	if got := heldOf(t, db, showID, "alice"); got != 2 {
		t.Fatalf("seats_held = %d, want 2", got)
	}
	assertInvariant(t, db, showID)
}

func TestLimitUnderConcurrency(t *testing.T) {
	db := testDB(t)
	showID := mkShow(t, db, 20, 4)

	const n = 10
	var ok, over atomic.Int32
	fireAll(n, func(i int) {
		_, _, err := Reserve(context.Background(), db, "alice", showID,
			[]string{fmt.Sprintf("S%d", i+1)}, fmt.Sprintf("k%d", i), 1000, 4)
		switch {
		case err == nil:
			ok.Add(1)
		case errors.Is(err, ErrOverLimit):
			over.Add(1)
		default:
			t.Errorf("unexpected error: %v", err)
		}
	})

	if ok.Load() != 4 || over.Load() != n-4 {
		t.Fatalf("ok=%d over=%d, want 4 and %d", ok.Load(), over.Load(), n-4)
	}
	if got := heldOf(t, db, showID, "alice"); got != 4 {
		t.Fatalf("seats_held = %d, want 4", got)
	}
	assertInvariant(t, db, showID) // confirmed seats == 4 == reservation seats
}

func TestStampede(t *testing.T) {
	db := testDB(t)
	const total, hot = 200, 20
	showID := mkShow(t, db, total, 4)

	// poll the invariant while the burst runs
	stop := make(chan struct{})
	var pollWG sync.WaitGroup
	pollWG.Add(1)
	go func() {
		defer pollWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			c, err := seatCounts(db, showID)
			if err != nil {
				t.Errorf("poll failed: %v", err)
				return
			}
			if sum := c["available"] + c["held"] + c["confirmed"]; sum != total {
				t.Errorf("invariant broken mid-burst: %v", c)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	const n = 2000
	var created, replays, taken, over, other atomic.Int32
	fireAll(n, func(i int) {
		user := fmt.Sprintf("u%d", i%500) // users repeat, so the limit bites too
		key := fmt.Sprintf("k%d", i)

		// 1 to 3 distinct hot seats
		cnt := 1 + rand.IntN(3)
		seats := make([]string, 0, cnt)
		for _, p := range rand.Perm(hot)[:cnt] {
			seats = append(seats, fmt.Sprintf("S%d", p+1))
		}

		attempts := 1
		if i%10 == 0 {
			attempts = 2 // a retry with the same key
		}
		for a := 0; a < attempts; a++ {
			_, replayed, err := Reserve(context.Background(), db, user, showID, seats, key, int64(len(seats))*1000, 4)
			switch {
			case err == nil && replayed:
				replays.Add(1)
			case err == nil:
				created.Add(1)
			case errors.Is(err, ErrSeatsUnavailable):
				taken.Add(1)
			case errors.Is(err, ErrOverLimit):
				over.Add(1)
			default:
				other.Add(1)
				t.Errorf("unexpected error: %v", err)
			}
		}
	})
	close(stop)
	pollWG.Wait()

	t.Logf("created=%d replays=%d seat_taken=%d over_limit=%d other=%d",
		created.Load(), replays.Load(), taken.Load(), over.Load(), other.Load())

	c, err := seatCounts(db, showID)
	if err != nil {
		t.Fatal(err)
	}
	if c["confirmed"] > hot {
		t.Fatalf("confirmed %d seats but only %d were contested", c["confirmed"], hot)
	}
	if c["confirmed"] == 0 {
		t.Fatal("nobody won anything, something is wrong")
	}
	assertInvariant(t, db, showID)
}
