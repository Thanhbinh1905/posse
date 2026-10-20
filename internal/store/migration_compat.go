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
	legacyProjectUUIDMigrationVersion            = 32
	projectUUIDMigrationVersion                  = 33
	legacyProjectUUIDMigrationChecksum           = "e6708eea3f1d552952f3566064aa362d32c6ab8333122e549c2153c0036047c1"
	legacyNoticeDeliveryMigrationVersion         = 32
	noticeDeliveryMigrationVersion               = 34
	legacyNoticeDeliveryMigrationChecksum        = "b2cede33cbe9a5103353f2ed9d1347d0de660d07f3bd13a7d43b4b2c2620705d"
	legacyReportBundleMigrationVersion           = 45
	publishedReportBundleMigrationVersion        = 46
	latestPublishedReportBundleMigrationVersion  = 47
	reportBundleMigrationVersion                 = 48
	legacyTaskContainmentMigrationVersion        = 53
	taskContainmentMigrationVersion              = 54
	taskContainmentMigrationChecksum             = "80f7ce98b714a0d7c2852e843fd6008312fd84c5047cd0999ef7ad845a19a955"
	legacyTaskPaneShellIdentityMigrationVersion  = 56
	taskTurnTokensMigrationVersion               = 56
	taskPaneShellIdentityMigrationVersion        = 59
	legacyTaskPaneShellIdentityMigrationChecksum = "e342aa13224885dd18cf0d364701b35677fe1175aa2a294a23ce05d585cc19fa"
	legacyReportBundleMigrationChecksum          = "ecf94dda04c45b4dc91895987653b885a1802999a0783cda7744b8f61dc2063b"
)

// normalizeLegacyNoticeDeliveryMigration moves the receipt migration from
// version 32 to 34 when opening a database created before main claimed 32.
func (db *DB) normalizeLegacyNoticeDeliveryMigration(ctx context.Context) error {
	var hasGoose int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='goose_db_version')`).Scan(&hasGoose); err != nil {
		return err
	}
	if hasGoose == 0 {
		return nil
	}
	var applied, hasReceipts, hasPublishHeads, hasUUIDColumn int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id=? AND is_applied=1)`, legacyNoticeDeliveryMigrationVersion).Scan(&applied); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='notice_delivery_receipts')`).Scan(&hasReceipts); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='publish_pre_push_heads')`).Scan(&hasPublishHeads); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('projects') WHERE name='project_uuid')`).Scan(&hasUUIDColumn); err != nil {
		return err
	}
	if applied == 0 || hasReceipts == 0 || hasPublishHeads != 0 || hasUUIDColumn != 0 {
		return nil
	}
	var hasChecksums int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='posse_migration_checksums')`).Scan(&hasChecksums); err != nil {
		return err
	}
	checksumPresent := false
	if hasChecksums != 0 {
		var recorded sql.NullString
		err := db.QueryRowContext(ctx, `SELECT sha256 FROM posse_migration_checksums WHERE version=?`, legacyNoticeDeliveryMigrationVersion).Scan(&recorded)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if recorded.Valid {
			checksumPresent = true
			if recorded.String != legacyNoticeDeliveryMigrationChecksum {
				return nil
			}
		}
	}
	sources, err := embeddedMigrations()
	if err != nil {
		return err
	}
	currentChecksum := ""
	for _, source := range sources {
		if source.version == noticeDeliveryMigrationVersion {
			currentChecksum = source.checksum
			break
		}
	}
	if currentChecksum == "" {
		return fmt.Errorf("Notice delivery migration version %d is missing", noticeDeliveryMigrationVersion)
	}
	var alreadyMoved int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id=?)`, noticeDeliveryMigrationVersion).Scan(&alreadyMoved); err != nil {
		return err
	}
	if alreadyMoved != 0 {
		return nil
	}
	if MigrationGate != nil {
		if err := MigrationGate(db.Path, []int64{32, 33}, os.Getenv(ForceMigrationEnv) == "1"); err != nil {
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
	result, err := tx.ExecContext(ctx, `UPDATE goose_db_version SET version_id=? WHERE version_id=? AND is_applied=1`, noticeDeliveryMigrationVersion, legacyNoticeDeliveryMigrationVersion)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("normalize legacy Notice delivery migration: updated %d applied Goose rows, want 1", changed)
	}
	if checksumPresent {
		if _, err := tx.ExecContext(ctx, `UPDATE posse_migration_checksums SET version=?,sha256=? WHERE version=?`, noticeDeliveryMigrationVersion, currentChecksum, legacyNoticeDeliveryMigrationVersion); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `INSERT INTO posse_migration_checksums(version,sha256) VALUES(?,?)`, noticeDeliveryMigrationVersion, currentChecksum); err != nil {
		return err
	}
	return tx.Commit()
}

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

// normalizeLegacyReportBundleMigration moves a published Report migration from
// versions 45, 46, or 47 to version 48. Main uses 45 for Review verdicts, 46
// for resource leases, and 47 for review receipts.
func (db *DB) normalizeLegacyReportBundleMigration(ctx context.Context) error {
	var hasGoose int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='goose_db_version')`).Scan(&hasGoose); err != nil {
		return err
	}
	if hasGoose == 0 {
		return nil
	}
	var reportKind, reportDigest, reportManifest, reportReviews, reportAcceptances, reviewTargets, reviewVerdicts, resourceLeases, reviewReceipts int
	for _, check := range []struct {
		query string
		value *int
	}{
		{`SELECT EXISTS(SELECT 1 FROM pragma_table_info('tasks') WHERE name='report_kind')`, &reportKind},
		{`SELECT EXISTS(SELECT 1 FROM pragma_table_info('tasks') WHERE name='report_digest')`, &reportDigest},
		{`SELECT EXISTS(SELECT 1 FROM pragma_table_info('tasks') WHERE name='report_manifest_json')`, &reportManifest},
		{`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='report_reviews')`, &reportReviews},
		{`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='report_acceptances')`, &reportAcceptances},
		{`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='review_targets')`, &reviewTargets},
		{`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='review_verdicts')`, &reviewVerdicts},
		{`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='resource_leases')`, &resourceLeases},
		{`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='review_receipts')`, &reviewReceipts},
	} {
		if err := db.QueryRowContext(ctx, check.query).Scan(check.value); err != nil {
			return err
		}
	}
	if reportKind == 0 || reportDigest == 0 || reportManifest == 0 || reportReviews == 0 || reportAcceptances == 0 {
		return nil
	}
	var hasChecksums int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='posse_migration_checksums')`).Scan(&hasChecksums); err != nil {
		return err
	}
	var sourceVersion int64
	checksumPresent := false
	for _, candidate := range []int64{legacyReportBundleMigrationVersion, publishedReportBundleMigrationVersion, latestPublishedReportBundleMigrationVersion} {
		var applied int
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id=? AND is_applied=1)`, candidate).Scan(&applied); err != nil {
			return err
		}
		if applied == 0 || candidate == legacyReportBundleMigrationVersion && (reviewTargets != 0 || reviewVerdicts != 0) {
			continue
		}
		candidateChecksumPresent := false
		if hasChecksums != 0 {
			var recorded sql.NullString
			err := db.QueryRowContext(ctx, `SELECT sha256 FROM posse_migration_checksums WHERE version=?`, candidate).Scan(&recorded)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if recorded.Valid {
				if recorded.String != legacyReportBundleMigrationChecksum {
					continue
				}
				candidateChecksumPresent = true
			}
		}
		if candidate == publishedReportBundleMigrationVersion && resourceLeases != 0 && !candidateChecksumPresent {
			continue
		}
		if candidate == latestPublishedReportBundleMigrationVersion && (reviewReceipts != 0 || !candidateChecksumPresent) {
			continue
		}
		sourceVersion = candidate
		checksumPresent = candidateChecksumPresent
		break
	}
	if sourceVersion == 0 {
		return nil
	}
	sources, err := embeddedMigrations()
	if err != nil {
		return err
	}
	currentChecksum := ""
	for _, source := range sources {
		if source.version == reportBundleMigrationVersion {
			currentChecksum = source.checksum
			break
		}
	}
	if currentChecksum == "" || currentChecksum != legacyReportBundleMigrationChecksum {
		return fmt.Errorf("report bundle migration version %d is missing or differs from its published version", reportBundleMigrationVersion)
	}
	var alreadyMoved int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id=?)`, reportBundleMigrationVersion).Scan(&alreadyMoved); err != nil {
		return err
	}
	if alreadyMoved != 0 {
		return nil
	}
	if hasChecksums != 0 {
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM posse_migration_checksums WHERE version=?)`, reportBundleMigrationVersion).Scan(&alreadyMoved); err != nil {
			return err
		}
		if alreadyMoved != 0 {
			return nil
		}
	}
	if MigrationGate != nil {
		if err := MigrationGate(db.Path, []int64{sourceVersion}, os.Getenv(ForceMigrationEnv) == "1"); err != nil {
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
	result, err := tx.ExecContext(ctx, `UPDATE goose_db_version SET version_id=? WHERE version_id=? AND is_applied=1`, reportBundleMigrationVersion, sourceVersion)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("normalize legacy Report bundle migration: updated %d applied Goose rows, want 1", changed)
	}
	if checksumPresent {
		if _, err := tx.ExecContext(ctx, `UPDATE posse_migration_checksums SET version=?,sha256=? WHERE version=?`, reportBundleMigrationVersion, currentChecksum, sourceVersion); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `INSERT INTO posse_migration_checksums(version,sha256) VALUES(?,?)`, reportBundleMigrationVersion, currentChecksum); err != nil {
		return err
	}
	return tx.Commit()
}

// normalizeLegacyTaskContainmentMigration moves the Task containment migration
// from version 53 to 54 because main now uses version 53 for Watch runtimes.
func (db *DB) normalizeLegacyTaskContainmentMigration(ctx context.Context) error {
	var hasGoose int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='goose_db_version')`).Scan(&hasGoose); err != nil {
		return err
	}
	if hasGoose == 0 {
		return nil
	}
	var applied, hasMode, hasPath, hasMarker, hasWatchTable int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id=? AND is_applied=1)`, legacyTaskContainmentMigrationVersion).Scan(&applied); err != nil {
		return err
	}
	for _, check := range []struct {
		query string
		value *int
	}{
		{`SELECT EXISTS(SELECT 1 FROM pragma_table_info('tasks') WHERE name='containment_mode')`, &hasMode},
		{`SELECT EXISTS(SELECT 1 FROM pragma_table_info('tasks') WHERE name='containment_path')`, &hasPath},
		{`SELECT EXISTS(SELECT 1 FROM pragma_table_info('tasks') WHERE name='containment_marker')`, &hasMarker},
		{`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='project_lookout_pollers')`, &hasWatchTable},
	} {
		if err := db.QueryRowContext(ctx, check.query).Scan(check.value); err != nil {
			return err
		}
	}
	if applied == 0 || hasMode == 0 || hasPath == 0 || hasMarker == 0 || hasWatchTable != 0 {
		return nil
	}
	var hasChecksums int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='posse_migration_checksums')`).Scan(&hasChecksums); err != nil {
		return err
	}
	checksumPresent := false
	if hasChecksums != 0 {
		var recorded sql.NullString
		err := db.QueryRowContext(ctx, `SELECT sha256 FROM posse_migration_checksums WHERE version=?`, legacyTaskContainmentMigrationVersion).Scan(&recorded)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if recorded.Valid {
			if recorded.String != taskContainmentMigrationChecksum {
				return nil
			}
			checksumPresent = true
		}
	}
	sources, err := embeddedMigrations()
	if err != nil {
		return err
	}
	currentChecksum := ""
	for _, source := range sources {
		if source.version == taskContainmentMigrationVersion {
			currentChecksum = source.checksum
			break
		}
	}
	if currentChecksum == "" || currentChecksum != taskContainmentMigrationChecksum {
		return fmt.Errorf("task containment migration version %d is missing or differs from its published version", taskContainmentMigrationVersion)
	}
	var alreadyMoved int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id=?)`, taskContainmentMigrationVersion).Scan(&alreadyMoved); err != nil {
		return err
	}
	if alreadyMoved != 0 {
		return nil
	}
	if hasChecksums != 0 {
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM posse_migration_checksums WHERE version=?)`, taskContainmentMigrationVersion).Scan(&alreadyMoved); err != nil {
			return err
		}
		if alreadyMoved != 0 {
			return nil
		}
	}
	if MigrationGate != nil {
		if err := MigrationGate(db.Path, []int64{legacyTaskContainmentMigrationVersion}, os.Getenv(ForceMigrationEnv) == "1"); err != nil {
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
	result, err := tx.ExecContext(ctx, `UPDATE goose_db_version SET version_id=? WHERE version_id=? AND is_applied=1`, taskContainmentMigrationVersion, legacyTaskContainmentMigrationVersion)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("normalize legacy Task containment migration: updated %d applied Goose rows, want 1", changed)
	}
	if checksumPresent {
		if _, err := tx.ExecContext(ctx, `UPDATE posse_migration_checksums SET version=?,sha256=? WHERE version=?`, taskContainmentMigrationVersion, currentChecksum, legacyTaskContainmentMigrationVersion); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `INSERT INTO posse_migration_checksums(version,sha256) VALUES(?,?)`, taskContainmentMigrationVersion, currentChecksum); err != nil {
		return err
	}
	return tx.Commit()
}

// normalizeLegacyTaskPaneShellIdentityMigration preserves databases from the
// Task branch's version 56 shell-identity migration after main claimed version
// 56 for turn tokens, 57 for workspace Watch errors, and 58 for validation candidates.
func (db *DB) normalizeLegacyTaskPaneShellIdentityMigration(ctx context.Context) error {
	var hasGoose int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='goose_db_version')`).Scan(&hasGoose); err != nil {
		return err
	}
	if hasGoose == 0 {
		return nil
	}
	var legacyApplied, hasShellIdentity, hasTurnTokens, hasMemberWatchErrors int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id=? AND is_applied=1)`, legacyTaskPaneShellIdentityMigrationVersion).Scan(&legacyApplied); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('tasks') WHERE name='pane_shell_identity')`).Scan(&hasShellIdentity); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='task_turn_tokens')`).Scan(&hasTurnTokens); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='member_watch_errors')`).Scan(&hasMemberWatchErrors); err != nil {
		return err
	}
	if legacyApplied == 0 || hasShellIdentity == 0 {
		return nil
	}
	var hasChecksums int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='posse_migration_checksums')`).Scan(&hasChecksums); err != nil {
		return err
	}
	var recorded sql.NullString
	if hasChecksums != 0 {
		err := db.QueryRowContext(ctx, `SELECT sha256 FROM posse_migration_checksums WHERE version=?`, legacyTaskPaneShellIdentityMigrationVersion).Scan(&recorded)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	sources, err := embeddedMigrations()
	if err != nil {
		return err
	}
	var turnTokensChecksum, shellIdentityChecksum string
	for _, source := range sources {
		switch source.version {
		case taskTurnTokensMigrationVersion:
			turnTokensChecksum = source.checksum
		case taskPaneShellIdentityMigrationVersion:
			shellIdentityChecksum = source.checksum
		}
	}
	if turnTokensChecksum == "" || shellIdentityChecksum == "" {
		return fmt.Errorf("task pane shell identity migration sources are missing")
	}
	if recorded.Valid && recorded.String != legacyTaskPaneShellIdentityMigrationChecksum && recorded.String != turnTokensChecksum {
		return nil
	}
	if hasMemberWatchErrors != 0 && hasTurnTokens != 0 && recorded.Valid && recorded.String == turnTokensChecksum {
		return nil
	}
	if hasTurnTokens == 0 && recorded.Valid && recorded.String != legacyTaskPaneShellIdentityMigrationChecksum {
		return &SchemaError{Code: "schema_diverged", Message: fmt.Sprintf("%s records migration %d as turn tokens but its table is missing", db.Path, legacyTaskPaneShellIdentityMigrationVersion)}
	}
	var alreadyApplied int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id=? AND is_applied=1)`, taskPaneShellIdentityMigrationVersion).Scan(&alreadyApplied); err != nil {
		return err
	}
	if alreadyApplied != 0 {
		return nil
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS posse_migration_checksums (version INTEGER PRIMARY KEY, sha256 TEXT NOT NULL)`); err != nil {
		return err
	}
	if hasTurnTokens == 0 {
		result, err := tx.ExecContext(ctx, `UPDATE goose_db_version SET version_id=? WHERE version_id=? AND is_applied=1`, taskPaneShellIdentityMigrationVersion, legacyTaskPaneShellIdentityMigrationVersion)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return fmt.Errorf("normalize legacy task pane shell identity migration: no applied Goose receipt moved")
		}
		if recorded.Valid {
			if _, err := tx.ExecContext(ctx, `UPDATE posse_migration_checksums SET version=?,sha256=? WHERE version=?`, taskPaneShellIdentityMigrationVersion, shellIdentityChecksum, legacyTaskPaneShellIdentityMigrationVersion); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `INSERT INTO posse_migration_checksums(version,sha256) VALUES(?,?)`, taskPaneShellIdentityMigrationVersion, shellIdentityChecksum); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `INSERT INTO goose_db_version(version_id,is_applied) VALUES(?,1)`, taskPaneShellIdentityMigrationVersion); err != nil {
			return err
		}
		if recorded.Valid && recorded.String == legacyTaskPaneShellIdentityMigrationChecksum {
			if _, err := tx.ExecContext(ctx, `UPDATE posse_migration_checksums SET sha256=? WHERE version=?`, turnTokensChecksum, legacyTaskPaneShellIdentityMigrationVersion); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
