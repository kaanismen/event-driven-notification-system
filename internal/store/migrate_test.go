package store

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kaanismen/insider-assessment/internal/testdb"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestMigrateURL(t *testing.T) {
	got, err := migrateURL("postgres://app:secret@db:5432/notifier?sslmode=disable")
	if err != nil {
		t.Fatalf("migrateURL: %v", err)
	}
	if want := "pgx5://app:secret@db:5432/notifier?sslmode=disable"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	for _, bad := range []string{"host=db password=secret", "mysql://app:secret@db/x", "::"} {
		_, err := migrateURL(bad)
		if err == nil {
			t.Errorf("migrateURL(%q) succeeded, want error", bad)
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("error leaks the password: %v", err)
		}
	}
}

func TestOpenDoesNotLeakPassword(t *testing.T) {
	_, err := Open(context.Background(), "postgres://app:secret@db:notaport/x")
	if err == nil {
		t.Fatal("Open succeeded, want error")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error leaks the password: %v", err)
	}
}

// schemaTables lists every table the migrations create.
var schemaTables = []string{"notifications", "idempotency_keys"}

func TestMigrationsUpDownUp(t *testing.T) {
	dbURL := testdb.New(t)
	ctx := context.Background()

	if err := Migrate(dbURL, discardLogger()); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	// Running again must be a no-op, as on every app restart.
	if err := Migrate(dbURL, discardLogger()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	pool, err := Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()

	for _, table := range schemaTables {
		if !tableExists(t, pool, table) {
			t.Fatalf("%s table missing after up", table)
		}
	}
	assertConstraints(t, pool)

	m, err := newMigrator(dbURL)
	if err != nil {
		t.Fatalf("newMigrator: %v", err)
	}
	if err := m.Down(); err != nil {
		t.Fatalf("Down: %v", err)
	}
	closeMigrator(m, discardLogger())

	for _, table := range schemaTables {
		if tableExists(t, pool, table) {
			t.Fatalf("%s table still exists after down", table)
		}
	}

	if err := Migrate(dbURL, discardLogger()); err != nil {
		t.Fatalf("Migrate after down: %v", err)
	}
	for _, table := range schemaTables {
		if !tableExists(t, pool, table) {
			t.Fatalf("%s table missing after second up", table)
		}
	}
}

// assertConstraints checks that the schema rejects states the application must never
// create, so a bug in later phases fails loudly instead of corrupting the queue.
func assertConstraints(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	const insert = `INSERT INTO notifications (id, channel, recipient, content, max_attempts, priority, status, locked_until)
		VALUES (gen_random_uuid(), $1, 'r', 'c', 8, $2, $3, $4)`

	if _, err := pool.Exec(ctx, insert, "sms", 2, "pending", nil); err != nil {
		t.Fatalf("valid insert rejected: %v", err)
	}

	invalid := []struct {
		name     string
		channel  string
		priority int
		status   string
	}{
		{"unknown channel", "fax", 2, "pending"},
		{"priority out of range", "sms", 4, "pending"},
		{"unknown status", "sms", 2, "lost"},
		{"processing without lease", "sms", 2, "processing"},
	}
	for _, tt := range invalid {
		if _, err := pool.Exec(ctx, insert, tt.channel, tt.priority, tt.status, nil); err == nil {
			t.Errorf("%s: insert accepted, want check violation", tt.name)
		}
	}
}

func tableExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(context.Background(),
		"SELECT to_regclass($1) IS NOT NULL", "public."+name).Scan(&exists)
	if err != nil {
		t.Fatalf("check table %s: %v", name, err)
	}
	return exists
}
