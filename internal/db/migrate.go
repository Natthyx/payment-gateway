package db

import (
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/rs/zerolog/log"
)

// RunMigrations applies all pending migrations using the standard
// golang-migrate library. Safe to invoke on every boot — already-applied
// migrations are no-ops.
//
// sourcePath: "file://migrations"
// databaseURL: must include schema prefix ("postgres://...") with sslmode set
func RunMigrations(sourcePath, databaseURL string) error {
	m, err := migrate.New(sourcePath, databaseURL)
	if err != nil {
		return fmt.Errorf("init migrator: %w", err)
	}
	defer func() {
		if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
			log.Warn().Errs("close_errors", []error{srcErr, dbErr}).Msg("migrator close had errors")
		}
	}()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("running migrations: %w", err)
	}
	v, dirty, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("read migration version: %w", err)
	}
	log.Info().Uint("version", v).Bool("dirty", dirty).Msg("migrations applied")
	return nil
}
