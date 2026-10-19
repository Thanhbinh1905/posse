package store

import (
	"context"
	"testing"
)

func TestHistoricalVocabularyMigrationReceiptUpgradesWithPublishedMain(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "upgrade", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Preserve admitted work"})
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the unreleased vocabulary branch's original version 53,
	// before main published its Watch runtime and containment migrations.
	for _, statement := range []string{
		`DROP TABLE project_lookout_pollers`,
		`ALTER TABLE tasks DROP COLUMN containment_mode`,
		`ALTER TABLE tasks DROP COLUMN containment_path`,
		`ALTER TABLE tasks DROP COLUMN containment_marker`,
		`DELETE FROM goose_db_version WHERE version_id IN (53,54)`,
		`DELETE FROM posse_migration_checksums WHERE version IN (53,54)`,
		`UPDATE goose_db_version SET version_id=53 WHERE version_id=55`,
		`UPDATE posse_migration_checksums SET version=53 WHERE version=55`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("reconstruct historical receipt: %s: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		db, err = Open(home)
		if err != nil {
			t.Fatal(err)
		}
		task, err := db.TaskByID(ctx, project.ID, id)
		if err != nil || task.ID != id || task.Title != "Preserve admitted work" || task.Type != "change" {
			t.Fatalf("upgrade lost Task identity: %#v %v", task, err)
		}
		if _, err := db.WatchRuntime(ctx, project.ID); err != nil {
			t.Fatalf("published Watch schema was not applied: %v", err)
		}
		for _, version := range []int{53, 54, 55} {
			var applied int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM goose_db_version WHERE version_id=? AND is_applied=1`, version).Scan(&applied); err != nil || applied != 1 {
				t.Fatalf("migration %d receipt count=%d: %v", version, applied, err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
