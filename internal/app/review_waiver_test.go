package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func TestReviewWaiverPublishesAndMergesOnlyWithExactHeadCI(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateWorking)
	freezeCILoopWithReview(t, f, 0, 0, "full-pipeline")
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("review_provider = \"greenhouse\"\n[defaults]\nmerge_mode = \"pr\"\nmerge_method = \"squash\"\nauto_teardown = \"never\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plan, err := f.task.AdmittedPlan()
	if err != nil {
		t.Fatal(err)
	}
	plan.ReviewProvider = store.GreenhouseReviewProvider
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET plan_json=? WHERE id=?`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=?`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	run := func(want string, args ...string) {
		t.Helper()
		code, output, stderr := f.run(args...)
		if (code == 0) != (want == "") || !strings.Contains(output, want) {
			t.Fatalf("posse %v: code=%d output=%s stderr=%s; want %q", args, code, output, stderr, want)
		}
	}
	const summary = "Deliver the committed change"
	const url = "https://github.com/acme/shop/pull/17"
	const quote = "I approve CI-only delivery for this exact head."
	run("", "signal", "done", summary, "--head", f.headSHA)
	run("full-pipeline requires a passing Greenhouse Verdict", "publish", summary)
	run("user_approval_required", "review", "waive", "t1", "--head", f.headSHA)
	run("review_waiver_head_mismatch", "review", "waive", "t1", "--head", strings.Repeat("a", 40), "--user-approved", quote)
	pane := os.Getenv("HERDR_PANE_ID")
	t.Setenv("HERDR_PANE_ID", f.task.PaneID)
	run("worker_forbidden", "review", "waive", "t1", "--head", f.headSHA, "--user-approved", quote)
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	run("", "review", "waive", "t1", "--head", f.headSHA, "--user-approved", quote)
	run("publish_required", "merge", "t1")
	t.Setenv("HERDR_PANE_ID", pane)
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcileGreenhouseRepositoryPrepublication(ctx, f.db, f.home, f.project, cfg); err != nil {
		t.Fatal(err)
	}
	messages, err := f.db.UndeliveredTaskMessages(ctx, f.task.ID)
	if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Body, "User waived Greenhouse review") {
		t.Fatalf("waiver did not queue publication: %+v, %v", messages, err)
	}
	if verdicts, err := f.db.VerdictsFor(ctx, f.task.ID); err != nil || len(verdicts) != 0 {
		t.Fatalf("waiver recorded a Verdict: %+v, %v", verdicts, err)
	}
	if _, withdrawn, err := f.db.WithdrawQueuedMessage(ctx, messages[0].ID); err != nil || !withdrawn {
		t.Fatalf("withdraw fixture instruction: %v, %v", withdrawn, err)
	}
	before, err := gitOutput(ctx, f.worktree, "ls-remote", "--heads", "origin", "refs/heads/posse/t1")
	if err != nil || before != "" {
		t.Fatalf("waiver pushed the branch: %q, %v", before, err)
	}
	if _, output, _ := f.run("review", "t1", "--full"); !strings.Contains(output, quote) || !strings.Contains(output, f.headSHA) {
		t.Fatalf("review history omitted the waiver: %s", output)
	}
	gitTest(t, f.worktree, "commit", "--allow-empty", "-m", "Advance beyond the waived head")
	advanced := strings.TrimSpace(gitTest(t, f.worktree, "rev-parse", "HEAD"))
	run("full-pipeline requires a passing Greenhouse Verdict", "publish", summary)
	gitTest(t, f.worktree, "reset", "--keep", f.headSHA)
	run("", "publish", summary)
	run("", "signal", "working", "Published the approved head")
	turn, err := f.db.TaskTurnToken(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	run("", "signal", "done", summary, "--head", f.headSHA, "--pr", url, "--turn", turn)
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	run("", "merge", "t1")
	poll := func(checks, head string) {
		t.Helper()
		f.setGraphQLState(t, "OPEN", checks, "APPROVED", "MERGEABLE", "", head)
		if err := f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	poll("PENDING", f.headSHA)
	run("checks_not_green", "merge", "t1", "--execute", "--user-approved", "Merge the PR after CI passes.")
	if data, err := os.ReadFile(f.ghLog); err != nil || strings.Contains(string(data), "pr merge") {
		t.Fatalf("merge reached the forge before CI passed: %s, %v", data, err)
	}
	poll("SUCCESS", advanced)
	run("checks_not_green", "merge", "t1", "--execute", "--user-approved", "Merge the PR after CI passes.")
	poll("SUCCESS", f.headSHA)
	run("merge_approval_required", "merge", "t1", "--execute")
	run("", "merge", "t1", "--execute", "--user-approved", "Merge the PR after CI passes.")
	if data, err := os.ReadFile(f.ghLog); err != nil || !strings.Contains(string(data), "--match-head-commit "+f.headSHA) {
		t.Fatalf("forge merge was not pinned to the waived head: %s, %v", data, err)
	}
}

func TestWorkspaceReviewWaiverIsMemberScopedAndRequiresGreenCI(t *testing.T) {
	f := newWorkspaceMergeFixture(t, "review_provider = \"greenhouse\"\n[defaults]\nmerge_mode = \"pr\"\nauto_teardown = \"never\"\n", []string{"worker", "e2e-tool"}, []string{"worker"})
	ctx := context.Background()
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief: taskplan.Select("brief", "workspace.md", []byte("---\ntype: change\ntitle: Workspace review waiver\ndone_when: exact head is approved\nrepos: [worker, e2e-tool]\n---\n")),
		Execution: taskplan.Execution{Scope: "workspace", Members: []taskplan.Member{
			{Name: "worker", Path: "worker", Base: "main", Target: "main", Delivery: "pr", Review: "full-pipeline", ReviewProvider: store.GreenhouseReviewProvider},
			{Name: "e2e-tool", Path: "e2e-tool", Base: "main", Target: "main", Delivery: "pr", Review: "full-pipeline", ReviewProvider: store.GreenhouseReviewProvider},
		}},
		Launch: taskplan.Launch{ProfileName: "author", Profile: config.Profile{Kind: "pi"}},
		Review: "full-pipeline", ReviewProvider: store.GreenhouseReviewProvider, Delivery: "pr",
		Autonomy: config.Autonomy{Review: "ask", Merge: "ask"},
		Paths:    taskplan.Paths{Scratch: filepath.Join(f.home, "scratch", "stack", "t1"), Output: filepath.Join(f.home, "projects", "stack", "tasks", "t1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET plan_json=? WHERE id=?`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `INSERT INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,?,?)`, f.task.ID, plan.Revision, encoded); err != nil {
		t.Fatal(err)
	}
	f.task.PlanJSON = encoded
	targets, err := workspaceSandboxTargets(ctx, f.db, f.project)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeWorkspaceRootBaseline(ctx, f.home, f.project, f.task, f.task.WorktreePath, targets, plan.Execution.RootContext); err != nil {
		t.Fatal(err)
	}
	cfg := f.config()
	repo := f.repos()["worker"]
	head := strings.TrimSpace(gitTest(t, repo.WorktreePath, "rev-parse", "HEAD"))
	if code := f.run("review", "waive", "t1", "--repo", "worker", "--head", head, "--user-approved", "I approve CI-only delivery for worker at this commit."); code != 0 {
		t.Fatalf("record workspace waiver: %d %s", code, &f.out)
	}
	if err := requireGreenhouseGatePass(ctx, f.db, f.project, cfg, f.task, head, "worker"); err != nil {
		t.Fatalf("approved Member did not clear review: %v", err)
	}
	if err := requireGreenhouseGatePass(ctx, f.db, f.project, cfg, f.task, head, "e2e-tool"); err == nil {
		t.Fatal("another Member reused the waiver for the same SHA")
	}
	otherID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{Seq: 2, Title: "Other Task", Type: "change", State: store.StateSpawning})
	if err != nil {
		t.Fatal(err)
	}
	other := f.task
	other.ID, other.Seq = otherID, 2
	if err := requireGreenhouseGatePass(ctx, f.db, f.project, cfg, other, head, "worker"); err == nil {
		t.Fatal("another Task reused the waiver for the same Member and SHA")
	}
	const repoURL = "https://github.com/acme/worker.git"
	target, err := projectTarget(ctx, f.db, f.project, "worker")
	if err != nil {
		t.Fatal(err)
	}
	origin := strings.TrimSpace(gitTest(t, target.Root, "remote", "get-url", "origin"))
	gitTest(t, target.Root, "config", "url."+origin+".insteadOf", repoURL)
	gitTest(t, target.Root, "remote", "set-url", "origin", repoURL)
	bin := t.TempDir()
	logPath := filepath.Join(bin, "merge.log")
	t.Setenv("WAIVER_MERGE_LOG", logPath)
	const url = "https://github.com/acme/worker/pull/7"
	t.Setenv("WAIVER_PR_URL", url)
	t.Setenv("WAIVER_PR_HEAD", head)
	script := `#!/bin/sh
case "$1 $2" in
'pr list') echo '[]' ;;
'pr create') echo "$WAIVER_PR_URL" ;;
'pr view') printf '{"url":"%s","state":"OPEN","headRefOid":"%s","headRefName":"posse/t1","baseRefName":"main","headRepository":{"nameWithOwner":"acme/worker"}}\n' "$WAIVER_PR_URL" "$WAIVER_PR_HEAD" ;;
'pr merge') printf '%s\n' "$*" >> "$WAIVER_MERGE_LOG" ;;
*) exit 90 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	f.service.Herdr = nil
	t.Setenv("POSSE_WORKER_HOME", f.home)
	t.Chdir(f.task.WorktreePath)
	if code := f.run("publish", "--repo", "worker", "Publish the approved Member"); code != 0 || !strings.Contains(f.out.String(), url) {
		t.Fatalf("publish approved Member: %d %s", code, &f.out)
	}
	if code := f.run("signal", "working", "Published the approved Member"); code != 0 {
		t.Fatalf("reopen the workspace Task: %d %s", code, &f.out)
	}
	turn, err := f.db.TaskTurnToken(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code := f.run("signal", "done", "Published the approved Member", "--turn", turn); code != 0 {
		t.Fatalf("complete the workspace Task: %d %s", code, &f.out)
	}
	t.Setenv("POSSE_WORKER_HOME", "")
	t.Chdir(f.workspace)
	if code := f.run("merge", "t1"); code != 0 {
		t.Fatalf("gate the approved Member PR: %d %s", code, &f.out)
	}
	repo = f.repos()["worker"]
	merge := []string{"merge", "t1", "--execute", "--user-approved", "Merge after CI passes."}
	if code := f.run(merge...); code == 0 || !strings.Contains(f.out.String(), "checks_not_green") {
		t.Fatalf("workspace merge without green CI: %d %s", code, &f.out)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("workspace merge reached the forge without green CI: %v", err)
	}
	if _, err := f.db.RecordMemberPRObservation(ctx, repo.Repo, store.PRObservation{
		ProjectID: f.project.ID, TaskID: f.task.ID, PRURL: repo.PRURL, HeadSHA: head, State: "OPEN",
		Checks: `{"state":"SUCCESS"}`, Mergeable: "MERGEABLE",
	}, store.MemberPREffect{}); err != nil {
		t.Fatal(err)
	}
	if code := f.run(merge...); code != 0 {
		t.Fatalf("workspace merge after green CI: %d %s", code, &f.out)
	}
	if log, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(log), "--match-head-commit "+head) {
		t.Fatalf("workspace merge was not pinned to the waived head: %s, %v", log, err)
	}
}
