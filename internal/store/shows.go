package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type CreateShowInput struct {
	Name         string
	Seats        []string // already trimmed, normalized, deduplicated
	PricePaise   int64
	PerUserLimit int // already defaulted to 4 by the handler
}

type SeatState struct {
	Seat   string `json:"seat"`
	Status string `json:"status"`
}

type ShowState struct {
	ID           int64       `json:"id"`
	Name         string      `json:"name"`
	PricePaise   int64       `json:"price_paise"`
	PerUserLimit int         `json:"per_user_limit"`
	TotalSeats   int         `json:"total_seats"`
	Available    int         `json:"available"`
	Held         int         `json:"held"`
	Confirmed    int         `json:"confirmed"`
	Seats        []SeatState `json:"seats,omitempty"`
}

func CreateShow(ctx context.Context, db *sql.DB, in CreateShowInput) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	showInsertQuery := "INSERT INTO shows (name, price_paise, total_seats, per_user_limit) VALUES (?, ?, ?, ?)"
	result, err := tx.ExecContext(ctx, showInsertQuery, in.Name, in.PricePaise, len(in.Seats), in.PerUserLimit)

	if err != nil {
		return 0, err
	}
	showID, err := result.LastInsertId()

	batch := 2000
	for i := 0; i < len(in.Seats); i += batch {
		end := i + batch
		if end > len(in.Seats) {
			end = len(in.Seats)
		}
		batchSeats := in.Seats[i:end]
		stmt := "INSERT into show_seats(show_id, seat_label) VALUES " +
			strings.TrimSuffix(strings.Repeat("(?, ?),", len(batchSeats)), ",")
		args := make([]any, 0, 2*len(batchSeats))
		for _, seat := range batchSeats {
			args = append(args, showID, seat)
		}
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(showID), nil
}

func GetShow(ctx context.Context, db *sql.DB, id int64, includeSeats bool) (ShowState, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ShowState{}, err
	}
	defer tx.Rollback()

	var s ShowState
	err = tx.QueryRowContext(ctx,
		"SELECT id, name, price_paise, per_user_limit, total_seats FROM shows WHERE id = ?", id).
		Scan(&s.ID, &s.Name, &s.PricePaise, &s.PerUserLimit, &s.TotalSeats)
	if errors.Is(err, sql.ErrNoRows) {
		return ShowState{}, ErrShowNotFound
	}
	if err != nil {
		return ShowState{}, err
	}

	// one GROUP BY = one consistent set of counts
	rows, err := tx.QueryContext(ctx,
		"SELECT status, COUNT(*) FROM show_seats WHERE show_id = ? GROUP BY status", id)
	if err != nil {
		return ShowState{}, err
	}
	for rows.Next() {
		var st string
		var c int
		if err := rows.Scan(&st, &c); err != nil {
			rows.Close()
			return ShowState{}, err
		}
		switch st {
		case "available":
			s.Available = c
		case "held":
			s.Held = c
		case "confirmed":
			s.Confirmed = c
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ShowState{}, err
	}

	if includeSeats {
		srows, err := tx.QueryContext(ctx,
			"SELECT seat_label, status FROM show_seats WHERE show_id = ? ORDER BY seat_label", id)
		if err != nil {
			return ShowState{}, err
		}
		defer srows.Close()
		s.Seats = make([]SeatState, 0, s.TotalSeats)
		for srows.Next() {
			var ss SeatState
			if err := srows.Scan(&ss.Seat, &ss.Status); err != nil {
				return ShowState{}, err
			}
			s.Seats = append(s.Seats, ss)
		}
		if err := srows.Err(); err != nil {
			return ShowState{}, err
		}
	}
	return s, nil
}
