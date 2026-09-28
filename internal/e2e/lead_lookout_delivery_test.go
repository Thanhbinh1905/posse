//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLeadLookoutOwnsNoticeUntilItExits(t *testing.T) {
	f := newPRLifecycleFixtureWithLead(t, true)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	runPosse(t, f.binary, f.repo, f.leadEnv, "lowkey", "on")
	if _, err := client.Run(context.Background(), "pane", "report-agent", f.project.LeadPaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	snapshot := herdr.Snapshot{}
	var snapshotErr error
	otherPane := ""
	if !waitForCondition(5*time.Second, func() bool {
		snapshot, snapshotErr = client.Snapshot(context.Background())
		if snapshotErr != nil {
			return false
		}
		for _, pane := range snapshot.Panes {
			if pane.WorkspaceID == f.project.HerdrWorkspaceID && pane.PaneID != f.project.LeadPaneID {
				otherPane = pane.PaneID
				break
			}
		}
		return otherPane != ""
	}) {
		t.Fatalf("no other pane to focus: snapshot=%#v err=%v", snapshot.Panes, snapshotErr)
	}
	if _, err := client.Call(context.Background(), "pane.focus", map[string]any{"pane_id": otherPane}); err != nil {
		t.Fatal(err)
	}
	snapshot, snapshotErr = client.Snapshot(context.Background())
	if snapshotErr != nil || snapshot.FocusedPaneID == f.project.LeadPaneID {
		t.Fatalf("Lead is focused: %#v %v", snapshot, snapshotErr)
	}
	logPath := filepath.Join(f.root, "lead-prompts.log")
	// The poll-only tab must not suppress fallback when the Lead has never
	// started a lookout.
	if _, err := f.db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, Kind: "task_done", Summary: "fallback before lookout starts", DataJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.leadEnv)
	if !waitForCondition(5*time.Second, func() bool {
		contents, _ := os.ReadFile(logPath)
		return strings.Contains(string(contents), "fallback before lookout starts")
	}) {
		contents, _ := os.ReadFile(logPath)
		t.Fatalf("typed fallback missing without Lead watcher: %s", contents)
	}
	// Start the actual Lead command in the isolated Project, not the poll-only tab.
	command := exec.Command(f.binary, "lookout", "--timeout", "10000")
	command.Dir, command.Env = f.repo, f.leadEnv
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	finished := false
	t.Cleanup(func() {
		if !finished {
			_ = syscall.Kill(command.Process.Pid, syscall.SIGCONT)
			_ = command.Process.Kill()
			<-done
		}
	})
	// A live process must have entered its wait loop before the Notice is created.
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		finished = true
		t.Fatalf("lookout exited before Notice: %v %s", err, output.String())
	default:
	}
	// Insert but do not commit until the watcher is paused. This leaves the
	// real lookout alive and waiting, without freezing it while it holds a DB
	// transaction. The idle-pane delivery path then runs first, deterministically.
	tx, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(context.Background(), `INSERT INTO notices(project_id, kind, summary, data_json, created_at) VALUES (?, ?, ?, ?, ?)`, f.project.ID, "task_done", "live lookout owns this Notice", `{}`, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(command.Process.Pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	paused := true
	defer func() {
		if paused {
			_ = syscall.Kill(command.Process.Pid, syscall.SIGCONT)
		}
	}()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// The same ordinary Project command that triggers the typed idle fallback.
	wake := exec.Command(f.binary)
	wake.Dir, wake.Env = f.repo, f.leadEnv
	if result, err := wake.CombinedOutput(); err != nil {
		latest, _ := client.Snapshot(context.Background())
		t.Fatalf("wake failed: %v %s snapshot=%#v", err, result, latest.Panes)
	}
	// If the fallback stole the Notice, it is no longer pending before
	// the watcher resumes. That is the user-visible failure in issue #64.
	var pending []store.Notice
	if !waitForCondition(5*time.Second, func() bool {
		pending, err = f.db.UndeliveredNotices(context.Background(), f.project.ID)
		return err == nil
	}) {
		t.Fatalf("read pending Notices: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != id {
		contents, _ := os.ReadFile(logPath)
		t.Fatalf("typed fallback stole live lookout Notice %d: pending=%#v prompt=%s", id, pending, contents)
	}
	if err := syscall.Kill(command.Process.Pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	paused = false
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatalf("lookout failed: %v %s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("lookout missed Notice %d: %s", id, output.String())
	}
	if !strings.Contains(output.String(), "live lookout owns this Notice") {
		t.Fatalf("lookout returned wrong batch: %s", output.String())
	}
	if contents, err := os.ReadFile(logPath); err == nil && strings.Contains(string(contents), "live lookout owns this Notice") {
		t.Fatalf("typed prompt stole the batch: %s", contents)
	}
	// The process is gone, so a fresh Notice must take the typed path immediately.
	if _, err := f.db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, Kind: "task_done", Summary: "fallback after lookout exit", DataJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	fallback := exec.Command(f.binary)
	fallback.Dir, fallback.Env = f.repo, f.leadEnv
	if result, err := fallback.CombinedOutput(); err != nil {
		latest, _ := client.Snapshot(context.Background())
		agent, agentErr := client.Call(context.Background(), "agent.get", map[string]any{"target": f.project.LeadPaneID})
		t.Fatalf("fallback failed: %v %s snapshot=%#v agent=%s agentErr=%v", err, result, latest.Panes, agent, agentErr)
	}
	if !waitForCondition(5*time.Second, func() bool {
		contents, _ := os.ReadFile(logPath)
		return strings.Contains(string(contents), "fallback after lookout exit")
	}) {
		contents, _ := os.ReadFile(logPath)
		t.Fatalf("typed fallback did not deliver after lookout exit: %s", contents)
	}

	// A watcher killed while waiting must not leave a stale ownership record.
	crashed := exec.Command(f.binary, "lookout", "--timeout", "10000")
	crashed.Dir, crashed.Env = f.repo, f.leadEnv
	if err := crashed.Start(); err != nil {
		t.Fatal(err)
	}
	crashedDone := make(chan error, 1)
	go func() { crashedDone <- crashed.Wait() }()
	crashedReaped := false
	t.Cleanup(func() {
		if !crashedReaped {
			_ = crashed.Process.Kill()
			<-crashedDone
		}
	})
	time.Sleep(300 * time.Millisecond)
	if err := crashed.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-crashedDone
	crashedReaped = true
	if _, err := f.db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, Kind: "task_done", Summary: "fallback after lookout crash", DataJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.leadEnv)
	if !waitForCondition(5*time.Second, func() bool {
		contents, _ := os.ReadFile(logPath)
		return strings.Contains(string(contents), "fallback after lookout crash")
	}) {
		contents, _ := os.ReadFile(logPath)
		t.Fatalf("typed fallback did not deliver after lookout crash: %s", contents)
	}
}
