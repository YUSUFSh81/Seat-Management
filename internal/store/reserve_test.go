package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

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
