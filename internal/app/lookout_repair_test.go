package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLookoutRepairFailureIsActionableAndPreservesRecoveryPolicy(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	root := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", leadWorkspaceLabel(project)); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: project.LeadPaneID, WorkspaceID: project.HerdrWorkspaceID, Label: project.LeadLabel}}}
	fake.Errors["tab.create"] = errors.New("injected tab creation failure")
	service := testService(home, fake)
	t.Setenv("HERDR_PANE_ID", "")
	output := &bytes.Buffer{}
	commandContext := &axi.Context{Context: ctx, Out: output, ErrOut: &bytes.Buffer{}}
	configPath := filepath.Join(home, "config.toml")
	configBefore := []byte("[defaults]\nauto_recover = false\n")
	if err := os.WriteFile(configPath, configBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	beforeRecovery, err := db.ProjectRecovery(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeLookout, err := db.LookoutRecovery(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}

	err = service.repairLookout(commandContext, db, home, project)
	var failure *axi.Error
	if !errors.As(err, &failure) {
		t.Fatalf("repair failure = %v, want structured lookout_repair_failed", err)
	}
	if failure.Code != "lookout_repair_failed" || !failure.Retryable || !strings.Contains(strings.Join(failure.Help, "\n"), "Check Herdr availability") {
		t.Fatalf("repair failure = %#v, want actionable retryable lookout_repair_failed", failure)
	}
	afterRecovery, err := db.ProjectRecovery(ctx, project.ID)
	if err != nil || afterRecovery != beforeRecovery {
		t.Fatalf("failed repair changed recovery policy: before=%#v after=%#v err=%v", beforeRecovery, afterRecovery, err)
	}
	afterLookout, err := db.LookoutRecovery(ctx, project.ID)
	if err != nil || afterLookout != beforeLookout {
		t.Fatalf("failed repair changed Lookout runtime: before=%#v after=%#v err=%v", beforeLookout, afterLookout, err)
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(configAfter, configBefore) {
		t.Fatalf("failed repair changed config: err=%v", err)
	}
}

func TestProjectLookoutCheckSeparatesPollOnlyAndLeadWatchers(t *testing.T) {
	project := store.Project{Name: "shop", Root: t.TempDir(), HerdrWorkspaceID: "w1", LeadPaneID: "w1:p1"}
	lead := lookoutProcess{PID: os.Getpid(), PaneID: project.LeadPaneID, WorkingDir: project.Root}
	pane := herdr.Pane{PaneID: "w1:p2", WorkspaceID: "w1", Label: lookoutTabLabel(project)}

	tests := []struct {
		name      string
		state     store.LookoutRecovery
		panes     []herdr.Pane
		processes []lookoutProcess
		want      []string
	}{
		{
			name:      "Lead watcher without poll-only tab",
			state:     store.LookoutRecovery{PaneID: "w1:p-old"},
			processes: []lookoutProcess{lead},
			want:      []string{"poll-only pane missing", "poll-only processes=0", "Lead delivery Lookout=running"},
		},
		{
			name:      "poll-only tab without live poller",
			state:     store.LookoutRecovery{PaneID: pane.PaneID},
			panes:     []herdr.Pane{pane},
			processes: []lookoutProcess{lead},
			want:      []string{"poll-only pane=w1:p2", "poll-only processes=0", "Lead delivery Lookout=running"},
		},
		{
			name:      "recorded pane mismatch",
			state:     store.LookoutRecovery{PaneID: "w1:p-old"},
			panes:     []herdr.Pane{pane},
			processes: []lookoutProcess{{PID: os.Getpid(), PaneID: pane.PaneID, PollOnly: true}, lead},
			want:      []string{"recorded pane mismatch (observed=w1:p2)", "poll-only processes=1", "Lead delivery Lookout=running"},
		},
		{
			name:      "poller pane mismatch",
			state:     store.LookoutRecovery{PaneID: pane.PaneID},
			panes:     []herdr.Pane{pane},
			processes: []lookoutProcess{{PID: os.Getpid(), PaneID: "w1:p-old", PollOnly: true, WorkingDir: project.Root}},
			want:      []string{"poll-only pane=w1:p2", "poll-only process pane=w1:p-old", "Lead delivery Lookout=missing"},
		},
		{
			name:  "both watchers missing",
			state: store.LookoutRecovery{PaneID: "w1:p-old"},
			want:  []string{"poll-only pane missing", "poll-only processes=0", "Lead delivery Lookout=missing"},
		},
		{
			name:      "healthy poll-only watcher",
			state:     store.LookoutRecovery{PaneID: pane.PaneID},
			panes:     []herdr.Pane{pane},
			processes: []lookoutProcess{{PID: os.Getpid(), PaneID: pane.PaneID, PollOnly: true}, lead},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			check := projectLookoutCheck(project, test.state, herdr.Snapshot{Panes: test.panes}, test.processes, nil)
			if test.name == "healthy poll-only watcher" {
				if check.Status != "ok" {
					t.Fatalf("healthy watcher status = %s: %s", check.Status, check.Detail)
				}
				return
			}
			if check.Status != "warn" {
				t.Fatalf("unhealthy watcher status = %s, want warn: %s", check.Status, check.Detail)
			}
			for _, want := range test.want {
				if !strings.Contains(check.Detail, want) {
					t.Errorf("watcher detail %q does not contain %q", check.Detail, want)
				}
			}
		})
	}
}
