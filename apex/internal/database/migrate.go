package database

import (
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

// Migrate runs all pending UP migrations from the given directory.
// We run migrations at startup rather than in a separate job so that:
// 1. The app and schema are always in sync
// 2. Rollback is explicit (you must write and deploy a down migration)
// In high-availability setups, use an advisory lock or separate migration job.
func Migrate(dsn, migrationsPath string) error {
	m, err := migrate.New(
		fmt.Sprintf("file://%s", migrationsPath),
		dsn,
	)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil {
		// ErrNoChange is not a real error — schema is already up-to-date
		if errors.Is(err, migrate.ErrNoChange) {
			return nil
		}
		return fmt.Errorf("run migrations: %w", err)
	}

	return nil
}
