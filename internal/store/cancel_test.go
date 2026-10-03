package store

import (
	"context"
	"errors"
	"testing"
)

func TestCancelThenRebook(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	showID := mkShow(t, db, 5, 4)

	a := mustReserve(t, db, "alice", showID, []string{"S1", "S2"}, "k1", 4)

	if _, err := Cancel(ctx, db, "alice", a.ReservationID); err != nil {
		t.Fatal(err)
	}
	if got := heldOf(t, db, showID, "alice"); got != 0 {
		t.Fatalf("seats_held = %d, want 0", got)
	}
	assertInvariant(t, db, showID)

	// double cancel is idempotent, and does not decrement twice
	if already, err := Cancel(ctx, db, "alice", a.ReservationID); err != nil || !already {
		t.Fatalf("second cancel: already=%v err=%v", already, err)
	}
	if got := heldOf(t, db, showID, "alice"); got != 0 {
		t.Fatalf("seats_held = %d after double cancel, want 0", got)
	}

	// someone else can book the freed seats
	mustReserve(t, db, "bob", showID, []string{"S1", "S2"}, "kb", 4)
	assertInvariant(t, db, showID)

	// cancelling alice's old reservation again must not free bob's seats
	if _, err := Cancel(ctx, db, "alice", a.ReservationID); err != nil {
		t.Fatal(err)
	}
	if got := seatStatus(t, db, showID, "S1"); got != "confirmed" {
		t.Fatalf("S1 = %q, want confirmed (a stale cancel resurrected a seat)", got)
	}
}

func TestCancelByNonOwner(t *testing.T) {
	db := testDB(t)
	showID := mkShow(t, db, 5, 4)
	a := mustReserve(t, db, "alice", showID, []string{"S1"}, "k1", 4)

	_, err := Cancel(context.Background(), db, "mallory", a.ReservationID)
	if !errors.Is(err, ErrReservationNotFound) {
		t.Fatalf("expected ErrReservationNotFound, got %v", err)
	}
	if got := seatStatus(t, db, showID, "S1"); got != "confirmed" {
		t.Fatalf("S1 = %q, want confirmed", got)
	}
	assertInvariant(t, db, showID)
}
