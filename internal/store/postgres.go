// Package store is the only package that talks to PostgreSQL.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// connectTimeout bounds the startup check. Without it, a wrong host makes the process hang
// instead of failing with a clear error.
const connectTimeout = 10 * time.Second

// Open creates a connection pool of at most maxConns connections (0 keeps the pgx
// default) and verifies that the database answers.
func Open(ctx context.Context, databaseURL string, maxConns int) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// Do not wrap err: pgx includes the full connection string, password and all.
		return nil, fmt.Errorf("parse DATABASE_URL: invalid connection string")
	}

	if maxConns > 0 {
		cfg.MaxConns = int32(min(maxConns, 1000)) //nolint:gosec // bounded above, cannot overflow
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}
