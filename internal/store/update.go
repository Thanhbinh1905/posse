package store

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
)

// PendingIdleMigrations reports pending migrations explicitly marked as unsafe
// while Tasks are live. New migrations needing this gate must start with
// "-- posse: requires-no-live-tasks".
func (db *DB) PendingIdleMigrations(ctx context.Context) ([]int64, error) {
	applied, err := db.appliedMigrations(ctx)
	if err != nil {
		return nil, err
	}
	sources, err := embeddedMigrations()
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	idle := map[int64]bool{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		data, err := migrationFiles.ReadFile(filepath.Join("migrations", entry.Name()))
		if err != nil {
			return nil, err
		}
		if strings.Contains(string(data), "-- posse: requires-no-live-tasks") {
			version, err := strconv.ParseInt(strings.SplitN(entry.Name(), "_", 2)[0], 10, 64)
			if err != nil {
				return nil, err
			}
			idle[version] = true
		}
	}
	pending := []int64{}
	for _, source := range sources {
		if idle[source.version] && !applied[source.version] {
			pending = append(pending, source.version)
		}
	}
	return pending, nil
}

// RefuseLiveMigration is also called before installing a downloaded build so
// refusal never leaves the installed binary pointing at a migration it cannot run.
func (db *DB) RefuseLiveMigration(ctx context.Context, pending []int64, force bool) error {
	if force || len(pending) == 0 {
		return nil
	}
	var count int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE state IN ('spawning','working','needs-decision','blocked','stalled','done','landing')`).Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return &SchemaError{Code: "migration_refused", Message: fmt.Sprintf("migrations %v require no live Tasks; %d Tasks are live", pending, count), Help: "Wait for live Tasks to finish, or run `posse update --force` if interruption is acceptable"}
	}
	return nil
}
