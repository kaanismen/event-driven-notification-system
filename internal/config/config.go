// Package config turns environment variables into a typed, validated Config.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Config holds every setting the process needs. It is built once at startup and passed
// down explicitly; nothing reads environment variables after Load returns.
type Config struct {
	HTTPAddr        string
	DatabaseURL     string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	// MaxAttempts is the delivery budget copied into each new notification (ADR-0002).
	MaxAttempts int
}

const (
	defaultHTTPAddr        = ":8080"
	defaultLogLevel        = "info"
	defaultShutdownTimeout = 15 * time.Second
	defaultMaxAttempts     = 8
	// maxMaxAttempts keeps a typo such as 1000 from turning a dead provider into hours
	// of retries per message.
	maxMaxAttempts = 20
)

// Load reads the configuration through getenv (os.Getenv in production, a map lookup in
// tests). It reports every invalid setting at once, so a misconfigured deployment fails
// with one complete message instead of one error per restart.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		HTTPAddr:        stringOr(getenv("HTTP_ADDR"), defaultHTTPAddr),
		DatabaseURL:     strings.TrimSpace(getenv("DATABASE_URL")),
		ShutdownTimeout: defaultShutdownTimeout,
		MaxAttempts:     defaultMaxAttempts,
	}

	var errs []error

	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(stringOr(getenv("LOG_LEVEL"), defaultLogLevel))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}

	if raw := strings.TrimSpace(getenv("SHUTDOWN_TIMEOUT")); raw != "" {
		d, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT: %w", err))
		case d <= 0:
			errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT: must be positive, got %s", d))
		default:
			cfg.ShutdownTimeout = d
		}
	}

	if raw := strings.TrimSpace(getenv("MAX_ATTEMPTS")); raw != "" {
		n, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("MAX_ATTEMPTS: %w", err))
		case n < 1 || n > maxMaxAttempts:
			errs = append(errs, fmt.Errorf("MAX_ATTEMPTS: must be between 1 and %d, got %d", maxMaxAttempts, n))
		default:
			cfg.MaxAttempts = n
		}
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

func stringOr(value, fallback string) string {
	if v := strings.TrimSpace(value); v != "" {
		return v
	}
	return fallback
}
