package store

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func prepareSupportedHomeAtVersion(t *testing.T, db *DB, version int64) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`DROP TABLE IF EXISTS greenhouse_task_watches`,
		`ALTER TABLE project_lookout_pollers DROP COLUMN executable_version`,
		`ALTER TABLE project_lookout_pollers DROP COLUMN executable_modified_at`,
		`ALTER TABLE project_lookout_pollers DROP COLUMN executable_size`,
		`DROP INDEX IF EXISTS tasks_project_seq_idx`,
		`DROP INDEX IF EXISTS tasks_project_pane_label_idx`,
		`DROP INDEX IF EXISTS tasks_project_workflow_idx`,
		`DROP TABLE IF EXISTS task_plan_input_migration`,
		`ALTER TABLE tasks DROP COLUMN plan_revision`,
		`ALTER TABLE tasks DROP COLUMN workflow_run_id`,
		`DELETE FROM goose_db_version`,
		`DELETE FROM posse_migration_checksums`,
		`INSERT INTO goose_db_version(version_id,is_applied) VALUES(0,1)`,
		`INSERT INTO goose_db_version(version_id,is_applied) VALUES(95,1)`,
		`INSERT INTO posse_migration_checksums(version,sha256) VALUES(95,'` + supportedLegacyMigrationChecksums[95] + `')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	var hasGreenhouseRequests bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='greenhouse_review_requests')`).Scan(&hasGreenhouseRequests); err != nil {
		t.Fatal(err)
	}
	if hasGreenhouseRequests && version < 20261123132235 {
		if _, err := db.ExecContext(ctx, `DROP TRIGGER greenhouse_review_requests_immutable`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE greenhouse_review_requests DROP COLUMN failure_count`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `CREATE TRIGGER greenhouse_review_requests_immutable BEFORE UPDATE ON greenhouse_review_requests
WHEN NOT (
    OLD.outcome = 'failed' AND OLD.grant_id IS NOT NULL AND
    NEW.id = OLD.id AND NEW.project_id = OLD.project_id AND NEW.task_id = OLD.task_id AND
    NEW.repo = OLD.repo AND NEW.requested_head = OLD.requested_head AND NEW.provider = OLD.provider AND
    NEW.requested_at = OLD.requested_at AND NEW.grant_id = OLD.grant_id AND
    NEW.settled_at >= OLD.settled_at AND NEW.outcome IN ('failed', 'verdict', 'refused', 'head_mismatch')
)
BEGIN SELECT RAISE(ABORT, 'Greenhouse review request is immutable'); END;`); err != nil {
			t.Fatal(err)
		}
	}
	sources, err := embeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		if source.version <= 95 || source.version > version || source.version >= baselineMigrationVersion {
			continue
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO goose_db_version(version_id,is_applied) VALUES(?,1)`, source.version); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO posse_migration_checksums(version,sha256) VALUES(?,?)`, source.version, source.checksum); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenAppliesBaselineAsOneMigration(t *testing.T) {
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE is_applied=1 AND version_id>0`).Scan(&applied); err != nil || applied != 3 {
		t.Fatalf("applied migration count = %d, %v; want baseline, review-dispute evidence and greenhouse attempt migrations", applied, err)
	}
	var version int64
	if err := db.QueryRow(`SELECT version_id FROM goose_db_version WHERE is_applied=1 AND version_id=?`, baselineMigrationVersion).Scan(&version); err != nil || version != baselineMigrationVersion {
		t.Fatalf("applied baseline version = %d, %v; want %d", version, err, baselineMigrationVersion)
	}
	var watches int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='greenhouse_task_watches'`).Scan(&watches); err != nil || watches != 1 {
		t.Fatalf("greenhouse_task_watches table count = %d, %v; want 1", watches, err)
	}
	for _, column := range []string{"executable_size", "executable_modified_at", "executable_version"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('project_lookout_pollers') WHERE name=?`, column).Scan(&count); err != nil || count != 1 {
			t.Fatalf("project_lookout_pollers.%s count = %d, %v; want 1", column, count, err)
		}
	}
}

func TestOpenUpgradesSupportedHomeFromVersion95WithData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "upgrade-check", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP TABLE provider_finding_events`,
		`DROP TABLE fix_round_provider_findings`,
		`DROP TABLE provider_findings`,
		`DROP TABLE provider_finding_patterns`,
		`DROP TABLE greenhouse_review_requests`,
		`DROP TABLE verdicts`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare version 95 home with data: %s: %v", statement, err)
		}
	}
	prepareSupportedHomeAtVersion(t, db, 95)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenAt(path)
	if err != nil {
		t.Fatalf("upgrade supported version 95 home: %v", err)
	}
	defer db.Close()
	got, err := db.ProjectByID(ctx, project.ID)
	if err != nil || got.Name != project.Name {
		t.Fatalf("project after upgrade = %#v, %v; want preserved project %q", got, err, project.Name)
	}
	for version := int64(96); version <= 99; version++ {
		var applied int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM goose_db_version WHERE version_id=? AND is_applied=1`, version).Scan(&applied); err != nil || applied != 1 {
			t.Fatalf("migration %d applied rows = %d, %v; want 1", version, applied, err)
		}
	}
}

func TestOpenUpgradesVersion99HomeThroughVersion101BeforeBaseline(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	path := filepath.Join(home, "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "upgrade-check", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	plan := admittedStorePlan(t, home).Bind("t1")
	plan.Inputs = append(plan.Inputs, taskplan.Select("review-round-file", "review/legacy.patch", bytes.Repeat([]byte{0x5a}, 1<<20)))
	inline, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Preserve inline plan", MergeMode: "local", PlanJSON: inline})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET plan_json=?,plan_revision=0 WHERE id=?`, inline, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=?`, inline, taskID); err != nil {
		t.Fatal(err)
	}
	prepareSupportedHomeAtVersion(t, db, 99)
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER require_plan_input_migration_before_baseline BEFORE INSERT ON goose_db_version WHEN NEW.version_id=20261120180000 AND (SELECT completed FROM task_plan_input_migration WHERE id=1)<>1 BEGIN SELECT RAISE(ABORT,'task plan input migration incomplete'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenAt(path)
	if err != nil {
		t.Fatalf("open version 99 home: %v", err)
	}
	defer db.Close()
	got, err := db.ProjectByID(ctx, project.ID)
	if err != nil || got.Name != project.Name {
		t.Fatalf("project after baseline = %#v, %v; want preserved project %q", got, err, project.Name)
	}
	for version := int64(100); version <= 101; version++ {
		var applied int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM goose_db_version WHERE version_id=? AND is_applied=1`, version).Scan(&applied); err != nil || applied != 1 {
			t.Fatalf("migration %d applied rows = %d, %v; want 1", version, applied, err)
		}
	}
	var completed int
	if err := db.QueryRowContext(ctx, `SELECT completed FROM task_plan_input_migration WHERE id=1`).Scan(&completed); err != nil || completed != 1 {
		t.Fatalf("task plan input migration completed = %d, %v; want 1 before baseline", completed, err)
	}
	var baselineApplied int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM goose_db_version WHERE version_id=? AND is_applied=1`, baselineMigrationVersion).Scan(&baselineApplied); err != nil || baselineApplied != 1 {
		t.Fatalf("baseline migration rows = %d, %v; want 1", baselineApplied, err)
	}
	migrated, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	storedPlan, err := migrated.AdmittedPlan()
	if err != nil || storedPlan == nil || storedPlan.SHA256() != plan.SHA256() || len(storedPlan.Inputs[len(storedPlan.Inputs)-1].Bytes) != 1<<20 {
		t.Fatalf("plan after upgrade lost content: plan=%v err=%v", storedPlan != nil, err)
	}
}

func TestOpenRefusesHomesOlderThanMinimumSupportedVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DELETE FROM goose_db_version WHERE version_id>94`,
		`DELETE FROM posse_migration_checksums WHERE version>94`,
		`INSERT INTO goose_db_version(version_id,is_applied) VALUES(94,1)`,
		`INSERT INTO posse_migration_checksums(version,sha256) VALUES(94,'old-release')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = OpenAt(path)
	if err == nil || !containsAll(err.Error(), "too old for direct upgrade", "v0.33.0") {
		t.Fatalf("Open error = %v; want minimum-version error naming v0.33.0", err)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(s, part) {
			return false
		}
	}
	return true
}
