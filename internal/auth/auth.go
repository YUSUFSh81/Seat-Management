package auth

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
)

type Claims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// Sign creates a token for a user. Used by cmd/token and the burst tool.
func Sign(secret []byte, userID, role string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := Claims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}

// Middleware verifies the bearer token and stores identity in Locals.
// Identity comes ONLY from here, never from the request body.
func Middleware(secret []byte) fiber.Handler {
	return func(c *fiber.Ctx) error {
		h := c.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			return unauthorized(c)
		}
		var claims Claims
		tok, err := jwt.ParseWithClaims(
			strings.TrimPrefix(h, "Bearer "),
			&claims,
			func(*jwt.Token) (any, error) { return secret, nil },
			jwt.WithValidMethods([]string{"HS256"}), // rejects "alg: none" and other algorithms
			jwt.WithExpirationRequired(),
		)
		if err != nil || !tok.Valid || claims.Subject == "" {
			return unauthorized(c)
		}
		c.Locals("user_id", claims.Subject)
		c.Locals("role", claims.Role)
		return c.Next()
	}
}

func RequireAdmin() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if role, _ := c.Locals("role").(string); role != "admin" {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		return c.Next()
	}
}

func UserID(c *fiber.Ctx) string {
	id, _ := c.Locals("user_id").(string)
	return id
}

func unauthorized(c *fiber.Ctx) error {
	return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
}
