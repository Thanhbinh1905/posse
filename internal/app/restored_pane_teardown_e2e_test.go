package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func newCompletedTaskPaneTeardownFixture(t *testing.T) (*prMergeFixture, store.Task) {
	t.Helper()
	fixture := newPRMergeFixture(t, "local", store.StateDone)
	attachPRFixtureSandbox(t, fixture)
	gitTest(t, fixture.repo, "merge", "--ff-only", "refs/heads/posse/t1")
	task, err := fixture.db.TaskByID(context.Background(), fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, task
}

func TestTeardownClosesRestoredCompletedPaneE2E(t *testing.T) {
	fixture, task := newCompletedTaskPaneTeardownFixture(t)
	adapter := restoredCompletedPaneAdapter(t, fixture, task)
	fixture.test.Chdir(fixture.repo)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")

	code, output, stderr := fixture.run("teardown", "t1")
	if code != 0 || !strings.Contains(output, "pane_close_proofs") || !strings.Contains(output, task.PaneID+":completed_worker_idle_shell") {
		t.Fatalf("restored pane teardown = %d, output=%s stderr=%s", code, output, stderr)
	}
	if containsPane(adapter.SnapshotValue, task.PaneID) {
		t.Fatalf("teardown left restored Worker pane open: %#v", adapter.SnapshotValue.Panes)
	}
	if _, err := fixture.db.SandboxByTask(context.Background(), task.ID); !store.IsNotFound(err) {
		t.Fatalf("Sandbox remains allocated after restored pane teardown: %v", err)
	}
}

func TestTeardownRefusesRestoredIdleShellWithoutTaskOwnershipTokenE2E(t *testing.T) {
	fixture, task := newCompletedTaskPaneTeardownFixture(t)
	adapter := restoredCompletedPaneAdapter(t, fixture, task)
	snapshot := adapter.SnapshotValue
	snapshot.Panes[1].Tokens = nil
	adapter.SnapshotValue = snapshot
	fixture.test.Chdir(fixture.repo)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")

	code, output, stderr := fixture.run("teardown", "t1")
	if code == 0 {
		t.Fatalf("teardown accepted an idle shell without the Task ownership token: output=%s stderr=%s", output, stderr)
	}
	if !containsPane(adapter.SnapshotValue, task.PaneID) {
		t.Fatal("teardown closed the unverified idle shell")
	}
	sandbox, err := fixture.db.SandboxByTask(context.Background(), task.ID)
	if err != nil || sandbox.State != "held" {
		t.Fatalf("Sandbox after unverified idle shell refusal = %#v, %v", sandbox, err)
	}
}

func TestApprovedDiscardClosesRestoredCompletedPaneE2E(t *testing.T) {
	fixture, task := newCompletedTaskPaneTeardownFixture(t)
	adapter := restoredCompletedPaneAdapter(t, fixture, task)
	fixture.test.Chdir(fixture.repo)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")

	code, output, stderr := fixture.run("teardown", "t1", "--discard", "--user-approved", "User approved this discard")
	if code != 0 || !strings.Contains(output, "pane_close_proofs") || !strings.Contains(output, task.PaneID+":"+completedPaneCloseProof) {
		t.Fatalf("restored pane discard = %d, output=%s stderr=%s", code, output, stderr)
	}
	if containsPane(adapter.SnapshotValue, task.PaneID) {
		t.Fatalf("discard left restored Worker pane open: %#v", adapter.SnapshotValue.Panes)
	}
	if _, err := fixture.db.SandboxByTask(context.Background(), task.ID); !store.IsNotFound(err) {
		t.Fatalf("Sandbox remains allocated after restored pane discard: %v", err)
	}
}

func TestTeardownRefusesRestoredPaneWithCanonicalReplacementWorkerE2E(t *testing.T) {
	fixture, task := newCompletedTaskPaneTeardownFixture(t)
	adapter := restoredCompletedPaneAdapter(t, fixture, task)
	snapshot := adapter.SnapshotValue
	snapshot.Panes[1].AgentStatus = "working"
	snapshot.Agents = []herdr.Agent{{Name: agentName(fixture.project.Name, 2, 1), PaneID: task.PaneID, Agent: "claude", AgentStatus: "working"}}
	adapter.SnapshotValue = snapshot
	fixture.test.Chdir(fixture.repo)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")

	code, output, stderr := fixture.run("teardown", "t1")
	if code == 0 || !strings.Contains(output+stderr, "canonical Worker identity") {
		t.Fatalf("canonical replacement Worker teardown = %d, output=%s stderr=%s", code, output, stderr)
	}
	if !containsPane(adapter.SnapshotValue, task.PaneID) {
		t.Fatal("teardown closed the replacement Worker pane")
	}
	sandbox, err := fixture.db.SandboxByTask(context.Background(), task.ID)
	if err != nil || sandbox.State != "held" {
		t.Fatalf("Sandbox after replacement Worker refusal = %#v, %v", sandbox, err)
	}
}

func restoredCompletedPaneAdapter(t *testing.T, fixture *prMergeFixture, task store.Task) *restoredPaneTeardownAdapter {
	t.Helper()
	const marker = "0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET launches=1, agent_name=?, containment_marker=? WHERE id=?`, agentName(fixture.project.Name, task.Seq, 1), marker, task.ID); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.TaskByID(context.Background(), fixture.project.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	fake, ok := fixture.service.Herdr.(*herdr.Fake)
	if !ok {
		t.Fatalf("fixture Herdr adapter = %T, want *herdr.Fake", fixture.service.Herdr)
	}
	snapshot := fake.SnapshotValue
	snapshot.ServerStartedAt = "restored-generation"
	for index := range snapshot.Panes {
		if snapshot.Panes[index].PaneID == task.PaneID {
			snapshot.Panes[index].Agent = "claude"
			snapshot.Panes[index].AgentStatus = "idle"
			snapshot.Panes[index].Tokens = map[string]string{taskPaneOwnershipTokenKey: marker}
		}
	}
	fake.SnapshotValue = snapshot
	adapter := &restoredPaneTeardownAdapter{Fake: fake, paneID: task.PaneID}
	fixture.service.Herdr = adapter
	if err := fixture.db.RememberProjectServerStartedAt(context.Background(), fixture.project.ID, snapshot.ServerStartedAt); err != nil {
		t.Fatal(err)
	}
	return adapter
}

type restoredPaneTeardownAdapter struct {
	*herdr.Fake
	paneID string
}

func (adapter *restoredPaneTeardownAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	result, err := adapter.Fake.Call(ctx, method, params)
	if err != nil || method != "pane.close" || params["pane_id"] != adapter.paneID {
		return result, err
	}
	panes := adapter.SnapshotValue.Panes[:0]
	for _, pane := range adapter.SnapshotValue.Panes {
		if pane.PaneID != adapter.paneID {
			panes = append(panes, pane)
		}
	}
	adapter.SnapshotValue.Panes = panes
	return result, nil
}
