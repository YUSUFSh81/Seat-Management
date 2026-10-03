package obs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	Confirmed      = promauto.NewCounter(prometheus.CounterOpts{Name: "reservations_confirmed_total", Help: "New reservations confirmed (replays excluded)."})
	SeatsConfirmed = promauto.NewCounter(prometheus.CounterOpts{Name: "seats_confirmed_total", Help: "Seats confirmed by new reservations."})
	Cancelled      = promauto.NewCounter(prometheus.CounterOpts{Name: "reservations_cancelled_total", Help: "Reservations cancelled (repeat cancels excluded)."})
	Declined       = promauto.NewCounterVec(prometheus.CounterOpts{Name: "reservations_declined_total", Help: "Declined or replayed reserve calls, by reason."}, []string{"reason"})
	Retries        = promauto.NewCounterVec(prometheus.CounterOpts{Name: "store_retries_total", Help: "Transaction retries, by reason."}, []string{"reason"})
	HTTPRequests   = promauto.NewCounterVec(prometheus.CounterOpts{Name: "http_requests_total", Help: "HTTP requests."}, []string{"method", "route", "status"})
	HTTPDuration   = promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "http_request_duration_seconds", Help: "HTTP latency.", Buckets: prometheus.DefBuckets}, []string{"route"})
)

// Decline counts a reserve outcome that was not a new confirmation and tags the log line.
// reasons: seat_taken, per_user_limit, idempotent_replay, idempotency_conflict
func Decline(c *fiber.Ctx, reason string) {
	Declined.WithLabelValues(reason).Inc()
	c.Locals("outcome", reason)
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// RequestLogger: request id, one JSON log line per request, HTTP metrics.
// Register it BEFORE recover, so a recovered panic shows up here as a 500.
func RequestLogger() fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		rid := c.Get("X-Request-ID")
		if rid == "" || len(rid) > 64 {
			rid = newID()
		}
		c.Set("X-Request-ID", rid)
		c.Locals("request_id", rid)

		err := c.Next()

		status := c.Response().StatusCode()
		if err != nil {
			status = http.StatusInternalServerError
			var fe *fiber.Error
			if errors.As(err, &fe) {
				status = fe.Code
			}
		}
		route := c.Route().Path
		dur := time.Since(start)
		HTTPRequests.WithLabelValues(c.Method(), route, strconv.Itoa(status)).Inc()
		HTTPDuration.WithLabelValues(route).Observe(dur.Seconds())

		if route == "/healthz" || route == "/readyz" || route == "/metrics" {
			return err // platform probes would flood the logs
		}
		uid, _ := c.Locals("user_id").(string)
		outcome, _ := c.Locals("outcome").(string)
		ev := log.Info()
		if status >= 500 {
			ev = log.Error().Err(err)
		}
		ev.Str("request_id", rid).Str("method", c.Method()).Str("route", route).
			Int("status", status).Int64("latency_ms", dur.Milliseconds()).
			Str("user_id", uid).Str("outcome", outcome).Msg("request")
		return err
	}
}

// SeatsCollector exports seat counts per status, read from the DB at scrape time
// (cached 1s so a scrape cannot add load during a burst). Aggregated over all shows.
type SeatsCollector struct {
	db     *sql.DB
	desc   *prometheus.Desc
	mu     sync.Mutex
	at     time.Time
	counts map[string]float64
}

func NewSeatsCollector(db *sql.DB) *SeatsCollector {
	return &SeatsCollector{
		db:   db,
		desc: prometheus.NewDesc("seats", "Seats by status across all shows.", []string{"status"}, nil),
	}
}

func (s *SeatsCollector) Describe(ch chan<- *prometheus.Desc) { ch <- s.desc }

func (s *SeatsCollector) Collect(ch chan<- prometheus.Metric) {
	s.mu.Lock()
	if s.counts == nil || time.Since(s.at) > time.Second {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		rows, err := s.db.QueryContext(ctx, "SELECT status, COUNT(*) FROM show_seats GROUP BY status")
		if err == nil {
			m := map[string]float64{"available": 0, "held": 0, "confirmed": 0}
			for rows.Next() {
				var st string
				var n float64
				if rows.Scan(&st, &n) == nil {
					m[st] = n
				}
			}
			rows.Close()
			s.counts, s.at = m, time.Now()
		} else {
			log.Error().Err(err).Msg("seats collector query failed")
		}
		cancel()
	}
	counts := s.counts // keep the last good value if the query failed
	s.mu.Unlock()

	for st, n := range counts {
		ch <- prometheus.MustNewConstMetric(s.desc, prometheus.GaugeValue, n, st)
	}
}
