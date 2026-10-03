package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/YUSUFSh81/Seat-Management/internal/auth"
	"github.com/YUSUFSh81/Seat-Management/internal/obs"
	"github.com/YUSUFSh81/Seat-Management/internal/store"
	"github.com/YUSUFSh81/Seat-Management/internal/store/handlers"
	"github.com/go-sql-driver/mysql"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/gofiber/fiber/v2/middleware/adaptor"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func registerTLS() error {
	pem := os.Getenv("DB_CA_CERT")
	if pem == "" {
		if path := os.Getenv("DB_CA_CERT_FILE"); path != "" {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			pem = string(b)
		}
	}
	if pem == "" {
		return nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(pem)) {
		return errors.New("invalid DB_CA_CERT")
	}
	return mysql.RegisterTLSConfig("custom", &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
}

func main() {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	zerolog.TimestampFunc = func() time.Time {
		return time.Now().UTC()
	}
	log.Logger = zerolog.New(os.Stdout).With().Timestamp().Str("service", "seat-api").Logger()

	dsn := mustEnv("DB_DSN")
	port := getEnv("PORT", "8080")

	if err := registerTLS(); err != nil {
		log.Fatal().Err(err).Msg("failed to register TLS for mysql")
	}

	poolSize := getEnvInt("DB_MAX_CONNS", 15)

	db, err := connectDB(dsn, 60*time.Second, poolSize)
	if err != nil {
		log.Fatal().Err(err).Msg("db connect failed")
	}

	hdb, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal().Err(err).Msg("health pool")
	}
	hdb.SetMaxOpenConns(1)
	hdb.SetMaxIdleConns(1)
	hdb.SetConnMaxLifetime(5 * time.Minute)
	defer hdb.Close()

	var maxConns int
	if err := db.QueryRow("SELECT @@max_connections").Scan(&maxConns); err == nil {
		log.Info().Int("db_max_connections", maxConns).Int("pool_size", poolSize).Msg("database limits")
	}

	mctx, mcancel := context.WithTimeout(context.Background(), 60*time.Second)
	if err := store.Migrate(mctx, db); err != nil {
		mcancel()
		log.Fatal().Err(err).Msg("migration failed")
	}
	mcancel()
	defer db.Close()
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ReadTimeout:           10 * time.Second,
		WriteTimeout:          30 * time.Second,
		BodyLimit:             8 * 1024 * 1024, // POST /shows can carry tens of thousands of seats
		Immutable:             true,
	})

	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	app.Get("/readyz", func(c *fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.UserContext(), time.Second)
		defer cancel()
		if err := hdb.PingContext(ctx); err != nil { // was db.PingContext
			return c.Status(fiber.StatusServiceUnavailable).
				JSON(fiber.Map{"status": "not_ready", "dependency": "db"})
		}
		return c.JSON(fiber.Map{"status": "ready"})
	})

	secret := []byte(mustEnv("JWT_SECRET"))

	// temporary route to verify auth, delete later
	// app.Get("/whoami", auth.Middleware(secret), func(c *fiber.Ctx) error {
	// 	return c.JSON(fiber.Map{"user_id": auth.UserID(c), "role": c.Locals("role")})
	// })
	// app.Get("/admin-check", auth.Middleware(secret), auth.RequireAdmin(), func(c *fiber.Ctx) error {
	// 	return c.JSON(fiber.Map{"ok": true})
	// })

	prometheus.MustRegister(obs.NewSeatsCollector(hdb), collectors.NewDBStatsCollector(db, "seatdb"))

	app.Use(obs.RequestLogger()) // outermost
	app.Use(recover.New())       // inside, so panics become errors the logger can see

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.HandlerFor(
		prometheus.DefaultGatherer,
		promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError, ErrorLog: obs.PromErrLog{}},
	)))

	app.Post("/shows", auth.Middleware(secret), auth.RequireAdmin(), handlers.CreateShow(db))
	app.Post("/shows/:show_id/reserve", auth.Middleware(secret), handlers.Reserve(db))
	app.Post("/reservations/:id/cancel", auth.Middleware(secret), handlers.Cancel(db))
	app.Get("/shows/:id", handlers.GetShow(db))

	go func() {
		log.Info().Str("port", port).Msg("listening")
		if err := app.Listen(":" + port); err != nil {
			log.Fatal().Err(err).Msg("listen failed")
		}
	}()

	// Graceful shutdown on SIGTERM / Ctrl+C
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info().Msg("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = app.ShutdownWithContext(ctx)

}

func connectDB(dsn string, maxWait time.Duration, poolSize int) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(poolSize)
	db.SetMaxIdleConns(poolSize)
	db.SetConnMaxLifetime(5 * time.Minute)

	deadline := time.Now().Add(maxWait)
	backoff := 500 * time.Millisecond
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err = db.PingContext(ctx)
		cancel()
		if err == nil {
			return db, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		log.Warn().Err(err).Msg("db not ready, retrying")
		time.Sleep(backoff)
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatal().Str("key", key).Msg("Missing required environment variable")
	}
	return v
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getEnvInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}
