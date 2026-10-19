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
		{name: "focused Worker", kind: "claude", status: "working", focused: true, wantCode: "pane_focused"},
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
			if fake.CallCount("agent.send_keys") != 0 {
				t.Fatalf("refused interrupt sent abort keys: %#v", fake.Calls)
			}
		})
	}
}
