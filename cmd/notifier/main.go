// Command notifier runs the notification API and the delivery workers in one process.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kaanismen/event-driven-notification-system/internal/config"
	"github.com/kaanismen/event-driven-notification-system/internal/events"
	"github.com/kaanismen/event-driven-notification-system/internal/httpapi"
	"github.com/kaanismen/event-driven-notification-system/internal/logging"
	"github.com/kaanismen/event-driven-notification-system/internal/metrics"
	"github.com/kaanismen/event-driven-notification-system/internal/provider"
	"github.com/kaanismen/event-driven-notification-system/internal/store"
	"github.com/kaanismen/event-driven-notification-system/internal/tracing"
	"github.com/kaanismen/event-driven-notification-system/internal/worker"
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

	pool, err := store.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := store.Migrate(cfg.DatabaseURL, logger); err != nil {
		return err
	}

	st := store.New(pool)
	m := metrics.New()
	m.WatchQueue(st, logger)

	// Tracing (ADR-0021) is configured by the standard OTEL_* variables and is off when no
	// endpoint is set. The tracer is passed to the API and the engine, not set globally.
	tp, stopTracing, err := tracing.Setup(ctx, os.Getenv, logger)
	if err != nil {
		return err
	}
	// Deferred before the engine and the HTTP server start, so it runs after they have
	// stopped: the spans of in-flight work are flushed before the process exits. ctx is
	// cancelled by then, so the flush gets its own deadline.
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := stopTracing(flushCtx); err != nil {
			logger.Warn("tracing: could not flush the last spans", "error", err)
		}
	}()
	tracer := tp.Tracer("github.com/kaanismen/event-driven-notification-system")

	// The engine stops on a shutdown signal or when run returns for any other reason
	// (deferred stopEngine), for example because the HTTP server failed to start.
	engineCtx, stopEngine := context.WithCancel(ctx)
	defer stopEngine()
	engine := worker.New(st, provider.NewClient(cfg.ProviderURL, cfg.ProviderTimeout), logger, m, cfg.Worker, tracer)
	engineDone := make(chan struct{})
	go func() {
		engine.Run(engineCtx)
		close(engineDone)
	}()
	// Only the host is logged: the full webhook.site URL works like a password.
	logger.Info("delivery engine started", "provider_host", hostOf(cfg.ProviderURL))

	// Real-time status (ADR-0020): one dedicated connection LISTENs for the trigger's
	// events and the hub fans them out to WebSocket subscribers. The listener has its own
	// context, cancelled only when run returns: if it stopped with the signal like the
	// engine, open connections would be told "events missed, reconnect" (1012) instead of
	// the "going away" (1001) that hub.Close sends when the HTTP server shuts down.
	hub := events.NewHub(m, events.DefaultBuffer)
	listenerCtx, stopListener := context.WithCancel(context.Background())
	defer stopListener()
	go events.NewListener(pool.Config().ConnConfig.Copy(), hub, logger).Run(listenerCtx)

	// draining flips to true on the shutdown signal, so readiness fails before the
	// listener closes (ADR-0016).
	var draining atomic.Bool
	ready := func(ctx context.Context) error {
		if draining.Load() {
			return errors.New("shutting down")
		}
		if err := pool.Ping(ctx); err != nil {
			// The reason is shown to callers, so the driver's error stays in the logs.
			logger.WarnContext(ctx, "readiness check: database ping failed", "error", err)
			return errors.New("database unreachable")
		}
		return nil
	}

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.NewHandler(httpapi.Deps{
			Logger: logger, Store: st, MaxAttempts: cfg.MaxAttempts, MaxScheduleAhead: cfg.MaxScheduleAhead,
			Ready: ready, Metrics: m.Handler(), Events: hub, Tracer: tracer,
		}),
		// Explicit timeouts: the zero values mean "wait forever", which lets slow or idle
		// clients hold connections open indefinitely.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	// Server.Shutdown neither waits for nor closes WebSocket connections, because they have
	// been hijacked out of its control; closing the hub when shutdown starts ends them.
	srv.RegisterOnShutdown(hub.Close)

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
		stopEngine()
		<-engineDone
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown signal received", "timeout", cfg.ShutdownTimeout.String())
	}

	// Fail readiness first and keep serving for a moment: a load balancer polling
	// /readyz then stops sending new requests here before the listener closes, instead
	// of seeing refused connections. The engine already stopped claiming.
	draining.Store(true)
	if cfg.ShutdownDrainDelay > 0 {
		logger.Info("readiness now failing; waiting before closing the listener", "delay", cfg.ShutdownDrainDelay.String())
		time.Sleep(cfg.ShutdownDrainDelay)
	}

	// A fresh context: ctx is already cancelled, but the shutdown itself needs time.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	// The engine already stopped claiming when ctx was cancelled. Shutdown stops accepting
	// connections and waits for in-flight requests to finish.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}

	// Wait for in-flight sends to record their results before the deferred pool.Close.
	select {
	case <-engineDone:
	case <-shutdownCtx.Done():
		logger.Warn("delivery engine did not stop within the shutdown timeout")
		return errors.New("graceful shutdown: delivery engine did not stop in time")
	}
	logger.Info("shutdown complete")
	return nil
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "invalid"
	}
	return u.Host
}
