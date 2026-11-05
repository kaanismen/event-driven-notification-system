// Command mockprovider runs the mock delivery provider as an HTTP server, so the full
// stack can be demonstrated and load tested without webhook.site.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kaanismen/event-driven-notification-system/internal/logging"
	"github.com/kaanismen/event-driven-notification-system/internal/provider/mock"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mockprovider:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := os.Getenv("MOCK_ADDR")
	if addr == "" {
		addr = ":8081"
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		level = slog.LevelInfo
	}
	logger := logging.New(os.Stdout, level).With("component", "mockprovider")

	opts, err := mock.ParseOptions(os.Getenv)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           mock.NewLogging(logger, opts),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("mock provider listening", "addr", addr,
			"fail_first", opts.FailFirst, "failure_rate", opts.FailureRate, "failure_status", opts.FailureStatus,
			"retry_after", opts.RetryAfter.String(), "latency", opts.Latency.String())
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}
