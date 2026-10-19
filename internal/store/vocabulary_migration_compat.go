package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// The unreleased vocabulary branch used 53 before main published Watch runtime
// and Task containment as 53/54. Only that exact receipt can move to 55.
const legacyVocabularyMigrationChecksum = "b974a5f9ae9d6341d5192d7217f552f0f2e673682992ad9c0e3449ff569779e3"

func (db *DB) normalizeLegacyVocabularyMigration(ctx context.Context) error {
	var ready int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('goose_db_version','posse_migration_checksums')`).Scan(&ready); err != nil || ready != 2 {
		return err
	}
	var checksum string
	if err := db.QueryRowContext(ctx, `SELECT sha256 FROM posse_migration_checksums WHERE version=53`).Scan(&checksum); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if checksum != legacyVocabularyMigrationChecksum {
		return nil
	}
	var applied, destination int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM goose_db_version WHERE version_id=53 AND is_applied=1`).Scan(&applied); err != nil {
		return err
	}
	if applied != 1 {
		return fmt.Errorf("legacy vocabulary migration has %d applied receipts, want 1", applied)
	}
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id=55) OR EXISTS(SELECT 1 FROM posse_migration_checksums WHERE version=55)`).Scan(&destination); err != nil {
		return err
	}
	if destination != 0 {
		return fmt.Errorf("legacy vocabulary migration conflicts with version 55")
	}
	sources, err := embeddedMigrations()
	if err != nil {
		return err
	}
	found := false
	for _, source := range sources {
		if source.version == 55 {
			found = source.checksum == checksum
		}
	}
	if !found {
		return fmt.Errorf("vocabulary migration version 55 differs from its original contents")
	}
	if MigrationGate != nil {
		if err := MigrationGate(db.Path, []int64{53}, os.Getenv(ForceMigrationEnv) == "1"); err != nil {
			return err
		}
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE goose_db_version SET version_id=55 WHERE version_id=53`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE posse_migration_checksums SET version=55 WHERE version=53 AND sha256=?`, checksum); err != nil {
		return err
	}
	return tx.Commit()
}
