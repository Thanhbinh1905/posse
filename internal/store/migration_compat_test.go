package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func TestOpenNormalizesPublishedReportMigrationVersions(t *testing.T) {
	for _, version := range []int64{legacyReportBundleMigrationVersion, publishedReportBundleMigrationVersion, latestPublishedReportBundleMigrationVersion} {
		t.Run(fmt.Sprintf("version %d", version), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "posse.db")
			db, projectID, reportID, digest, manifest := seedAcceptedReportForMigrationTest(t, ctx, path)
			statements := []string{
				`DROP TRIGGER review_receipts_immutable`,
				`DROP TABLE review_receipts`,
				`DELETE FROM goose_db_version WHERE version_id IN (45,46,47,48)`,
				`DELETE FROM posse_migration_checksums WHERE version IN (45,46,47,48)`,
			}
			switch version {
			case legacyReportBundleMigrationVersion:
				statements = append(statements,
					`DROP TRIGGER review_verdicts_immutable`,
					`DROP TABLE review_verdicts`,
					`DROP TRIGGER review_targets_immutable`,
					`DROP TABLE review_targets`,
					`DROP TABLE resource_leases`,
				)
			case publishedReportBundleMigrationVersion:
				statements = append(statements, `DROP TABLE resource_leases`)
			}
			for _, statement := range statements {
				if _, err := db.ExecContext(ctx, statement); err != nil {
					t.Fatalf("prepare database from published Report migration: %v", err)
				}
			}
			if version > legacyReportBundleMigrationVersion {
				for applied := int64(legacyReportBundleMigrationVersion); applied < version; applied++ {
					if _, err := db.ExecContext(ctx, `INSERT INTO goose_db_version(version_id,is_applied) VALUES(?,1)`, applied); err != nil {
						t.Fatalf("restore main migration %d: %v", applied, err)
					}
				}
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO goose_db_version(version_id,is_applied) VALUES(?,1)`, version); err != nil {
				t.Fatalf("restore Report migration %d: %v", version, err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO posse_migration_checksums(version,sha256) VALUES(?,?)`, version, legacyReportBundleMigrationChecksum); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			db, err := OpenAt(path)
			if err != nil {
				t.Fatalf("open database after migration renumbering: %v", err)
			}
			defer db.Close()
			rebuilt, err := db.TaskByID(ctx, projectID, reportID)
			if err != nil || rebuilt.ReportKind != "spec" || rebuilt.ReportDigest != digest || rebuilt.ReportManifestJSON != manifest {
				t.Fatalf("Report Task after migration renumbering = %#v, %v", rebuilt, err)
			}
			reviews, err := db.ReportReviewsForTask(ctx, reportID)
			if err != nil || len(reviews) != 1 || reviews[0].ReportDigest != digest {
				t.Fatalf("Report reviews after migration renumbering = %#v, %v", reviews, err)
			}
			acceptance, err := db.ReportAcceptanceForTask(ctx, reportID)
			if err != nil || acceptance == nil || acceptance.UserQuote != "User approves this exact Report" {
				t.Fatalf("Report acceptance after migration renumbering = %#v, %v", acceptance, err)
			}
			for _, appliedVersion := range []int64{45, 46, 47, 48} {
				var applied int
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM goose_db_version WHERE version_id=? AND is_applied=1`, appliedVersion).Scan(&applied); err != nil || applied != 1 {
					t.Fatalf("migration %d applied rows = %d, %v; want 1", appliedVersion, applied, err)
				}
			}
			for _, table := range []string{"review_targets", "review_verdicts", "resource_leases", "review_receipts"} {
				var exists int
				if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, table).Scan(&exists); err != nil || exists != 1 {
					t.Fatalf("table %s after migration renumbering exists=%d, err=%v", table, exists, err)
				}
			}
		})
	}
}

func TestOpenRenumbersLegacyTaskContainmentMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP TABLE project_lookout_pollers`,
		`DELETE FROM goose_db_version WHERE version_id=54`,
		`DELETE FROM posse_migration_checksums WHERE version=54`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare database from Task containment migration version 53: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE posse_migration_checksums SET sha256=? WHERE version=53`, taskContainmentMigrationChecksum); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = OpenAt(path)
	if err != nil {
		t.Fatalf("open database after moving Task containment migration to version 54: %v", err)
	}
	defer db.Close()
	for _, version := range []int64{53, 54} {
		var applied int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM goose_db_version WHERE version_id=? AND is_applied=1`, version).Scan(&applied); err != nil || applied != 1 {
			t.Fatalf("migration %d applied rows = %d, %v; want 1", version, applied, err)
		}
	}
	for _, column := range []string{"containment_mode", "containment_path", "containment_marker"} {
		var exists int
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('tasks') WHERE name=?)`, column).Scan(&exists); err != nil || exists != 1 {
			t.Fatalf("Task containment column %s exists=%d, err=%v", column, exists, err)
		}
	}
	var watchTable int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='project_lookout_pollers')`).Scan(&watchTable); err != nil || watchTable != 1 {
		t.Fatalf("Lookout poller table exists=%d, err=%v", watchTable, err)
	}
	sources, err := embeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int64{53, 54} {
		var expected, recorded string
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

func seedAcceptedReportForMigrationTest(t *testing.T, ctx context.Context, path string) (*DB, int64, int64, string, string) {
	t.Helper()
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	reportID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "research", Title: "Publication report", Profile: "author", ReportKind: "spec"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(ctx, reportID, "author", "model-a", true); err != nil {
		t.Fatal(err)
	}
	transitionTaskToReported(t, ctx, db, reportID)
	manifest := `{"version":1,"task":"t1","files":[{"path":"report.md","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`
	digest := reportManifestDigest(manifest)
	if err := db.RecordReportBundle(ctx, reportID, digest, manifest); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, reportID, StateReported, StateTornDown, "cli", "Sandbox released"); err != nil {
		t.Fatal(err)
	}
	reviewerID, err := db.CreateTask(ctx, project.ID, Task{Seq: 2, Type: "review", ReviewsTaskID: reportID, Title: "Independent report review", Profile: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(ctx, reviewerID, "reviewer", "model-b", true); err != nil {
		t.Fatal(err)
	}
	transitionTaskToReported(t, ctx, db, reviewerID)
	if _, err := db.RecordReportReview(ctx, reportID, reviewerID, digest, "pass"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcceptReport(ctx, reportID, digest, "lead", "User approves this exact Report"); err != nil {
		t.Fatal(err)
	}
	return db, project.ID, reportID, digest, manifest
}
