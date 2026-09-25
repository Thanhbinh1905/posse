package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestProjectHomeUsesCompactRowAndStateDerivedHelp(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
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
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 5, Type: "ship", Title: "Ready to land", ShortName: "ready-to-land", LandingMode: "local", Branch: "posse/t5", AgentName: "posse-shop-t5-1", PaneID: "w2:p1", PaneLabel: "posse:shop:t5"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateWorking, store.StateDone, "worker", "finished"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: taskID, Kind: "task_done", Summary: "Ready to land", DataJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "working"},
		{PaneID: "w2:p1", WorkspaceID: "w2", Label: "posse:shop:t5", Agent: "claude", AgentStatus: "idle"},
	}}
	var output bytes.Buffer
	cli := testService(home, fake).CLI()
	cli.Out = &output
	t.Chdir(repo)
	if code := cli.Run([]string{"--full"}); code != 0 {
		t.Fatalf("home failed: code=%d output=%s", code, output.String())
	}
	for _, expected := range []string{"project{name,mode,autonomy,lead}:", "shop,local,\"review=ask land=ask\",working", "Run `posse show ready-to-land`", "Run `posse land ready-to-land`", "Run `posse ack 1`"} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("home output missing %q:\n%s", expected, output.String())
		}
	}
	if strings.Contains(output.String(), "open_tasks") || strings.Contains(output.String(), "open_notices") || strings.Contains(output.String(), "t5") || !strings.Contains(output.String(), "ready-to-land") {
		t.Fatalf("home output included roster fields, omitted the Task name, or used a hardcoded Task id: %s", output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"roster", "--json"}); code != 0 {
		t.Fatalf("roster JSON failed: code=%d output=%s", code, output.String())
	}
	var roster struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(output.Bytes(), &roster); err != nil || len(roster.Tasks) != 1 || roster.Tasks[0]["id"] != "ready-to-land" {
		t.Fatalf("roster omitted the Task name: err=%v output=%s rows=%#v", err, output.String(), roster.Tasks)
	}
	output.Reset()
	if code := cli.Run([]string{"--json", "roster", "--full"}); code != 0 || strings.Contains(output.String(), "posse/t5") {
		t.Fatalf("full roster exposed legacy branch: code=%d output=%s", code, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"--json", "show", "ready-to-land"}); code != 0 || strings.Contains(output.String(), `"id":"t5"`) || !strings.Contains(output.String(), `"id":"ready-to-land"`) {
		t.Fatalf("show by visible Rider name failed: code=%d output=%s", code, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"--json", "show", "ready-to-land", "--full"}); code != 0 || strings.Contains(output.String(), "posse/t5") || strings.Contains(output.String(), "posse-shop-t5") {
		t.Fatalf("full show exposed legacy Task identity: code=%d output=%s", code, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"--json"}); code != 0 {
		t.Fatalf("JSON home failed: code=%d output=%s", code, output.String())
	}
	var jsonHome struct {
		Project map[string]any `json:"project"`
	}
	if err := json.Unmarshal(output.Bytes(), &jsonHome); err != nil {
		t.Fatalf("decode JSON home: %v: %s", err, output.String())
	}
	if len(jsonHome.Project) != 4 || jsonHome.Project["name"] != "shop" || jsonHome.Project["lead"] != "working" {
		t.Fatalf("JSON home project row = %#v", jsonHome.Project)
	}
}

func TestShowIncludesSignalsAndTransitionsOnlyWithFull(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 5, Type: "ship", Title: "Inspect", Profile: "deep", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateWorking, store.StateDone, "worker", "finished"); err != nil {
		t.Fatal(err)
	}
	if err := db.ChangeTaskProfile(ctx, taskID, "deep", "fast"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	var output bytes.Buffer
	cli := testService(home, nil).CLI()
	cli.Out = &output
	if code := cli.Run([]string{"--json", "show", "t5"}); code != 0 {
		t.Fatalf("show failed: code=%d output=%s", code, output.String())
	}
	var compact map[string]any
	if err := json.Unmarshal(output.Bytes(), &compact); err != nil {
		t.Fatalf("decode compact show: %v: %s", err, output.String())
	}
	if _, exists := compact["signals"]; exists {
		t.Fatal("default show exposed Signals")
	}
	if _, exists := compact["transitions"]; exists {
		t.Fatal("default show exposed transitions")
	}
	if compact["profile"] != "fast" {
		t.Fatalf("default show Profile = %#v, want current Profile fast", compact["profile"])
	}
	output.Reset()
	if code := cli.Run([]string{"--json", "roster"}); code != 0 {
		t.Fatalf("roster failed: code=%d output=%s", code, output.String())
	}
	var roster struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(output.Bytes(), &roster); err != nil || len(roster.Tasks) != 1 || roster.Tasks[0]["profile"] != "fast" {
		t.Fatalf("roster omitted the current Profile: %#v, %v, output=%s", roster, err, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"--json", "show", "t5", "--full"}); code != 0 {
		t.Fatalf("show --full failed: code=%d output=%s", code, output.String())
	}
	var full map[string]any
	if err := json.Unmarshal(output.Bytes(), &full); err != nil {
		t.Fatalf("decode full show: %v: %s", err, output.String())
	}
	if _, exists := full["signals"]; !exists {
		t.Fatal("show --full omitted Signals")
	}
	if _, exists := full["transitions"]; !exists {
		t.Fatal("show --full omitted transitions")
	}
}

func TestPeekReadsPaneIdAfterLabelReconciliation(t *testing.T) {
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
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Read output", LandingMode: "local", PaneID: "w2:p-old", PaneLabel: "posse:shop:t1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "working"},
		{PaneID: "w3:p1", WorkspaceID: "w3", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "working"},
	}}
	fake.Results["pane.read"] = json.RawMessage(`{"text":"latest output"}`)
	var output bytes.Buffer
	cli := testService(home, fake).CLI()
	cli.Out = &output
	t.Chdir(repo)
	if code := cli.Run([]string{"peek", "t1"}); code != 0 {
		t.Fatalf("peek failed: code=%d output=%s", code, output.String())
	}
	var paneID string
	for _, call := range fake.Calls {
		if call.Method == "pane.read" {
			paneID, _ = call.Params["pane_id"].(string)
		}
	}
	if paneID != "w3:p1" || !strings.Contains(output.String(), "latest output") {
		t.Fatalf("peek used stale pane id %q or missed output: %s", paneID, output.String())
	}
}
