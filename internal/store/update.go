package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MigrationVersion records one row from Goose's migration history.
type MigrationVersion struct {
	Version int64
	Applied bool
}

// MigrationChecksum records one migration checksum stored by Posse.
type MigrationChecksum struct {
	Version int64
	SHA256  string
}

// MigrationState records the migration tables before and after setup so update
// rollback cannot pair an old binary with a newly migrated database.
type MigrationState struct {
	GooseTableExists    bool
	GooseVersions       []MigrationVersion
	ChecksumTableExists bool
	MigrationChecksums  []MigrationChecksum
}

// ReadMigrationState reads a home's migration history without applying migrations.
func ReadMigrationState(ctx context.Context, home string) (MigrationState, error) {
	db, err := OpenReadOnly(home)
	if errors.Is(err, os.ErrNotExist) {
		return MigrationState{}, nil
	}
	if err != nil {
		return MigrationState{}, err
	}
	state, stateErr := db.MigrationState(ctx)
	return state, errors.Join(stateErr, db.Close())
}

// MigrationState reads the migration history from an already-open database.
func (db *DB) MigrationState(ctx context.Context) (MigrationState, error) {
	state := MigrationState{}
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='goose_db_version')`).Scan(&state.GooseTableExists); err != nil {
		return MigrationState{}, err
	}
	if state.GooseTableExists {
		rows, err := db.QueryContext(ctx, `SELECT version_id, is_applied FROM goose_db_version ORDER BY version_id, is_applied`)
		if err != nil {
			return MigrationState{}, err
		}
		for rows.Next() {
			var version int64
			var applied int
			if err := rows.Scan(&version, &applied); err != nil {
				_ = rows.Close()
				return MigrationState{}, err
			}
			state.GooseVersions = append(state.GooseVersions, MigrationVersion{Version: version, Applied: applied != 0})
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return MigrationState{}, err
		}
		if err := rows.Close(); err != nil {
			return MigrationState{}, err
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='posse_migration_checksums')`).Scan(&state.ChecksumTableExists); err != nil {
		return MigrationState{}, err
	}
	if state.ChecksumTableExists {
		rows, err := db.QueryContext(ctx, `SELECT version, sha256 FROM posse_migration_checksums ORDER BY version`)
		if err != nil {
			return MigrationState{}, err
		}
		for rows.Next() {
			var checksum MigrationChecksum
			if err := rows.Scan(&checksum.Version, &checksum.SHA256); err != nil {
				_ = rows.Close()
				return MigrationState{}, err
			}
			state.MigrationChecksums = append(state.MigrationChecksums, checksum)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return MigrationState{}, err
		}
		if err := rows.Close(); err != nil {
			return MigrationState{}, err
		}
	}
	return state, nil
}

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
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE state IN ('spawning','working','needs-decision','blocked','stalled','done','merging')`).Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return &SchemaError{Code: "migration_refused", Message: fmt.Sprintf("migrations %v require no live Tasks; %d Tasks are live", pending, count), Help: "Wait for live Tasks to finish, or run `posse update --force` if interruption is acceptable"}
	}
	return nil
}
