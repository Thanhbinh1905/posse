package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func installFullPipelinePlan(t *testing.T, fixture *prMergeFixture, workflowOptIn ...bool) {
	t.Helper()
	runWorkflows := len(workflowOptIn) > 0 && workflowOptIn[0]
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief:     taskplan.Select("brief", "change.md", []byte("---\ntype: change\ntitle: Candidate\ndone_when: candidate is promoted\n---\nImplement the requested change.\n")),
		Execution: taskplan.Execution{Scope: "repo", Base: "main", Target: "main"},
		Launch:    taskplan.Launch{ProfileName: "author", Profile: config.Profile{Kind: "claude"}},
		Review:    "full-pipeline", Delivery: "pr", Autonomy: config.Autonomy{Review: "ask", Merge: "ask"},
		Paths:        taskplan.Paths{Scratch: filepath.Join(fixture.home, "scratch", "shop", "t1"), Output: filepath.Join(fixture.home, "projects", "shop", "tasks", "t1")},
		WorkflowJobs: runWorkflows,
	})
	if err != nil {
		t.Fatalf("resolve full-pipeline plan: %v", err)
	}
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET plan_json=? WHERE id=?`, encoded, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `INSERT INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,1,?)`, fixture.task.ID, encoded); err != nil {
		t.Fatal(err)
	}
	fixture.task, err = fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
}

func submitCandidateForTest(t *testing.T, fixture *prMergeFixture) error {
	t.Helper()
	return fixture.service.submitPrivateCandidate(
		&axi.Context{Context: context.Background(), Out: &bytes.Buffer{}}, fixture.db, fixture.home,
		fixture.project, fixture.task, "", "Candidate summary", "go test ./... -> passed", "Intent proof: verified", "Risk: low\nRollback: revert", false,
	)
}

func runCandidatePromotionForTest(t *testing.T, fixture *prMergeFixture) error {
	t.Helper()
	ctx := context.Background()
	cfg, err := fixture.service.prepareProject(ctx, fixture.db, fixture.project)
	if err != nil {
		return err
	}
	fixture.output.Reset()
	return fixture.service.promotePrivateCandidate(
		&axi.Context{Context: ctx, Out: &fixture.output, ErrOut: &fixture.errOut}, fixture.db, fixture.home,
		fixture.project, fixture.task, "", cfg,
	)
}

func submitThroughCLI(t *testing.T, fixture *prMergeFixture) (string, string) {
	t.Helper()
	code, output, stderr := fixture.run("publish", "Candidate summary", "--verify", "go test ./... -> passed", "--proof", "Intent proof: verified", "--risk", "Risk: low\nRollback: revert")
	if code != 0 {
		t.Fatalf("full-pipeline publish exit=%d output=%s stderr=%s", code, output, stderr)
	}
	return output, stderr
}

func recordCandidateReviewPass(t *testing.T, fixture *prMergeFixture, candidateHead string) {
	t.Helper()
	ctx := context.Background()
	if _, err := fixture.db.NextTaskLaunchWithIdentity(ctx, fixture.task.ID, "author", "model-a", true); err != nil {
		t.Fatal(err)
	}
	var err error
	fixture.task, err = fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	reviewID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{
		Seq: 2, Type: "review", ReviewsTaskID: fixture.task.ID, Title: "Independent candidate review", State: store.StateSpawning,
		MergeMode: "local", ReviewTargets: []store.ReviewTarget{{Repository: fixture.project.Name, Commit: candidateHead}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.NextTaskLaunchWithIdentity(ctx, reviewID, "reviewer", "model-b", true); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, reviewID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	review, err := fixture.db.TaskByID(ctx, fixture.project.ID, reviewID)
	if err != nil {
		t.Fatal(err)
	}
	if state, err := fixture.db.RecordWorkerSignalWithVerdict(ctx, review, "done", "Review passed", map[string]string{}, "task_done", &store.ReviewVerdictInput{
		Verdict: store.ReviewPass, ReportSHA256: strings.Repeat("a", 64),
	}); err != nil || state != store.StateReported {
		t.Fatalf("record independent candidate Review: state=%s err=%v", state, err)
	}
}

func runCandidateCLI(t *testing.T, fixture *prMergeFixture, candidate store.ValidationCandidate, args ...string) (int, string, string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(candidate.WorktreePath); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(previous) }()
	t.Setenv(workerHomeEnv, fixture.home)
	t.Setenv("HERDR_PANE_ID", fixture.task.PaneID)
	fixture.output.Reset()
	fixture.errOut.Reset()
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = &fixture.output, &fixture.errOut
	code := cli.Run(args)
	return code, fixture.output.String(), fixture.errOut.String()
}

func recordCandidateStages(t *testing.T, fixture *prMergeFixture) {
	t.Helper()
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent and E2E behavior verified"); code != 0 {
		t.Fatalf("test stage: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "docs", "--no-comments", "passed", "--no-owner-doc-change", "No owner documentation changes are warranted", "--proof", "No unnecessary comments; complete PR description prepared"); code != 0 {
		t.Fatalf("docs stage: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "lint", "--proof", "Configured Gates passed", "--coverage", "No repository workflow jobs are configured locally"); code != 0 {
		t.Fatalf("lint stage: exit=%d output=%s stderr=%s", code, output, stderr)
	}
}

func addCandidateFix(t *testing.T, fixture *prMergeFixture) string {
	t.Helper()
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate.WorktreePath, "validation-fix.txt"), []byte("candidate-only fix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, candidate.WorktreePath, "add", "validation-fix.txt")
	gitTest(t, candidate.WorktreePath, "commit", "-m", "validation fix")
	return strings.TrimSpace(gitTest(t, candidate.WorktreePath, "rev-parse", "HEAD"))
}

func TestFullPipelineMergeRequiresSubmittedValidatedCandidate(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	installFullPipelinePlan(t, fixture)
	cfg, err := fixture.service.prepareProject(context.Background(), fixture.db, fixture.project)
	if err != nil {
		t.Fatal(err)
	}
	err = requireSelectedReview(context.Background(), fixture.db, fixture.project, cfg, fixture.task)
	if err == nil || !strings.Contains(err.Error(), "no submitted private candidate") {
		t.Fatalf("full-pipeline merge check error=%v, want private candidate barrier", err)
	}
}

func TestFullPipelinePublishSubmitsPrivateCandidateWithoutPushing(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	output, _ := submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.State != store.ValidationCandidateActive || !strings.Contains(output, candidate.CandidateSHA) {
		t.Fatalf("submission output=%s candidate=%+v", output, candidate)
	}
	if remote, err := remoteBranchHead(context.Background(), fixture.worktree, "refs/heads/posse/t1"); err != nil || remote != "" {
		t.Fatalf("submission pushed origin Task branch: head=%q err=%v", remote, err)
	}
	if log, err := os.ReadFile(fixture.ghLog); err == nil && strings.Contains(string(log), "pr create") {
		t.Fatalf("private submission opened a pull request: %s", log)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestFullPipelineStagesAreOrderedAndCandidateMovementInvalidatesThem(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if code, output, _ := runCandidateCLI(t, fixture, candidate, "validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent verified"); code != 1 || !strings.Contains(output, "validation_review_required") {
		t.Fatalf("test skipped the required Review stage: exit=%d output=%s", code, output)
	}
	recordCandidateReviewPass(t, fixture, candidate.CandidateSHA)
	if code, output, _ := runCandidateCLI(t, fixture, candidate, "validate", "docs", "--no-comments", "passed", "--no-owner-doc-change", "No owner documentation changes are warranted", "--proof", "No unnecessary comments; description complete"); code != 1 || !strings.Contains(output, "validation_stage_order") {
		t.Fatalf("docs skipped the required Test stage: exit=%d output=%s", code, output)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent and E2E behavior verified"); code != 0 {
		t.Fatalf("test stage: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if code, output, _ := runCandidateCLI(t, fixture, candidate, "validate", "lint", "--proof", "Gates passed", "--coverage", "no local workflows configured"); code != 1 || !strings.Contains(output, "validation_stage_order") {
		t.Fatalf("lint skipped the required Docs stage: exit=%d output=%s", code, output)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "docs", "--no-comments", "passed", "--no-owner-doc-change", "No owner documentation changes are warranted", "--proof", "No unnecessary comments; complete PR description prepared"); code != 0 {
		t.Fatalf("docs stage: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "lint", "--proof", "Configured Gates passed", "--coverage", "No repository workflow jobs are configured locally"); code != 0 {
		t.Fatalf("lint stage: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	addCandidateFix(t, fixture)
	candidate, err = fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "status"); code != 0 {
		t.Fatalf("refresh candidate validation status: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	candidate, err = fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	stages, err := fixture.db.ValidationStages(context.Background(), fixture.task.ID)
	if err != nil || len(stages) != 3 {
		t.Fatalf("stage history=%+v err=%v", stages, err)
	}
	for _, stage := range stages {
		if stage.Status != "invalidated" || stage.InvalidatedSHA != candidate.CandidateSHA {
			t.Fatalf("candidate change did not invalidate %s result: %+v", stage.Stage, stage)
		}
	}
	if remote, err := remoteBranchHead(context.Background(), fixture.worktree, "refs/heads/posse/t1"); err != nil || remote != "" {
		t.Fatalf("validation or invalidation published before a fresh complete sequence: remote=%q err=%v", remote, err)
	}
}

func TestFullPipelineTargetMovementInvalidatesAndRefreshesCandidate(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	oldCandidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	recordCandidateReviewPass(t, fixture, oldCandidate.CandidateSHA)
	recordCandidateStages(t, fixture)
	gitTest(t, fixture.repo, "commit", "--allow-empty", "-m", "advance frozen target")
	gitTest(t, fixture.repo, "push", "origin", "main")
	if code, output, stderr := runCandidateCLI(t, fixture, oldCandidate, "validate", "status"); code != 0 || !strings.Contains(output, "target_current: false") || !strings.Contains(output, "publication_ready: false") {
		t.Fatalf("target movement did not revoke validation readiness: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	stages, err := fixture.db.ValidationStages(context.Background(), fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range stages {
		if stage.Status != "invalidated" {
			t.Fatalf("target movement retained a passed local stage: %+v", stage)
		}
	}
	if err := runCandidatePromotionForTest(t, fixture); err == nil || !strings.Contains(err.Error(), "frozen target moved after local validation") {
		t.Fatalf("promotion accepted a candidate with a moved target: %v", err)
	}
	if remote, err := remoteBranchHead(context.Background(), fixture.worktree, "refs/heads/posse/t1"); err != nil || remote != "" {
		t.Fatalf("target-stale candidate was published: remote=%q err=%v", remote, err)
	}
	submitThroughCLI(t, fixture)
	newCandidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if newCandidate.ID == oldCandidate.ID || newCandidate.State != store.ValidationCandidateActive || newCandidate.TargetSHA == oldCandidate.TargetSHA {
		t.Fatalf("target refresh did not create a new candidate binding: old=%+v new=%+v", oldCandidate, newCandidate)
	}
}

func TestFullPipelineRepairsReturnThroughPrivateCandidateValidation(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	plan, err := fixture.task.AdmittedPlan()
	if err != nil || plan == nil {
		t.Fatalf("admitted plan=%v err=%v", plan, err)
	}
	observation := store.PRObservation{HeadSHA: fixture.headSHA}
	check := failedCheck{Name: "unit", URL: "https://example.test/check/1"}
	ci := renderCIFixInstruction(fixture.task, plan, observation, "https://example.test/pr/1", "", []failedCheck{check}, map[string]string{"unit": "failed"})
	for _, required := range []string{"submit a private candidate", "Review -> test -> docs -> lint", "do not push the fix directly", "from that candidate checkout to promote it"} {
		if !strings.Contains(strings.ToLower(ci), strings.ToLower(required)) {
			t.Errorf("full-pipeline CI instruction omitted %q: %s", required, ci)
		}
	}
	gate := gateFixInstruction(fixture.task, plan.Inputs, fixture.headSHA, "", "/private/candidate", newGateCommandFailure("go test ./...", "failed", errors.New("exit 1")))
	for _, required := range []string{"/private/candidate", "Review -> test -> docs -> lint", "from the private candidate checkout", "Do not push"} {
		if !strings.Contains(gate, required) {
			t.Errorf("full-pipeline Gate instruction omitted %q: %s", required, gate)
		}
	}
}

func TestFullPipelineWorkflowJobsRequireFrozenUserOptInAndBlockPromotion(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	writeWorkflow(t, fixture.worktree, "ci.yml", `name: candidate workflow
on: pull_request
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - run: test -f change.txt
`)
	gitTest(t, fixture.worktree, "add", ".github/workflows/ci.yml")
	gitTest(t, fixture.worktree, "commit", "-m", "add local workflow")
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	recordCandidateReviewPass(t, fixture, candidate.CandidateSHA)
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "test", "--verify", "fixture test -> passed", "--proof", "Intent and E2E behavior verified"); code != 0 {
		t.Fatalf("test stage: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "docs", "--no-comments", "passed", "--no-owner-doc-change", "No owner documentation changes are warranted", "--proof", "No unnecessary comments; complete PR description prepared"); code != 0 {
		t.Fatalf("docs stage: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if code, output, _ := runCandidateCLI(t, fixture, candidate, "validate", "lint", "--proof", "Configured Gates passed", "--coverage", "Local workflow execution was not enabled"); code != 1 || !strings.Contains(output, "workflow_jobs_blocked") {
		t.Fatalf("unapproved workflow execution did not block lint: exit=%d output=%s", code, output)
	}
	stages, err := fixture.db.ValidationStages(context.Background(), fixture.task.ID)
	if err != nil || len(stages) != 3 {
		t.Fatalf("stage history=%+v err=%v", stages, err)
	}
	var lint *store.ValidationStageResult
	for index := range stages {
		if stages[index].Stage == "lint" {
			lint = &stages[index]
		}
	}
	if lint == nil || lint.Status != "failed" || lint.CandidateSHA != candidate.CandidateSHA {
		t.Fatalf("failed lint result was not bound to candidate: %+v", lint)
	}
	var coverage localWorkflowCoverage
	if err := json.Unmarshal([]byte(lint.Coverage), &coverage); err != nil || coverage.Enabled || len(coverage.Workflows) != 1 || len(coverage.Workflows[0].Jobs) != 1 || coverage.Workflows[0].Jobs[0].Status != "not_applicable" {
		t.Fatalf("disabled job was not durably reported as not applicable: %+v err=%v raw=%s", coverage, err, lint.Coverage)
	}
	if err := runCandidatePromotionForTest(t, fixture); err == nil || !strings.Contains(err.Error(), "no current passing lint result") {
		t.Fatalf("promotion accepted the failed workflow gate: %v", err)
	}
	if remote, err := remoteBranchHead(context.Background(), fixture.worktree, "refs/heads/posse/t1"); err != nil || remote != "" {
		t.Fatalf("unapproved workflow job moved origin: head=%q err=%v", remote, err)
	}
}

func TestSupportedWorkflowFailureIsRecordedAndBlocksPromotion(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture, true)
	writeWorkflow(t, fixture.worktree, "ci.yml", `name: candidate failure
on: pull_request
jobs:
  failing-check:
    runs-on: ubuntu-latest
    steps:
      - run: echo workflow-failure-output; exit 7
`)
	gitTest(t, fixture.worktree, "add", ".github/workflows/ci.yml")
	gitTest(t, fixture.worktree, "commit", "-m", "add failing local workflow")
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	recordCandidateReviewPass(t, fixture, candidate.CandidateSHA)
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "test", "--verify", "fixture test -> passed", "--proof", "Intent and E2E behavior verified"); code != 0 {
		t.Fatalf("test stage: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "docs", "--no-comments", "passed", "--no-owner-doc-change", "No owner documentation changes are warranted", "--proof", "No unnecessary comments; complete PR description prepared"); code != 0 {
		t.Fatalf("docs stage: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if code, output, _ := runCandidateCLI(t, fixture, candidate, "validate", "lint", "--proof", "Gates passed", "--coverage", "Execute pull-request workflow jobs"); code != 1 || !strings.Contains(output, "gate_failed") {
		t.Fatalf("supported workflow failure did not fail lint: exit=%d output=%s", code, output)
	}
	stages, err := fixture.db.ValidationStages(context.Background(), fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var lint *store.ValidationStageResult
	for index := range stages {
		if stages[index].Stage == "lint" {
			lint = &stages[index]
		}
	}
	if lint == nil || lint.Status != "failed" || lint.CandidateSHA != candidate.CandidateSHA || !strings.Contains(lint.Coverage, "workflow-failure-output") {
		t.Fatalf("failed workflow outcome was not candidate-bound and retained: %+v", lint)
	}
	if err := runCandidatePromotionForTest(t, fixture); err == nil || !strings.Contains(err.Error(), "no current passing lint result") {
		t.Fatalf("promotion accepted a failing workflow: %v", err)
	}
	if remote, err := remoteBranchHead(context.Background(), fixture.worktree, "refs/heads/posse/t1"); err != nil || remote != "" {
		t.Fatalf("failing workflow moved origin: head=%q err=%v", remote, err)
	}
}

func TestCandidatePromotionFastForwardsTaskBranchAndOrigin(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	gitTest(t, fixture.worktree, "push", "origin", "refs/heads/posse/t1:refs/heads/posse/t1")
	submitted := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
	submitThroughCLI(t, fixture)
	candidateHead := addCandidateFix(t, fixture)
	recordCandidateReviewPass(t, fixture, candidateHead)
	recordCandidateStages(t, fixture)
	fixture.setGHHead(t, candidateHead)
	fixture.setGraphQLState(t, "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", candidateHead)
	if err := runCandidatePromotionForTest(t, fixture); err != nil {
		t.Fatalf("promote candidate: %v (%s)", err, fixture.output.String())
	}
	localHead := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
	remoteHead := strings.TrimSpace(gitTest(t, fixture.remote, "show-ref", "--hash", "refs/heads/posse/t1"))
	if localHead != candidateHead || remoteHead != candidateHead || !gitAncestor(context.Background(), fixture.worktree, submitted, candidateHead) {
		t.Fatalf("promotion heads local=%s remote=%s candidate=%s submitted=%s", localHead, remoteHead, candidateHead, submitted)
	}
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil || candidate.State != store.ValidationCandidatePromoted || candidate.PromotedSHA != candidateHead {
		t.Fatalf("promoted candidate=%+v err=%v", candidate, err)
	}
}

func TestCandidatePromotionRefusesMovedRemoteAndDirtySandbox(t *testing.T) {
	for _, scenario := range []string{"remote moved", "dirty sandbox"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newPRMergeFixture(t, "pr", store.StateWorking)
			installFullPipelinePlan(t, fixture)
			gitTest(t, fixture.worktree, "push", "origin", "refs/heads/posse/t1:refs/heads/posse/t1")
			submitThroughCLI(t, fixture)
			addCandidateFix(t, fixture)
			candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			candidateHead := strings.TrimSpace(gitTest(t, candidate.WorktreePath, "rev-parse", "HEAD"))
			recordCandidateReviewPass(t, fixture, candidateHead)
			recordCandidateStages(t, fixture)
			if scenario == "remote moved" {
				if err := os.WriteFile(filepath.Join(fixture.repo, "remote-only.txt"), []byte("external change\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				gitTest(t, fixture.repo, "add", "remote-only.txt")
				gitTest(t, fixture.repo, "commit", "-m", "external branch movement")
				diverged := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "HEAD"))
				gitTest(t, fixture.repo, "push", "origin", diverged+":refs/heads/posse/divergence-fixture")
				gitTest(t, fixture.remote, "update-ref", "refs/heads/posse/t1", diverged)
			} else {
				if err := os.WriteFile(filepath.Join(fixture.worktree, "uncommitted.txt"), []byte("keep local data\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
			remoteBefore := strings.TrimSpace(gitTest(t, fixture.remote, "show-ref", "--hash", "refs/heads/posse/t1"))
			err = runCandidatePromotionForTest(t, fixture)
			if err == nil {
				t.Fatal("promotion succeeded despite a moved remote or dirty Sandbox")
			}
			if scenario == "remote moved" && !strings.Contains(err.Error(), "origin Task branch moved after candidate submission") {
				t.Fatalf("remote movement error=%v", err)
			}
			after := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
			remoteAfter := strings.TrimSpace(gitTest(t, fixture.remote, "show-ref", "--hash", "refs/heads/posse/t1"))
			if before != after || remoteBefore != remoteAfter {
				t.Fatalf("refusal changed author/origin heads: local %s -> %s, remote %s -> %s, candidate=%s", before, after, remoteBefore, remoteAfter, candidate.CandidateSHA)
			}
		})
	}
}

func TestPrivateCandidateSubmissionStaysLocalAndRecoversCheckout(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	if err := submitCandidateForTest(t, fixture); err != nil {
		t.Fatalf("submit private candidate: %v", err)
	}
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.State != store.ValidationCandidateActive {
		t.Fatalf("candidate state = %s, want active", candidate.State)
	}
	if remote, err := remoteBranchHead(context.Background(), fixture.worktree, "refs/heads/posse/t1"); err != nil || remote != "" {
		t.Fatalf("Task branch on origin = %q, err=%v; submission must not push", remote, err)
	}
	refHead, found, err := readGitRef(context.Background(), fixture.worktree, candidate.RefName)
	if err != nil || !found || refHead != candidate.SubmittedSHA {
		t.Fatalf("private ref head=%q found=%v err=%v", refHead, found, err)
	}
	checkoutHead, err := gitOutput(context.Background(), candidate.WorktreePath, "rev-parse", "HEAD")
	if err != nil || checkoutHead != candidate.SubmittedSHA {
		t.Fatalf("candidate checkout head=%q err=%v", checkoutHead, err)
	}
	checkoutRef, err := gitOutput(context.Background(), candidate.WorktreePath, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || checkoutRef != candidate.RefName {
		t.Fatalf("candidate checkout ref=%q err=%v", checkoutRef, err)
	}
	if err := os.WriteFile(filepath.Join(candidate.WorktreePath, "validation-fix.txt"), []byte("candidate-only fix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, candidate.WorktreePath, "add", "validation-fix.txt")
	gitTest(t, candidate.WorktreePath, "commit", "-m", "validation fix")
	candidateHead := strings.TrimSpace(gitTest(t, candidate.WorktreePath, "rev-parse", "HEAD"))
	if _, err := gitOutput(context.Background(), fixture.worktree, "worktree", "remove", "--force", candidate.WorktreePath); err != nil {
		t.Fatalf("remove candidate checkout to simulate a crash: %v", err)
	}
	if err := fixture.service.ensureCandidateCheckout(context.Background(), fixture.db, fixture.home, fixture.task, candidate); err != nil {
		t.Fatalf("recover private candidate checkout: %v", err)
	}
	recoveredHead, err := gitOutput(context.Background(), candidate.WorktreePath, "rev-parse", "HEAD")
	if err != nil || recoveredHead != candidateHead {
		t.Fatalf("recovered candidate checkout head=%q, want %q, err=%v", recoveredHead, candidateHead, err)
	}
	updated, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil || updated.CandidateSHA != candidateHead {
		t.Fatalf("recovered candidate record=%+v err=%v", updated, err)
	}
}

func TestCandidateAuthorMovementRequiresResubmission(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.worktree, "author-followup.txt"), []byte("author movement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.worktree, "add", "author-followup.txt")
	gitTest(t, fixture.worktree, "commit", "-m", "author moved during validation")
	actual, err := gitOutput(context.Background(), fixture.worktree, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	code, output, stderr := fixture.run("signal", "working", "Still working")
	if code == 0 || !strings.Contains(output+stderr, candidate.SubmittedSHA) || !strings.Contains(output+stderr, actual) {
		t.Fatalf("movement Signal exit=%d output=%s stderr=%s; want submitted %s and current %s", code, output, stderr, candidate.SubmittedSHA, actual)
	}
	stale, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil || stale.State != store.ValidationCandidateStale {
		t.Fatalf("candidate after author movement=%+v err=%v", stale, err)
	}
	submitThroughCLI(t, fixture)
	fresh, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Generation != candidate.Generation+1 || fresh.State != store.ValidationCandidateActive || fresh.SubmittedSHA != actual {
		t.Fatalf("fresh candidate=%+v; want generation %d at %s", fresh, candidate.Generation+1, actual)
	}
	archivedHead, found, err := readGitRef(context.Background(), fixture.worktree, candidateArchiveRefName(fixture.task, stale))
	if err != nil || !found || archivedHead != stale.CandidateSHA {
		t.Fatalf("archived candidate head=%q found=%v err=%v", archivedHead, found, err)
	}
}
