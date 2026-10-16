package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func TestOpenNormalizesPublishedReportMigrationVersions(t *testing.T) {
	for _, version := range []int64{legacyReportBundleMigrationVersion, publishedReportBundleMigrationVersion} {
		t.Run(fmt.Sprintf("version %d", version), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "posse.db")
			db, projectID, reportID, digest, manifest := seedAcceptedReportForMigrationTest(t, ctx, path)
			if version == legacyReportBundleMigrationVersion {
				for _, statement := range []string{
					`DROP TRIGGER review_verdicts_immutable`,
					`DROP TABLE review_verdicts`,
					`DROP TRIGGER review_targets_immutable`,
					`DROP TABLE review_targets`,
					`DROP TABLE resource_leases`,
					`DELETE FROM goose_db_version WHERE version_id IN (45,46,47)`,
					`DELETE FROM posse_migration_checksums WHERE version IN (45,46,47)`,
					`INSERT INTO goose_db_version(version_id,is_applied) VALUES(45,1)`,
				} {
					if _, err := db.ExecContext(ctx, statement); err != nil {
						t.Fatalf("prepare database from published Report migration: %v", err)
					}
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO posse_migration_checksums(version,sha256) VALUES(45,?)`, legacyReportBundleMigrationChecksum); err != nil {
					t.Fatal(err)
				}
			} else {
				for _, statement := range []string{
					`DROP TABLE resource_leases`,
					`DELETE FROM goose_db_version WHERE version_id=46`,
					`DELETE FROM posse_migration_checksums WHERE version=46`,
					`UPDATE goose_db_version SET version_id=46 WHERE version_id=47 AND is_applied=1`,
					`UPDATE posse_migration_checksums SET version=46 WHERE version=47`,
				} {
					if _, err := db.ExecContext(ctx, statement); err != nil {
						t.Fatalf("prepare database from published Report migration: %v", err)
					}
				}
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
			for _, appliedVersion := range []int64{45, 46, 47} {
				var applied int
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM goose_db_version WHERE version_id=? AND is_applied=1`, appliedVersion).Scan(&applied); err != nil || applied != 1 {
					t.Fatalf("migration %d applied rows = %d, %v; want 1", appliedVersion, applied, err)
				}
			}
			for _, table := range []string{"review_targets", "review_verdicts", "resource_leases"} {
				var exists int
				if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, table).Scan(&exists); err != nil || exists != 1 {
					t.Fatalf("table %s after migration renumbering exists=%d, err=%v", table, exists, err)
				}
			}
		})
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
	reportID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "scout", Title: "Publication report", Profile: "author", ReportKind: "spec"})
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
	if err := db.Transition(ctx, reportID, StateReported, StateTornDown, "cli", "Mount released"); err != nil {
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
