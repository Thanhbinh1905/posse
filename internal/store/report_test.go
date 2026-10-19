package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestReportAcceptanceRequiresCurrentIndependentPassAndSurvivesRebuild(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := OpenAt(filepath.Join(home, "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	reportID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "research", Title: "Publication report", ReportKind: "spec"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(ctx, reportID, "author", "model-a", true); err != nil {
		t.Fatal(err)
	}
	transitionTaskToReported(t, ctx, db, reportID)
	const manifest = `{"version":1,"task":"t1","files":[{"path":"report.md","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`
	manifestHash := sha256.Sum256([]byte(manifest))
	digest := hex.EncodeToString(manifestHash[:])
	if err := db.RecordReportBundle(ctx, reportID, strings.Repeat("a", 64), manifest); err == nil {
		t.Fatal("Report bundle accepted a digest that does not hash its manifest")
	}
	if err := db.RecordReportBundle(ctx, reportID, digest, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcceptReport(ctx, reportID, digest, "user", ""); err == nil || !strings.Contains(err.Error(), "torn-down") {
		t.Fatalf("reported Research was accepted before Teardown: %v", err)
	}
	if err := db.Transition(ctx, reportID, StateReported, StateTornDown, "cli", "Sandbox released"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcceptReport(ctx, reportID, digest, "lead", "User approves"); !errors.Is(err, ErrIndependentReportRequired) {
		t.Fatalf("acceptance without review = %v, want ErrIndependentReportRequired", err)
	}
	changedManifest := manifest + " "
	if err := db.RecordReportBundle(ctx, reportID, reportManifestDigest(changedManifest), changedManifest); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("changed finalized bundle was accepted: %v", err)
	}
	reviewID, err := db.CreateTask(ctx, project.ID, Task{Seq: 2, Type: "review", ReviewsTaskID: reportID, Title: "Independent report review", Profile: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(ctx, reviewID, "author", "model-a", true); err != nil {
		t.Fatal(err)
	}
	transitionTaskToReported(t, ctx, db, reviewID)
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET launches=2 WHERE id=?`, reportID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordReportReview(ctx, reportID, reviewID, digest, "pass"); err == nil || !strings.Contains(err.Error(), "complete author and reviewer launch identities") {
		t.Fatalf("incomplete author launch history was accepted: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET launches=1 WHERE id=?`, reportID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordReportReview(ctx, reportID, reviewID, digest, "pass"); err == nil || !strings.Contains(err.Error(), "not independent") {
		t.Fatalf("same-identity review was accepted: %v", err)
	}
	independentReviewID, err := db.CreateTask(ctx, project.ID, Task{Seq: 3, Type: "review", ReviewsTaskID: reportID, Title: "Fresh independent report review", Profile: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(ctx, independentReviewID, "reviewer", "model-b", true); err != nil {
		t.Fatal(err)
	}
	transitionTaskToReported(t, ctx, db, independentReviewID)
	if _, err := db.RecordReportReview(ctx, reportID, independentReviewID, digest, "pass"); err != nil {
		t.Fatalf("independent review: %v", err)
	}
	changesReviewID, err := db.CreateTask(ctx, project.ID, Task{Seq: 4, Type: "review", ReviewsTaskID: reportID, Title: "Changes requested", Profile: "reviewer-c"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(ctx, changesReviewID, "reviewer-c", "model-c", true); err != nil {
		t.Fatal(err)
	}
	transitionTaskToReported(t, ctx, db, changesReviewID)
	if _, err := db.RecordReportReview(ctx, reportID, changesReviewID, digest, "changes_requested"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcceptReport(ctx, reportID, digest, "lead", "User approves"); !errors.Is(err, ErrIndependentReportRequired) {
		t.Fatalf("acceptance after a current changes-requested review = %v", err)
	}
	finalReviewID, err := db.CreateTask(ctx, project.ID, Task{Seq: 5, Type: "review", ReviewsTaskID: reportID, Title: "Final independent pass", Profile: "reviewer-d"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(ctx, finalReviewID, "reviewer-d", "model-d", true); err != nil {
		t.Fatal(err)
	}
	transitionTaskToReported(t, ctx, db, finalReviewID)
	if _, err := db.RecordReportReview(ctx, reportID, finalReviewID, digest, "pass"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcceptReport(ctx, reportID, digest, "lead", " "); err == nil || !strings.Contains(err.Error(), "approval quote") {
		t.Fatalf("Lead acceptance without User quote = %v", err)
	}
	acceptance, err := db.AcceptReport(ctx, reportID, digest, "lead", "User explicitly accepts the spec")
	if err != nil {
		t.Fatal(err)
	}
	if acceptance.ReportDigest != digest || acceptance.AcceptedBy != "user" || acceptance.RecordedBy != "lead" || acceptance.Provenance != "lead-quote" || acceptance.UserQuote != "User explicitly accepts the spec" {
		t.Fatalf("acceptance provenance = %#v", acceptance)
	}
	if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
		t.Fatalf("rebuild from Task snapshots: %v", err)
	}
	rebuilt, err := db.TaskByID(ctx, project.ID, reportID)
	if err != nil || rebuilt.ReportDigest != digest || rebuilt.ReportKind != "spec" || rebuilt.ReportManifestJSON != manifest || rebuilt.State != StateTornDown {
		t.Fatalf("rebuilt Report Task = %#v, err=%v", rebuilt, err)
	}
	reviews, err := db.ReportReviewsForTask(ctx, reportID)
	if err != nil || len(reviews) != 3 || reviews[2].ReportDigest != digest || reviews[2].ReviewerLaunch != 1 || reviews[2].ReviewTaskID != finalReviewID {
		t.Fatalf("rebuilt Report reviews = %#v, err=%v", reviews, err)
	}
	rebuiltAcceptance, err := db.ReportAcceptanceForTask(ctx, reportID)
	if err != nil || rebuiltAcceptance == nil || *rebuiltAcceptance != acceptance {
		t.Fatalf("rebuilt Report acceptance = %#v, err=%v", rebuiltAcceptance, err)
	}
	if _, err := db.AcceptReport(ctx, reportID, digest, "user", ""); err != nil {
		t.Fatalf("idempotent User acceptance after rebuild: %v", err)
	}
}

func transitionTaskToReported(t *testing.T, ctx context.Context, db *DB, taskID int64) {
	t.Helper()
	for _, state := range []State{StateWorking, StateDone, StateReported} {
		current, err := db.TaskByID(ctx, 1, taskID)
		if err != nil {
			t.Fatal(err)
		}
		source := "worker"
		if state == StateWorking || state == StateReported {
			source = "cli"
		}
		if err := db.Transition(ctx, taskID, current.State, state, source, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
}
