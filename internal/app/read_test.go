package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
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
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nmerge_mode = \"local\"\n"), 0o600); err != nil {
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
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 5, Type: "change", Title: "Ready to merge", ShortName: "ready-to-merge", MergeMode: "local", Branch: "posse/t5", AgentName: "posse-shop-t5-1", PaneID: "w2:p1", PaneLabel: "posse:shop:t5"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateWorking, store.StateDone, "worker", "finished"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: taskID, Kind: "task_done", Summary: "Ready to merge", DataJSON: `{}`}); err != nil {
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
	for _, expected := range []string{"project{name,mode,autonomy,lead}:", "shop,local,\"review=ask merge=ask\",working", "Run `posse show t5`", "Run `posse merge t5`", "Run `posse ack 1`"} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("home output missing %q:\n%s", expected, output.String())
		}
	}
	if strings.Contains(output.String(), "open_tasks") || strings.Contains(output.String(), "open_notices") || !strings.Contains(output.String(), "t5") || !strings.Contains(output.String(), "ready-to-merge") {
		t.Fatalf("home output included list fields, omitted the Task name, or used a hardcoded Task id: %s", output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"list", "--json"}); code != 0 {
		t.Fatalf("list JSON failed: code=%d output=%s", code, output.String())
	}
	var list struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(output.Bytes(), &list); err != nil || len(list.Tasks) != 1 || list.Tasks[0]["id"] != "t5" || list.Tasks[0]["name"] != "ready-to-merge" {
		t.Fatalf("list omitted the Task name: err=%v output=%s rows=%#v", err, output.String(), list.Tasks)
	}
	output.Reset()
	if code := cli.Run([]string{"--json", "list", "--full"}); code != 0 || strings.Contains(output.String(), "posse/t5") {
		t.Fatalf("full list exposed legacy branch: code=%d output=%s", code, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"--json", "show", "ready-to-merge"}); code != 0 || !strings.Contains(output.String(), `"id":"t5"`) || !strings.Contains(output.String(), `"name":"ready-to-merge"`) {
		t.Fatalf("show by visible Worker name failed: code=%d output=%s", code, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"--json", "show", "ready-to-merge", "--full"}); code != 0 || strings.Contains(output.String(), "posse/t5") || strings.Contains(output.String(), "posse-shop-t5") {
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
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 5, Type: "change", Title: "Inspect", Profile: "deep", MergeMode: "local"})
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
	if code := cli.Run([]string{"--json", "list"}); code != 0 {
		t.Fatalf("list failed: code=%d output=%s", code, output.String())
	}
	var list struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(output.Bytes(), &list); err != nil || len(list.Tasks) != 1 || list.Tasks[0]["profile"] != "fast" {
		t.Fatalf("list omitted the current Profile: %#v, %v, output=%s", list, err, output.String())
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

func TestShowMarksUndeliverableAndWithdrawnMessagesAsHistory(t *testing.T) {
	ctx := context.Background()
	fixture := newRelaunchFixture(t, store.StateDone)
	for index, status := range []string{"undeliverable", "withdrawn"} {
		if _, err := fixture.db.ExecContext(ctx, `INSERT INTO messages(task_id,body,created_at,status) VALUES(?,?,?,?)`, fixture.task.ID, status+" instruction", fixture.task.CreatedAt+int64(index)+1, status); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(filepath.Join(filepath.Dir(fixture.home), "repo"))
	var output bytes.Buffer
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"--json", "show", "t1"}); code != 0 {
		t.Fatalf("show failed: code=%d output=%s", code, output.String())
	}
	var result struct {
		Messages []struct {
			Status      string `json:"status"`
			History     *bool  `json:"history"`
			BlocksSends *bool  `json:"blocks_sends"`
		} `json:"undelivered_messages"`
		Help []string `json:"help"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode show output: %v: %s", err, output.String())
	}
	if len(result.Messages) != 2 {
		t.Fatalf("show returned %d message rows, want 2: %s", len(result.Messages), output.String())
	}
	for _, message := range result.Messages {
		if message.Status != "undeliverable" && message.Status != "withdrawn" {
			t.Errorf("unexpected message status %q", message.Status)
		}
		if message.History == nil || !*message.History || message.BlocksSends == nil || *message.BlocksSends {
			t.Errorf("%s message is not marked as non-blocking history: %#v", message.Status, message)
		}
	}
	if !strings.Contains(strings.Join(result.Help, " "), "do not block new sends when this Task accepts instructions") {
		t.Errorf("show did not explain that message history does not block new sends: %#v", result.Help)
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
	if _, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Read output", MergeMode: "local", PaneID: "w2:p-old", PaneLabel: "posse:shop:t1"}); err != nil {
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

func TestFullJSONHomeCarriesBoardFields(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	brief := "---\ntype: change\ntitle: E2E Brief title\ndone_when: commit exists\nticket: 571\n---\nDo the work.\n\n## Intent\nUser's words: \"one view\"\n\n## Notes\nNot intent.\n"
	if err := os.WriteFile(filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "brief.md"), []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
	if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	created := time.Now().Add(-2 * time.Hour).UnixMilli()
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET profile='deep',created_at=? WHERE id=?`, created, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	for round, status := range []string{"verdict", "admitted"} {
		reviewID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: round + 2, Type: "review", Title: "Review", ReviewsTaskID: fixture.task.ID})
		if err != nil {
			t.Fatal(err)
		}
		verdict := any(nil)
		if status == "verdict" {
			verdict = "changes_requested"
		}
		if _, err := fixture.db.ExecContext(ctx, `INSERT INTO review_rounds(ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,verdict,created_at) VALUES(?,?,?,'shop','full',?,?,?,?,?,'[]',?,?,1)`,
			fixture.task.ID, round+1, reviewID, sha, sha, sha, sha, strings.Repeat("b", 64), status, verdict); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.db.AddSignal(ctx, fixture.task.ID, "working", "rebased on main", nil); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	code, output, errOutput := fixture.run("--json", "--full")
	if code != 0 {
		t.Fatalf("home exit=%d output=%s error=%s", code, output, errOutput)
	}
	var home struct {
		Pool  map[string]any   `json:"pool"`
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(output), &home); err != nil {
		t.Fatalf("decode home: %v: %s", err, output)
	}
	if home.Pool["used"] != float64(3) || home.Pool["max"] != float64(cfg.Defaults.MaxWorkers) {
		t.Errorf("pool = %#v, want t1 and its two Review Rounds of max_workers=%d", home.Pool, cfg.Defaults.MaxWorkers)
	}
	var row map[string]any
	for _, task := range home.Tasks {
		if task["id"] == "t1" {
			row = task
		}
	}
	if row == nil {
		t.Fatalf("home omitted t1: %s", output)
	}
	want := map[string]any{
		"profile":      "deep",
		"created_at":   time.UnixMilli(created).UTC().Format(time.RFC3339),
		"ticket":       "#571",
		"ci":           "failure",
		"review_round": float64(2),
		"intent":       "User's words: \"one view\"",
		"pr_state":     "OPEN",
		"pr_url":       task.PRURL,
		"last_signal":  "working: rebased on main",
	}
	for key, value := range want {
		if row[key] != value {
			t.Errorf("t1 %s = %#v, want %#v", key, row[key], value)
		}
	}
	if age, _ := row["age"].(string); !strings.HasPrefix(age, "2h0m") {
		t.Errorf("t1 age = %#v, want about 2h", row["age"])
	}
}

func TestShowIncludesBlockedDialog(t *testing.T) {
	fixture := newRelaunchFixture(t, store.StateBlocked)
	fixture.fake.SnapshotValue.Panes[0].AgentStatus = "blocked"
	fixture.fake.Results["pane.read"] = json.RawMessage(`{"text":"\n Bash command\n   rm -rf build/*\n Do you want to proceed?\n"}`)
	t.Chdir(filepath.Join(filepath.Dir(fixture.home), "repo"))
	var output bytes.Buffer
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"--json", "show", "t1"}); code != 0 {
		t.Fatalf("show failed: code=%d output=%s", code, output.String())
	}
	var result struct {
		Dialog string `json:"blocked_dialog"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode show output: %v: %s", err, output.String())
	}
	for _, want := range []string{"rm -rf build/*", "Do you want to proceed?"} {
		if !strings.Contains(result.Dialog, want) {
			t.Fatalf("blocked_dialog lacks %q: %s", want, output.String())
		}
	}
}
