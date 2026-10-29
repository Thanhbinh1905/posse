//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// requireReviewPassNextStep asserts that a passing Review Round of a done
// full-pipeline Change Task queued its Worker the validate-then-publish step
// and raised a Notice, then delivers it with one Watch poll.
func requireReviewPassNextStep(t *testing.T, f *prLifecycleFixture, leadEnv []string, dir string, change store.Task, want ...string) {
	t.Helper()
	ctx := context.Background()
	var messageID int64
	var body string
	if err := f.db.QueryRowContext(ctx, `SELECT id,body FROM messages WHERE task_id=? ORDER BY id DESC LIMIT 1`, change.ID).Scan(&messageID, &body); err != nil {
		t.Fatalf("no instruction reached Change Task %s after its Review Round passed: %v", taskIDOf(change), err)
	}
	for _, text := range append([]string{"posse validate test", "posse validate docs", "posse validate lint", "posse publish"}, want...) {
		if !strings.Contains(body, text) {
			t.Fatalf("next-step instruction lacks %q:\n%s", text, body)
		}
	}
	var notices int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='candidate_validation_queued' AND json_extract(data_json,'$.message_id')=?`, change.ID, messageID).Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("candidate_validation_queued Notices = %d, %v; want 1", notices, err)
	}
	deliverQueuedInstruction(t, f, leadEnv, dir, change, messageID)
}

// deliverQueuedInstruction delivers a queued instruction to the idle Worker of
// change with one Watch poll.
func deliverQueuedInstruction(t *testing.T, f *prLifecycleFixture, leadEnv []string, dir string, change store.Task, messageID int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := herdr.NewWithEnv("herdr", f.env).Run(ctx, "pane", "report-agent", change.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, dir, setEnv(leadEnv, "POSSE_WATCH_TOKEN", ""), "watch", "--poll-only", "--timeout", "2500"); code != 0 {
		t.Fatalf("posse watch --poll-only: %d %s", code, output)
	}
	var status string
	if err := f.db.QueryRowContext(ctx, `SELECT status FROM messages WHERE id=?`, messageID).Scan(&status); err != nil || status != "delivered" {
		t.Fatalf("message %d is %q (%v) after one Watch poll, want delivered", messageID, status, err)
	}
}

// deliverReviewPassNextStep delivers the instruction the last passing Review
// Round queued to change, as the Watch does for an idle done Worker.
func deliverReviewPassNextStep(t *testing.T, f *prLifecycleFixture, leadEnv []string, dir string, change store.Task) {
	t.Helper()
	var messageID int64
	if err := f.db.QueryRowContext(context.Background(), `SELECT CAST(json_extract(data_json,'$.message_id') AS INTEGER) FROM notices WHERE task_id=? AND kind='candidate_validation_queued' ORDER BY id DESC LIMIT 1`, change.ID).Scan(&messageID); err != nil {
		t.Fatalf("no candidate_validation_queued Notice for %s: %v", taskIDOf(change), err)
	}
	deliverQueuedInstruction(t, f, leadEnv, dir, change, messageID)
}

// A done full-pipeline Worker that requested Review needs no Lead nudge to
// continue after the Round passes (#718).
func TestFullPipelineReviewPassQueuesWorkerToValidateAndPublish(t *testing.T) {
	f := reviewIdentityFixture(t)
	stopBackgroundWatch(t, f)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", "full-pipeline")
	if code, output := startReviewIdentityTask(t, f, "Review pass next step", "author", ""); code != 0 {
		t.Fatalf("start Change Task: %d %s", code, output)
	}
	change := f.mustTask(t, "t1")
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "change.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, change.WorktreePath, "add", "change.txt")
	gitTest(t, f.env, change.WorktreePath, "commit", "-m", "add candidate change")
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "publish", "Candidate summary", "--verify", "go test ./... -> passed", "--proof", "Intent behavior verified", "--risk", "Risk: low\nRollback: revert"); code != 0 {
		t.Fatalf("submit candidate: %d %s", code, output)
	}
	candidate, err := f.db.LatestValidationCandidate(context.Background(), change.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, workerEnv, "signal", "done", "Candidate submitted; requesting a Review Round", "--turn", turn); code != 0 {
		t.Fatalf("request Review: %d %s", code, output)
	}

	passCandidateReview(t, f, f.repo, change, "t2")
	requireReviewPassNextStep(t, f, f.leadEnv, f.repo, change, candidate.WorktreePath)
	if change = f.mustTask(t, "t1"); change.State != store.StateWorking {
		t.Fatalf("Change Task state=%s after the next step was delivered, want working", change.State)
	}

	recordCandidateStages(t, f, workerEnv, candidate, func(string) {})
	if code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, workerEnv, "publish", "Candidate summary"); code != 0 || !strings.Contains(output, "promoted: candidate") {
		t.Fatalf("promote validated candidate: %d %s", code, output)
	}
	if turn, err = f.db.TaskTurnToken(context.Background(), change.ID); err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "signal", "done", "Validated candidate promoted", "--turn", turn); code != 0 {
		t.Fatalf("done Signal after promotion: %d %s", code, output)
	}
}

// The clouddbv1 t30 sequence: a workspace Worker submits every Member, asks
// for Review with done, and the Round passes.
func TestFullPipelineWorkspaceReviewPassQueuesWorkerToValidateAndPublish(t *testing.T) {
	f := reviewIdentityFixture(t)
	ws := newFullPipelineWorkspace(t, f)
	brief := filepath.Join(f.root, "workspace-review-pass.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Workspace review pass\ndone_when: checked\nrepos: [api, web]\n---\nChange both Members.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, ws.root, ws.leadEnv, "start", "--brief", brief, "--name", "workspace-review-pass", "--profile", "author"); code != 0 {
		t.Fatalf("start workspace Change Task: code=%d %s", code, output)
	}
	change := mustProjectTask(t, f, ws.project.ID, "t1")
	repos, err := f.db.TaskRepos(context.Background(), change.ID)
	if err != nil || len(repos) != 2 {
		t.Fatalf("workspace Members: %+v %v", repos, err)
	}
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	var want []string
	for _, repo := range repos {
		if err := os.WriteFile(filepath.Join(repo.WorktreePath, "change.txt"), []byte(repo.Repo+" change\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, f.env, repo.WorktreePath, "add", "change.txt")
		gitTest(t, f.env, repo.WorktreePath, "commit", "-m", "change "+repo.Repo)
		if code, output := runReleaseCLI(t, f.binary, repo.WorktreePath, workerEnv,
			"publish", "--repo", repo.Repo, repo.Repo+" candidate summary", "--verify", "go test ./... -> passed",
			"--proof", "Intent behavior verified", "--risk", "Risk: low\nRollback: revert"); code != 0 {
			t.Fatalf("submit %s private candidate: code=%d %s", repo.Repo, code, output)
		}
		candidate, err := f.db.LatestValidationCandidate(context.Background(), change.ID, repo.Repo)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, candidate.WorktreePath, "--repo "+repo.Repo)
	}
	turn, err := f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "signal", "done", "Candidates submitted; requesting a Review Round", "--turn", turn); code != 0 {
		t.Fatalf("request Review: code=%d %s", code, output)
	}

	passCandidateReviewAs(t, f, ws.leadEnv, ws.root, change, "t2")
	requireReviewPassNextStep(t, f, ws.leadEnv, ws.root, change, want...)
}
