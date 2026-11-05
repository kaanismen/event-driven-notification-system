package store

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"

	"github.com/golang-migrate/migrate/v4"
	// Blank import: the package registers the "pgx5" database driver in its init function.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/kaanismen/event-driven-notification-system/migrations"
)

// Migrate applies every pending up migration. golang-migrate holds a Postgres advisory
// lock while it runs, so several instances starting at once apply each migration exactly
// once (ADR-0011).
func Migrate(databaseURL string, logger *slog.Logger) error {
	m, err := newMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer closeMigrator(m, logger)

	switch err := m.Up(); {
	case errors.Is(err, migrate.ErrNoChange):
		// Nothing to do: the schema is current.
	case err != nil:
		return fmt.Errorf("apply migrations: %w", err)
	}

	version, dirty, err := m.Version()
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	logger.Info("database schema is current", "version", version, "dirty", dirty)
	return nil
}

func newMigrator(databaseURL string) (*migrate.Migrate, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("load embedded migrations: %w", err)
	}

	dbURL, err := migrateURL(databaseURL)
	if err != nil {
		return nil, err
	}

	m, err := migrate.NewWithSourceInstance("iofs", src, dbURL)
	if err != nil {
		return nil, fmt.Errorf("open migration target: %w", err)
	}
	return m, nil
}

// migrateURL rewrites postgres:// to pgx5://, the scheme under which golang-migrate
// registers its pgx v5 driver. The driver turns it back into postgres:// to connect.
func migrateURL(databaseURL string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		// The raw value is not repeated: it contains the password.
		return "", errors.New("DATABASE_URL must be a postgres:// URL")
	}
	u.Scheme = "pgx5"
	return u.String(), nil
}

func closeMigrator(m *migrate.Migrate, logger *slog.Logger) {
	srcErr, dbErr := m.Close()
	if err := errors.Join(srcErr, dbErr); err != nil {
		logger.Warn("close migrator", "error", err)
	}
}
