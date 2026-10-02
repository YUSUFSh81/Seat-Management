package store

import (
	"context"
	"database/sql"
	"strings"
)

type CreateShowInput struct {
	Name         string
	Seats        []string // already trimmed, normalized, deduplicated
	PricePaise   int64
	PerUserLimit int // already defaulted to 4 by the handler
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
