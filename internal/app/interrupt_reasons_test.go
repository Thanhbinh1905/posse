package app

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestInterruptReportsRefusalReasons(t *testing.T) {
	for _, tc := range []struct {
		name, kind, status, wantCode, wantOutput string
		focused                                  bool
	}{
		{name: "unsupported kind", kind: "custom", status: "working", wantCode: "interrupt_unsupported_kind"},
		{name: "blocked Worker", kind: "claude", status: "blocked", wantCode: "worker_blocked"},
		{name: "focused Worker", kind: "claude", status: "working", focused: true, wantOutput: "interrupted: true"},
		{name: "not working", kind: "claude", status: "idle", wantOutput: "reason: agent_not_working"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			initRepo(t, repo)
			home := filepath.Join(root, "posse")
			db, err := store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			project, err := db.CreateProject(ctx, "shop", repo, "main")
			if err != nil {
				t.Fatal(err)
			}
			taskID, err := db.CreateTask(ctx, project.ID, store.Task{
				Seq: 1, Type: "change", Title: "Interrupt test", MergeMode: "local",
				PaneID: "w2:p1", PaneLabel: "posse:shop:t1", HerdrWorkspaceID: "w2", WorktreePath: repo,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			fake := herdr.NewFake()
			fake.Results["pane.read"] = []byte(`{"read":{"text":"worker output"}}`)
			focusedPane := ""
			if tc.focused {
				focusedPane = "w2:p1"
			}
			fake.SnapshotValue = herdr.Snapshot{
				FocusedPaneID: focusedPane,
				Panes: []herdr.Pane{{
					PaneID: "w2:p1", WorkspaceID: "w2", Label: "posse:shop:t1",
					Agent: tc.kind, AgentStatus: tc.status, Focused: tc.focused,
				}},
			}
			fake.BeforeCall = func(method string) {
				if method == "agent.send_keys" {
					fake.SnapshotValue.Panes[0].AgentStatus = "idle"
				}
			}
			service := testService(home, fake)
			t.Chdir(repo)
			var output bytes.Buffer
			cli := service.CLI()
			cli.Out, cli.ErrOut = &output, &output
			code := cli.Run([]string{"interrupt", "t1"})
			if tc.wantCode != "" {
				if code == 0 || !strings.Contains(output.String(), tc.wantCode) {
					t.Fatalf("interrupt result = %d %s; want %s refusal", code, output.String(), tc.wantCode)
				}
			} else if code != 0 || !strings.Contains(output.String(), tc.wantOutput) {
				t.Fatalf("interrupt result = %d %s; want %q", code, output.String(), tc.wantOutput)
			}
			wantKeys := 0
			if tc.focused {
				wantKeys = 1
			}
			if fake.CallCount("agent.send_keys") != wantKeys {
				t.Fatalf("agent.send_keys calls = %d, want %d: %#v", fake.CallCount("agent.send_keys"), wantKeys, fake.Calls)
			}
		})
	}
}

func TestKeysReachFocusedBlockedWorker(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Keys test", MergeMode: "local", PaneID: "w2:p1", PaneLabel: "posse:shop:t1", HerdrWorkspaceID: "w2", WorktreePath: repo})
	if err == nil {
		err = db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started")
	}
	if err == nil {
		err = db.Transition(ctx, taskID, store.StateWorking, store.StateBlocked, "herdr", "blocked")
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{FocusedPaneID: "w2:p1", Panes: []herdr.Pane{{PaneID: "w2:p1", WorkspaceID: "w2", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "blocked", Focused: true}}}
	t.Chdir(repo)
	var output bytes.Buffer
	cli := testService(home, fake).CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"keys", "t1", "enter"}); code != 0 || fake.CallCount("agent.send_keys") != 1 || !strings.Contains(output.String(), "state: sent") {
		t.Fatalf("focused keys command: code=%d output=%s calls=%#v", code, output.String(), fake.Calls)
	}
	requireInterventions(t, home, project.ID, store.InterventionKeys, "")

	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_interventions BEFORE INSERT ON interventions BEGIN SELECT RAISE(ABORT, 'record failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if code := cli.Run([]string{"keys", "t1", "enter"}); code != 0 || fake.CallCount("agent.send_keys") != 2 || !strings.Contains(output.String(), "state: sent") {
		t.Fatalf("keys after the intervention record failed: code=%d output=%s calls=%#v", code, output.String(), fake.Calls)
	}
}
