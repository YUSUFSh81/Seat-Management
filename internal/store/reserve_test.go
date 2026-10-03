package store

import (
	"context"
	"database/sql"
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
