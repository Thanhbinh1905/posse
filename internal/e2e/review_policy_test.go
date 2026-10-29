//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func signalReviewPolicyTask(t *testing.T, f *prLifecycleFixture, task store.Task, args ...string) string {
	t.Helper()
	code, output := signalReviewPolicyTaskAllowFailure(t, f, task, args...)
	if code != 0 {
		t.Fatalf("posse signal done: exit %d\n%s", code, output)
	}
	return output
}

func signalReviewPolicyTaskAllowFailure(t *testing.T, f *prLifecycleFixture, task store.Task, args ...string) (int, string) {
	t.Helper()
	env := setEnv(f.env, "HERDR_PANE_ID", task.PaneID)
	env = setEnv(env, "POSSE_WORKER_HOME", f.home)
	// Do not complete a turn before the fake has consumed its launch prompt.
	// A late prompt must not turn the completed pane back into a working pane.
	ready := filepath.Join(f.root, "review-working-"+task.PaneID)
	if !waitForCondition(10*time.Second, func() bool {
		_, err := os.Stat(ready)
		return err == nil
	}) {
		t.Fatalf("Review-policy fake did not consume the launch prompt for %s", task.PaneID)
	}
	command := exec.Command(f.binary, append([]string{"signal", "done", "Checked"}, args...)...)
	command.Dir, command.Env = task.WorktreePath, env
	output, err := command.CombinedOutput()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	// The identity fixture reports working after its launch prompt and then waits
	// for input. Complete that fake turn too, so reconciliation does not correctly
	// interpret its permanently-working pane as resumed work.
	if _, err := herdr.NewWithEnv("herdr", f.env).Run(context.Background(), "pane", "report-agent", task.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	return code, string(output)
}

func TestSelectedReviewPolicyLocalMerge(t *testing.T) {
	f := reviewIdentityFixture(t)
	stopBackgroundWatch(t, f)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", "quick")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "review_loop.auto_start", "false")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "autonomy.yolo", "true", "--project", "shop")
	if code, out := startReviewIdentityTask(t, f, "Selected policy", "author", ""); code != 0 {
		t.Fatalf("start: %d %s", code, out)
	}
	change := f.mustTask(t, "t1")
	gitTest(t, f.env, change.WorktreePath, "commit", "--allow-empty", "-m", "candidate")
	signalReviewPolicyTask(t, f, change)
	before := gitTest(t, f.env, f.repo, "rev-parse", "main")
	for _, args := range [][]string{{"merge", "t1"}, {"merge", "t1", "--execute"}} {
		code, out := runReviewIdentityCLI(t, f, args...)
		if code != 1 || !strings.Contains(out, "review_required") {
			t.Fatalf("missing pass admitted: %d %s", code, out)
		}
	}
	if code, out := startReviewIdentityTask(t, f, "Independent verdict", "reviewer", "t1"); code != 0 {
		t.Fatalf("review: %d %s", code, out)
	}
	review := f.mustTask(t, "t2")
	report := filepath.Join(f.root, "posse", "scratch", "shop", "t2", "report.md")
	if err := os.WriteFile(report, []byte("No findings.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--verdict", "pass")
	if code, out := runReviewIdentityCLI(t, f, "merge", "t1"); code != 0 {
		t.Fatalf("current pass refused: %d %s", code, out)
	}
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "unreviewed-follow-up.txt"), []byte("new review scope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, change.WorktreePath, "add", "unreviewed-follow-up.txt")
	gitTest(t, f.env, change.WorktreePath, "commit", "-m", "unreviewed follow-up")
	code, out := runReviewIdentityCLI(t, f, "merge", "t1")
	if code != 1 || !strings.Contains(out, "review_required") {
		t.Fatalf("stale pass admitted: %d %s", code, out)
	}
	if got := gitTest(t, f.env, f.repo, "rev-parse", "main"); got != before {
		t.Fatal("refused Merge changed main")
	}
	if code, out := startReviewIdentityTask(t, f, "Current verdict", "reviewer", "t1"); code != 0 {
		t.Fatalf("second review: %d %s", code, out)
	}
	review = f.mustTask(t, "t3")
	report = filepath.Join(f.home, "scratch", "shop", "t3", "report.md")
	if err := os.WriteFile(report, []byte("Current candidate passes.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--verdict", "pass")
	// The new pass is current, but the Gate must run again for the changed branch.
	code, out = runReviewIdentityCLI(t, f, "merge", "t1", "--execute")
	if code != 1 || !strings.Contains(out, "branch_moved") {
		t.Fatalf("missing re-gate: %d %s", code, out)
	}
	if code, out = runReviewIdentityCLI(t, f, "merge", "t1", "--execute"); code != 0 {
		t.Fatalf("current pass merge: %d %s", code, out)
	}
	if got := strings.TrimSpace(gitTest(t, f.env, f.repo, "rev-parse", "main")); got != strings.TrimSpace(gitTest(t, f.env, f.repo, "rev-parse", "refs/heads/"+change.Branch)) {
		t.Fatal("merge did not merge the reviewed candidate")
	}
}

func TestQuickLocalMergeUsesExactHeadPassWithoutOrigin(t *testing.T) {
	f := reviewIdentityFixture(t)
	stopBackgroundWatch(t, f)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", "quick")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "autonomy.yolo", "true", "--project", "shop")
	if code, output := startReviewIdentityTask(t, f, "Local without origin", "author", ""); code != 0 {
		t.Fatalf("start Change: %d %s", code, output)
	}
	change := f.mustTask(t, "t1")
	gitTest(t, f.env, change.WorktreePath, "commit", "--allow-empty", "-m", "candidate")
	signalReviewPolicyTask(t, f, change)
	if code, output := startReviewIdentityTask(t, f, "Pass local candidate", "reviewer", "t1"); code != 0 {
		t.Fatalf("start Review: %d %s", code, output)
	}
	review := f.mustTask(t, "t2")
	report := filepath.Join(f.home, "scratch", "shop", "t2", "report.md")
	if err := os.WriteFile(report, []byte("Exact candidate passes.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--verdict", "pass")
	gitTest(t, f.env, change.WorktreePath, "remote", "remove", "origin")
	if code, output := runReviewIdentityCLI(t, f, "merge", "t1"); code != 0 {
		t.Fatalf("exact-head quick pass did not permit local merge without origin: %d %s", code, output)
	}
}

func TestFrozen015LocalReviewPlanKeepsItsEvidenceRequirement(t *testing.T) {
	f := reviewIdentityFixture(t)
	stopBackgroundWatch(t, f)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", "quick")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "review_loop.auto_start", "false")
	if code, out := startReviewIdentityTask(t, f, "Legacy local review", "author", ""); code != 0 {
		t.Fatalf("start: %d %s", code, out)
	}
	change := f.mustTask(t, "t1")
	plan, err := change.AdmittedPlan()
	if err != nil || plan == nil {
		t.Fatalf("read admitted plan: %#v %v", plan, err)
	}
	plan.Version, plan.Review = 3, "local"
	for i := range plan.Preferences {
		if plan.Preferences[i].Name == "review" {
			plan.Preferences[i].Value = "local"
		}
	}
	legacyJSON, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `UPDATE tasks SET plan_json=? WHERE id=?`, legacyJSON, change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=? AND revision=1`, legacyJSON, change.ID); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, change.WorktreePath, "commit", "--allow-empty", "-m", "candidate")
	signalReviewPolicyTask(t, f, change)
	if code, out := runReviewIdentityCLI(t, f, "merge", "t1"); code != 1 || !strings.Contains(out, "review_required") {
		t.Fatalf("frozen local plan skipped its review: %d %s", code, out)
	}
	if code, out := startReviewIdentityTask(t, f, "Review legacy local plan", "reviewer", "t1"); code != 0 {
		t.Fatalf("review: %d %s", code, out)
	}
	review := f.mustTask(t, "t2")
	report := filepath.Join(f.home, "scratch", "shop", "t2", "report.md")
	if err := os.WriteFile(report, []byte("Candidate passes.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--verdict", "pass")
	if code, out := runReviewIdentityCLI(t, f, "merge", "t1"); code != 0 {
		t.Fatalf("frozen local plan rejected its qualifying pass: %d %s", code, out)
	}
}

func TestFullPipelineCanBeFrozenAndSubmitsPrivateCandidateWithoutPublishing(t *testing.T) {
	f := reviewIdentityFixture(t)
	stopBackgroundWatch(t, f)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.merge_mode", "pr")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", "full-pipeline")
	if code, out := startReviewIdentityTask(t, f, "Frozen full pipeline", "author", ""); code != 0 {
		t.Fatalf("start: %d %s", code, out)
	}
	change := f.mustTask(t, "t1")
	plan, err := change.AdmittedPlan()
	if err != nil || plan == nil || plan.Review != "full-pipeline" {
		t.Fatalf("selected mode was not frozen: %#v %v", plan, err)
	}
	gitTest(t, f.env, change.WorktreePath, "commit", "--allow-empty", "-m", "candidate")
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	code, out := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "publish", "Candidate summary")
	if code != 0 || !strings.Contains(out, "candidate_head") || !strings.Contains(out, "validation required before publication") {
		t.Fatalf("full-pipeline did not submit a private candidate without promoting it: %d %s", code, out)
	}
	stored := f.mustTask(t, "t1")
	if stored.PRURL != "" {
		t.Fatalf("unvalidated candidate opened a PR: %s", stored.PRURL)
	}
	command := exec.Command("git", "--git-dir", f.remote, "show-ref", "--verify", "--quiet", "refs/heads/"+change.Branch)
	command.Env = f.env
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("private candidate submission pushed the Task branch: %s", output)
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("could not verify the remote Task branch remained absent: %v %s", err, output)
	}
}

func TestFullPipelineValidatesPrivateCandidateBeforeBareOriginPublication(t *testing.T) {
	f := reviewIdentityFixture(t)
	stopBackgroundWatch(t, f)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.merge_mode", "pr")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", "full-pipeline")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "full_pipeline.run_workflows", "true", "--project", "shop")
	if code, output := startReviewIdentityTask(t, f, "Validate before publish", "author", ""); code != 0 {
		t.Fatalf("start Change Task: code=%d %s", code, output)
	}
	change := f.mustTask(t, "t1")
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "change.txt"), []byte("candidate content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, change.WorktreePath, "add", "change.txt")
	gitTest(t, f.env, change.WorktreePath, "commit", "-m", "add candidate content")
	workflowDir := filepath.Join(change.WorktreePath, ".github", "workflows")
	if err := os.MkdirAll(workflowDir, 0700); err != nil {
		t.Fatal(err)
	}
	workflow := `name: Candidate CI
on: pull_request
jobs:
  local-shell:
    runs-on: ubuntu-latest
    steps:
      - name: Verify candidate and sanitized environment
        run: |
          test -f change.txt
          test -z "${GH_TOKEN:-}"
          printf 'ran\n' > "$POSSE_TASK_TEMP/workflow-ran"
  windows:
    runs-on: windows-latest
    steps:
      - run: exit 99
  secret-dependent:
    runs-on: ubuntu-latest
    env:
      ACTION_TOKEN: ${{ secrets.ACTION_TOKEN }}
    steps:
      - run: exit 99
`
	if err := os.WriteFile(filepath.Join(workflowDir, "ci.yml"), []byte(workflow), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, change.WorktreePath, "add", ".github/workflows/ci.yml")
	gitTest(t, f.env, change.WorktreePath, "commit", "-m", "add candidate workflow fixture")
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	workerEnv = setEnv(workerEnv, "GH_TOKEN", "fixture-secret-must-not-reach-workflow")
	code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv,
		"publish", "Candidate summary", "--verify", "go test ./... -> passed",
		"--proof", "Intent behavior verified", "--risk", "Risk: low\nRollback: revert")
	if code != 0 || !strings.Contains(output, "validation required before publication") {
		t.Fatalf("submit private candidate: code=%d %s", code, output)
	}
	candidate, err := f.db.LatestValidationCandidate(context.Background(), change.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	assertTaskBranchAbsentOnBareOrigin(t, f, change.Branch)
	if code, output := runReviewIdentityCLI(t, f, "review", "start", "t1", "--profile", "reviewer"); code != 0 {
		t.Fatalf("start candidate Review Round: code=%d %s", code, output)
	}
	review := f.mustTask(t, "t2")
	waitForReviewIdentityAgent(t, f, review.PaneID)
	reviewHead := strings.TrimSpace(gitTest(t, f.env, review.WorktreePath, "rev-parse", "HEAD"))
	if reviewHead != candidate.CandidateSHA {
		t.Fatalf("Reviewer checkout head=%s, want private candidate %s", reviewHead, candidate.CandidateSHA)
	}
	report := filepath.Join(f.home, "scratch", "shop", "t2", "report.md")
	findings := filepath.Join(f.home, "scratch", "shop", "t2", "findings.json")
	if err := os.WriteFile(report, []byte("The private candidate passed review.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(findings, []byte(`{"schema":"posse.review-findings.v1","patterns":[],"resolutions":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--findings", findings, "--verdict", "pass")
	assertTaskBranchAbsentOnBareOrigin(t, f, change.Branch)
	for _, args := range [][]string{
		{"validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent/E2E behavior verified"},
		{"validate", "docs", "--no-comments", "passed", "--no-owner-doc-change", "No owner documentation changes are warranted", "--proof", "No unnecessary comments; complete PR description prepared"},
		{"validate", "lint", "--proof", "Configured Gates passed", "--coverage", "Run candidate CI shell steps; report Windows and secret-dependent jobs as not applicable"},
	} {
		code, output = runReleaseCLI(t, f.binary, candidate.WorktreePath, workerEnv, args...)
		if code != 0 {
			t.Fatalf("candidate stage %v failed: %d %s", args, code, output)
		}
		assertTaskBranchAbsentOnBareOrigin(t, f, change.Branch)
	}
	stages, err := f.db.ValidationStages(context.Background(), change.ID)
	if err != nil || len(stages) != 3 {
		t.Fatalf("persisted local stages=%+v err=%v", stages, err)
	}
	var lintCoverage string
	for _, stage := range stages {
		if stage.Status != "passed" || stage.CandidateSHA != candidate.CandidateSHA {
			t.Fatalf("stage is not bound to final private head: %+v", stage)
		}
		if stage.Stage == "lint" {
			lintCoverage = stage.Coverage
		}
	}
	var coverage struct {
		Schema    string `json:"schema"`
		Enabled   bool   `json:"enabled"`
		Workflows []struct {
			Jobs []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"jobs"`
		} `json:"workflows"`
	}
	if err := json.Unmarshal([]byte(lintCoverage), &coverage); err != nil || coverage.Schema != "posse.local-workflow-coverage.v1" || !coverage.Enabled || len(coverage.Workflows) != 1 || len(coverage.Workflows[0].Jobs) != 3 {
		t.Fatalf("candidate-bound workflow coverage is incomplete: %+v err=%v raw=%s", coverage, err, lintCoverage)
	}
	jobs := map[string]string{}
	for _, job := range coverage.Workflows[0].Jobs {
		jobs[job.Name] = job.Status
	}
	if jobs["local-shell"] != "passed" || jobs["windows"] != "not_applicable" || jobs["secret-dependent"] != "not_applicable" {
		t.Fatalf("workflow job outcomes are not explicit: %#v", jobs)
	}
	if data, err := os.ReadFile(filepath.Join(f.home, "scratch", "shop", "t1", "workflow-ran")); err != nil || string(data) != "ran\n" {
		t.Fatalf("supported workflow job did not execute in Task scratch: %q err=%v", data, err)
	}
	code, output = runReleaseCLI(t, f.binary, candidate.WorktreePath, workerEnv, "publish", "Candidate summary")
	if code != 0 || !strings.Contains(output, "promoted: candidate") {
		t.Fatalf("promote validated candidate: %d %s", code, output)
	}
	publishedCommand := exec.Command("git", "--git-dir", f.remote, "rev-parse", "refs/heads/"+change.Branch)
	publishedCommand.Env = f.env
	publishedOutput, err := publishedCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("read published bare-origin head: %v %s", err, publishedOutput)
	}
	published := strings.TrimSpace(string(publishedOutput))
	if published != candidate.CandidateSHA {
		t.Fatalf("bare origin published %s, want validated candidate %s", published, candidate.CandidateSHA)
	}
}

func TestFullPipelineCIRepairRevalidatesPrivateCandidateBeforeRepublish(t *testing.T) {
	f := reviewIdentityFixture(t)
	h := startCIRepairChange(t, f, [2]string{"ci_loop.max_fix_rounds", "2"})
	firstHead := h.publishThenFailCI(t)
	var fixRoundCount int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`, h.change.ID).Scan(&fixRoundCount); err != nil || fixRoundCount != 1 {
		t.Fatalf("CI fix rounds=%d err=%v", fixRoundCount, err)
	}
	// Posse records delivery once Herdr confirms the new turn, which can land
	// after the fake Worker already finished the repair.
	var messageStatus string
	var messageErr error
	if !waitForCondition(15*time.Second, func() bool {
		messageErr = f.db.QueryRowContext(context.Background(), `SELECT m.status FROM validation_fix_rounds r JOIN messages m ON m.id=r.message_id WHERE r.ship_task_id=? AND r.kind='ci'`, h.change.ID).Scan(&messageStatus)
		return messageErr == nil && messageStatus == "delivered"
	}) {
		t.Fatalf("CI repair instruction status=%q err=%v, want delivered", messageStatus, messageErr)
	}
	workerInput, err := os.ReadFile(filepath.Join(f.root, "ci-worker-input.log"))
	if err != nil || !strings.Contains(string(workerInput), "Required CI checks failed") || !strings.Contains(string(workerInput), firstHead) {
		t.Fatalf("fake Worker did not consume the expected head-bound CI repair: %s err=%v", workerInput, err)
	}
	h.assertOriginHead(t, firstHead)
	candidate := h.submitCandidate(t, "Repaired CI candidate")
	if candidate.CandidateSHA == firstHead {
		t.Fatal("CI repair did not produce a fresh private candidate")
	}
	h.validateCandidate(t, candidate, "t3")
	if code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, h.workerEnv, "publish", "Repaired CI candidate"); code != 0 || !strings.Contains(output, "promoted: candidate") {
		t.Fatalf("republish repaired candidate: %d %s", code, output)
	}
	if err := os.WriteFile(filepath.Join(f.root, "ci-worker-allow-done"), []byte("complete\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(10*time.Second, func() bool {
		signalLog, err := os.ReadFile(filepath.Join(f.root, "ci-worker-signal.log"))
		return err == nil && strings.Contains(string(signalLog), "signal-ok")
	}) {
		signalLog, _ := os.ReadFile(filepath.Join(f.root, "ci-worker-signal.log"))
		t.Fatalf("fake Worker did not complete the repaired task: %s", signalLog)
	}
	f.waitTaskState(t, "t1", store.StateDone)
	h.assertOriginHead(t, candidate.CandidateSHA)
	f.writeGraphQL(t, "pr1", "OPEN", "SUCCESS", "REVIEW_REQUIRED", "MERGEABLE", "", candidate.CandidateSHA)
	runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t1")
	observation, err := f.db.LatestPRObservation(context.Background(), h.change.ID)
	if err != nil || observation.HeadSHA != candidate.CandidateSHA || !strings.Contains(observation.Checks, "SUCCESS") {
		t.Fatalf("fresh published head lacks a green CI observation: %#v err=%v", observation, err)
	}
	stages, err := f.db.ValidationStages(context.Background(), h.change.ID)
	if err != nil || len(stages) != 6 {
		t.Fatalf("validation stages for both candidates=%+v err=%v", stages, err)
	}
	for i, stage := range stages[3:] {
		if stage.Stage != []string{"test", "docs", "lint"}[i] || stage.CandidateSHA != candidate.CandidateSHA || stage.Status != "passed" {
			t.Fatalf("final stages are unordered or stale: %+v", stages)
		}
	}
}

func assertTaskBranchAbsentOnBareOrigin(t *testing.T, f *prLifecycleFixture, branch string) {
	t.Helper()
	command := exec.Command("git", "--git-dir", f.remote, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	command.Env = f.env
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("Task branch was published before local validation: %s", output)
	}
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("could not audit bare origin branch refs: %v %s", err, output)
	}
}

func TestSelectedReviewPolicyNoneKeepsMergeAuthorityWithoutCommandGates(t *testing.T) {
	f := reviewIdentityFixture(t)
	stopBackgroundWatch(t, f)
	// No explicit review setting: the built-in policy is none.
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "review_loop.enabled", "true")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "review_loop.max_rounds", "1")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "review_loop.auto_start", "true")
	if code, out := startReviewIdentityTask(t, f, "No independent review", "author", ""); code != 0 {
		t.Fatalf("start: %d %s", code, out)
	}
	change := f.mustTask(t, "t1")
	gitTest(t, f.env, change.WorktreePath, "commit", "--allow-empty", "-m", "candidate")
	signalReviewPolicyTask(t, f, change)
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE tasks SET pr_url=? WHERE id=?`, "https://github.com/acme/shop/pull/17", change.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if code, out := runReviewIdentityCLI(t, f, "show", "t1", "--json"); code != 0 || !strings.Contains(out, `"review":"none"`) {
		t.Fatalf("show did not reflect the admitted review:none policy: %d %s", code, out)
	}
	before := gitTest(t, f.env, f.repo, "rev-parse", "main")
	code, out := runReviewIdentityCLI(t, f, "merge", "t1", "--execute")
	if code != 1 || !strings.Contains(out, "merge_approval_required") {
		t.Fatalf("authority bypassed: %d %s", code, out)
	}
	code, out = runReviewIdentityCLI(t, f, "merge", "t1")
	if code != 0 {
		t.Fatalf("local merge readiness: %d %s", code, out)
	}
	if got := gitTest(t, f.env, f.repo, "rev-parse", "main"); got != before {
		t.Fatal("refusal changed target")
	}
	if code, out = runReviewIdentityCLIWithResourceConflictRetry(t, f, "merge", "t1", "--execute", "--user-approved", "Merge this checked Task"); code != 0 {
		t.Fatalf("none refused: %d %s", code, out)
	}
	db, err = store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rounds, err := db.ReviewRoundsFor(context.Background(), change.ID)
	if err != nil || len(rounds) != 0 {
		t.Fatalf("review policy none unexpectedly admitted auto-start Review Rounds: %#v, %v", rounds, err)
	}
}

func runReviewIdentityCLIWithResourceConflictRetry(t *testing.T, f *prLifecycleFixture, args ...string) (int, string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, output := runReviewIdentityCLI(t, f, args...)
		if code == 0 || !strings.Contains(output, `"resource_conflict"`) || !time.Now().Before(deadline) {
			return code, output
		}
		// Project sync owns the same root-path lease; retry its safe transient conflict.
		time.Sleep(100 * time.Millisecond)
	}
}

func TestSelectedReviewPolicyConfigNeedsUserConsent(t *testing.T) {
	f := reviewIdentityFixture(t)
	for _, key := range []string{"defaults.review", "repositories.api.review"} {
		for _, args := range [][]string{{"config", "set", key, "quick", "--project", "shop"}, {"config", "unset", key, "--project", "shop"}} {
			code, out := runReviewIdentityCLI(t, f, args...)
			if code != 1 || !strings.Contains(out, "user_only") {
				t.Fatalf("unapproved policy write: %d %s", code, out)
			}
		}
		const quote = "  Use local independent review.  "
		code, out := runReviewIdentityCLI(t, f, "config", "set", key, "quick", "--project", "shop", "--user-approved", quote)
		if code != 0 {
			t.Fatalf("consented policy: %d %s", code, out)
		}
		var recorded string
		if err := f.db.QueryRowContext(context.Background(), "SELECT user_quote FROM config_approvals WHERE key=? ORDER BY id DESC LIMIT 1", key).Scan(&recorded); err != nil || recorded != quote {
			t.Fatalf("consent lost: %q %v", recorded, err)
		}
	}
}

func TestSelectedReviewPolicyWorkspaceAdmission(t *testing.T) {
	f := reviewIdentityFixture(t)
	root := filepath.Join(f.root, "policy-stack")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, root, "clone", filepath.Join(f.root, "origin.git"), filepath.Join(root, "api"))
	project, err := f.db.CreateWorkspaceProject(context.Background(), "policy-stack", root, []store.ProjectRepo{{Name: "api", Path: "api", DefaultBranch: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := createWorkspace(herdr.NewWithEnv("herdr", f.env), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetProjectLead(context.Background(), project.ID, workspace.Workspace.WorkspaceID, workspace.RootPane.PaneID, "posse:policy-stack:lead"); err != nil {
		t.Fatal(err)
	}
	leadEnv := setEnv(f.env, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	for _, setting := range [][2]string{{"defaults.merge_mode", "local"}, {"defaults.review", "full-pipeline"}, {"repositories.api.review", "none"}} {
		runPosse(t, f.binary, root, f.env, "config", "set", setting[0], setting[1], "--project", "policy-stack")
	}
	brief := filepath.Join(f.root, "member-policy.md")
	writeBrief := func(fields string) {
		t.Helper()
		title := "Member policy"
		if fields != "" {
			title = "Lighter member"
		}
		if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: "+title+"\ndone_when: checked\nrepos: [api]\n"+fields+"---\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeBrief("")
	runPosse(t, f.binary, root, leadEnv, "start", "--brief", brief, "--name", "member-policy", "--profile", "author")
	tasks, err := f.db.Tasks(context.Background(), project.ID, true)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("workspace Task: %#v %v", tasks, err)
	}
	plan, err := tasks[0].AdmittedPlan()
	if err != nil || plan == nil || len(plan.Execution.Members) != 1 || plan.Execution.Members[0].Review != "none" || plan.Execution.Members[0].ReviewSource != "repositories.api.review" {
		t.Fatalf("member override not admitted: %#v %v", plan, err)
	}
	// A stricter live override does not rewrite the already admitted policy.
	runPosse(t, f.binary, root, f.env, "config", "set", "repositories.api.review", "quick", "--project", "policy-stack")
	writeBrief("review: none\n")
	cmd := exec.Command(f.binary, "start", "--brief", brief, "--name", "lighter-member", "--profile", "author")
	cmd.Dir, cmd.Env = root, leadEnv
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "scoped User choice") {
		t.Fatalf("unapproved Member override: %v %s", err, out)
	}
	const quote = "  Use no independent review for this Member Task.  "
	runPosse(t, f.binary, root, leadEnv, "start", "--brief", brief, "--name", "lighter-member", "--profile", "author", "--user-approved", quote)
	tasks, err = f.db.Tasks(context.Background(), project.ID, true)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("scoped workspace Task: %#v %v", tasks, err)
	}
	for _, task := range tasks {
		plan, err := task.AdmittedPlan()
		if err != nil || plan.Execution.Members[0].Review != "none" {
			t.Fatalf("Member policy changed: %#v %v", plan, err)
		}
		if task.ShortName == "lighter-member" && (plan.UserChoice == nil || plan.UserChoice.Quote != quote || task.AutonomyMerge != "ask") {
			t.Fatalf("Member consent lost or escalated: %#v", plan)
		}
	}
}

func TestReviewPolicyLegacyConfigMigration(t *testing.T) {
	f := reviewIdentityFixture(t)
	for _, value := range []string{"always", "on_risk", "never"} {
		code, out := runReviewIdentityCLI(t, f, "config", "set", "defaults.review", value, "--user-approved", "Select review")
		if code != 1 || !strings.Contains(out, "legacy review policy") || !strings.Contains(out, "none, quick or full-pipeline") {
			t.Errorf("migration %s: %d %s", value, code, out)
		}
	}
}
