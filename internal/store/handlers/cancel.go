package handlers

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/YUSUFSh81/Seat-Management/internal/auth"
	"github.com/YUSUFSh81/Seat-Management/internal/obs"
	"github.com/YUSUFSh81/Seat-Management/internal/store"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"
)

func Cancel(db *sql.DB) fiber.Handler {
	return func(c *fiber.Ctx) error {
		id, err := strconv.ParseInt(c.Params("id"), 10, 64)
		if err != nil || id < 1 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid reservation id"})
		}

		already, err := store.Cancel(c.UserContext(), db, auth.UserID(c), id)
		switch {
		case err == nil:
			if !already {
				obs.Cancelled.Inc()
			}
			return c.JSON(fiber.Map{"reservation_id": strconv.FormatInt(id, 10), "status": "cancelled", "already_cancelled": already})
		case errors.Is(err, store.ErrReservationNotFound):
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "reservation not found"})
		case errors.Is(err, context.Canceled):
			return nil
		default:
			log.Error().Err(err).Msg("cancel failed")
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}
	}
}
