package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
)

//go:embed migrations/*.sql 
var migrationFS embed.FS

func Migrate(ctx context.Context, db *sql.DB) error {
	// One dedicated connection, because GET_LOCK is tied to the connection.
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// If two instances start at once, only one migrates at a time.
	var got int
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK('seat_migrate', 30)").Scan(&got); err != nil {
		return err
	}
	if got != 1 {
		return fmt.Errorf("could not acquire migration lock")
	}
	defer conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK('seat_migrate')")

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    VARCHAR(64) PRIMARY KEY,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	) ENGINE=InnoDB`); err != nil {
		return err
	}

	entries, err := migrationFS.ReadDir("migrations") // returned sorted by filename
	if err != nil {
		return err
	}
	for _, e := range entries {
		version := e.Name()

		var exists int
		if err := conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM schema_migrations WHERE version=?", version).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			continue
		}

		body, err := migrationFS.ReadFile("migrations/" + version)
		if err != nil {
			return err
		}
		for _, stmt := range strings.Split(string(body), ";") {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migration %s: %w", version, err)
			}
		}
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO schema_migrations (version) VALUES (?)", version); err != nil {
			return err
		}
		log.Info().Str("version", version).Msg("migration applied")
	}
	return nil
}
