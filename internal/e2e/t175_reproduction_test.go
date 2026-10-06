//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/app"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type t175Adapter struct {
	*herdr.Client
	lead, target   string
	failures       int
	beforeInfo     func()
	beforeSnapshot func()
}

func (a *t175Adapter) Snapshot(ctx context.Context) (herdr.Snapshot, error) {
	if a.beforeSnapshot != nil {
		a.beforeSnapshot()
	}
	return a.Client.Snapshot(ctx)
}

func (a *t175Adapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method == "agent.prompt" && params["target"] == a.lead {
		a.failures++
		return nil, &herdr.Error{Code: "agent_not_ready", Message: "agent is no longer the pane foreground process"}
	}
	if method == "pane.process_info" && params["pane_id"] == a.target && a.beforeInfo != nil {
		callback := a.beforeInfo
		a.beforeInfo = nil
		callback()
	}
	return a.Client.Call(ctx, method, params)
}

func TestT175OwnedToForeignSameKindRestartDuringProcessInspectionIsPreserved(t *testing.T) {
	f := newRiderTabsFixture(t)
	ctx := context.Background()
	task := f.ride(t, "t1", "Foreign restart", "foreign-restart")
	f.fail(t, "t1")
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.CreateNotice(ctx, store.Notice{ProjectID: f.projectID, TaskID: task.ID, Kind: "worker_failed", Summary: "Rider failed", DataJSON: `{}`})
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(f.home, "config.toml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config = append(config, []byte("\n[kinds.claude]\nnotice_delivery = \"prompt\"\n")...)
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}

	stop := func() {
		t.Helper()
		if _, err := f.client.Run(ctx, "agent", "send-keys", task.PaneID, "ctrl+c"); err != nil {
			t.Fatal(err)
		}
		if !waitForCondition(10*time.Second, func() bool {
			for _, pane := range f.snapshot(t).Panes {
				if pane.PaneID == task.PaneID {
					return pane.Agent == ""
				}
			}
			return false
		}) {
			t.Fatal("agent did not return to its shell")
		}
	}
	start := func(label, name string) int {
		t.Helper()
		if _, err := f.client.Call(ctx, "pane.rename", map[string]any{"pane_id": task.PaneID, "label": label}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.client.Run(ctx, "agent", "start", name, "--kind", "claude", "--pane", task.PaneID); err != nil {
			t.Fatal(err)
		}
		for _, agent := range f.snapshot(t).Agents {
			if agent.PaneID == task.PaneID && agent.Name == name {
				return foregroundPID(t, f, task.PaneID)
			}
		}
		t.Fatalf("agent %q did not appear in Herdr's snapshot", name)
		return 0
	}

	// Start a foreign Claude agent before Teardown. At the actual Mount-release
	// snapshot boundary, replace it with a Task-named Claude agent, then replace
	// that process with another foreign Claude agent before process_info reads.
	stop()
	start("foreign-agent", "foreign-agent")
	work := filepath.Join(task.WorktreePath, "foreign-work.txt")
	if err := os.WriteFile(work, []byte("foreign work must survive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := &t175Adapter{Client: f.client, lead: f.leadPaneID, target: task.PaneID}
	ownedPID, foreignPID := 0, 0
	adapter.beforeSnapshot = func() {
		ro, err := store.OpenReadOnly(f.home)
		if err != nil {
			t.Fatal(err)
		}
		intent, err := ro.IntentByTask(ctx, task.ID)
		_ = ro.Close()
		if err != nil || intent.Step != "in_progress:mount.release" || ownedPID != 0 {
			return
		}
		stop()
		ownedPID = start(task.PaneLabel, task.AgentName)
		adapter.beforeInfo = func() {
			stop()
			foreignPID = start("foreign-restarted", "foreign-restarted")
		}
	}
	if _, err := f.client.Run(ctx, "pane", "report-agent", f.leadPaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Call(ctx, "pane.focus", map[string]any{"pane_id": f.userBefore.RootPane.PaneID}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range f.leadEnv {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			t.Setenv(key, value)
		}
	}
	t.Chdir(f.repo)
	cli := app.New(f.home, adapter).CLI()
	var output bytes.Buffer
	cli.Out, cli.ErrOut = &output, &output
	code := cli.Run([]string{"unsaddle", "t1", "--discard", "--user-approved", "User approved this discard"})
	if adapter.failures != 1 || ownedPID == 0 || foreignPID == 0 || ownedPID == foreignPID {
		t.Fatalf("the real replacement sequence did not cross the guard boundary: failures=%d owned=%d foreign=%d", adapter.failures, ownedPID, foreignPID)
	}

	contents, fileErr := os.ReadFile(work)
	after := f.snapshot(t)
	foreignAlive := false
	for _, agent := range after.Agents {
		if agent.PaneID == task.PaneID && agent.Name == "foreign-restarted" {
			foreignAlive = true
		}
	}
	t.Logf("owned snapshot PID=%d, foreign process_info PID=%d, exit=%d output=%s foreign alive=%v file error=%v", ownedPID, foreignPID, code, output.String(), foreignAlive, fileErr)
	if code == 0 || !strings.Contains(output.String(), "unsaddle_incomplete") || !foreignAlive || fileErr != nil || string(contents) != "foreign work must survive\n" {
		t.Fatalf("foreign replacement was not preserved: exit=%d alive=%v file=%q error=%v output=%s", code, foreignAlive, contents, fileErr, output.String())
	}
	if taskAfter := f.task(t, "t1"); taskAfter.State != store.StateFailed {
		t.Fatalf("Task after refused teardown = %#v", taskAfter)
	}
	branchCheck := exec.CommandContext(ctx, "git", "-C", f.repo, "show-ref", "--verify", "refs/heads/"+task.Branch)
	if output, err := branchCheck.CombinedOutput(); err != nil {
		t.Fatalf("Task branch after refused teardown: %v: %s", err, output)
	}
	ro, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	mounts, err := ro.Mounts(ctx, f.projectID)
	if err != nil || len(mounts) != 1 || mounts[0].State != "held" {
		t.Fatalf("Mount after refused teardown = %#v, %v", mounts, err)
	}
}

func foregroundPID(t *testing.T, f *riderTabsFixture, paneID string) int {
	t.Helper()
	raw, err := f.client.Call(context.Background(), "pane.process_info", map[string]any{"pane_id": paneID})
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		ProcessInfo struct {
			Foreground int `json:"foreground_process_group_id"`
		} `json:"process_info"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || response.ProcessInfo.Foreground <= 1 {
		t.Fatalf("invalid process info: %s %v", raw, err)
	}
	return response.ProcessInfo.Foreground
}
