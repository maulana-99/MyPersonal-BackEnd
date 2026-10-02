package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Port             string
	Env              string
	DatabaseURL      string
	RedisAddr        string
	RedisPassword    string
	JWTSecret        string
	JWTAccessExpiry  time.Duration
	JWTRefreshExpiry time.Duration
}

// Load reads configuration from the environment and validates it.
// Returns an error listing every problem at once, so one restart fixes all.
//
// Two kinds of keys, deliberately distinct:
//   - required: plain os.Getenv, no fallback — a missing value is a hard error.
//   - optional: getEnv, where "" is treated as unset and a sane default applies.
func Load() (*Config, error) {
	_ = godotenv.Load() // ignore if .env missing (prod uses real env vars)

	var problems []error

	port := getEnv("PORT", "8080")
	if n, err := strconv.Atoi(port); err != nil {
		problems = append(problems, fmt.Errorf("PORT must be numeric, got %q", port))
	} else if n < 1 || n > 65535 {
		problems = append(problems, fmt.Errorf("PORT must be within 1-65535, got %d", n))
	}

	// Required: no default exists that would actually work.
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		problems = append(problems, errors.New("DATABASE_URL must be set"))
	}

	jwtSecret := os.Getenv("JWT_SECRET")
	switch {
	case jwtSecret == "":
		problems = append(problems, errors.New("JWT_SECRET must be set"))
	case len(jwtSecret) < 32:
		problems = append(problems, errors.New("JWT_SECRET must be at least 32 characters"))
	}

	// Optional: a wrong value only makes ENV misleading, not the app broken.
	env := getEnv("ENV", "development")
	if env != "development" && env != "production" {
		problems = append(problems, fmt.Errorf("ENV must be development or production, got %q", env))
	}

	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}

	accessExpiry := parseDuration("JWT_ACCESS_EXPIRY", 15*time.Minute)
	refreshExpiry := parseDuration("JWT_REFRESH_EXPIRY", 7*24*time.Hour)

	return &Config{
		Port:             port,
		Env:              env,
		DatabaseURL:      databaseURL,
		RedisAddr:        getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:    os.Getenv("REDIS_PASSWORD"),
		JWTSecret:        jwtSecret,
		JWTAccessExpiry:  accessExpiry,
		JWTRefreshExpiry: refreshExpiry,
	}, nil
}

// parseDuration falls back to the default on unset or malformed input — an
// unparseable expiry should not take the API down.
func parseDuration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
