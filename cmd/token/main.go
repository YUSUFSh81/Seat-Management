package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/YUSUFSh81/Seat-Management/internal/auth"
)

func main() {
	user := flag.String("user", "u1", "user id (becomes the sub claim)")
	role := flag.String("role", "user", "user or admin")
	ttl := flag.Duration("ttl", 24*time.Hour, "token lifetime")
	flag.Parse()

	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		fmt.Fprintln(os.Stderr, "JWT_SECRET is not set")
		os.Exit(1)
	}
	tok, err := auth.Sign([]byte(secret), *user, *role, *ttl)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(tok)
}
