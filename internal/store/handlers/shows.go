package handlers

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

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
	SeatLabel string `json:"seat_label"`
	Status    string `json:"status"`
}

func CreateShow(db *sql.DB) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var requestBody CreateShowReq
		if err := c.BodyParser(&requestBody); err != nil {
			return c.Status(fiber.ErrBadRequest.Code).JSON(fiber.Map{"error": "invalid req body"})
		}

		limit := 4
		if requestBody.PerUserLimit != nil {
			limit = *requestBody.PerUserLimit
		}

		// TODO: create the show and seats in DB
		if strings.TrimSpace(requestBody.Name) == "" {
			return c.Status(fiber.ErrBadRequest.Code).JSON(fiber.Map{"error": "name not provided"})
		}
		if requestBody.PricePaise <= 0 {
			return c.Status(fiber.ErrBadRequest.Code).JSON(fiber.Map{"error": "price must be greater than 0"})
		}
		const maxSeats = 50000

		n := len(requestBody.Seats)
		if n == 0 {
			return c.Status(fiber.ErrBadRequest.Code).JSON(fiber.Map{"error": "seats not provided"})
		}
		if n > maxSeats {
			return c.Status(fiber.ErrBadRequest.Code).JSON(fiber.Map{"error": "too many seats", "max": maxSeats, "got": n})
		}
		duplicateSeats := make(map[string]struct{}, n)
		cleanSeats := make([]string, 0, n)
		seats := make([]Seat, 0, n)

		for _, rawSeat := range requestBody.Seats {
			seat := strings.TrimSpace(strings.ToUpper(rawSeat))
			if seat == "" {
				return c.Status(fiber.ErrBadRequest.Code).JSON(fiber.Map{"error": "seat cannot be empty"})
			}
			if len(seat) > 16 || strings.ContainsAny(seat, " \t\r\n") {
				return c.Status(fiber.ErrBadRequest.Code).JSON(fiber.Map{"error": "invalid seat label"})
			}

			if _, ok := duplicateSeats[seat]; ok {
				return c.Status(fiber.ErrBadRequest.Code).JSON(fiber.Map{"error": "duplicate seats provided"})
			}
			duplicateSeats[seat] = struct{}{}

			cleanSeats = append(cleanSeats, seat)
			seats = append(seats, Seat{
				SeatLabel: seat,
				Status:    "available",
			})

		}

		in := store.CreateShowInput{
			Name:         requestBody.Name,
			Seats:        cleanSeats,
			PricePaise:   requestBody.PricePaise,
			PerUserLimit: limit,
		}
		showID, err := store.CreateShow(c.UserContext(), db, in)
		if err != nil {
			log.Error().Err(err).Msg("failed to create show")
			return c.Status(fiber.ErrInternalServerError.Code).JSON(fiber.Map{"error": "failed to create show"})
		}

		response := CreateShowRes{
			ID:         showID,
			Name:       requestBody.Name,
			PricePaise: requestBody.PricePaise,
			TotalSeats: len(cleanSeats),
			Seats:      seats,
		}

		return c.Status(fiber.StatusCreated).JSON(response)
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
