package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

// ForceMigrationEnv set to 1 lets the User run a build against a schema it
// does not know or that diverged, and apply its migrations anyway.
const ForceMigrationEnv = "POSSE_FORCE_MIGRATION"

// MigrationGate decides whether this process may apply pending migrations to
// an existing database at path. nil allows them. The posse binary sets it.
var MigrationGate func(path string, pending []int64, forced bool) error

// SchemaError is a database whose schema this build must not touch.
type SchemaError struct {
	Code    string // schema_unknown | schema_diverged | migration_refused
	Message string
	Help    string
}

func (e *SchemaError) Error() string { return e.Message }

type migrationSource struct {
	version  int64
	checksum string
}

func embeddedMigrations() ([]migrationSource, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	sources := []migrationSource{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, _ := strings.Cut(name, "_")
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration %s has no version prefix", name)
		}
		data, err := fs.ReadFile(migrationFiles, path.Join("migrations", name))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		sources = append(sources, migrationSource{version: version, checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].version < sources[j].version })
	return sources, nil
}

// checkSchema refuses a database this build does not know or that another
// build migrated differently, and asks MigrationGate before migrating an
// existing database. It runs under the migration lock, before goose.
func (db *DB) checkSchema(ctx context.Context, sources []migrationSource) error {
	var hasTable int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='posse_migration_checksums'`).Scan(&hasTable); err != nil {
		return err
	}
	if hasTable == 0 {
		if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS posse_migration_checksums (version INTEGER PRIMARY KEY, sha256 TEXT NOT NULL)`); err != nil {
			return err
		}
	}
	applied, err := db.appliedMigrations(ctx)
	if err != nil {
		return err
	}
	recorded := map[int64]string{}
	rows, err := db.QueryContext(ctx, `SELECT version, sha256 FROM posse_migration_checksums`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var version int64
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			_ = rows.Close()
			return err
		}
		recorded[version] = checksum
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	forced := os.Getenv(ForceMigrationEnv) == "1"
	known := map[int64]string{}
	for _, source := range sources {
		known[source.version] = source.checksum
	}
	unknown, diverged := []string{}, []string{}
	for version := range applied {
		checksum, ok := known[version]
		if !ok {
			unknown = append(unknown, strconv.FormatInt(version, 10))
		} else if previous, ok := recorded[version]; ok && previous != checksum {
			diverged = append(diverged, strconv.FormatInt(version, 10))
		}
	}
	sort.Strings(unknown)
	sort.Strings(diverged)
	if len(unknown) > 0 && !forced {
		return &SchemaError{Code: "schema_unknown", Message: fmt.Sprintf("%s has migrations this posse build does not know: %s", db.Path, strings.Join(unknown, ", ")),
			Help: "Run the posse build that applied them (the installed `posse`), or set " + ForceMigrationEnv + "=1 to use this build anyway"}
	}
	if len(diverged) > 0 && !forced {
		return &SchemaError{Code: "schema_diverged", Message: fmt.Sprintf("%s was migrated by a posse build whose migrations %s differ from this build's", db.Path, strings.Join(diverged, ", ")),
			Help: "Run the posse build that applied them (the installed `posse`), or set " + ForceMigrationEnv + "=1 to use this build anyway"}
	}
	pending := []int64{}
	for _, source := range sources {
		if !applied[source.version] {
			pending = append(pending, source.version)
		}
	}
	if len(applied) > 0 && len(pending) > 0 && MigrationGate != nil {
		if err := MigrationGate(db.Path, pending, forced); err != nil {
			var schema *SchemaError
			if errors.As(err, &schema) {
				return schema
			}
			return &SchemaError{Code: "migration_refused", Message: err.Error(),
				Help: "Run the installed `posse` to migrate the database, or set " + ForceMigrationEnv + "=1 from the User's shell"}
		}
	}
	return nil
}

// recordChecksums stores, in one transaction, the checksum of every applied
// migration this build knows and that has none yet.
func (db *DB) recordChecksums(ctx context.Context, sources []migrationSource) error {
	applied, err := db.appliedMigrations(ctx)
	if err != nil {
		return err
	}
	recorded := map[int64]bool{}
	rows, err := db.QueryContext(ctx, `SELECT version FROM posse_migration_checksums`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			_ = rows.Close()
			return err
		}
		recorded[version] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	missing := []migrationSource{}
	for _, source := range sources {
		if applied[source.version] && !recorded[source.version] {
			missing = append(missing, source)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, source := range missing {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO posse_migration_checksums(version, sha256) VALUES(?, ?)`, source.version, source.checksum); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) appliedMigrations(ctx context.Context) (map[int64]bool, error) {
	applied := map[int64]bool{}
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='goose_db_version'`).Scan(&exists); err != nil || exists == 0 {
		return applied, err
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT version_id FROM goose_db_version WHERE version_id > 0 AND is_applied`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		applied[version] = true
	}
	return applied, rows.Err()
}
