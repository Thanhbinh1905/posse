package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func newCompletedTaskPaneTeardownFixture(t *testing.T) (*prMergeFixture, store.Task) {
	t.Helper()
	fixture := newPRMergeFixture(t, "local", store.StateDone)
	attachPRFixtureSandbox(t, fixture)
	gitTest(t, fixture.repo, "merge", "--ff-only", "refs/heads/posse/t1")
	ctx := context.Background()
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, task
}

func TestTeardownClosesRestoredCompletedPaneE2E(t *testing.T) {
	fixture, task := newCompletedTaskPaneTeardownFixture(t)
	client, server, leadWorkspace, leadPane, workerWorkspace, workerPane := startRestoredPaneHerdr(t, fixture)
	defer stopRestoredPaneHerdr(t, client, server)

	if err := fixture.db.SetProjectLead(context.Background(), fixture.project.ID, leadWorkspace, leadPane, fixture.project.LeadLabel); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.UpdateTaskLaunch(context.Background(), task.ID, task.WorktreePath, workerWorkspace, workerPane, task.PaneLabel, agentName(fixture.project.Name, task.Seq, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET launches=1 WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "pane", "report-agent", workerPane, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	before, err := client.Snapshot(context.Background())
	if err != nil || !containsPane(before, workerPane) {
		t.Fatalf("restored Worker pane missing before teardown: %s %v", workerPane, err)
	}
	if err := fixture.db.RememberProjectServerStartedAt(context.Background(), fixture.project.ID, before.ServerStartedAt); err != nil {
		t.Fatal(err)
	}
	fixture.service.Herdr = client
	fixture.test.Chdir(fixture.repo)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", leadPane)

	code, output, stderr := fixture.run("teardown", "t1")
	if code != 0 || !strings.Contains(output, "pane_close_proofs") || !strings.Contains(output, workerPane+":completed_worker_idle_shell") {
		t.Fatalf("restored pane teardown = %d, output=%s stderr=%s", code, output, stderr)
	}
	after, err := client.Snapshot(context.Background())
	if err != nil || containsPane(after, workerPane) {
		t.Fatalf("teardown left restored Worker pane open: %#v %v", after.Panes, err)
	}
	if _, err := fixture.db.SandboxByTask(context.Background(), task.ID); !store.IsNotFound(err) {
		t.Fatalf("Sandbox remains allocated after restored pane teardown: %v", err)
	}
}

func TestApprovedDiscardClosesRestoredCompletedPaneE2E(t *testing.T) {
	fixture := newPRMergeFixture(t, "local", store.StateDone)
	attachPRFixtureSandbox(t, fixture)
	task := fixture.task
	client, server, leadWorkspace, leadPane, workerWorkspace, workerPane := startRestoredPaneHerdr(t, fixture)
	defer stopRestoredPaneHerdr(t, client, server)
	if err := fixture.db.SetProjectLead(context.Background(), fixture.project.ID, leadWorkspace, leadPane, fixture.project.LeadLabel); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.UpdateTaskLaunch(context.Background(), task.ID, task.WorktreePath, workerWorkspace, workerPane, task.PaneLabel, agentName(fixture.project.Name, task.Seq, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET launches=1 WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "pane", "report-agent", workerPane, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Snapshot(context.Background())
	if err != nil || !containsPane(snapshot, workerPane) {
		t.Fatalf("restored Worker pane missing before discard: %s %v", workerPane, err)
	}
	if err := fixture.db.RememberProjectServerStartedAt(context.Background(), fixture.project.ID, snapshot.ServerStartedAt); err != nil {
		t.Fatal(err)
	}
	fixture.service.Herdr = client
	fixture.test.Chdir(fixture.repo)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", leadPane)

	code, output, stderr := fixture.run("teardown", "t1", "--discard", "--user-approved", "User approved this discard")
	if code != 0 || !strings.Contains(output, "pane_close_proofs") || !strings.Contains(output, workerPane+":"+completedPaneCloseProof) {
		t.Fatalf("restored pane discard = %d, output=%s stderr=%s", code, output, stderr)
	}
	after, err := client.Snapshot(context.Background())
	if err != nil || containsPane(after, workerPane) {
		t.Fatalf("discard left restored Worker pane open: %#v %v", after.Panes, err)
	}
	if _, err := fixture.db.SandboxByTask(context.Background(), task.ID); !store.IsNotFound(err) {
		t.Fatalf("Sandbox remains allocated after restored pane discard: %v", err)
	}
}

func TestTeardownRefusesRestoredPaneWithCanonicalReplacementWorkerE2E(t *testing.T) {
	fixture, task := newCompletedTaskPaneTeardownFixture(t)
	client, server, leadWorkspace, leadPane, workerWorkspace, workerPane := startRestoredPaneHerdr(t, fixture)
	defer stopRestoredPaneHerdr(t, client, server)

	if err := fixture.db.SetProjectLead(context.Background(), fixture.project.ID, leadWorkspace, leadPane, fixture.project.LeadLabel); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.UpdateTaskLaunch(context.Background(), task.ID, task.WorktreePath, workerWorkspace, workerPane, task.PaneLabel, agentName(fixture.project.Name, task.Seq, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET launches=1 WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "pane", "run", workerPane, "sleep", "30"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "pane", "report-agent", workerPane, "--source", "posse.fake", "--agent", agentName(fixture.project.Name, 2, 1), "--state", "working"); err != nil {
		t.Fatal(err)
	}
	before, err := client.Snapshot(context.Background())
	if err != nil || !containsPane(before, workerPane) {
		t.Fatalf("replacement Worker pane missing before teardown: %s %v", workerPane, err)
	}
	if err := fixture.db.RememberProjectServerStartedAt(context.Background(), fixture.project.ID, before.ServerStartedAt); err != nil {
		t.Fatal(err)
	}
	fixture.service.Herdr = client
	fixture.test.Chdir(fixture.repo)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", leadPane)

	code, output, stderr := fixture.run("teardown", "t1")
	if code == 0 || !strings.Contains(output+stderr, "canonical Worker identity") {
		t.Fatalf("canonical replacement Worker teardown = %d, output=%s stderr=%s", code, output, stderr)
	}
	after, err := client.Snapshot(context.Background())
	if err != nil || !containsPane(after, workerPane) {
		t.Fatalf("teardown closed the replacement Worker pane: %#v %v", after.Panes, err)
	}
	sandbox, err := fixture.db.SandboxByTask(context.Background(), task.ID)
	if err != nil || sandbox.State != "held" {
		t.Fatalf("Sandbox after replacement Worker refusal = %#v, %v", sandbox, err)
	}
}

func startRestoredPaneHerdr(t *testing.T, fixture *prMergeFixture) (*herdr.Client, *herdr.ServerProcess, string, string, string, string) {
	t.Helper()
	root, err := os.MkdirTemp(os.Getenv("TMPDIR"), "posse-e2e-t1264-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	for _, dir := range []string{filepath.Join(root, "home"), filepath.Join(root, "posse")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	client := herdr.NewWithEnv("herdr", herdr.IsolatedTestEnvironment(root))
	server, err := client.StartIsolatedServer()
	if err != nil {
		t.Fatal(err)
	}
	wait := func() {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			status, err := client.Status(context.Background())
			if err == nil && status.Running {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("isolated Herdr server did not start")
	}
	wait()
	createWorkspace := func(label, cwd string) (string, string) {
		t.Helper()
		raw, err := client.Call(context.Background(), "workspace.create", map[string]any{"cwd": cwd, "label": label, "focus": false})
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Workspace struct {
				ID string `json:"workspace_id"`
			} `json:"workspace"`
			RootPane struct {
				ID string `json:"pane_id"`
			} `json:"root_pane"`
		}
		if err := json.Unmarshal(raw, &result); err != nil || result.Workspace.ID == "" || result.RootPane.ID == "" {
			t.Fatalf("workspace.create = %s: %v", raw, err)
		}
		return result.Workspace.ID, result.RootPane.ID
	}
	leadWorkspace, leadPane := createWorkspace("lead", fixture.repo)
	workerWorkspace, workerPane := createWorkspace("worker", fixture.worktree)
	if _, err := client.Run(context.Background(), "pane", "rename", workerPane, fixture.task.PaneLabel); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "pane", "report-agent", workerPane, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	beforeRestart, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stopRestoredPaneHerdr(t, client, server)
	server, err = client.StartIsolatedServer()
	if err != nil {
		t.Fatal(err)
	}
	wait()
	afterRestart, err := client.Snapshot(context.Background())
	if err != nil || afterRestart.ServerStartedAt == beforeRestart.ServerStartedAt || !containsPane(afterRestart, workerPane) {
		t.Fatalf("isolated Herdr restart did not restore Worker pane %s: before=%s after=%s err=%v", workerPane, beforeRestart.ServerStartedAt, afterRestart.ServerStartedAt, err)
	}
	return client, server, leadWorkspace, leadPane, workerWorkspace, workerPane
}

func stopRestoredPaneHerdr(t *testing.T, client *herdr.Client, server *herdr.ServerProcess) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = client.StopIsolatedServer(ctx)
	done := make(chan struct{})
	go func() {
		_ = server.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		_ = server.Kill()
		<-done
	}
}
