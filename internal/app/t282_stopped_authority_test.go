//go:build e2e

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

// t282StaleLaunchAdapter reproduces a User launching a replacement from the
// shell while stopPaneAgent is between its process observations. It forwards
// every real Herdr response; only the first process-info response is delayed.
type t282StaleLaunchAdapter struct {
	*herdr.Client
	t           *testing.T
	kind        string
	readyFile   string
	injected    bool
	liveAtClear bool
}

func (a *t282StaleLaunchAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method == "pane.process_info" && !a.injected {
		a.injected = true
		raw, err := a.Client.Call(ctx, method, params)
		if err != nil {
			return nil, err
		}
		paneID, _ := params["pane_id"].(string)
		if _, err := a.Client.Run(ctx, "pane", "run", paneID, a.kind); err != nil {
			return nil, err
		}
		if !waitForT282File(a.readyFile, 10*time.Second) {
			return nil, fmt.Errorf("replacement %s never reported readiness", a.kind)
		}
		a.t.Logf("replacement %s reported readiness; returning the captured shell response %s", a.kind, raw)
		return raw, nil
	}
	if method == "pane.clear_agent_authority" {
		raw, err := a.Client.Call(ctx, "pane.process_info", params)
		if err != nil {
			return nil, err
		}
		var info struct {
			ProcessInfo struct {
				Group int `json:"foreground_process_group_id"`
				Shell int `json:"shell_pid"`
			} `json:"process_info"`
		}
		if err := json.Unmarshal(raw, &info); err != nil {
			return nil, err
		}
		a.liveAtClear = info.ProcessInfo.Group != info.ProcessInfo.Shell
		a.t.Logf("actual process at clear boundary=%s", raw)
	}
	return a.Client.Call(ctx, method, params)
}

func TestStoppedRiderConcurrentReplacementPreservesLifecycleAuthority(t *testing.T) {
	for _, kind := range []string{"codex", "opencode"} {
		t.Run(kind, func(t *testing.T) {
			service, client, paneID, root := newT282LifecycleService(t, kind)
			ctx := context.Background()
			if _, err := service.startAgent(ctx, map[string]any{
				"name": "t282-stopped-rider", "kind": kind, "pane_id": paneID, "args": []string{},
			}); err != nil {
				t.Fatal(err)
			}
			if err := service.waitAgentReady(ctx, paneID); err != nil {
				t.Fatal(err)
			}
			group, err := service.paneAgentGroup(ctx, paneID)
			if err != nil || group <= 1 {
				t.Fatalf("original Rider missing: group=%d err=%v", group, err)
			}
			if err := syscall.Kill(-group, syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			if !waitForT282Condition(5*time.Second, func() bool {
				current, err := service.paneAgentGroup(ctx, paneID)
				return err == nil && current == 0
			}) {
				t.Fatalf("original Rider did not return the pane to its shell")
			}
			raw, err := client.Call(ctx, "agent.get", map[string]any{"target": paneID})
			if err != nil || !strings.Contains(string(raw), `"agent":"`+kind+`"`) {
				t.Fatalf("stopped Rider did not retain stale authority: %s: %v", raw, err)
			}

			agentPath := filepath.Join(root, "bin", kind)
			replacement := "#!/bin/sh\n" +
				"herdr pane report-agent \"$HERDR_PANE_ID\" --source posse.fake --agent " + kind + " --state idle >/dev/null 2>&1\n" +
				": > \"$HOME/external-ready\"\n" +
				"while IFS= read -r line; do herdr pane report-agent \"$HERDR_PANE_ID\" --source posse.fake --agent " + kind + " --state working >/dev/null 2>&1; done\n"
			if err := os.WriteFile(agentPath, []byte(replacement), 0o700); err != nil {
				t.Fatal(err)
			}
			adapter := &t282StaleLaunchAdapter{
				Client: client, t: t, kind: kind, readyFile: filepath.Join(root, "home", "external-ready"),
			}
			service.Herdr = adapter
			err = service.stopPaneAgent(ctx, paneID)
			if adapter.liveAtClear {
				t.Fatalf("live replacement authority was cleared: err=%v", err)
			}
			if err == nil {
				t.Fatal("stopping a pane that changed to a replacement agent did not return a retryable conflict")
			}
			if !adapter.injected {
				t.Fatal("replacement was not launched at the stopped-pane observation boundary")
			}
			replacementGroup, err := service.paneAgentGroup(ctx, paneID)
			if err != nil || replacementGroup <= 1 {
				t.Fatalf("replacement process did not survive: group=%d err=%v", replacementGroup, err)
			}
			final, err := client.Call(ctx, "agent.get", map[string]any{"target": paneID})
			if err != nil || strings.Contains(string(final), `"agent_status":"unknown"`) {
				t.Fatalf("replacement lifecycle authority was lost: %s: %v", final, err)
			}
			t.Cleanup(func() { _ = syscall.Kill(-replacementGroup, syscall.SIGTERM) })
		})
	}
}

func newT282LifecycleService(t *testing.T, kind string) (*Service, *herdr.Client, string, string) {
	t.Helper()
	base := os.Getenv("POSSE_E2E_TMP_ROOT")
	if base == "" {
		base = os.Getenv("TMPDIR")
	}
	if base == "" {
		base = os.TempDir()
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(base, "posse-e2e-app-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{bin, filepath.Join(root, "home"), filepath.Join(root, "posse")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\n" +
		"herdr pane report-agent \"$HERDR_PANE_ID\" --source posse.fake --agent " + kind + " --state idle >/dev/null 2>&1\n" +
		"while IFS= read -r line; do herdr pane report-agent \"$HERDR_PANE_ID\" --source posse.fake --agent " + kind + " --state working >/dev/null 2>&1; done\n"
	if err := os.WriteFile(filepath.Join(bin, kind), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	env := herdr.IsolatedTestEnvironment(root)
	for i, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			env[i] = "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")
		}
	}
	client := herdr.NewWithEnv("herdr", env)
	server, err := client.StartIsolatedServer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.StopIsolatedServer(ctx)
		done := make(chan struct{})
		go func() { _ = server.Wait(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			_ = server.Kill()
			<-done
		}
	})
	if !waitForT282Condition(15*time.Second, func() bool {
		status, err := client.Status(context.Background())
		return err == nil && status.Running
	}) {
		t.Fatal("isolated Herdr server did not start")
	}
	raw, err := client.Call(context.Background(), "workspace.create", map[string]any{"cwd": root, "label": "t282", "focus": false})
	if err != nil {
		t.Fatal(err)
	}
	var workspace struct {
		RootPane struct {
			ID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	if err := json.Unmarshal(raw, &workspace); err != nil || workspace.RootPane.ID == "" {
		t.Fatalf("workspace: %s: %v", raw, err)
	}
	return testService(filepath.Join(root, "posse"), client), client, workspace.RootPane.ID, root
}

func waitForT282File(path string, timeout time.Duration) bool {
	return waitForT282Condition(timeout, func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
}

func waitForT282Condition(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return condition()
}
