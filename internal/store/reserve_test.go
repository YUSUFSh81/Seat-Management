package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

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
