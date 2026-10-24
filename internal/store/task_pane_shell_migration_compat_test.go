package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenRenumbersLegacyTaskPaneShellIdentityMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP INDEX notices_task_kind_idx`,
		`DROP INDEX notices_sandbox_episode_idx`,
		`DROP INDEX notices_project_id_idx`,
		`ALTER TABLE pr_body_markers DROP COLUMN validation_digest`,
		`DROP TABLE merge_queue_entries`,
		`ALTER TABLE completed_rider_busy DROP COLUMN baseline`,
		`DROP TABLE research_attempts`,
		`DROP TABLE member_watch_errors`,
		`DROP TABLE validation_stage_results`,
		`DROP TABLE validation_candidates`,
		`DROP TABLE task_turn_tokens`,
		`ALTER TABLE lead_start_claims DROP COLUMN pid`,
		`ALTER TABLE lead_start_claims DROP COLUMN holder`,
		`DELETE FROM goose_db_version WHERE version_id >= 56`,
		`DELETE FROM posse_migration_checksums WHERE version >= 56`,
		`INSERT INTO goose_db_version(version_id,is_applied) VALUES(56,1)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare database from the legacy pane shell migration: %s: %v", statement, err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO posse_migration_checksums(version,sha256) VALUES(?,?)`, legacyTaskPaneShellIdentityMigrationVersion, legacyTaskPaneShellIdentityMigrationChecksum); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = OpenAt(path)
	if err != nil {
		t.Fatalf("open database after pane shell migration renumbering: %v", err)
	}
	defer db.Close()
	for _, table := range []string{"task_turn_tokens", "member_watch_errors", "validation_candidates", "validation_stage_results"} {
		var exists int
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, table).Scan(&exists); err != nil || exists != 1 {
			t.Fatalf("table %s after migration renumbering exists=%d, err=%v", table, exists, err)
		}
	}
	var shellIdentity int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('tasks') WHERE name='pane_shell_identity')`).Scan(&shellIdentity); err != nil || shellIdentity != 1 {
		t.Fatalf("pane shell identity column after migration renumbering exists=%d, err=%v", shellIdentity, err)
	}
	assertAppliedMigrationChecksums(t, db, 56, 57, 58, 59, 60, 61, 62)
}

func TestOpenRenumbersAppliedVersion60TaskPaneShellIdentityMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DELETE FROM goose_db_version WHERE version_id=62`,
		`DELETE FROM posse_migration_checksums WHERE version=62`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare database from the legacy version 60 pane shell migration: %s: %v", statement, err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE posse_migration_checksums SET sha256=? WHERE version=60`, legacyTaskPaneShellIdentityMigrationChecksum); err != nil {
		t.Fatalf("prepare legacy version 60 checksum: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = OpenAt(path)
	if err != nil {
		t.Fatalf("open database after version 60 pane shell migration renumbering: %v", err)
	}
	defer db.Close()
	assertAppliedMigrationChecksums(t, db, 60, 61, 62)
}

func TestOpenRenumbersAppliedVersion61TaskPaneShellIdentityMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DELETE FROM goose_db_version WHERE version_id IN (61,62)`,
		`DELETE FROM posse_migration_checksums WHERE version IN (61,62)`,
		`ALTER TABLE lead_start_claims DROP COLUMN pid`,
		`ALTER TABLE lead_start_claims DROP COLUMN holder`,
		`INSERT INTO goose_db_version(version_id,is_applied) VALUES(61,1)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare database from the legacy version 61 pane shell migration: %s: %v", statement, err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO posse_migration_checksums(version,sha256) VALUES(61,?)`, legacyTaskPaneShellIdentityMigrationChecksum); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = OpenAt(path)
	if err != nil {
		t.Fatalf("open database after version 61 pane shell migration renumbering: %v", err)
	}
	defer db.Close()
	var holder, pid, shellIdentity int
	for _, check := range []struct {
		query string
		value *int
	}{
		{`SELECT EXISTS(SELECT 1 FROM pragma_table_info('lead_start_claims') WHERE name='holder')`, &holder},
		{`SELECT EXISTS(SELECT 1 FROM pragma_table_info('lead_start_claims') WHERE name='pid')`, &pid},
		{`SELECT EXISTS(SELECT 1 FROM pragma_table_info('tasks') WHERE name='pane_shell_identity')`, &shellIdentity},
	} {
		if err := db.QueryRowContext(ctx, check.query).Scan(check.value); err != nil {
			t.Fatal(err)
		}
	}
	if holder != 1 || pid != 1 || shellIdentity != 1 {
		t.Fatalf("migrated schema holder=%d pid=%d shellIdentity=%d, want all columns", holder, pid, shellIdentity)
	}
	assertAppliedMigrationChecksums(t, db, 60, 61, 62)
}

func assertAppliedMigrationChecksums(t *testing.T, db *DB, versions ...int64) {
	t.Helper()
	ctx := context.Background()
	sources, err := embeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range versions {
		var applied int
		var expected, recorded string
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM goose_db_version WHERE version_id=? AND is_applied=1`, version).Scan(&applied); err != nil || applied != 1 {
			t.Fatalf("migration %d applied rows = %d, %v; want 1", version, applied, err)
		}
		for _, source := range sources {
			if source.version == version {
				expected = source.checksum
				break
			}
		}
		if err := db.QueryRowContext(ctx, `SELECT sha256 FROM posse_migration_checksums WHERE version=?`, version).Scan(&recorded); err != nil || recorded != expected {
			t.Fatalf("migration %d checksum = %q, err=%v; want %q", version, recorded, err, expected)
		}
	}
}
