package handlers

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/YUSUFSh81/Seat-Management/internal/auth"
	"github.com/YUSUFSh81/Seat-Management/internal/obs"
	"github.com/YUSUFSh81/Seat-Management/internal/store"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"
)

var seatRe = regexp.MustCompile(`^[A-Z0-9-]{1,16}$`)

type ReserveSeatReq struct {
	IdempotencyKey string   `json:"idempotency_key"`
	Seats          []string `json:"seats"`
}

type ReserveSeatRes struct {
	ReservationID int64    `json:"reservation_id,string"`
	ShowID        int64    `json:"show_id,string"`
	UserID        string   `json:"user_id"`
	Seats         []string `json:"seats"`
	AmountPaise   int64    `json:"amount_paise"`
	Status        string   `json:"status"`
}

type showInfo struct {
	price int64
	limit int
}

var showCache sync.Map // show id -> showInfo; only found shows are cached, never "not found"

func loadShow(ctx context.Context, db *sql.DB, id int64) (showInfo, error) {
	if v, ok := showCache.Load(id); ok {
		return v.(showInfo), nil
	}
	var s showInfo
	err := db.QueryRowContext(ctx, "SELECT price_paise, per_user_limit FROM shows WHERE id = ?", id).Scan(&s.price, &s.limit)
	if err != nil {
		return showInfo{}, err // includes sql.ErrNoRows, which the handler maps to 404
	}
	showCache.Store(id, s)
	return s, nil
}

func Reserve(db *sql.DB) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userId := auth.UserID(c)
		showId := c.Params("show_id")
		if showId == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request"})
		}
		sId, err := strconv.ParseInt(showId, 10, 64)
		if err != nil || sId < 1 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request"})
		}
		var req ReserveSeatReq
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request"})
		}

		const maxSeatsPerRequest = 100

		if len(req.Seats) == 0 || len(req.Seats) > maxSeatsPerRequest {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid number of seats"})
		}

		// slices.Sort(req.Seats)
		if h := c.Get("Idempotency-Key"); h != "" {
			if req.IdempotencyKey != "" && req.IdempotencyKey != h {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "idempotency key in header and body differ"})
			}
			req.IdempotencyKey = h
		}

		if len(req.IdempotencyKey) < 1 || len(req.IdempotencyKey) > 128 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid idempotency key"})
		}

		n := len(req.Seats)
		duplicateSeats := make(map[string]struct{}, n)
		cleanSeats := make([]string, 0, n)

		for _, seat := range req.Seats {
			seat := strings.TrimSpace(strings.ToUpper(seat))
			if seat == "" {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "seat cannot be empty"})
			}
			if !seatRe.MatchString(seat) {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid seat label", "seat": seat})
			}
			if _, ok := duplicateSeats[seat]; ok {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "duplicate seats provided"})
			}
			duplicateSeats[seat] = struct{}{}
			cleanSeats = append(cleanSeats, seat)
		}

		info, err := loadShow(c.UserContext(), db, sId)
		if err != nil {
			if err == sql.ErrNoRows {
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "show does not exist"})
			}
			log.Error().Err(err).Msg("failed to check if show exists")
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}

		if len(cleanSeats) > info.limit {
			obs.Decline(c, "per_user_limit")
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "exceeded per user limit"})
		}

		reserveData, replay, err := store.Reserve(c.UserContext(), db, userId, int64(sId), cleanSeats, req.IdempotencyKey, int64(len(cleanSeats))*info.price, info.limit)
		var su *store.SeatsUnavailableError
		if err != nil {
			switch {
			case errors.As(err, &su):
				obs.Decline(c, "seat_taken")
				return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "seats unavailable", "seats": su.Seats})
			case errors.Is(err, store.ErrOverLimit):
				obs.Decline(c, "per_user_limit")
				return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "exceeded per user limit"})
			case errors.Is(err, store.ErrIdempotencyConflict):
				obs.Decline(c, "idempotency_conflict")
				return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "idempotency key conflict"})

			case errors.Is(err, context.Canceled):
				log.Warn().Msg("request canceled")
				return nil

			default:
				log.Error().Err(err).Msg("failed to reserve seat")
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
			}
		}

		res := ReserveSeatRes{
			ReservationID: reserveData.ReservationID,
			ShowID:        reserveData.ShowID,
			UserID:        reserveData.UserID,
			Seats:         reserveData.Seats,
			AmountPaise:   reserveData.AmountPaise,
			Status:        reserveData.Status,
		}
		if replay {
			obs.Decline(c, "idempotent_replay")
			c.Set("Idempotent-Replay", "true")
		} else {
			obs.Confirmed.Inc()
			obs.SeatsConfirmed.Add(float64(len(reserveData.Seats)))
			c.Locals("outcome", "confirmed")
		}
		return c.Status(fiber.StatusCreated).JSON(res)
	}
}
