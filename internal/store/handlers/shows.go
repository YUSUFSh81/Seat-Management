package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/YUSUFSh81/Seat-Management/internal/store"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"
)

type CreateShowReq struct {
	Name         string   `json:"name"`
	Seats        []string `json:"seats"`
	PricePaise   int64    `json:"price_paise"`
	PerUserLimit *int     `json:"per_user_limit"`
}

type CreateShowRes struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	PricePaise int64  `json:"price_paise"`
	TotalSeats int    `json:"total_seats"`
	Seats      []Seat `json:"seats"`
}

type Seat struct {
	Seat   string `json:"seat"`
	Status string `json:"status"`
}

const (
	maxSeats           = 50000
	maxSeatsPerRequest = 100
	maxNameLen         = 255
)

func badRequest(c *fiber.Ctx, msg string, extra fiber.Map) error {
	body := fiber.Map{"error": msg}
	for k, v := range extra {
		body[k] = v
	}
	return c.Status(fiber.StatusBadRequest).JSON(body)
}

func CreateShow(db *sql.DB) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var req CreateShowReq
		if err := json.Unmarshal(c.Body(), &req); err != nil {
			return badRequest(c, "invalid JSON body", nil)
		}

		name := strings.TrimSpace(req.Name)
		if name == "" {
			return badRequest(c, "name not provided", nil)
		}
		if utf8.RuneCountInString(name) > maxNameLen {
			return badRequest(c, "name too long", fiber.Map{"max": maxNameLen})
		}
		if req.PricePaise <= 0 {
			return badRequest(c, "price must be greater than 0", nil)
		}

		limit := 4
		if req.PerUserLimit != nil {
			limit = *req.PerUserLimit
		}
		if limit < 1 || limit > maxSeatsPerRequest {
			return badRequest(c, "per_user_limit out of range", fiber.Map{"min": 1, "max": maxSeatsPerRequest})
		}

		n := len(req.Seats)
		if n == 0 {
			return badRequest(c, "seats not provided", nil)
		}
		if n > maxSeats {
			return badRequest(c, "too many seats", fiber.Map{"max": maxSeats, "got": n})
		}

		seen := make(map[string]struct{}, n)
		cleanSeats := make([]string, 0, n)
		seats := make([]Seat, 0, n)
		for i, raw := range req.Seats {
			seat := strings.TrimSpace(strings.ToUpper(raw))
			if seat == "" {
				return badRequest(c, "empty seat label", fiber.Map{"index": i})
			}
			if !seatRe.MatchString(seat) {
				return badRequest(c, "invalid seat label", fiber.Map{"index": i, "seat": seat})
			}
			if _, dup := seen[seat]; dup {
				return badRequest(c, "duplicate seat label", fiber.Map{"seat": seat})
			}
			seen[seat] = struct{}{}
			cleanSeats = append(cleanSeats, seat)
			seats = append(seats, Seat{Seat: seat, Status: "available"})
		}

		showID, err := store.CreateShow(c.UserContext(), db, store.CreateShowInput{
			Name:         name,
			Seats:        cleanSeats,
			PricePaise:   req.PricePaise,
			PerUserLimit: limit,
		})
		if err != nil {
			log.Error().Err(err).Msg("failed to create show")
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create show"})
		}

		return c.Status(fiber.StatusCreated).JSON(CreateShowRes{
			ID:         showID,
			Name:       name,
			PricePaise: req.PricePaise,
			TotalSeats: len(cleanSeats),
			Seats:      seats,
		})
	}
}

func GetShow(db *sql.DB) fiber.Handler {
	return func(c *fiber.Ctx) error {
		id, err := strconv.ParseInt(c.Params("id"), 10, 64)
		if err != nil || id < 1 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid show id"})
		}
		s, err := store.GetShow(c.UserContext(), db, id, c.QueryBool("include_seats", true))
		switch {
		case err == nil:
			return c.JSON(s)
		case errors.Is(err, store.ErrShowNotFound):
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "show not found"})
		case errors.Is(err, context.Canceled):
			return nil
		default:
			log.Error().Err(err).Msg("get show failed")
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}
	}
}
