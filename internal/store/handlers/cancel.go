package handlers

import (
	"database/sql"
	"strconv"

	"github.com/gofiber/fiber/v2"
)

func Cancel(db *sql.DB) fiber.Handler {
	return func(c *fiber.Ctx) error {
		id, err := strconv.ParseInt(c.Params("id"), 10, 64)
		if err != nil || id < 1 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid reservation id"})
		}

		return nil
	}
}
