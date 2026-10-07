package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// legacyProjectUUIDMigrationChecksum identifies the project UUID migration
// that briefly shipped as version 32 before main used that version.
const (
	legacyProjectUUIDMigrationVersion  = 32
	projectUUIDMigrationVersion        = 33
	legacyProjectUUIDMigrationChecksum = "e6708eea3f1d552952f3566064aa362d32c6ab8333122e549c2153c0036047c1"
)

// normalizeLegacyProjectUUIDMigration moves the earlier Project UUID
// migration from version 32 to 33 so version 32 can be applied to homes that
// need the publish-pre-push-heads table. It runs under the migration lock.
func (db *DB) normalizeLegacyProjectUUIDMigration(ctx context.Context) error {
	var hasGoose, hasChecksums int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='goose_db_version')`).Scan(&hasGoose); err != nil {
		return err
	}
	if hasGoose == 0 {
		return nil
	}
	var legacyApplied, hasUUIDColumn, hasUUIDIndex, hasPublishHeads int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id=? AND is_applied=1)`, legacyProjectUUIDMigrationVersion).Scan(&legacyApplied); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('projects') WHERE name='project_uuid')`).Scan(&hasUUIDColumn); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='index' AND name='projects_project_uuid_idx')`).Scan(&hasUUIDIndex); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='publish_pre_push_heads')`).Scan(&hasPublishHeads); err != nil {
		return err
	}
	if legacyApplied == 0 || hasUUIDColumn == 0 || hasUUIDIndex == 0 || hasPublishHeads != 0 {
		return nil
	}
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='posse_migration_checksums')`).Scan(&hasChecksums); err != nil {
		return err
	}
	checksum := ""
	checksumPresent := false
	if hasChecksums != 0 {
		var recorded sql.NullString
		err := db.QueryRowContext(ctx, `SELECT sha256 FROM posse_migration_checksums WHERE version=?`, legacyProjectUUIDMigrationVersion).Scan(&recorded)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if recorded.Valid {
			checksum, checksumPresent = recorded.String, true
		}
	}
	if checksumPresent && checksum != legacyProjectUUIDMigrationChecksum {
		return nil
	}
	sources, err := embeddedMigrations()
	if err != nil {
		return err
	}
	currentUUIDChecksum := ""
	for _, source := range sources {
		if source.version == projectUUIDMigrationVersion {
			currentUUIDChecksum = source.checksum
			break
		}
	}
	if currentUUIDChecksum == "" {
		return fmt.Errorf("Project UUID migration version %d is missing", projectUUIDMigrationVersion)
	}
	var hasVersion33 int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id=?)`, projectUUIDMigrationVersion).Scan(&hasVersion33); err != nil {
		return err
	}
	if hasVersion33 != 0 {
		return nil
	}
	if hasChecksums != 0 {
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM posse_migration_checksums WHERE version=?)`, projectUUIDMigrationVersion).Scan(&hasVersion33); err != nil {
			return err
		}
		if hasVersion33 != 0 {
			return nil
		}
	}
	if MigrationGate != nil {
		if err := MigrationGate(db.Path, []int64{legacyProjectUUIDMigrationVersion}, os.Getenv(ForceMigrationEnv) == "1"); err != nil {
			return err
		}
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS posse_migration_checksums (version INTEGER PRIMARY KEY, sha256 TEXT NOT NULL)`); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE goose_db_version SET version_id=? WHERE version_id=? AND is_applied=1`, projectUUIDMigrationVersion, legacyProjectUUIDMigrationVersion)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("normalize legacy Project UUID migration: updated %d applied Goose rows, want 1", changed)
	}
	if checksumPresent {
		if _, err := tx.ExecContext(ctx, `UPDATE posse_migration_checksums SET version=?,sha256=? WHERE version=?`, projectUUIDMigrationVersion, currentUUIDChecksum, legacyProjectUUIDMigrationVersion); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `INSERT INTO posse_migration_checksums(version,sha256) VALUES(?,?)`, projectUUIDMigrationVersion, currentUUIDChecksum); err != nil {
		return err
	}
	return tx.Commit()
}
