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
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/herdr"
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

func singleCandidateLane(project store.Project, task store.Task) candidateLane {
	return candidateLane{owner: project, ownerTask: task, project: project, task: task}
}

func submitCandidateForTest(t *testing.T, fixture *prMergeFixture) error {
	t.Helper()
	return fixture.service.submitPrivateCandidate(
		&axi.Context{Context: context.Background(), Out: &bytes.Buffer{}}, fixture.db, fixture.home,
		singleCandidateLane(fixture.project, fixture.task), config.Config{}, "Candidate summary", "go test ./... -> passed", "Intent proof: verified", "Risk: low\nRollback: revert", false,
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
		singleCandidateLane(fixture.project, fixture.task), cfg,
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

func TestSameProfileRelaunchKeepsCandidateAndReviewPass(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	planForRelaunch, err := fixture.task.AdmittedPlan()
	if err != nil || planForRelaunch == nil {
		t.Fatalf("admitted plan = %#v, %v", planForRelaunch, err)
	}
	previousPlanSHA := planForRelaunch.SHA256()
	planForRelaunch.Revision++
	planForRelaunch.PreviousSHA256 = previousPlanSHA
	planForRelaunch.Inputs = append(planForRelaunch.Inputs, taskplan.Select("skill", "posse-spec", []byte("test skill content\n")))
	if err := fixture.db.ReviseTaskPlan(context.Background(), fixture.task, *planForRelaunch); err != nil {
		t.Fatalf("add selected launch skill: %v", err)
	}
	fixture.task, err = fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(fixture.home, "config.toml")
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, append(configBytes, []byte("\n[profiles.author]\nkind = \"claude\"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := dispatch.Resolve(cfg, fixture.task.Type, planForRelaunch.Launch.ProfileName)
	if err != nil {
		t.Fatal(err)
	}
	launch, err := frozenLaunch(resolution, cfg.Kinds)
	if err != nil {
		t.Fatal(err)
	}
	nextPlan, err := planForRelaunch.Reconfigure(launch)
	if err != nil {
		t.Fatal(err)
	}
	nextPlan, err = selectReconfiguredLaunchInputs(nextPlan, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.ReviseTaskPlan(context.Background(), fixture.task, nextPlan); err != nil {
		t.Fatalf("freeze effective launch configuration: %v", err)
	}
	fixture.task, err = fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	recordCandidateReviewPass(t, fixture, candidate.CandidateSHA)
	plan, err := fixture.task.AdmittedPlan()
	if err != nil || plan == nil {
		t.Fatalf("admitted plan = %#v, %v", plan, err)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent and E2E behavior verified"); code != 0 {
		t.Fatalf("test stage: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	before := plan.SHA256()
	ctx := context.Background()
	if _, err := fixture.db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, fixture.project.ID, fixture.worktree, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET profile='author', mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, fixture.task.ID, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, fixture.task.ID, store.StateWorking, store.StateLost, "cli", "simulate Worker loss before relaunch"); err != nil {
		t.Fatal(err)
	}
	fixture.task, err = fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	fake := fixture.service.Herdr.(*herdr.Fake)
	fixture.project.HerdrWorkspaceID, fixture.project.LeadPaneID, fixture.project.LeadLabel = "w1", "w1:p1", "posse:shop:lead"
	lead := herdr.Pane{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"}
	fake.SnapshotValue.Panes = []herdr.Pane{lead}
	fake.Results["tab.create"] = json.RawMessage(`{"tab":{"tab_id":"w1:t1","workspace_id":"w1"},"root_pane":{"pane_id":"w1:p2","tab_id":"w1:t1"}}`)
	fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	fake.BeforeCall = func(method string) {
		if method == "agent.start" {
			fake.SnapshotValue.Panes = []herdr.Pane{lead, {PaneID: "w1:p2", WorkspaceID: "w1", TabID: "w1:t1", Label: fixture.task.PaneLabel, Agent: "claude"}}
		}
	}
	cfg, err = config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.relaunchTask(ctx, fixture.db, fixture.home, fixture.project, cfg, fixture.task, plan.Launch.ProfileName); err != nil {
		t.Fatalf("relaunch Task with its admitted Profile: %v", err)
	}
	current, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentPlan, err := current.AdmittedPlan()
	if err != nil || currentPlan == nil || currentPlan.SHA256() != before {
		var changedFields []string
		if currentPlan != nil {
			changedFields = currentPlan.ChangedFields(*plan)
		}
		t.Fatalf("same-Profile relaunch changed admitted plan: fields=%v err=%v", changedFields, err)
	}
	currentCandidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil || currentCandidate.ID != candidate.ID || currentCandidate.CandidateSHA != candidate.CandidateSHA {
		t.Fatalf("same-Profile relaunch changed candidate: before=%+v after=%+v err=%v", candidate, currentCandidate, err)
	}
	verdicts, err := fixture.db.ReviewVerdictsFor(context.Background(), fixture.task.ID)
	if err != nil || len(verdicts) != 1 || verdicts[0].Verdict != store.ReviewPass || verdicts[0].Targets[0].Commit != candidate.CandidateSHA {
		t.Fatalf("same-Profile relaunch lost Review pass: %#v, %v", verdicts, err)
	}
	stage, err := fixture.db.ValidationStage(ctx, candidate.ID, "test", candidate.CandidateSHA, candidate.TargetSHA, before)
	if err != nil || stage.Status != "passed" {
		t.Fatalf("same-Profile relaunch invalidated test evidence: %#v, %v", stage, err)
	}
	changedReview := *currentPlan
	changedReview.PreviousSHA256 = currentPlan.SHA256()
	changedReview.Revision++
	changedReview.Review = "quick"
	changedReview.Preferences[1].Value = "quick"
	if err := fixture.db.ReviseTaskPlan(ctx, current, changedReview); err != nil {
		t.Fatalf("revise Review policy: %v", err)
	}
	revisedTask, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	revisedPlan, err := revisedTask.AdmittedPlan()
	if err != nil || revisedPlan == nil || revisedPlan.Review != "quick" {
		t.Fatalf("admitted Review policy after revision = %#v, %v", revisedPlan, err)
	}
	currentCandidate, err = fixture.db.LatestValidationCandidate(ctx, fixture.task.ID, "")
	if err != nil || currentCandidate.State != store.ValidationCandidateStale {
		t.Fatalf("review-relevant plan revision left candidate usable: %+v, %v", currentCandidate, err)
	}
	stages, err := fixture.db.ValidationStages(ctx, fixture.task.ID)
	if err != nil || len(stages) != 1 || stages[0].Status != "invalidated" {
		t.Fatalf("review-relevant plan revision retained candidate validation evidence: %+v, %v", stages, err)
	}
	verdicts, err = fixture.db.ReviewVerdictsFor(ctx, fixture.task.ID)
	if err != nil || len(verdicts) != 0 {
		t.Fatalf("review-relevant plan revision retained old verdicts: %#v, %v", verdicts, err)
	}
}

func TestFullPipelineStaleCandidateDoneNamesStaleStateAndNextStep(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	gitTest(t, fixture.worktree, "commit", "--allow-empty", "-m", "advance Task branch")
	t.Setenv("HERDR_PANE_ID", fixture.task.PaneID)
	if code, output, _ := fixture.run("signal", "done", "Need review"); code == 0 || !strings.Contains(output, "stale") || !strings.Contains(output, "posse publish") {
		t.Fatalf("stale candidate Signal exit=%d output=%s", code, output)
	}
}

func TestPublishingStaleCandidateNamesCommitPreservingRecovery(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.MarkValidationCandidateStale(context.Background(), candidate.ID, candidate.TargetSHA); err != nil {
		t.Fatal(err)
	}
	code, output, stderr := runCandidateCLI(t, fixture, candidate, "publish", "--refresh")
	combined := output + stderr
	if code == 0 || !strings.Contains(combined, "candidate_not_promotable") || !strings.Contains(combined, "merge") || !strings.Contains(combined, "--refresh") || !strings.Contains(combined, "Task Sandbox") {
		t.Fatalf("stale candidate recovery guidance: exit=%d output=%s stderr=%s", code, output, stderr)
	}
}

func TestFullPipelineStaleCandidateWithUnchangedAuthorAllowsDoneSignal(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.MarkValidationCandidateStale(context.Background(), candidate.ID, candidate.TargetSHA); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PANE_ID", fixture.task.PaneID)
	turn, err := fixture.db.TaskTurnToken(context.Background(), fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := fixture.run("signal", "done", "Request a review", "--turn", turn); strings.Contains(output+stderr, "candidate_author_moved") {
		t.Fatalf("done Signal reported unchanged Task branch as moved: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if code, output, stderr := fixture.run("publish", "Refresh stale candidate", "--verify", "go test ./internal/app -run TestFullPipelineStaleCandidateWithUnchangedAuthorAllowsDoneSignal -count=1 -> passed", "--proof", "Same Task branch head verified", "--risk", "Risk: low\nRollback: revert"); code != 0 {
		t.Fatalf("publish with unchanged Task branch exit=%d output=%s stderr=%s", code, output, stderr)
	}
}

func TestFullPipelineTargetRefreshKeepsReviewFixInPrivateCandidate(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	fixPath := filepath.Join(candidate.WorktreePath, "review-fix.txt")
	if err := os.WriteFile(fixPath, []byte("review fix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, candidate.WorktreePath, "add", "review-fix.txt")
	gitTest(t, candidate.WorktreePath, "commit", "-m", "apply Review fix")
	fixSHA := strings.TrimSpace(gitTest(t, candidate.WorktreePath, "rev-parse", "HEAD"))
	turn, err := fixture.db.TaskTurnToken(context.Background(), fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "signal", "done", "Review fix completed", "--turn", turn); code != 0 {
		t.Fatalf("record private Review fix: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if code, output, stderr := fixture.run("signal", "working", "Continue after the Review fix"); code != 0 {
		t.Fatalf("resume after private Review fix: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	recordCandidateReviewPass(t, fixture, fixSHA)
	candidate, err = fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.repo, "target-change.txt"), []byte("target advanced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.repo, "add", "target-change.txt")
	gitTest(t, fixture.repo, "commit", "-m", "advance target")
	gitTest(t, fixture.repo, "push", "origin", "main")
	gitTest(t, fixture.worktree, "fetch", "origin")
	gitTest(t, fixture.worktree, "merge", "--no-edit", "origin/main")
	code, output, stderr := fixture.run("publish", "--refresh", "Refreshed candidate", "--verify", "go test ./internal/app -run TestFullPipelineTargetRefreshKeepsReviewFixInPrivateCandidate -count=1 -> passed", "--proof", "Review fix retained", "--risk", "Risk: low\nRollback: revert")
	if code == 0 || !strings.Contains(output+stderr, fixSHA) || !strings.Contains(strings.ToLower(output+stderr), "merge") {
		t.Fatalf("refresh without merging the private fix should print the required Task-branch merge step: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	gitTest(t, fixture.worktree, "merge", "--no-edit", fixSHA)
	code, output, stderr = fixture.run("publish", "--refresh")
	if code != 0 {
		t.Fatalf("refresh after following the printed merge step: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	refreshed, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(context.Background(), refreshed.WorktreePath, "show", refreshed.CandidateSHA+":review-fix.txt"); err != nil {
		t.Fatalf("refreshed candidate %s lost Review fix %s from prior candidate %s: %v", refreshed.CandidateSHA, fixSHA, candidate.CandidateSHA, err)
	}
	if refreshed.Summary != "Candidate summary" || refreshed.Verification != "go test ./... -> passed" || refreshed.Proof != "Intent proof: verified" || refreshed.Risk != "Risk: low\nRollback: revert" {
		t.Fatalf("refresh discarded previous description fields: %+v", refreshed)
	}
	if !strings.Contains(output, "previously recorded Review resolutions carry over") {
		t.Fatalf("refresh did not tell the Worker that prior Review resolutions carry: %s", output)
	}
	turn, err = fixture.db.TaskTurnToken(context.Background(), fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, refreshed, "signal", "done", "Fresh candidate is ready", "--turn", turn); code != 0 {
		t.Fatalf("done without new resolutions after refresh: exit=%d output=%s stderr=%s", code, output, stderr)
	}
}

func TestFullPipelineTargetRefreshRefusesUnrecordedPrivateFix(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate.WorktreePath, "unrecorded-fix.txt"), []byte("private fix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, candidate.WorktreePath, "add", "unrecorded-fix.txt")
	gitTest(t, candidate.WorktreePath, "commit", "-m", "apply unrecorded Review fix")
	fixSHA := strings.TrimSpace(gitTest(t, candidate.WorktreePath, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(fixture.repo, "target-change.txt"), []byte("target advanced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.repo, "add", "target-change.txt")
	gitTest(t, fixture.repo, "commit", "-m", "advance target")
	gitTest(t, fixture.repo, "push", "origin", "main")
	gitTest(t, fixture.worktree, "fetch", "origin")
	gitTest(t, fixture.worktree, "merge", "--no-edit", "origin/main")
	code, output, stderr := fixture.run("publish", "Refresh candidate with unrecorded fix", "--verify", "go test ./internal/app -run TestFullPipelineTargetRefreshRefusesUnrecordedPrivateFix -count=1 -> passed", "--proof", "Private candidate fix commit retained", "--risk", "Risk: low\nRollback: revert")
	if code == 0 || !strings.Contains(output+stderr, fixSHA) || !strings.Contains(output+stderr, "candidate_review_fix_unmerged") {
		t.Fatalf("publish did not name the unrecorded private fix: exit=%d output=%s stderr=%s", code, output, stderr)
	}
}

func TestFullPipelineStaleCandidateAllowsNeedsDecisionSignal(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.MarkValidationCandidateStale(context.Background(), candidate.ID, candidate.SubmittedSHA); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PANE_ID", fixture.task.PaneID)
	if code, output, stderr := fixture.run("signal", "needs-decision", "Need direction"); code != 0 {
		t.Fatalf("needs-decision with stale candidate exit=%d output=%s stderr=%s", code, output, stderr)
	}
}

func TestFullPipelinePublishRefreshesCandidateAfterTaskBranchMergeInOneCall(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	if err := os.WriteFile(filepath.Join(fixture.repo, "target-change.txt"), []byte("target advanced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.repo, "add", "target-change.txt")
	gitTest(t, fixture.repo, "commit", "-m", "advance target")
	gitTest(t, fixture.repo, "push", "origin", "main")
	gitTest(t, fixture.worktree, "fetch", "origin")
	gitTest(t, fixture.worktree, "merge", "--no-edit", "origin/main")
	if code, output, stderr := fixture.run("publish", "Candidate summary", "--verify", "go test ./... -> passed", "--proof", "Intent proof: verified", "--risk", "Risk: low\nRollback: revert"); code != 0 {
		t.Fatalf("publish after target merge exit=%d output=%s stderr=%s", code, output, stderr)
	}
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil || candidate.State != store.ValidationCandidateActive {
		t.Fatalf("fresh candidate after merge = %+v, %v", candidate, err)
	}
}

func TestFullPipelineDoneNoticeAndShowNameReviewStart(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	plan, err := fixture.task.AdmittedPlan()
	if err != nil || plan == nil {
		t.Fatalf("admitted plan=%v err=%v", plan, err)
	}
	plan.ValidationLoops = &taskplan.ValidationLoops{Review: config.ReviewLoop{Enabled: true, MaxRounds: 3, BlockAt: "P1"}}
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET plan_json=? WHERE id=?`, encoded, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	fixture.task, err = fixture.db.TaskByID(context.Background(), fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "signal", "done", "Candidate submitted"); code != 0 {
		t.Fatalf("candidate done Signal exit=%d output=%s stderr=%s", code, output, stderr)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	foundNextAction := false
	for _, notice := range notices {
		if notice.TaskID == fixture.task.ID && notice.Kind == "task_done" && strings.Contains(notice.Summary, "posse review start t1") {
			foundNextAction = true
		}
	}
	if !foundNextAction {
		t.Fatalf("task_done Notice omitted the Review command: %#v", notices)
	}
	_ = os.Unsetenv(workerHomeEnv)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	if code, output, stderr := fixture.run("show", "t1"); code != 0 || !strings.Contains(output, "posse review start t1") {
		t.Fatalf("show omitted the Review command: exit=%d output=%s stderr=%s", code, output, stderr)
	}
}

func TestValidationStatusUsesMergeReviewEvidence(t *testing.T) {
	for _, evidenceKind := range []string{"carry", "accept-head"} {
		t.Run(evidenceKind, func(t *testing.T) {
			fixture := newPRMergeFixture(t, "pr", store.StateWorking)
			installFullPipelinePlan(t, fixture)
			submitThroughCLI(t, fixture)
			candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if evidenceKind == "carry" {
				if err := fixture.db.RecordReviewCarry(context.Background(), store.ReviewCarry{
					ChangeTaskID: fixture.task.ID, Repository: fixture.project.Name,
					FromHead: strings.Repeat("b", 40), ToHead: candidate.CandidateSHA,
					TargetCommitSHA: strings.Repeat("c", 40), Tree: strings.Repeat("d", 40),
				}); err != nil {
					t.Fatal(err)
				}
			} else if err := fixture.db.RecordReviewDisposition(context.Background(), fixture.task.ID, 0, []string{candidate.CandidateSHA}, "User accepted this head", "P1", "user", ""); err != nil {
				t.Fatal(err)
			}
			recordCandidateStages(t, fixture)
			if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "status"); code != 0 || !strings.Contains(output, "review_pass: true") || !strings.Contains(output, "publication_ready: true") {
				t.Fatalf("active candidate status did not match Merge review evidence: exit=%d output=%s stderr=%s", code, output, stderr)
			}
			if err := fixture.db.BeginValidationCandidatePromotion(context.Background(), candidate.ID); err != nil {
				t.Fatal(err)
			}
			if err := fixture.db.CompleteValidationCandidatePromotion(context.Background(), candidate.ID, candidate.CandidateSHA); err != nil {
				t.Fatal(err)
			}
			if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "status"); code != 0 || !strings.Contains(output, "candidate_state: promoted") || !strings.Contains(output, "review_pass: true") || !strings.Contains(output, "publication_ready: false") {
				t.Fatalf("promoted candidate status did not match Merge review evidence: exit=%d output=%s stderr=%s", code, output, stderr)
			}
		})
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

func TestFullPipelinePromotedCandidateAllowsNextGenerationAfterTargetAndAuthorMove(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	gitTest(t, fixture.worktree, "push", "origin", "refs/heads/posse/t1:refs/heads/posse/t1")
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	candidateHead := addCandidateFix(t, fixture)
	recordCandidateReviewPass(t, fixture, candidateHead)
	recordCandidateStages(t, fixture)
	fixture.setGHHead(t, candidateHead)
	fixture.setGraphQLState(t, "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", candidateHead)
	if err := runCandidatePromotionForTest(t, fixture); err != nil {
		t.Fatalf("promote candidate: %v", err)
	}

	gitTest(t, candidate.WorktreePath, "commit", "--allow-empty", "-m", "fix failed CI")
	newHead := strings.TrimSpace(gitTest(t, candidate.WorktreePath, "rev-parse", "HEAD"))
	gitTest(t, fixture.worktree, "merge", "--ff-only", newHead)
	gitTest(t, fixture.repo, "commit", "--allow-empty", "-m", "advance frozen target")
	gitTest(t, fixture.repo, "push", "origin", "main")

	statusCode, statusOutput, statusStderr := runCandidateCLI(t, fixture, candidate, "validate", "status")
	if statusCode != 0 || !strings.Contains(statusOutput, "target_current: false") {
		t.Errorf("status for moved promoted candidate: exit=%d output=%s stderr=%s", statusCode, statusOutput, statusStderr)
	}
	publishCode, publishOutput, publishStderr := fixture.run("publish", "Candidate summary", "--verify", "go test ./internal/app -> passed", "--proof", "Intent proof: verified", "--risk", "Risk: low\nRollback: revert")
	if publishCode != 0 {
		t.Errorf("publish next generation: exit=%d output=%s stderr=%s", publishCode, publishOutput, publishStderr)
	}
	fresh, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil || fresh.Generation != candidate.Generation+1 || fresh.State != store.ValidationCandidateActive || fresh.SubmittedSHA != strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD")) {
		t.Errorf("next candidate=%+v err=%v", fresh, err)
	}
}

func TestFullPipelineTargetMovedValidationHelpExplainsMergeRefresh(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.repo, "commit", "--allow-empty", "-m", "advance frozen target")
	gitTest(t, fixture.repo, "push", "origin", "main")
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent verified"); code == 0 || !strings.Contains(output, "merge the latest target") || !strings.Contains(output, "do not rebase") {
		t.Fatalf("stale candidate help did not explain the carry-preserving refresh: exit=%d output=%s stderr=%s", code, output, stderr)
	}
}

func TestFullPipelineDoneChecksCandidateContinuityAgainstFixInstruction(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	fix, err := fixture.db.QueueValidationFixInstruction(context.Background(), fixture.task.ID, "ci", candidate.CandidateSHA, "ci:continuity", "Fix the failing CI")
	if err != nil || !fix.Queued {
		t.Fatalf("queue candidate fix instruction: %#v, %v", fix, err)
	}
	gitTest(t, candidate.WorktreePath, "commit", "--allow-empty", "--amend", "-m", "rewrite candidate history")
	_, err = fixture.service.recordWorkerSignalWithContinuity(context.Background(), fixture.db, fixture.project, fixture.task, "done", "Finished the fix", nil, "task_done", nil, false)
	if err == nil || !strings.Contains(err.Error(), "does not descend from expected head") {
		t.Fatalf("discontinuous full-pipeline candidate was accepted: %v", err)
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
	ci := renderCIFixInstruction(fixture.task, plan, observation, repositoryForge{}, "https://example.test/pr/1", "", []failedCheck{check}, map[string]string{"unit": "failed"})
	for _, required := range []string{"submit a private candidate", "Review -> test -> docs -> lint", "do not push the fix directly", "from that candidate checkout to promote it"} {
		if !strings.Contains(strings.ToLower(ci), strings.ToLower(required)) {
			t.Errorf("full-pipeline CI instruction omitted %q: %s", required, ci)
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
	if code, output, _ := runCandidateCLI(t, fixture, candidate, "validate", "lint", "--proof", "Gates passed", "--coverage", "Execute pull-request workflow jobs"); code != 1 || !strings.Contains(output, "workflow_jobs_failed") {
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

func TestCandidatePromotionRefusesMergedTaskPR(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	gitTest(t, fixture.worktree, "push", "origin", "refs/heads/posse/t1:refs/heads/posse/t1")
	submitted := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
	if err := fixture.db.RecordVerifiedPRHead(context.Background(), fixture.task.ID, "https://github.com/acme/shop/pull/17", submitted); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET pr_url=? WHERE id=?`, "https://github.com/acme/shop/pull/17", fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	submitThroughCLI(t, fixture)
	candidateHead := addCandidateFix(t, fixture)
	recordCandidateReviewPass(t, fixture, candidateHead)
	recordCandidateStages(t, fixture)
	fixture.setGHHead(t, submitted)
	t.Setenv("POSSE_TEST_GH_VIEW_STATE", "MERGED")

	before := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
	remoteBefore := strings.TrimSpace(gitTest(t, fixture.remote, "show-ref", "--hash", "refs/heads/posse/t1"))
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	code, output, stderr := runCandidateCLI(t, fixture, candidate, "publish", "Candidate summary")
	if code == 0 || !strings.Contains(output+stderr, "pull request merged") {
		t.Fatalf("publish result = %d, %s %s; want refusal for merged PR", code, output, stderr)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD")); got != before {
		t.Fatalf("Sandbox advanced to %s after merged-PR refusal, want %s", got, before)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.remote, "show-ref", "--hash", "refs/heads/posse/t1")); got != remoteBefore {
		t.Fatalf("remote advanced to %s after merged-PR refusal, want %s", got, remoteBefore)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "pr create") {
		t.Fatalf("merged PR refusal created another pull request: %s", log)
	}
}

func TestCandidatePromotionResumesAfterCrashBetweenPushAndPullRequest(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	gitTest(t, fixture.worktree, "push", "origin", "refs/heads/posse/t1:refs/heads/posse/t1")
	submitThroughCLI(t, fixture)
	candidateHead := addCandidateFix(t, fixture)
	recordCandidateReviewPass(t, fixture, candidateHead)
	recordCandidateStages(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	// An earlier promotion recorded promoting, fast-forwarded and pushed the
	// Task branch, then died before it opened or updated the PR.
	if err := fixture.db.BeginValidationCandidatePromotion(context.Background(), candidate.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.StartIntent(context.Background(), fixture.project.ID, fixture.task.ID, "publish", "candidate.push", "{}", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE intents SET process_id=0,process_boot_id='',process_start_time='' WHERE task_id=?`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.worktree, "merge", "--ff-only", candidateHead)
	gitTest(t, fixture.worktree, "push", "origin", candidateHead+":refs/heads/posse/t1")
	fixture.setGHHead(t, candidateHead)
	fixture.setGraphQLState(t, "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", candidateHead)
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "status"); code != 1 || !strings.Contains(output, "candidate_promotion_in_progress") || !strings.Contains(output, candidateHead[:7]) || !strings.Contains(output, "posse publish") {
		t.Fatalf("validation during promotion: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if code, output, stderr := fixture.run("publish"); code != 1 || !strings.Contains(output, "candidate_promotion_in_progress") || !strings.Contains(output, candidateHead[:7]) || !strings.Contains(output, "posse publish") {
		t.Fatalf("summary-free publish during promotion: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if code, output, stderr := fixture.run("publish", "Candidate summary"); code != 1 || !strings.Contains(output, "candidate_promotion_in_progress") || !strings.Contains(output, candidateHead[:7]) || !strings.Contains(output, "posse publish") {
		t.Fatalf("publish during promotion: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "publish"); code != 0 {
		t.Fatalf("resume interrupted promotion: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	candidate, err = fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil || candidate.State != store.ValidationCandidatePromoted || candidate.PromotedSHA != candidateHead {
		t.Fatalf("resumed candidate=%+v err=%v", candidate, err)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "intent_stuck" && strings.Contains(notice.Summary, "unknown interrupted command") {
			t.Fatalf("publish recovery reported an unknown interrupted command: %+v", notice)
		}
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

func TestFullPipelinePublishRefreshesCandidateAfterSquashOnUnchangedTarget(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	if err := os.WriteFile(filepath.Join(fixture.worktree, "second-change.txt"), []byte("second worker change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.worktree, "add", "second-change.txt")
	gitTest(t, fixture.worktree, "commit", "-m", "second worker change")
	submitThroughCLI(t, fixture)
	oldCandidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	gitTest(t, fixture.worktree, "reset", "--soft", "refs/remotes/origin/main")
	gitTest(t, fixture.worktree, "commit", "-m", "squash Task changes")
	squashedHead := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
	if squashedHead == oldCandidate.SubmittedSHA {
		t.Fatal("squash did not rewrite Task history")
	}
	if tree := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD^{tree}")); tree != strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", oldCandidate.SubmittedSHA+"^{tree}")) {
		t.Fatalf("squash changed the Task tree: got %s", tree)
	}
	if target := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "refs/remotes/origin/main")); target != oldCandidate.TargetSHA {
		t.Fatalf("target moved during squash: got %s, candidate target %s", target, oldCandidate.TargetSHA)
	}

	if code, output, stderr := fixture.run("publish", "Candidate summary", "--verify", "go test ./... -> passed", "--proof", "Intent proof: verified", "--risk", "Risk: low\nRollback: revert"); code != 0 {
		t.Fatalf("publish after squash on unchanged target exit=%d output=%s stderr=%s", code, output, stderr)
	}
	fresh, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == oldCandidate.ID || fresh.Generation != oldCandidate.Generation+1 || fresh.State != store.ValidationCandidateActive || fresh.SubmittedSHA != squashedHead || fresh.TargetSHA != oldCandidate.TargetSHA {
		t.Fatalf("fresh candidate=%+v; old candidate=%+v", fresh, oldCandidate)
	}
	candidates, err := fixture.db.ValidationCandidates(context.Background(), fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var oldState string
	for _, candidate := range candidates {
		if candidate.ID == oldCandidate.ID {
			oldState = candidate.State
		}
	}
	if oldState != store.ValidationCandidateSuperseded {
		t.Fatalf("old candidate state after squash refresh=%q, want %q", oldState, store.ValidationCandidateSuperseded)
	}
}

func TestFullPipelineSquashRefreshDoesNotAllowReturningToPromotedHead(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	promoted, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.BeginValidationCandidatePromotion(context.Background(), promoted.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.CompleteValidationCandidatePromotion(context.Background(), promoted.ID, promoted.CandidateSHA); err != nil {
		t.Fatal(err)
	}
	promoted, err = fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	gitTest(t, fixture.worktree, "commit", "--amend", "-m", "rewritten history with the same tree")
	submitThroughCLI(t, fixture)
	pending, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.worktree, "reset", "--hard", promoted.PromotedSHA)
	if code, output, stderr := fixture.run("publish", "Candidate summary", "--verify", "go test ./... -> passed", "--proof", "Intent proof: verified", "--risk", "Risk: low\nRollback: revert"); code == 0 || !strings.Contains(output+stderr, "returned to the promoted head") {
		t.Fatalf("publish accepted a return to the promoted head: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	stale, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil || stale.ID != pending.ID || stale.State != store.ValidationCandidateStale {
		t.Fatalf("pending candidate after rejected return=%+v err=%v", stale, err)
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

func TestWorkerProtocolGuidesFullPipelineLanes(t *testing.T) {
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief:     taskplan.Select("brief", "change.md", []byte("---\ntype: change\ntitle: Candidate\ndone_when: candidate is promoted\n---\nImplement the requested change.\n")),
		Execution: taskplan.Execution{Scope: "repo", Base: "main", Target: "main"},
		Launch:    taskplan.Launch{ProfileName: "author", Profile: config.Profile{Kind: "claude"}},
		Review:    "full-pipeline", Delivery: "local", Autonomy: config.Autonomy{Review: "ask", Merge: "ask"},
		Paths: taskplan.Paths{Scratch: "/home/user/.posse/scratch/shop/t1", Output: "/home/user/.posse/projects/shop/tasks/t1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	brief := dispatch.Brief{Type: "change", DoneWhen: "candidate is promoted"}
	local := workerProtocol("/home/user/.posse", store.Project{Name: "shop"}, store.Task{Seq: 1, Type: "change", MergeMode: "local", PlanJSON: encoded}, brief, "/tmp/launch.md", "/home/user/.posse/scratch/shop/t1", "none")
	for _, want := range []string{"Full-pipeline validation", "`posse publish \"<summary>\" --verify", "`posse validate test --verify", "local delivery only fast-forwards the Task branch", "To request a Review Round, run `posse signal done", "Never use needs-decision to ask for Review", "Risk that states how to roll back", "without a new candidate"} {
		if !strings.Contains(local, want) {
			t.Fatalf("local full-pipeline protocol lacks %q: %s", want, local)
		}
	}
	workspace := workerProtocol("/home/user/.posse", store.Project{Name: "stack", Kind: store.ProjectKindWorkspace}, store.Task{Seq: 2, Type: "change", PlanJSON: encoded}, brief, "/tmp/launch.md", "/home/user/.posse/scratch/stack/t2", "none")
	if !strings.Contains(workspace, "`posse validate lint --repo <member>") || !strings.Contains(workspace, "`posse publish --repo <member> \"<summary>\"` to promote") || !strings.Contains(workspace, "after submitting every changed Member and end the turn") {
		t.Fatalf("workspace full-pipeline protocol lacks per-Member commands: %s", workspace)
	}
	quick := workerProtocol("/home/user/.posse", store.Project{Name: "shop"}, store.Task{Seq: 3, Type: "change", MergeMode: "local"}, brief, "/tmp/launch.md", "/home/user/.posse/scratch/shop/t3", "none")
	if strings.Contains(quick, "Full-pipeline") {
		t.Fatalf("protocol without full-pipeline mentions it: %s", quick)
	}
}

func TestLocalFullPipelineBarrierRequiresGatedHeadToBeTheCandidate(t *testing.T) {
	fixture := newPRMergeFixture(t, "local", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitted := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
	submitThroughCLI(t, fixture)
	candidateHead := addCandidateFix(t, fixture)
	recordCandidateReviewPass(t, fixture, candidateHead)
	recordCandidateStages(t, fixture)
	if err := runCandidatePromotionForTest(t, fixture); err != nil {
		t.Fatalf("promote local candidate: %v (%s)", err, fixture.output.String())
	}
	if head := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD")); head != candidateHead {
		t.Fatalf("local promotion left Task branch at %s, want %s", head, candidateHead)
	}
	ctx := context.Background()
	cfg, err := fixture.service.prepareProject(ctx, fixture.db, fixture.project)
	if err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	task.GatedSHA = candidateHead
	if err := requireSelectedReview(ctx, fixture.db, fixture.project, cfg, task); err != nil {
		t.Fatalf("validated local candidate refused: %v", err)
	}
	// Local Merge fast-forwards to the gated head, so a gated head other than
	// the candidate fails even while the branch tip is the candidate.
	task.GatedSHA = submitted
	if err := requireSelectedReview(ctx, fixture.db, fixture.project, cfg, task); err == nil || !strings.Contains(err.Error(), "gated head") {
		t.Fatalf("gated head %s accepted for candidate %s: %v", submitted, candidateHead, err)
	}
}

// Once every full-pipeline Member merged on the forge, before the PR poll
// records it, the barrier admits those Members; a quick Member in the same
// workspace must still need its own exact-head review.
func TestMixedWorkspaceQuickMemberKeepsReviewAfterFullPipelineMembersMerge(t *testing.T) {
	f := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"pr\"\nauto_teardown = \"never\"\n", []string{"worker", "e2e-tool"}, []string{"worker", "e2e-tool"})
	ctx := context.Background()
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief: taskplan.Select("brief", "workspace.md", []byte("---\ntype: change\ntitle: Mixed\ndone_when: checked\nrepos: [worker, e2e-tool]\n---\n")),
		Execution: taskplan.Execution{Scope: "workspace", Members: []taskplan.Member{
			{Name: "e2e-tool", Path: "e2e-tool", Delivery: "local", Review: "quick", ReviewSource: "repositories.e2e-tool.review"},
			{Name: "worker", Path: "worker", Delivery: "pr", Review: "full-pipeline", ReviewSource: "defaults.review"},
		}},
		Launch: taskplan.Launch{ProfileName: "author", Profile: config.Profile{Kind: "claude"}},
		Review: "full-pipeline", Delivery: "pr", Autonomy: config.Autonomy{Review: "ask", Merge: "ask"},
		Paths: taskplan.Paths{Scratch: filepath.Join(f.home, "scratch", "stack", "t1"), Output: filepath.Join(f.home, "projects", "stack", "tasks", "t1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, "UPDATE tasks SET plan_json=? WHERE id=?", encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `INSERT INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,1,?)`, f.task.ID, encoded); err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(f.workspace, "worker")
	gitTest(t, origin, "remote", "set-url", "origin", "https://github.com/acme/worker.git")
	gitTest(t, origin, "config", "url.file://"+filepath.Join(f.root, "remotes", "worker.git")+".insteadOf", "https://github.com/acme/worker.git")
	worktree := filepath.Join(f.task.WorktreePath, "worker")
	head := strings.TrimSpace(gitTest(t, worktree, "rev-parse", "refs/heads/posse/t1"))
	// The forge merged the PR at the validated head and moved origin main.
	frozenTarget := strings.TrimSpace(gitTest(t, worktree, "rev-parse", "refs/heads/posse/t1~1"))
	gitTest(t, worktree, "push", "origin", head+":refs/heads/posse/t1")
	gitTest(t, f.root, "--git-dir", filepath.Join(f.root, "remotes", "worker.git"), "update-ref", "refs/heads/main", head)
	prURL := "https://github.com/acme/worker/pull/7"
	if _, err := f.db.ExecContext(ctx, `UPDATE task_repos SET landing_mode=CASE repo WHEN 'worker' THEN 'pr' ELSE 'local' END, state=CASE repo WHEN 'worker' THEN 'merging' ELSE state END, gated_sha=CASE repo WHEN 'worker' THEN ? ELSE gated_sha END, pr_url=CASE repo WHEN 'worker' THEN ? ELSE pr_url END WHERE task_id=?`, head, prURL, f.task.ID); err != nil {
		t.Fatal(err)
	}
	candidate, err := f.db.CreateValidationCandidate(ctx, store.ValidationCandidate{
		TaskID: f.task.ID, Repo: "worker", SubmittedSHA: head, CandidateSHA: head, TargetSHA: frozenTarget,
		RefName: "refs/posse/candidate/t1", WorktreePath: filepath.Join(f.home, "projects", "stack", "tasks", "t1", "candidates", "worker", "g1"),
		Summary: "s", Verification: "v -> ok", Proof: "p", Risk: "Rollback: revert",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.ActivateValidationCandidate(ctx, candidate.ID, head); err != nil {
		t.Fatal(err)
	}
	if err := f.db.BeginValidationCandidatePromotion(ctx, candidate.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CompleteValidationCandidatePromotion(ctx, candidate.ID, head); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(f.root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$1 $2\" in\n \"pr view\") printf '{\"state\":\"MERGED\",\"headRefOid\":\"" + head + "\"}\\n' ;;\n *) exit 90 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	task, err := f.db.TaskByID(ctx, f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.config()
	if err := requireFullPipelineValidationAt(ctx, f.db, f.project, cfg, task, "", ""); err != nil {
		t.Fatalf("forge-merged full-pipeline Member was not admitted: %v", err)
	}
	err = requireSelectedReview(ctx, f.db, f.project, cfg, task)
	if err == nil || !strings.Contains(err.Error(), "review_required") && !strings.Contains(err.Error(), "no passing Review") {
		t.Fatalf("quick Member merged without its own review: %v", err)
	}
}

func TestCIFixInstructionNamesRunJobAndLogCommand(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	observation := store.PRObservation{HeadSHA: fixture.headSHA}
	check := failedCheck{Name: "unit", URL: "https://github.com/acme/app/actions/runs/11/job/22", RunID: "11", JobID: "22"}
	ci := renderCIFixInstruction(fixture.task, nil, observation, repositoryForge{Kind: "github"}, "https://github.com/acme/app/pull/1", "", []failedCheck{check}, map[string]string{"unit": "failed"})
	for _, required := range []string{"Run: 11", "Job: 22", "`gh run view --job 22 --log-failed --repo acme/app`"} {
		if !strings.Contains(ci, required) {
			t.Errorf("CI fix instruction omitted %q: %s", required, ci)
		}
	}
}

func TestRequireSubmittedCandidateKeepsStoreErrorsDistinctFromMissing(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	db, err := store.Open(filepath.Join(root, "home"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "t", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	task := store.Task{ID: taskID}
	var refusal *axi.Error
	_, err = requireSubmittedCandidate(ctx, db, task, candidateLane{})
	if !errors.As(err, &refusal) || refusal.Code != "candidate_missing" {
		t.Fatalf("absent candidate = %v, want candidate_missing", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = requireSubmittedCandidate(ctx, db, task, candidateLane{})
	if err == nil || errors.As(err, &refusal) {
		t.Fatalf("store error = %v, want it returned unchanged", err)
	}
}

func TestDocsStageNamesEachIncompleteDescriptionField(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	recordCandidateReviewPass(t, fixture, candidate.CandidateSHA)
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent and E2E behavior verified"); code != 0 {
		t.Fatalf("test stage: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE validation_candidates SET verification='',risk='Risk: low' WHERE id=?`, candidate.ID); err != nil {
		t.Fatal(err)
	}
	docs := []string{"validate", "docs", "--no-comments", "passed", "--no-owner-doc-change", "none warranted", "--proof", "reviewed"}
	code, output, _ := runCandidateCLI(t, fixture, candidate, docs...)
	if code != 1 || !strings.Contains(output, "validation_pr_description_incomplete") {
		t.Fatalf("docs accepted an incomplete description: exit=%d output=%s", code, output)
	}
	for _, want := range []string{"--verify", "Risk must state how to roll back", "posse publish"} {
		if !strings.Contains(output, want) {
			t.Fatalf("refusal lacks %q: %s", want, output)
		}
	}
	for _, phrasing := range []string{"Rollback: revert", "we can roll back by reverting", "roll-back: revert", "Rolling back is a revert"} {
		if _, err := fixture.db.ExecContext(context.Background(), `UPDATE validation_candidates SET verification='go test -> passed',risk=? WHERE id=?`, "Risk: low\n"+phrasing, candidate.ID); err != nil {
			t.Fatal(err)
		}
		if code, output, stderr := runCandidateCLI(t, fixture, candidate, docs...); code != 0 {
			t.Fatalf("docs refused risk phrasing %q: exit=%d output=%s stderr=%s", phrasing, code, output, stderr)
		}
	}
}

func TestPublishResubmissionWithoutFlagsKeepsReviewedCandidateDescription(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	recordCandidateReviewPass(t, fixture, candidate.CandidateSHA)
	if err := fixture.db.MarkValidationCandidateStale(context.Background(), candidate.ID, candidate.TargetSHA); err != nil {
		t.Fatal(err)
	}

	code, output, stderr := fixture.run("publish", "Candidate summary")
	if code != 0 {
		t.Fatalf("resubmit reviewed head: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	candidate, err = fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Summary != "Candidate summary" || candidate.Verification != "go test ./... -> passed" || candidate.Proof != "Intent proof: verified" || candidate.Risk != "Risk: low\nRollback: revert" {
		t.Fatalf("resubmission dropped candidate description: %+v", candidate)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent and E2E behavior verified"); code != 0 {
		t.Fatalf("test stage: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "docs", "--no-comments", "passed", "--no-owner-doc-change", "No owner documentation changes are warranted", "--proof", "Complete PR description prepared"); code != 0 {
		t.Fatalf("docs stage after same-head resubmission: exit=%d output=%s stderr=%s", code, output, stderr)
	}
}

func TestDocsHelpUpdatesDescriptionFromReviewedCandidateCheckout(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	fixSHA := addCandidateFix(t, fixture)
	recordCandidateReviewPass(t, fixture, fixSHA)
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent and E2E behavior verified"); code != 0 {
		t.Fatalf("test stage: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE validation_candidates SET summary='',verification='',proof='',risk='' WHERE id=?`, candidate.ID); err != nil {
		t.Fatal(err)
	}
	code, output, _ := runCandidateCLI(t, fixture, candidate, "validate", "docs", "--no-comments", "passed", "--no-owner-doc-change", "No owner documentation changes are warranted", "--proof", "Complete PR description prepared")
	if code != 1 || !strings.Contains(output, "validation_pr_description_incomplete") {
		t.Fatalf("docs accepted incomplete description: exit=%d output=%s", code, output)
	}
	if !strings.Contains(output, "candidate checkout") {
		t.Fatalf("docs help does not name a candidate-current path: %s", output)
	}
	code, output, stderr := runCandidateCLI(t, fixture, candidate, "publish", "--refresh", "Candidate summary", "--verify", "go test ./... -> passed", "--proof", "Intent proof: verified", "--risk", "Risk: low\nRollback: revert")
	if code != 0 {
		t.Fatalf("follow docs help in candidate checkout: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	updated, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if updated.CandidateSHA != fixSHA || updated.State != store.ValidationCandidateActive || updated.Summary == "" || updated.Verification == "" || updated.Proof == "" || updated.Risk == "" {
		t.Fatalf("candidate stopped being current or description was not updated: %+v", updated)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, updated, "validate", "docs", "--no-comments", "passed", "--no-owner-doc-change", "No owner documentation changes are warranted", "--proof", "Complete PR description prepared"); code != 0 {
		t.Fatalf("docs after following help: exit=%d output=%s stderr=%s", code, output, stderr)
	}
}

func TestPublishOnUnchangedCandidateAppliesNewDescriptionFlags(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	before, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	code, output, stderr := fixture.run("publish", "Better summary", "--verify", "go vet ./... -> clean", "--proof", "New proof", "--risk", "Risk: none\nRollback: git revert")
	if code != 0 || !strings.Contains(output, "resumed") {
		t.Fatalf("resubmit exit=%d output=%s stderr=%s", code, output, stderr)
	}
	after, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID || after.CandidateSHA != before.CandidateSHA {
		t.Fatalf("resubmission replaced the candidate: before=%+v after=%+v", before, after)
	}
	if after.Summary != "Better summary" || after.Verification != "go vet ./... -> clean" || after.Proof != "New proof" || after.Risk != "Risk: none\nRollback: git revert" {
		t.Fatalf("description not updated: %+v", after)
	}
	if code, output, stderr := fixture.run("publish", "Better summary", "--risk", "Risk: none\nRollback: git revert"); code != 0 {
		t.Fatalf("partial resubmit exit=%d output=%s stderr=%s", code, output, stderr)
	}
	partial, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil || partial.Verification != after.Verification || partial.Proof != after.Proof {
		t.Fatalf("flags that were not supplied must keep their stored values: %+v err=%v", partial, err)
	}
}

func TestResubmittedDescriptionInvalidatesDocsAndLint(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	recordCandidateReviewPass(t, fixture, candidate.CandidateSHA)
	for _, args := range [][]string{
		{"validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent verified"},
		{"validate", "docs", "--no-comments", "passed", "--no-owner-doc-change", "none warranted", "--proof", "reviewed"},
		{"validate", "lint", "--proof", "Gates passed", "--coverage", "no local workflows configured"},
	} {
		if code, output, stderr := runCandidateCLI(t, fixture, candidate, args...); code != 0 {
			t.Fatalf("%v: exit=%d output=%s stderr=%s", args, code, output, stderr)
		}
	}
	if code, output, stderr := fixture.run("publish", "Candidate summary", "--risk", "Risk: low"); code != 0 {
		t.Fatalf("resubmit exit=%d output=%s stderr=%s", code, output, stderr)
	}
	stages, err := fixture.db.ValidationStages(context.Background(), fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range stages {
		want := "invalidated"
		if stage.Stage == "test" {
			want = "passed"
		}
		if stage.Status != want {
			t.Fatalf("%s status=%s, want %s after the description changed", stage.Stage, stage.Status, want)
		}
	}
	if code, output, _ := runCandidateCLI(t, fixture, candidate, "publish"); code == 0 {
		t.Fatalf("promotion accepted a description whose docs and lint results were invalidated: %s", output)
	}
}
