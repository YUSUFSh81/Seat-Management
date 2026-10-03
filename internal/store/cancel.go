package store

import (
	"context"
	"database/sql"
	"errors"
	"math/rand"
	"time"
)

func Cancel(ctx context.Context, db *sql.DB, userID string, reservationID int64) (alreadyCancelled bool, err error) {

	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		already, err := cancelOnce(ctx, db, userID, reservationID)
		if err == nil || !retryable(err) {
			return already, err
		}
		lastErr = err
		base := time.Duration(1<<attempt) * 10 * time.Millisecond
		select {
		case <-time.After(base/2 + time.Duration(rand.Int63n(int64(base)))):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return false, lastErr
}

func cancelOnce(ctx context.Context, db *sql.DB, userID string, reservationID int64) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var showID int64
	var status string
	var n int

	err = tx.QueryRowContext(ctx,
		"SELECT show_id, status, JSON_LENGTH(seats) FROM reservations WHERE id = ? AND user_id = ? FOR UPDATE",
		reservationID, userID).Scan(&showID, &status, &n)

	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrReservationNotFound
	}

	if err != nil {
		return false, err
	}

	if status == "cancelled" {
		return true, nil
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE reservations SET status = 'cancelled' WHERE id = ? AND user_id = ? AND status = 'confirmed'",
		reservationID, userID); err != nil {
		return false, err
	}

	// same lock order as Reserve: user_show, then show_seats
	if _, err := tx.ExecContext(ctx,
		"UPDATE user_show SET seats_held = seats_held - ? WHERE show_id = ? AND user_id = ?",
		n, showID, userID); err != nil {
		return false, err
	}

	// reservation_id in the WHERE: this can never free a seat that belongs to someone else
	if _, err := tx.ExecContext(ctx,
		"UPDATE show_seats SET status = 'available', reservation_id = NULL WHERE reservation_id = ? AND status = 'confirmed'",
		reservationID); err != nil {
		return false, err
	}
	return false, tx.Commit()
}
