// Command notifier runs the notification API (and, from Phase 3, the delivery workers).
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

	"github.com/kaanismen/insider-assessment/internal/config"
	"github.com/kaanismen/insider-assessment/internal/httpapi"
	"github.com/kaanismen/insider-assessment/internal/logging"
	"github.com/kaanismen/insider-assessment/internal/store"
)

func main() {
	// main only translates run's result into an exit code; keeping the logic in run means
	// deferred cleanup (closing the pool, flushing) still executes, which os.Exit skips.
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "notifier:", err)
		os.Exit(1)
	}
}

func run() error {
	// ctx is cancelled on Ctrl+C or SIGTERM (what `docker stop` sends), which starts the
	// graceful shutdown below.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	logger := logging.New(os.Stdout, cfg.LogLevel)

	pool, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := store.Migrate(cfg.DatabaseURL, logger); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: httpapi.NewHandler(logger, store.New(pool), cfg.MaxAttempts),
		// Explicit timeouts: the zero values mean "wait forever", which lets slow or idle
		// clients hold connections open indefinitely.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	// ListenAndServe blocks, so it runs in its own goroutine and reports back on a
	// buffered channel; the buffer lets that goroutine exit even if nobody reads.
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.HTTPAddr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		// The server stopped on its own (for example, the port is taken).
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown signal received", "timeout", cfg.ShutdownTimeout.String())
	}

	// A fresh context: ctx is already cancelled, but the shutdown itself needs time.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	// Shutdown stops accepting connections and waits for in-flight requests to finish.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	logger.Info("shutdown complete")
	return nil
}
