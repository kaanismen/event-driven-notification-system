// Package testdb gives integration tests their own empty PostgreSQL database.
//
// `go test ./...` runs test packages in parallel, so tests must never share a schema.
// Each call creates a fresh database on the server named by TEST_DATABASE_URL and drops
// it when the test ends. Without TEST_DATABASE_URL the test is skipped, so plain
// `go test ./...` still runs every unit test on a machine without Docker.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// EnvVar names the admin connection URL of the test server.
const EnvVar = "TEST_DATABASE_URL"

// New creates an empty database and returns its connection URL.
func New(t *testing.T) string {
	t.Helper()

	adminURL := os.Getenv(EnvVar)
	if adminURL == "" {
		t.Skipf("%s not set; skipping integration test (run `make test`)", EnvVar)
	}

	name := "test_" + randomSuffix(t)
	exec(t, adminURL, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	t.Cleanup(func() {
		// FORCE closes connections the test forgot to close, so cleanup cannot hang.
		exec(t, adminURL, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvVar, err)
	}
	u.Path = "/" + name
	return u.String()
}

func exec(t *testing.T, connURL, sql string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, connURL)
	if err != nil {
		t.Fatalf("connect to test server: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(b)
}
