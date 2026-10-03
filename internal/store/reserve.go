package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

type ReservationData struct {
	ReservationID  int64
	ShowID         int64
	UserID         string
	Seats          []string
	AmountPaise    int64
	Status         string
	IdempotencyKey string
	RequestHash    string
	CreatedAt      time.Time
}

const (
	errDuplicateEntry  = 1062
	errDeadlock        = 1213
	errLockWaitTimeout = 1205
)

var (
	ErrOverLimit           = errors.New("over_limit")
	ErrSeatsUnavailable    = errors.New("seats_unavailable")
	ErrIdempotencyConflict = errors.New("idempotency_key_reused")
	ErrShowNotFound        = errors.New("show_not_found")
	errRetry               = errors.New("retry attempt")
)

func mysqlErrNo(err error) uint16 {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}

func retryable(err error) bool {
	if errors.Is(err, errRetry) {
		return true
	}
	n := mysqlErrNo(err)
	if n == errDeadlock || n == errLockWaitTimeout {
		return true
	}
	return false
}

type SeatsUnavailableError struct{ Seats []string }

func (e *SeatsUnavailableError) Error() string        { return "seats_unavailable" }
func (e *SeatsUnavailableError) Is(target error) bool { return target == ErrSeatsUnavailable }

func Reserve(ctx context.Context, db *sql.DB, userID string, showID int64, seats []string, idempotencyKey string, amountPaise int64, limit int) (ReservationData, bool, error) {
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		data, replay, err := reserveOnce(ctx, db, userID, showID, seats, idempotencyKey, amountPaise, limit)

		if err == nil || !retryable(err) {
			return data, replay, err
		}

		lastErr = err
		base := time.Duration(1<<attempt) * 10 * time.Millisecond
		sleep := base/2 + time.Duration(rand.Int63n(int64(base)))

		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return ReservationData{}, false, ctx.Err()
		}
	}

	return ReservationData{}, false, lastErr

}

func reserveOnce(ctx context.Context, db *sql.DB, userID string, showID int64, seats []string, idempotencyKey string, amountPaise int64, limit int) (ReservationData, bool, error) {
	sorted := slices.Clone(seats)
	slices.Sort(sorted)

	// insert into user_shows
	query := "INSERT IGNORE INTO user_show(show_id, user_id, seats_held) VALUES (?, ?, ?)"

	_, err := db.ExecContext(ctx, query, showID, userID, 0)
	if err != nil {
		return ReservationData{}, false, err
	}

	// Start transaction
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ReservationData{}, false, err
	}
	defer tx.Rollback()

	sum := sha256.Sum256(fmt.Appendf(nil, "%d|%s", showID, strings.Join(sorted, ",")))
	hash := hex.EncodeToString(sum[:])

	marshaledSeats, err := json.Marshal(sorted)
	if err != nil {
		return ReservationData{}, false, err
	}

	// insert reservation
	reserveQuery := "Insert into reservations(show_id, user_id, idempotency_key, request_hash, seats, amount_paise, status) values (?, ?, ?, ?, ?, ?, ?)"

	reserveResult, err := tx.ExecContext(ctx, reserveQuery, showID, userID, idempotencyKey, hash, string(marshaledSeats), amountPaise, "confirmed")
	if err != nil {
		if mysqlErrNo(err) == errDuplicateEntry {
			tx.Rollback()
			return lookExistingReservation(ctx, db, userID, idempotencyKey, hash)
		}
		return ReservationData{}, false, err
	}

	resID, err := reserveResult.LastInsertId()
	if err != nil {
		return ReservationData{}, false, err
	}

	updateUserShowQuery := "update user_show SET seats_held = seats_held + ? WHERE show_id = ? AND user_id = ? AND seats_held + ? <= ?;"
	result, err := tx.ExecContext(ctx, updateUserShowQuery, len(sorted), showID, userID, len(sorted), limit)

	if err != nil {
		return ReservationData{}, false, err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return ReservationData{}, false, err
	}

	if affected == 0 {
		return ReservationData{}, false, ErrOverLimit
	}

	// update seat status

	ph := strings.TrimSuffix(strings.Repeat("?,", len(sorted)), ",")

	seatStatusQuery := fmt.Sprintf(`
		UPDATE show_seats 
		SET status = 'confirmed', reservation_id = ?
		WHERE show_id = ? 
		AND seat_label IN (%s)
		AND status = 'available';
	`, ph)

	args := make([]interface{}, 0, len(sorted)+2)
	args = append(args, resID, showID)
	for _, seat := range sorted {
		args = append(args, seat)
	}

	result, err = tx.ExecContext(ctx, seatStatusQuery, args...)
	if err != nil {
		return ReservationData{}, false, err
	}

	affected, err = result.RowsAffected()
	if err != nil {
		return ReservationData{}, false, err
	}

	if affected < int64(len(sorted)) {
		tx.Rollback()

		unavailable, err := findUnavailableSeats(ctx, db, showID, sorted)
		if err != nil {
			return ReservationData{}, false, err
		}

		return ReservationData{}, false, &SeatsUnavailableError{Seats: unavailable}
	}

	if err := tx.Commit(); err != nil {
		return ReservationData{}, false, err
	}

	reserveData := ReservationData{
		ReservationID:  resID,
		ShowID:         showID,
		UserID:         userID,
		Seats:          sorted,
		AmountPaise:    amountPaise,
		Status:         "confirmed",
		IdempotencyKey: idempotencyKey,
		RequestHash:    hash,
		CreatedAt:      time.Now(),
	}
	return reserveData, false, nil
}

func lookExistingReservation(ctx context.Context, db *sql.DB, userID string, idempotencyKey string, hash string) (ReservationData, bool, error) {
	var res ReservationData

	var seatsRaw []byte
	query := "Select id,show_id,user_id,idempotency_key,request_hash,seats,amount_paise,status,created_at from reservations where user_id = ? and idempotency_key = ?"
	err := db.QueryRowContext(ctx, query, userID, idempotencyKey).Scan(&res.ReservationID, &res.ShowID, &res.UserID, &res.IdempotencyKey, &res.RequestHash, &seatsRaw, &res.AmountPaise, &res.Status, &res.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ReservationData{}, false, errRetry
		}
		return res, false, err
	}

	if err := json.Unmarshal(seatsRaw, &res.Seats); err != nil {
		return ReservationData{}, false, err
	}

	// check if request hash matches
	if res.RequestHash != hash {
		return ReservationData{}, false, ErrIdempotencyConflict
	}

	// if everything is good return the reservation data
	return res, true, nil
}

func findUnavailableSeats(ctx context.Context, db *sql.DB, showID int64, seats []string) ([]string, error) {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(seats)), ",")
	query := fmt.Sprintf(`
		SELECT seat_label
		FROM show_seats
		WHERE show_id = ? AND seat_label IN (%s) AND status = 'confirmed'
	`, ph)

	args := make([]interface{}, 0, len(seats)+1)
	args = append(args, showID)
	for _, seat := range seats {
		args = append(args, seat)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var unavailable []string
	for rows.Next() {
		var seatLabel string
		if err := rows.Scan(&seatLabel); err != nil {
			return nil, err
		}
		unavailable = append(unavailable, seatLabel)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}
	return unavailable, nil
}
