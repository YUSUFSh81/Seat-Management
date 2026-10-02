package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	_ "github.com/go-sql-driver/mysql"
)

func testDB(t *testing.T) *sql.DB {
	dsn := os.Getenv("TEST_DB_DSN")
	if dsn == "" {
		t.Skip("TEST_DB_DSN not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestCreateShowRollsBackOnFailure(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	var before int
	db.QueryRow("SELECT COUNT(*) FROM shows").Scan(&before)

	// 3000 seats means two batches (2000 + 1000). The last seat duplicates
	// the first, so the SECOND batch fails after the first one succeeded.
	seats := make([]string, 3000)
	for i := range seats {
		seats[i] = fmt.Sprintf("S%d", i)
	}
	seats[2999] = "S0"

	_, err := CreateShow(ctx, db, CreateShowInput{Name: "bad", Seats: seats, PricePaise: 100, PerUserLimit: 4})
	if err == nil {
		t.Fatal("expected an error")
	}

	var after int
	db.QueryRow("SELECT COUNT(*) FROM shows").Scan(&after)
	if after != before {
		t.Fatalf("show row leaked: before=%d after=%d", before, after)
	}
}
