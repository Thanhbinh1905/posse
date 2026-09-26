package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func forcePushBareMain(t *testing.T, fixture *prLandingFixture) string {
	checkout := filepath.Join(fixture.root, "rewrite")
	gitTest(t, fixture.root, "clone", "--branch", "main", fixture.remote, checkout)
	gitTest(t, checkout, "config", "user.name", "x")
	gitTest(t, checkout, "config", "user.email", "x@example.test")
	gitTest(t, checkout, "commit", "--amend", "-m", "rewritten root", "--allow-empty")
	gitTest(t, checkout, "push", "--force", "origin", "main")
	return strings.TrimSpace(gitTest(t, checkout, "rev-parse", "HEAD"))
}

func TestReviewForcePushedOriginIsRefusedOnCheckedOutDefault(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	before := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/heads/main"))
	forcePushBareMain(t, fixture)
	cfg, _ := config.Load(fixture.home, fixture.project.Name)
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	after := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/heads/main"))
	t.Logf("result=%+v err=%v", result, err)
	if after != before || result.Status != "root_behind" || result.Reason != "origin history diverged from the local default branch" {
		t.Fatalf("force-pushed origin rewound or was accepted: before=%s after=%s %+v", before, after, result)
	}
}

func TestReviewForcePushedOriginIsRefusedWhenDefaultNotCheckedOut(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	gitTest(t, fixture.repo, "checkout", "-b", "feature")
	before := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/heads/main"))
	forcePushBareMain(t, fixture)
	cfg, _ := config.Load(fixture.home, fixture.project.Name)
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	after := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/heads/main"))
	t.Logf("result=%+v err=%v", result, err)
	if after != before || result.Status != "root_behind" || result.Reason != "origin history diverged from the local default branch" {
		t.Fatalf("force-pushed origin rewound or was accepted: before=%s after=%s %+v", before, after, result)
	}
}

func TestReviewNoOriginDoesNothing(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	gitTest(t, fixture.repo, "remote", "remove", "origin")
	cfg, _ := config.Load(fixture.home, fixture.project.Name)
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	if err != nil || result.Status != "no_origin" {
		t.Fatalf("%+v %v", result, err)
	}
	if code, out, e := fixture.run("show", "t1"); code != 0 {
		t.Fatalf("show failed without origin: %s %s", out, e)
	}
}

// Two watched PRs in different repositories wedge prepareProject.
func TestReviewCrossRepoPRsWedgeReconcile(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	if code, out, e := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("land %s %s", out, e)
	}
	ctx := context.Background()
	id, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 2, Type: "ship", Title: "other", State: store.StateSpawning, LandingMode: "no-mistakes", Branch: "posse/t2", BaseRef: "main", AutonomyLand: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	_ = fixture.db.Transition(ctx, id, store.StateSpawning, store.StateWorking, "cli", "x")
	_ = fixture.db.Transition(ctx, id, store.StateWorking, store.StateDone, "cli", "x")
	_ = fixture.db.UpdateTaskLanding(ctx, id, "https://github.com/other/fork/pull/3", "")
	_ = fixture.db.Transition(ctx, id, store.StateDone, store.StateLanding, "cli", "x")
	_, _ = fixture.db.ExecContext(ctx, `UPDATE tasks SET state='landing', pr_url='https://github.com/other/fork/pull/3' WHERE id=?`, id)
	cfg, _ := config.Load(fixture.home, fixture.project.Name)
	if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatalf("one repository's invalid PR blocked the Project poll: %v", err)
	}
	if _, err := fixture.db.LatestPRObservation(ctx, fixture.task.ID); err != nil {
		t.Fatalf("valid PR alias was not observed: %v", err)
	}
	notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	badPRAttributed := false
	for _, notice := range notices {
		if notice.TaskID == id && notice.Kind == "pr_watch_failing" {
			badPRAttributed = true
		}
	}
	if !badPRAttributed {
		t.Fatalf("bad PR failure was not attributed to t2: %#v", notices)
	}
	fake := fixture.service.Herdr.(*herdr.Fake)
	fake.Results["pane.read"] = []byte(`{"text":"worker output"}`)
	fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{PaneID: "w3:p1", WorkspaceID: "w3", Label: "posse:shop:t2", Agent: "claude", AgentStatus: "idle"})
	_, _ = fixture.db.ExecContext(ctx, `UPDATE tasks SET pane_id='w3:p1', herdr_workspace_id='w3', pane_label='posse:shop:t2' WHERE id=?`, id)
	worktree3 := filepath.Join(fixture.root, "mount-t3")
	gitTest(t, fixture.repo, "worktree", "add", "-b", "posse/t3", worktree3, "refs/heads/main")
	task3, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 3, Type: "ship", Title: "working task", State: store.StateSpawning, LandingMode: "pr", Branch: "posse/t3", BaseRef: "main", WorktreePath: worktree3, HerdrWorkspaceID: "w4", PaneID: "w4:p1", PaneLabel: "posse:shop:t3"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, task3, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{PaneID: "w4:p1", WorkspaceID: "w4", Label: "posse:shop:t3", CWD: worktree3, Agent: "claude", AgentStatus: "working"})
	for _, args := range [][]string{{"show", "t1"}, {}, {"roster"}} {
		if _, err := fixture.db.ExecContext(ctx, `UPDATE project_watch_state SET pr_polled_at=0`); err != nil {
			t.Fatal(err)
		}
		code, out, e := fixture.run(args...)
		t.Logf("%v exit=%d out=%s err=%s", args, code, out, e)
		if code != 0 {
			t.Errorf("command %v wedged by cross-repo PR watch: %s", args, e)
		}
	}
	fixture.test.Chdir(worktree3)
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = &fixture.output, &fixture.errOut
	if code := cli.Run([]string{"holler", "working", "Still making progress"}); code != 0 {
		t.Fatalf("holler was blocked by another Task's PR watch failure: exit=%d output=%s error=%s", code, fixture.output.String(), fixture.errOut.String())
	}
	fixture.test.Chdir(fixture.repo)
	if code, output, errOutput := fixture.run("lookout", "--timeout", "1"); code != 0 {
		t.Fatalf("lookout was blocked by another Task's PR watch failure: exit=%d output=%s error=%s", code, output, errOutput)
	}
}

// A landed Task whose auto teardown fails wedges every reconcile.
func TestReviewAutoTeardownFailureWedgesReconcile(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	configText := "[defaults]\nlanding_mode = \"pr\"\nmerge_method = \"squash\"\nauto_unsaddle = \"landed\"\n"
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, e := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("land %s %s", out, e)
	}
	fake := fixture.service.Herdr.(*herdr.Fake)
	fake.Results["pane.read"] = []byte(`{"text":"worker output"}`)
	fake.SnapshotValue.Agents = []herdr.Agent{{Name: "posse-shop-t1-1", PaneID: "w2:p1"}}
	fake.Errors["pane.close"] = errors.New("boom")
	fixture.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", fixture.headSHA, fixture.headSHA)
	worktree2 := filepath.Join(fixture.root, "mount-t2")
	gitTest(t, fixture.repo, "worktree", "add", "-b", "posse/t2", worktree2, "refs/heads/main")
	task2, err := fixture.db.CreateTask(context.Background(), fixture.project.ID, store.Task{Seq: 2, Type: "ship", Title: "other", State: store.StateSpawning, LandingMode: "pr", Branch: "posse/t2", BaseRef: "main", WorktreePath: worktree2, HerdrWorkspaceID: "w3", PaneID: "w3:p1", PaneLabel: "posse:shop:t2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(context.Background(), task2, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{PaneID: "w3:p1", WorkspaceID: "w3", Label: "posse:shop:t2", CWD: worktree2, Agent: "claude", AgentStatus: "working"})
	fake.SnapshotValue.Agents = append(fake.SnapshotValue.Agents, herdr.Agent{Name: "posse-shop-t2-1", PaneID: "w3:p1"})
	for i, args := range [][]string{{"show", "t1"}, {}, {"roster"}, {"show", "t1"}} {
		if i == 0 {
			_, _ = fixture.db.ExecContext(context.Background(), `UPDATE project_watch_state SET pr_polled_at=0`)
		}
		code, out, e := fixture.run(args...)
		t.Logf("%v exit=%d out=%s err=%s", args, code, out, e)
		if code != 0 {
			t.Errorf("command %v wedged by failing auto teardown", args)
		}
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "unsaddle_incomplete") != 1 {
		t.Fatalf("automatic Teardown failure Notice count = %d, want one: %#v, %v", countNoticeKind(notices, "unsaddle_incomplete"), notices, err)
	}
	if got := fake.CallCount("pane.close"); got < 2 {
		t.Fatalf("automatic Teardown was not retried: %d attempts", got)
	}
	fixture.test.Chdir(worktree2)
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = &fixture.output, &fixture.errOut
	if code := cli.Run([]string{"holler", "working", "Task two still works"}); code != 0 {
		t.Fatalf("holler was blocked by another Task's teardown failure: exit=%d output=%s error=%s", code, fixture.output.String(), fixture.errOut.String())
	}
	fixture.test.Chdir(fixture.repo)
	if code, output, errOutput := fixture.run("lookout", "--timeout", "1"); code != 0 {
		t.Fatalf("lookout was blocked by another Task's teardown failure: exit=%d output=%s error=%s", code, output, errOutput)
	}
	if got := fake.CallCount("pane.close"); got < 2 {
		t.Fatalf("unacknowledged automatic Teardown was not retried: %d attempts", got)
	}
}

// A hanging gh stalls every reconciling command.
func TestReviewHangingGHStallsShow(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	if code, out, e := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("land %s %s", out, e)
	}
	script := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(fixture.bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	_, _ = fixture.db.ExecContext(context.Background(), `UPDATE project_watch_state SET pr_polled_at=0`)
	previousTimeout := externalCommandTimeout
	externalCommandTimeout = 10 * time.Millisecond
	t.Cleanup(func() { externalCommandTimeout = previousTimeout })
	code, out, errOutput := fixture.run("show", "t1")
	if code != 0 {
		t.Fatalf("reconciling show was blocked by hanging gh: output=%s error=%s", out, errOutput)
	}
}

// Closed PR -> done -> land again: task is stuck in landing with no Notice.
func TestReviewRelandAfterClosedPR(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	if code, out, e := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("land %s %s", out, e)
	}
	cfg, _ := config.Load(fixture.home, fixture.project.Name)
	ctx := context.Background()
	fixture.setGraphQLState(t, "CLOSED", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
	if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GH_URL", "https://github.com/acme/shop/pull/18")
	code, out, e := fixture.run("land", "t1")
	if code != 0 {
		t.Fatalf("re-land after closed PR failed: output=%s error=%s", out, e)
	}
	task, _ := fixture.db.Task(ctx, fixture.project.ID, "t1")
	notices, _ := fixture.db.Notices(ctx, fixture.project.ID, false)
	var observations int
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pr_observations WHERE task_id=? AND state='CLOSED' AND pr_url=?`, fixture.task.ID, "https://github.com/acme/shop/pull/17").Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if task.State != store.StateLanding || task.PRURL != "https://github.com/acme/shop/pull/18" || observations != 1 || countNoticeKind(notices, "pr_closed") != 1 {
		t.Fatalf("closed PR was reused after re-land: task=%#v notices=%#v", task, notices)
	}
}

// pr_conflict instructions merge origin/main, keeping the Worker branch pushable without force.
func TestReviewMergedConflictBranchPush(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	if code, out, e := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("land %s %s", out, e)
	}
	updateBareMain(t, fixture, "upstream.txt", "x\n")
	gitTest(t, fixture.worktree, "fetch", "origin")
	gitTest(t, fixture.worktree, "merge", "--no-edit", "origin/main")
	ctx := context.Background()
	_ = fixture.db.Transition(ctx, fixture.task.ID, store.StateLanding, store.StateWorking, "lead", "fix")
	_ = fixture.db.ClearTaskGatedSHA(ctx, fixture.task.ID)
	newHead := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
	fixture.setGHHead(t, newHead)
	if err := os.WriteFile(fixture.ghOpenPRs, []byte(`[{"url":"https://github.com/acme/shop/pull/17","headRefName":"posse/t1","headRefOid":"`+newHead+`"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, e := fixture.run("publish", "Merged conflict fix"); code != 0 {
		t.Fatalf("publish fix: %d %s %s", code, out, e)
	}
	if code, out, e := fixture.run("holler", "done", "Merged conflict fix", "--pr", "https://github.com/acme/shop/pull/17"); code != 0 {
		t.Fatalf("signal fix: %d %s %s", code, out, e)
	}
	code, out, e := fixture.run("land", "t1")
	if code != 0 {
		t.Fatalf("merged conflict fix could not land without rebasing: exit=%d output=%s error=%s", code, out, e)
	}
}

func TestReviewInterruptedPRCreateIsAdoptedOnReconcile(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	if err := fixture.db.SetTaskGatedSHA(ctx, fixture.task.ID, fixture.headSHA); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.StartIntent(ctx, fixture.project.ID, fixture.task.ID, "land --open-pr", "done:pr.create", `{}`, 2147483647); err != nil {
		t.Fatal(err)
	}
	openPRs := filepath.Join(fixture.root, "open-prs.json")
	if err := os.WriteFile(openPRs, []byte(`[ {"url":"https://github.com/acme/shop/pull/18","headRefName":"posse/t1","headRefOid":"`+fixture.headSHA+`"} ]`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GH_OPEN_PRS", openPRs)
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.reconcileIntents(ctx, fixture.db, fixture.project, cfg, herdr.Snapshot{}); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.Task(ctx, fixture.project.ID, "t1")
	if err != nil || task.State != store.StateLanding || task.PRURL != "https://github.com/acme/shop/pull/18" {
		t.Fatalf("interrupted PR create was not adopted: task=%#v err=%v", task, err)
	}
	intents, err := fixture.db.Intents(ctx, fixture.project.ID)
	if err != nil || len(intents) != 0 {
		t.Fatalf("recovered PR create intent remains: %#v err=%v", intents, err)
	}
}

func TestReviewReconcileFailuresDoNotBlockCommandSurface(t *testing.T) {
	t.Run("checkout sync", func(t *testing.T) {
		fixture := newPRLandingFixture(t, "pr", store.StateWorking)
		workerRoot := addReviewWorkingTask(t, fixture, 2)
		offlineRemote := fixture.remote + ".offline"
		if err := os.Rename(fixture.remote, offlineRemote); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Rename(offlineRemote, fixture.remote) })
		assertReviewCommandSurface(t, fixture, workerRoot, false)
		notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
		if err != nil || countNoticeKind(notices, "root_behind") != 1 {
			t.Fatalf("fetch failure was not recorded once: notices=%#v err=%v", notices, err)
		}
	})

	t.Run("interrupted intent", func(t *testing.T) {
		fixture := newPRLandingFixture(t, "pr", store.StateDone)
		workerRoot := addReviewWorkingTask(t, fixture, 2)
		ctx := context.Background()
		if err := fixture.db.SetTaskGatedSHA(ctx, fixture.task.ID, fixture.headSHA); err != nil {
			t.Fatal(err)
		}
		if err := fixture.db.StartIntent(ctx, fixture.project.ID, fixture.task.ID, "land --open-pr", "done:pr.create", `{}`, 2147483647); err != nil {
			t.Fatal(err)
		}
		t.Setenv("POSSE_TEST_GH_FAIL", "1")
		if code, output, errOutput := fixture.run(); code != 0 {
			t.Fatalf("posse was blocked by interrupted intent recovery: exit=%d output=%s error=%s", code, output, errOutput)
		}
		t.Setenv("POSSE_TEST_GH_FAIL", "")
		assertReviewCommandSurface(t, fixture, workerRoot, false)
		notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
		if err != nil || countNoticeKind(notices, "intent_stuck") != 1 {
			t.Fatalf("intent recovery failure was not recorded once: notices=%#v err=%v", notices, err)
		}
	})

	t.Run("Herdr snapshot unavailable", func(t *testing.T) {
		fixture := newPRLandingFixture(t, "pr", store.StateWorking)
		workerRoot := addReviewWorkingTask(t, fixture, 2)
		fake := fixture.service.Herdr.(*herdr.Fake)
		fake.Errors["session.snapshot"] = &herdr.Error{Code: "herdr_unavailable", Message: "simulated snapshot outage"}
		assertReviewCommandSurface(t, fixture, workerRoot, false)
		if got := fake.CallCount("session.snapshot"); got == 0 {
			t.Fatal("Project reconciliation did not attempt a Herdr snapshot")
		}
	})

	t.Run("timed out gh", func(t *testing.T) {
		fixture := newPRLandingFixture(t, "pr", store.StateDone)
		workerRoot := addReviewWorkingTask(t, fixture, 2)
		if code, output, errOutput := fixture.run("land", "t1"); code != 0 {
			t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
		}
		if _, err := fixture.db.AckNotices(context.Background(), fixture.project.ID, []string{"all"}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture.bin, "gh"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		previousTimeout := externalCommandTimeout
		externalCommandTimeout = 10 * time.Millisecond
		t.Cleanup(func() { externalCommandTimeout = previousTimeout })
		assertReviewCommandSurface(t, fixture, workerRoot, true)
	})
}

func addReviewWorkingTask(t *testing.T, fixture *prLandingFixture, sequence int) string {
	t.Helper()
	worktree := filepath.Join(fixture.root, fmt.Sprintf("mount-t%d", sequence))
	branch := fmt.Sprintf("posse/t%d", sequence)
	gitTest(t, fixture.repo, "worktree", "add", "-b", branch, worktree, "refs/heads/main")
	taskID, err := fixture.db.CreateTask(context.Background(), fixture.project.ID, store.Task{
		Seq: sequence, Type: "ship", Title: "working task", State: store.StateSpawning, LandingMode: "pr", Branch: branch,
		BaseRef: "main", WorktreePath: worktree, HerdrWorkspaceID: fmt.Sprintf("w%d", sequence+1),
		PaneID: fmt.Sprintf("w%d:p1", sequence+1), PaneLabel: fmt.Sprintf("posse:shop:t%d", sequence),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(context.Background(), taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	fake := fixture.service.Herdr.(*herdr.Fake)
	fake.Results["pane.read"] = []byte(`{"text":"worker output"}`)
	fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{
		PaneID: fmt.Sprintf("w%d:p1", sequence+1), WorkspaceID: fmt.Sprintf("w%d", sequence+1),
		Label: fmt.Sprintf("posse:shop:t%d", sequence), CWD: worktree, Agent: "claude", AgentStatus: "working",
	})
	return worktree
}

func assertReviewCommandSurface(t *testing.T, fixture *prLandingFixture, workerRoot string, forcePRPoll bool) {
	t.Helper()
	for _, args := range [][]string{{}, {"roster"}, {"show", "t1"}} {
		if forcePRPoll {
			if _, err := fixture.db.ExecContext(context.Background(), `UPDATE project_watch_state SET pr_polled_at=0`); err != nil {
				t.Fatal(err)
			}
		}
		code, output, errOutput := fixture.run(args...)
		if code != 0 {
			t.Fatalf("command %v failed during reconciliation: exit=%d output=%s error=%s", args, code, output, errOutput)
		}
	}
	fixture.test.Chdir(workerRoot)
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = &fixture.output, &fixture.errOut
	if code := cli.Run([]string{"holler", "working", "still making progress"}); code != 0 {
		t.Fatalf("holler failed during reconciliation: exit=%d output=%s error=%s", code, fixture.output.String(), fixture.errOut.String())
	}
	fixture.test.Chdir(fixture.repo)
	if forcePRPoll {
		if _, err := fixture.db.ExecContext(context.Background(), `UPDATE project_watch_state SET pr_polled_at=0`); err != nil {
			t.Fatal(err)
		}
	}
	if code, output, errOutput := fixture.run("lookout", "--timeout", "1"); code != 0 {
		t.Fatalf("lookout failed during reconciliation: exit=%d output=%s error=%s", code, output, errOutput)
	}
}
