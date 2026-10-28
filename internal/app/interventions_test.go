package app

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func requireInterventions(t *testing.T, home string, projectID int64, kind string, reasons ...string) {
	t.Helper()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Interventions(context.Background(), projectID, kind)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, row := range rows {
		got = append(got, row.Reason)
	}
	if !slices.Equal(got, reasons) {
		t.Fatalf("%s interventions reasons = %q, want %q", kind, got, reasons)
	}
}

func TestSendRecordsInterventionWithReason(t *testing.T) {
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
	id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Fix", MergeMode: "local", PaneID: "w2:p1", PaneLabel: "posse:shop:t1", HerdrWorkspaceID: "w2", WorktreePath: repo})
	if err == nil {
		err = db.Transition(ctx, id, store.StateSpawning, store.StateWorking, "cli", "started")
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.Results["pane.read"] = []byte(`{"text":"worker output"}`)
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w2:p1", WorkspaceID: "w2", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "working"}}}
	t.Chdir(repo)
	var output bytes.Buffer
	cli := testService(home, fake).CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"send", "t1", "Fix the lint error", "--reason", "ci"}); code != 0 {
		t.Fatalf("send: code=%d output=%s", code, output.String())
	}
	fake.SnapshotValue.Panes[0].AgentStatus = "idle"
	if code := cli.Run([]string{"send", "t1", "--interrupt", "Stop and rebase", "--reason", "conflict"}); code != 0 {
		t.Fatalf("send --interrupt: code=%d output=%s", code, output.String())
	}
	requireInterventions(t, home, project.ID, store.InterventionSend, "ci")
	requireInterventions(t, home, project.ID, store.InterventionInterrupt, "conflict")
}
