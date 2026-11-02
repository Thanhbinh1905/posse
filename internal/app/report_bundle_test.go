package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestReportBundleDigestCoversExactReportAndAttachmentBytes(t *testing.T) {
	home := t.TempDir()
	project := store.Project{Name: "shop"}
	task := store.Task{Seq: 1}
	root := filepath.Join(home, "projects", project.Name, "tasks", "t1")
	for path, contents := range map[string]string{
		"report.md":        "Final Report\n",
		"evidence/data.md": "Supporting evidence\n",
		"brief.md":         "not part of the Report bundle\n",
	} {
		fullPath := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest, encoded, err := buildReportBundleManifest(root, task)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 2 || manifest.Files[0].Path != "evidence/data.md" || manifest.Files[1].Path != "report.md" {
		t.Fatalf("manifest files = %#v", manifest.Files)
	}
	manifestPath := filepath.Join(root, reportBundleManifestName)
	if err := os.WriteFile(manifestPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	task.ReportDigest = reportDigest(encoded)
	task.ReportManifestJSON = string(encoded)
	if err := verifyReportBundle(home, project, task); err != nil {
		t.Fatalf("verify unchanged Report bundle: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "evidence", "data.md")); err != nil {
		t.Fatal(err)
	}
	if err := verifyReportBundle(home, project, task); err == nil || !strings.Contains(err.Error(), `report attachment "evidence/data.md" is missing`) {
		t.Fatalf("missing attachment verification = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "evidence", "data.md"), []byte("changed evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyReportBundle(home, project, task); err == nil || !strings.Contains(err.Error(), "changed after Teardown") {
		t.Fatalf("changed attachment verification = %v", err)
	}
	if err := os.Remove(filepath.Join(root, "evidence", "data.md")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "evidence", "data.md")); err != nil {
		t.Fatal(err)
	}
	if err := verifyReportBundle(home, project, task); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("escaping symlink verification = %v", err)
	}
}

func TestExplicitReportAttachmentRejectsMissingAndEscapingPaths(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(t.TempDir(), "posse")
	project := store.Project{Name: "shop"}
	task := store.Task{Seq: 1, WorktreePath: root}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := saveExplicitAttachment(task, home, project, "missing.txt"); err == nil {
		t.Fatal("missing explicit attachment was accepted")
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if err := saveExplicitAttachment(task, home, project, "escape.txt"); err == nil || !strings.Contains(err.Error(), "stay inside the Worker's worktree") {
		t.Fatalf("escaping explicit attachment error = %v", err)
	}
}

func TestAcceptPublicationReportRequiresUserApprovalAndIndependentReview(t *testing.T) {
	fixture := newPublicationReportFixture(t, "spec")
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_PANE_ID", "")
	t.Chdir(fixture.project.Root)

	code, output := runCLI(t, fixture.service, "accept", "t1")
	if code != 1 || !strings.Contains(output, "report_review_required") {
		t.Fatalf("acceptance without independent review = code %d: %s", code, output)
	}
	if acceptance, err := fixture.db.ReportAcceptanceForTask(context.Background(), fixture.task.ID); err != nil || acceptance != nil {
		t.Fatalf("rejected acceptance was recorded: %#v, %v", acceptance, err)
	}
	addPublicationReportReview(t, fixture)

	if err := fixture.db.SetProjectLead(context.Background(), fixture.project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	code, output = runCLI(t, fixture.service, "accept", "t1")
	if code != 1 || !strings.Contains(output, "user_only") {
		t.Fatalf("Lead acceptance without quote = code %d: %s", code, output)
	}
	code, output = runCLI(t, fixture.service, "accept", "t1", "--user-approved", "User explicitly accepts this exact spec")
	if code != 0 || !strings.Contains(output, "provenance: lead-quote") || !strings.Contains(output, "User explicitly accepts this exact spec") {
		t.Fatalf("Lead acceptance with exact User quote = code %d: %s", code, output)
	}
	acceptance, err := fixture.db.ReportAcceptanceForTask(context.Background(), fixture.task.ID)
	if err != nil || acceptance == nil || acceptance.AcceptedBy != "user" || acceptance.RecordedBy != "lead" || acceptance.Provenance != "lead-quote" || acceptance.UserQuote != "User explicitly accepts this exact spec" {
		t.Fatalf("recorded acceptance = %#v, %v", acceptance, err)
	}

	code, output = runCLI(t, fixture.service, "show", "t1")
	if code != 0 || !strings.Contains(output, fixture.task.ReportDigest) || !strings.Contains(output, "report_acceptance") {
		t.Fatalf("show omitted Report digest or acceptance provenance: code %d: %s", code, output)
	}
}

func TestOrdinaryResearchReportDoesNotRequireAcceptance(t *testing.T) {
	fixture := newPublicationReportFixture(t, "")
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_PANE_ID", "")
	t.Chdir(fixture.project.Root)
	code, output := runCLI(t, fixture.service, "accept", "t1")
	if code != 1 || !strings.Contains(output, "report_acceptance_not_required") {
		t.Fatalf("ordinary Research acceptance response = code %d: %s", code, output)
	}
	if err := fixture.db.SetProjectLead(context.Background(), fixture.project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	code, output = runCLI(t, fixture.service, "show", "t1")
	if code != 0 || !strings.Contains(output, fixture.task.ReportDigest) {
		t.Fatalf("ordinary Research Report is not readable without acceptance: code %d: %s", code, output)
	}
}

type publicationReportFixture struct {
	home    string
	service *Service
	db      *store.DB
	project store.Project
	task    store.Task
}

func newPublicationReportFixture(t *testing.T, reportKind string) publicationReportFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "research", Title: "Publication report", Profile: "author", ReportKind: reportKind, AutonomyReview: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(ctx, taskID, "author", "model-a", true); err != nil {
		t.Fatal(err)
	}
	transitionAppTaskToReported(t, ctx, db, project.ID, taskID)
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(home, "projects", project.Name, "tasks", "t1")
	for path, contents := range map[string]string{"report.md": "Publication Report\n", "evidence.md": "Supporting evidence\n"} {
		if err := os.WriteFile(filepath.Join(artifactRoot, path), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := finalizeReportBundle(ctx, db, home, project, task); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateReported, store.StateTornDown, "cli", "Sandbox released"); err != nil {
		t.Fatal(err)
	}
	task, err = db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	return publicationReportFixture{home: home, service: testService(home, nil), db: db, project: project, task: task}
}

func TestReviewStartReviewsPublicationReportAndAllowsAcceptance(t *testing.T) {
	fixture := newStartFixture(t)
	ctx := context.Background()
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	reportID, err := db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 1, Type: "research", Title: "Publication Report", Profile: "claude", ReportKind: "spec"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(ctx, reportID, "claude", "model-a", true); err != nil {
		t.Fatal(err)
	}
	transitionAppTaskToReported(t, ctx, db, fixture.project.ID, reportID)
	report := store.Task{ID: reportID, Seq: 1}
	artifactRoot := filepath.Join(fixture.home, "projects", fixture.project.Name, "tasks", "t1")
	if err := os.MkdirAll(artifactRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "report.md"), []byte("Publication spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "evidence.md"), []byte("Supporting evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	project, err := db.ProjectByID(ctx, fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizeReportBundle(ctx, db, fixture.home, project, report); err != nil {
		t.Fatal(err)
	}
	report, err = db.TaskByID(ctx, fixture.project.ID, reportID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, reportID, store.StateReported, store.StateTornDown, "cli", "Sandbox released"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	start := func(args ...string) (int, string) {
		t.Helper()
		var output bytes.Buffer
		cli := fixture.service.CLI()
		cli.Out, cli.ErrOut = &output, &output
		return cli.Run(args), output.String()
	}
	if code, output := start("review", "start", "t1", "--profile", "claude"); code != 0 {
		t.Fatalf("Report review start code=%d: %s", code, output)
	}
	db, err = store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := db.Tasks(ctx, fixture.project.ID, true)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("Tasks after Report review admission = %#v, %v", tasks, err)
	}
	review := tasks[1]
	plan, err := review.AdmittedPlan()
	if err != nil || plan == nil || plan.ReviewOf != reportID {
		t.Fatalf("Report Review plan = %#v, %v", plan, err)
	}
	if !strings.Contains(string(plan.Inputs[0].Bytes), "Report bundle digest") || !strings.Contains(string(plan.Inputs[0].Bytes), "attachments/") {
		t.Fatalf("Report Review Brief is not bound to the sealed Report: %s", plan.Inputs[0].Bytes)
	}
	target, err := reportReviewTargetFromPlan(plan, reportID)
	if err != nil || target == nil || target.ReportDigest != report.ReportDigest || target.ManifestJSON != report.ReportManifestJSON {
		t.Fatalf("frozen Report review target = %#v, err=%v; Report = %#v", target, err, report)
	}
	for _, file := range []string{"report.md", "report-manifest.json", "report-target.json", filepath.Join("attachments", "evidence.md")} {
		if _, err := os.Stat(filepath.Join(plan.Paths.Output, "review", file)); err != nil {
			t.Fatalf("frozen Report review input %s unavailable: %v", file, err)
		}
	}
	boundReview, err := db.TaskByID(ctx, fixture.project.ID, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reportPath := filepath.Join(boundReview.WorktreePath, "worker-report.md")
	if err := os.WriteFile(reportPath, []byte("Report review passed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_PANE_ID", "")
	if code, output := start("accept", "t1"); code == 0 || !strings.Contains(output, "report_review_required") {
		t.Fatalf("Report accepted before a Review pass: code=%d: %s", code, output)
	}
	evidencePath := filepath.Join(fixture.home, "projects", fixture.project.Name, "tasks", "t1", "evidence.md")
	if err := os.WriteFile(evidencePath, []byte("changed after admission\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", boundReview.PaneID)
	if code, output := start("signal", "done", "Reviewed the exact Report", "--report", reportPath, "--verdict", "pass"); code == 0 || !strings.Contains(output, "report_bundle_invalid") {
		t.Fatalf("changed Report attachment received a verdict: code=%d: %s", code, output)
	}
	if err := os.WriteFile(evidencePath, []byte("Supporting evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := start("signal", "done", "Reviewed the exact Report", "--report", reportPath); code == 0 || !strings.Contains(output, "Report Review Task requires --verdict") {
		t.Fatalf("Report Review without a verdict code=%d: %s", code, output)
	}
	db, err = store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER fail_report_review_insert BEFORE INSERT ON report_reviews BEGIN SELECT RAISE(ABORT, 'forced Report review write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	failedCode, failedOutput := start("signal", "done", "Reviewed the exact Report", "--report", reportPath, "--verdict", "pass")
	if failedCode == 0 || !strings.Contains(failedOutput, "forced Report review write failure") {
		t.Fatalf("Report Review did not reach the forced verdict write failure: code=%d: %s", failedCode, failedOutput)
	}
	db, err = store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	currentReview, err := db.TaskByID(ctx, fixture.project.ID, boundReview.ID)
	if err != nil {
		t.Fatal(err)
	}
	signals, err := db.TaskSignals(ctx, boundReview.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	reviews, err := db.ReportReviewsForTask(ctx, reportID)
	if err != nil {
		t.Fatal(err)
	}
	if currentReview.State != store.StateWorking || len(signals) != 0 || len(reviews) != 0 {
		t.Fatalf("failed verdict write left a terminal Review or Signal: state=%s signals=%#v reviews=%#v", currentReview.State, signals, reviews)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_report_review_insert`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if code, output := start("signal", "done", "Reviewed the exact Report", "--report", reportPath, "--verdict", "pass"); code != 0 {
		t.Fatalf("Report review completion code=%d: %s", code, output)
	}
	db, err = store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	reviews, err = db.ReportReviewsForTask(ctx, reportID)
	if err != nil || len(reviews) != 1 || reviews[0].ReportDigest != report.ReportDigest || reviews[0].Verdict != "pass" || reviews[0].ReviewTaskID != boundReview.ID {
		t.Fatalf("recorded Report Review = %#v, %v", reviews, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_PANE_ID", "")
	if code, output := start("accept", "t1"); code != 0 || !strings.Contains(output, "accepted_by: user") {
		t.Fatalf("Report acceptance after CLI Review code=%d: %s", code, output)
	}
}

func addPublicationReportReview(t *testing.T, fixture publicationReportFixture) {
	t.Helper()
	ctx := context.Background()
	reviewID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 2, Type: "review", ReviewsTaskID: fixture.task.ID, Title: "Independent Report review", Profile: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.NextTaskLaunchWithIdentity(ctx, reviewID, "reviewer", "model-b", true); err != nil {
		t.Fatal(err)
	}
	transitionAppTaskToReported(t, ctx, fixture.db, fixture.project.ID, reviewID)
	if _, err := fixture.db.RecordReportReview(ctx, fixture.task.ID, reviewID, fixture.task.ReportDigest, "pass"); err != nil {
		t.Fatal(err)
	}
}

func transitionAppTaskToReported(t *testing.T, ctx context.Context, db *store.DB, projectID, taskID int64) {
	t.Helper()
	for _, state := range []store.State{store.StateWorking, store.StateDone, store.StateReported} {
		task, err := db.TaskByID(ctx, projectID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		source := "worker"
		if state == store.StateWorking || state == store.StateReported {
			source = "cli"
		}
		if err := db.Transition(ctx, taskID, task.State, state, source, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReportBundleDigestIsSHA256OfManifest(t *testing.T) {
	value := []byte(`{"version":1}`)
	want := sha256.Sum256(value)
	if got := reportDigest(value); got != hex.EncodeToString(want[:]) {
		t.Fatalf("Report digest = %q, want %x", got, want)
	}
}
