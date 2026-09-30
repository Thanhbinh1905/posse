package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/runtime"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLocalLandingRequiresApprovalAndTeardownAudits(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	worktree := filepath.Join(root, "worktree")
	initRepo(t, repo)
	baseSHA := strings.TrimSpace(gitTest(t, repo, "rev-parse", "refs/heads/main"))
	gitTest(t, repo, "tag", "main", baseSHA)
	gitTest(t, repo, "worktree", "add", "-b", "posse/t1", worktree, "refs/heads/main")
	if err := os.WriteFile(filepath.Join(worktree, "change.txt"), []byte("worker change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, worktree, "add", "change.txt")
	gitTest(t, worktree, "commit", "-m", "worker change")

	home := filepath.Join(root, "posse-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nlanding_mode = \"local\"\nauto_unsaddle = \"never\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 1, Type: "ship", Title: "Fix", LandingMode: "local", Branch: "posse/t1", BaseRef: "main", WorktreePath: worktree, HerdrWorkspaceID: "w2", PaneID: "w2:p1", PaneLabel: "posse:shop:t1", AutonomyLand: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), taskID, store.StateWorking, store.StateDone, "worker", "Worker Signal"); err != nil {
		t.Fatal(err)
	}
	baseFake := herdr.NewFake()
	fake := &changingSnapshotAdapter{Fake: baseFake}
	fake.snapshot = herdr.Snapshot{FocusedPaneID: "w1:p1", Workspaces: []herdr.Workspace{{WorkspaceID: "w2", Root: worktree}}, Panes: []herdr.Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "working", Focused: true},
		{PaneID: "w2:p1", WorkspaceID: "w2", TabID: "w2:t1", Label: "posse:shop:t1", CWD: worktree},
		{PaneID: "w2:p2", WorkspaceID: "w2", TabID: "w2:t1", Label: "user-notes", CWD: repo},
	}}
	service := testService(home, fake)
	output := &bytes.Buffer{}
	errorsOut := &bytes.Buffer{}
	cli := service.CLI()
	cli.Out = output
	cli.ErrOut = errorsOut
	t.Chdir(repo)

	if code := cli.Run([]string{"land", "t1", "--merge"}); code != 1 {
		t.Fatalf("land without approval exit = %d; output=%s", code, output.String())
	}
	output.Reset()
	task, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil || task.State != store.StateDone {
		t.Fatalf("task state changed before merge approval = %q, %v", task.State, err)
	}
	if notices, err := db.Notices(context.Background(), project.ID, false); err != nil || len(notices) != 0 {
		t.Fatalf("land without approval created Notices = %#v, %v", notices, err)
	}

	if code := cli.Run([]string{"land", "t1", "--merge", "--user-approved", "User approved this merge"}); code != 0 {
		t.Fatalf("approved local merge exit = %d; output=%s error=%s", code, output.String(), errorsOut.String())
	}
	if got := strings.TrimSpace(gitTest(t, repo, "branch", "--show-current")); got != "main" {
		t.Fatalf("Project checkout branch = %q", got)
	}
	if got := strings.TrimSpace(gitTest(t, repo, "show", "refs/heads/main:change.txt")); got != "worker change" {
		t.Fatalf("merged file = %q", got)
	}
	mainSHA := strings.TrimSpace(gitTest(t, repo, "rev-parse", "refs/heads/main"))
	if !strings.Contains(output.String(), mainSHA) {
		t.Fatalf("landed_ref does not name refs/heads/main %s: %s", mainSHA, output.String())
	}

	output.Reset()
	gitTest(t, repo, "worktree", "remove", worktree)
	if code := cli.Run([]string{"unsaddle", "t1"}); code != 0 {
		t.Fatalf("landed teardown exit = %d; output=%s error=%s", code, output.String(), errorsOut.String())
	}
	if fake.CallCount("worktree.remove") != 0 {
		t.Fatal("teardown must keep the Mount instead of asking Herdr to remove it")
	}
	if fake.CallCount("workspace.close") != 0 || fake.CallCount("pane.close") != 1 {
		t.Fatalf("teardown did not close only the Task pane: %#v", fake.Calls)
	}
	if !strings.Contains(output.String(), "w2:p2") {
		t.Fatalf("Teardown did not list the foreign pane: %s", output.String())
	}
	foreignPaneRemains := false
	for _, pane := range fake.currentSnapshot().Panes {
		if pane.PaneID == "w2:p2" {
			foreignPaneRemains = true
		}
	}
	if !foreignPaneRemains {
		t.Fatal("Teardown closed the User's pane")
	}
	task, err = db.Task(context.Background(), project.ID, "t1")
	if err != nil || task.State != store.StateTornDown {
		t.Fatalf("teardown state = %q, %v", task.State, err)
	}
	var mergeApprovals, tornTransitions int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM approvals WHERE task_id=? AND action='merge'`, taskID).Scan(&mergeApprovals); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM transitions WHERE task_id=? AND to_state='torn-down'`, taskID).Scan(&tornTransitions); err != nil {
		t.Fatal(err)
	}
	if mergeApprovals != 1 || tornTransitions != 1 {
		t.Fatalf("approval and teardown transition counts = %d, %d", mergeApprovals, tornTransitions)
	}
}

func newLandedLocalTeardownFixture(t *testing.T) (*prLandingFixture, store.Task, config.Config) {
	t.Helper()
	fixture := newPRLandingFixture(t, "local", store.StateDone)
	t.Cleanup(func() { _ = fixture.db.Close() })
	attachPRFixtureMount(t, fixture)
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(`[defaults]
landing_mode = "local"
auto_unsaddle = "finished"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.repo, "merge", "--ff-only", "refs/heads/posse/t1")
	ctx := context.Background()
	if err := fixture.db.Transition(ctx, fixture.task.ID, store.StateDone, store.StateLanding, "cli", "gate passed"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, fixture.task.ID, store.StateLanding, store.StateLanded, "cli", "merged"); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, task, cfg
}

func TestLandTreatsConcurrentAutoTeardownAsSuccess(t *testing.T) {
	fixture, task, cfg := newLandedLocalTeardownFixture(t)
	ctx := context.Background()
	// Model the other CLI that owns teardown with a live parent process id.
	if err := fixture.db.StartIntent(ctx, fixture.project.ID, task.ID, "unsaddle", "in_progress:panes.close", `{"discard":false}`, os.Getppid()); err != nil {
		t.Fatal(err)
	}
	intent, err := fixture.db.IntentByTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	finishDone := make(chan error, 1)
	go func() {
		<-release
		if err := fixture.db.Transition(ctx, task.ID, store.StateLanded, store.StateTornDown, "cli", "automatic teardown completed"); err != nil {
			finishDone <- err
			return
		}
		finishDone <- fixture.db.FinishIntent(ctx, intent.ID, intent.ProcessID)
	}()
	defer releaseOnce.Do(func() { close(release) })

	type teardownOutcome struct {
		completedElsewhere bool
		err                error
	}
	helpDone := make(chan teardownOutcome, 1)
	go func() {
		_, completedElsewhere, err := fixture.service.teardownLandedTask(ctx, fixture.db, fixture.project, cfg, task)
		helpDone <- teardownOutcome{completedElsewhere: completedElsewhere, err: err}
	}()
	var outcome teardownOutcome
	select {
	case outcome = <-helpDone:
	case <-time.After(time.Second):
	}
	releaseOnce.Do(func() { close(release) })
	if outcome.err == nil && !outcome.completedElsewhere {
		outcome = <-helpDone
	}
	if err := <-finishDone; err != nil {
		t.Fatalf("concurrent Teardown completion failed: %v", err)
	}
	if outcome.err != nil || !outcome.completedElsewhere {
		t.Fatalf("concurrent local Land Teardown = completedElsewhere %v, err %v", outcome.completedElsewhere, outcome.err)
	}
	landed, err := fixture.db.TaskByID(ctx, fixture.project.ID, task.ID)
	if err != nil || landed.State != store.StateTornDown {
		t.Fatalf("Task after concurrent Teardown = %#v, %v", landed, err)
	}
}

func TestLandReturnsErrorWhenConcurrentTeardownFails(t *testing.T) {
	fixture, task, cfg := newLandedLocalTeardownFixture(t)
	ctx := context.Background()
	if err := fixture.db.StartIntent(ctx, fixture.project.ID, task.ID, "unsaddle", "in_progress:panes.close", `{"discard":false}`, os.Getppid()); err != nil {
		t.Fatal(err)
	}
	intent, err := fixture.db.IntentByTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		completedElsewhere bool
		err                error
	}
	done := make(chan outcome, 1)
	go func() {
		_, completedElsewhere, err := fixture.service.teardownLandedTask(ctx, fixture.db, fixture.project, cfg, task)
		done <- outcome{completedElsewhere: completedElsewhere, err: err}
	}()
	select {
	case got := <-done:
		t.Fatalf("Land returned before the competing Teardown finished: %#v", got)
	case <-time.After(200 * time.Millisecond):
	}
	// Model a failed Teardown owner finishing its intent without the state transition.
	if err := fixture.db.FinishIntent(ctx, intent.ID, intent.ProcessID); err != nil {
		t.Fatal(err)
	}
	got := <-done
	current, err := fixture.db.TaskByID(ctx, fixture.project.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	mount, err := fixture.db.MountByTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var tornDown int
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transitions WHERE task_id=? AND to_state='torn-down'`, task.ID).Scan(&tornDown); err != nil {
		t.Fatal(err)
	}
	if got.err == nil || got.completedElsewhere || current.State != store.StateLanded || mount.State != "held" || tornDown != 0 {
		t.Fatalf("failed competing Teardown result=%#v state=%s Mount=%s torn-down transitions=%d", got, current.State, mount.State, tornDown)
	}
}

func TestLandAcceptsTeardownCompletedBeforeClaim(t *testing.T) {
	fixture, task, cfg := newLandedLocalTeardownFixture(t)
	fake := fixture.service.Herdr.(*herdr.Fake)
	snapshot := fake.SnapshotValue
	snapshot.Panes[1].Agent = ""
	adapter := &changingSnapshotAdapter{Fake: fake, snapshot: snapshot}
	fixture.service.Herdr = adapter
	ctx := context.Background()
	first, err := fixture.service.unsaddleTask(ctx, fixture.db, fixture.project, cfg, task, false, "")
	if err != nil || first.AlreadyTornDown {
		t.Fatalf("competing Teardown result=%#v err=%v", first, err)
	}
	_, completedElsewhere, landErr := fixture.service.teardownLandedTask(ctx, fixture.db, fixture.project, cfg, task)
	paneCloses := fake.CallCount("pane.close")
	staleRetry, retryErr := fixture.service.unsaddleTask(ctx, fixture.db, fixture.project, cfg, task, false, "")
	if retryErr != nil || !staleRetry.AlreadyTornDown || fake.CallCount("pane.close") != paneCloses {
		t.Fatalf("stale Teardown claim repeated side effects: result=%#v err=%v pane closes before=%d after=%d", staleRetry, retryErr, paneCloses, fake.CallCount("pane.close"))
	}
	current, readErr := fixture.db.TaskByID(ctx, fixture.project.ID, task.ID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	mounts, err := fixture.db.Mounts(ctx, fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var mount store.Mount
	for _, candidate := range mounts {
		if candidate.ID == task.MountID {
			mount = candidate
			break
		}
	}
	var tornDown int
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transitions WHERE task_id=? AND to_state='torn-down'`, task.ID).Scan(&tornDown); err != nil {
		t.Fatal(err)
	}
	if landErr != nil || !completedElsewhere || current.State != store.StateTornDown || mount.State != "idle" || tornDown != 1 {
		t.Fatalf("completed competing Teardown result=%v err=%v state=%s Mount=%s torn-down transitions=%d", completedElsewhere, landErr, current.State, mount.State, tornDown)
	}
}

func TestLocalMergeRefusesBranchMovedAfterGate(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	worktree := filepath.Join(root, "worktree")
	initRepo(t, repo)
	gitTest(t, repo, "worktree", "add", "-b", "posse/t1", worktree, "main")
	if err := os.WriteFile(filepath.Join(worktree, "change.txt"), []byte("gated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, worktree, "add", "change.txt")
	gitTest(t, worktree, "commit", "-m", "gated change")
	gatedSHA := strings.TrimSpace(gitTest(t, worktree, "rev-parse", "HEAD"))

	home := filepath.Join(root, "posse-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nlanding_mode = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 1, Type: "ship", Title: "Fix", LandingMode: "local", Branch: "posse/t1", BaseRef: "main", WorktreePath: worktree, HerdrWorkspaceID: "w2", PaneID: "w2:p1", PaneLabel: "posse:shop:t1", AutonomyLand: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		from, to store.State
		source   string
	}{{store.StateSpawning, store.StateWorking, "cli"}, {store.StateWorking, store.StateDone, "worker"}} {
		if err := db.Transition(context.Background(), taskID, step.from, step.to, step.source, "test"); err != nil {
			t.Fatal(err)
		}
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"},
		{PaneID: "w2:p1", WorkspaceID: "w2", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "idle"},
	}}
	service := testService(home, fake)
	var output, errorsOut bytes.Buffer
	cli := service.CLI()
	cli.Out = &output
	cli.ErrOut = &errorsOut
	t.Chdir(repo)
	if code := cli.Run([]string{"land", "t1"}); code != 0 {
		t.Fatalf("gate command exit = %d; output=%s error=%s", code, output.String(), errorsOut.String())
	}
	task, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil || task.State != store.StateLanding || task.GatedSHA != gatedSHA {
		t.Fatalf("gated Task = %#v, %v", task, err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "after-gate.txt"), []byte("moved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, worktree, "add", "after-gate.txt")
	gitTest(t, worktree, "commit", "-m", "move after gate")
	output.Reset()
	if code := cli.Run([]string{"land", "t1", "--merge", "--user-approved", "User approved merge"}); code != 1 || !strings.Contains(output.String(), "branch_moved") {
		t.Fatalf("moved branch merge exit=%d output=%s error=%s", code, output.String(), errorsOut.String())
	}
	task, err = db.Task(context.Background(), project.ID, "t1")
	if err != nil || task.State != store.StateDone || task.GatedSHA != "" {
		t.Fatalf("moved branch did not return Task to re-gate state: %#v, %v", task, err)
	}
	if _, err := gitOutput(context.Background(), repo, "cat-file", "-e", "main:after-gate.txt"); err == nil {
		t.Fatal("branch moved after Gate was merged")
	}
	var approvalCount int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM approvals WHERE task_id=? AND action='merge'`, taskID).Scan(&approvalCount); err != nil {
		t.Fatal(err)
	}
	if approvalCount != 0 {
		t.Fatalf("branch moved after Gate recorded %d merge approvals", approvalCount)
	}
}

func TestUnsaddleChecksReAdoptedWorkspaceAgainstMountBeforeClosing(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	worktree := filepath.Join(root, "mount")
	initRepo(t, repo)
	gitTest(t, repo, "worktree", "add", "-b", "posse/t1", worktree, "main")
	home := filepath.Join(root, "posse-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nlanding_mode = \"local\"\nauto_unsaddle = \"never\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{
		Seq: 1, Type: "ship", Title: "Workspace recovery", LandingMode: "local", Branch: "posse/t1", BaseRef: "main",
		WorktreePath: worktree, HerdrWorkspaceID: "w-old", PaneID: "w2:p-old", PaneLabel: "posse:shop:t1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, project.ID, worktree, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, taskID, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	workerPane := herdr.Pane{PaneID: "w3:p1", WorkspaceID: "w3", TabID: "w3:t1", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "working"}
	snapshot := herdr.Snapshot{Panes: []herdr.Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"},
		workerPane,
	}}
	if _, err := runtime.ReconcileSnapshot(ctx, db, project.ID, snapshot, time.Now()); err != nil {
		t.Fatal(err)
	}
	adopted, err := db.Task(ctx, project.ID, "t1")
	if err != nil || adopted.HerdrWorkspaceID != "w3" || adopted.PaneID != "w3:p1" {
		t.Fatalf("Task was not re-adopted from its label: %#v, %v", adopted, err)
	}
	if err := db.Transition(ctx, taskID, store.StateWorking, store.StateDone, "worker", "finished"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateDone, store.StateLanding, "cli", "gate passed"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateLanding, store.StateLanded, "cli", "merged"); err != nil {
		t.Fatal(err)
	}
	fake := &changingSnapshotAdapter{Fake: herdr.NewFake()}
	fake.snapshot = herdr.Snapshot{
		Panes:  snapshot.Panes,
		Agents: []herdr.Agent{{Name: "posse-shop-t1-1", PaneID: "w3:p1"}},
		Workspaces: []herdr.Workspace{{WorkspaceID: "w3", Root: repo, Worktree: struct {
			CheckoutPath     string `json:"checkout_path"`
			RepoKey          string `json:"repo_key"`
			IsLinkedWorktree bool   `json:"is_linked_worktree"`
		}{CheckoutPath: repo}}},
	}
	service := testService(home, fake)
	t.Chdir(repo)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out = &output
	if code := cli.Run([]string{"unsaddle", "t1"}); code != 0 || !strings.Contains(output.String(), "torn-down") {
		t.Fatalf("unsaddle did not close the re-adopted pane by its Task label: code=%d output=%s", code, output.String())
	}
	// A legacy Rider workspace disappears with its last tab; posse never closes a workspace.
	if fake.CallCount("tab.close") != 1 || fake.CallCount("workspace.close") != 0 {
		t.Fatalf("Task-only tab in the moved workspace was not closed by tab: %#v", fake.Calls)
	}
	updated, err := db.Task(ctx, project.ID, "t1")
	if err != nil || updated.State != store.StateTornDown || updated.HerdrWorkspaceID != "w3" {
		t.Fatalf("moved workspace teardown state: %#v, %v", updated, err)
	}
	mounts, err := db.Mounts(ctx, project.ID)
	if err != nil || len(mounts) != 1 || mounts[0].State != "idle" {
		t.Fatalf("moved workspace teardown did not release its Mount: %#v, %v", mounts, err)
	}
}

type changingSnapshotAdapter struct {
	*herdr.Fake
	mu         sync.Mutex
	snapshot   herdr.Snapshot
	afterClose func()
}

func (adapter *changingSnapshotAdapter) Snapshot(ctx context.Context) (herdr.Snapshot, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.snapshot, nil
}

func (adapter *changingSnapshotAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	raw, err := adapter.Fake.Call(ctx, method, params)
	if err != nil {
		return raw, err
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	switch method {
	case "pane.close":
		if adapter.afterClose != nil {
			adapter.afterClose()
		}
		paneID, _ := params["pane_id"].(string)
		kept := adapter.snapshot.Panes[:0]
		for _, pane := range adapter.snapshot.Panes {
			if pane.PaneID != paneID {
				kept = append(kept, pane)
			}
		}
		adapter.snapshot.Panes = kept
	case "tab.close":
		tabID, _ := params["tab_id"].(string)
		kept := adapter.snapshot.Panes[:0]
		for _, pane := range adapter.snapshot.Panes {
			if pane.TabID != tabID {
				kept = append(kept, pane)
			}
		}
		adapter.snapshot.Panes = kept
	case "workspace.close":
		if adapter.afterClose != nil {
			adapter.afterClose()
		}
		workspaceID, _ := params["workspace_id"].(string)
		kept := adapter.snapshot.Panes[:0]
		for _, pane := range adapter.snapshot.Panes {
			if pane.WorkspaceID != workspaceID {
				kept = append(kept, pane)
			}
		}
		adapter.snapshot.Panes = kept
	}
	return raw, nil
}

func (adapter *changingSnapshotAdapter) currentSnapshot() herdr.Snapshot {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.snapshot
}
