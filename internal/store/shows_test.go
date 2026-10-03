package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
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
	db.SetMaxOpenConns(30)
	t.Cleanup(func() { db.Close() })
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}

	// safety: never truncate anything except a database whose name ends in _test
	var dbName string
	if err := db.QueryRow("SELECT DATABASE()").Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(dbName, "_test") {
		t.Fatalf("refusing to truncate non-test database %q", dbName)
	}
	for _, tbl := range []string{"show_seats", "reservations", "user_show", "shows"} {
		if _, err := db.Exec("TRUNCATE TABLE " + tbl); err != nil {
			t.Fatal(err)
		}
	}
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

func TestGetShowReconciles(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	showID := mkShow(t, db, 6, 4)
	r := mustReserve(t, db, "alice", showID, []string{"S1", "S2"}, "k1", 4)
	mustReserve(t, db, "bob", showID, []string{"S3"}, "k2", 4)
	if _, err := Cancel(ctx, db, "alice", r.ReservationID); err != nil {
		t.Fatal(err)
	}

	s, err := GetShow(ctx, db, showID, true)
	if err != nil {
		t.Fatal(err)
	}
	if s.Available+s.Held+s.Confirmed != s.TotalSeats {
		t.Fatalf("invariant broken: %+v", s)
	}
	if s.Available != 5 || s.Confirmed != 1 || len(s.Seats) != 6 {
		t.Fatalf("unexpected state: %+v", s)
	}
}
