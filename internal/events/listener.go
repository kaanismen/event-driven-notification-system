package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// Channel is the NOTIFY channel the trigger of migration 000005 announces changes on.
const Channel = "notification_status"

const (
	connectTimeout = 10 * time.Second
	minRetry       = time.Second
	maxRetry       = 30 * time.Second
)

// Listener holds one dedicated database connection that LISTENs for status changes and
// publishes them to the hub. It is a separate connection, not one from the pool: a
// LISTEN belongs to its session, and a pooled connection would be handed to other work.
type Listener struct {
	config   *pgx.ConnConfig
	hub      *Hub
	logger   *slog.Logger
	minRetry time.Duration
}

// NewListener returns a listener that connects with config.
func NewListener(config *pgx.ConnConfig, hub *Hub, logger *slog.Logger) *Listener {
	return &Listener{config: config, hub: hub, logger: logger, minRetry: minRetry}
}

// Run listens until ctx is cancelled. When the connection fails it tells the hub, which
// ends every subscription (events may have been missed), and reconnects with a growing
// delay.
func (l *Listener) Run(ctx context.Context) {
	retry := l.minRetry
	for {
		connected, err := l.listen(ctx)
		l.hub.SetConnected(false)
		if ctx.Err() != nil {
			return
		}
		if connected {
			retry = l.minRetry // it worked for a while: start the backoff from the bottom
		}
		l.logger.Warn("status event listener disconnected; reconnecting",
			"error", err, "retry_in", retry.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
		retry = min(retry*2, maxRetry)
	}
}

// listen runs one connection until it fails. connected reports whether LISTEN succeeded.
func (l *Listener) listen(ctx context.Context) (connected bool, err error) {
	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := pgx.ConnectConfig(connectCtx, l.config)
	cancel()
	if err != nil {
		return false, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return false, fmt.Errorf("listen: %w", err)
	}
	l.hub.SetConnected(true)
	l.logger.Info("listening for status events", "channel", Channel)

	for {
		msg, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true, fmt.Errorf("wait for notification: %w", err)
		}
		var ev Event
		if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil {
			// Only our trigger writes to this channel, so this is a bug, not a client
			// error; one bad message must not stop the stream.
			l.logger.Error("unreadable status event", "payload", msg.Payload, "error", err)
			continue
		}
		l.hub.Publish(ev)
	}
}
