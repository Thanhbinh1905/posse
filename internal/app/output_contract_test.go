package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestTaskOutputContract(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "home")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []store.Task{
		{Seq: 1, Type: "ship", Title: "Duplicate", ShortName: "duplicate", LandingMode: "local"},
		{Seq: 2, Type: "ship", Title: "Duplicate", ShortName: "duplicate", LandingMode: "local"},
		{Seq: 3, Type: "ship", Title: "Failed work", ShortName: "failed-work", LandingMode: "local"},
		{Seq: 4, Type: "ship", Title: "Lost work", ShortName: "lost-work", LandingMode: "local"},
	} {
		id, err := db.CreateTask(ctx, project.ID, task)
		if err != nil {
			t.Fatal(err)
		}
		if task.Seq >= 3 {
			if err := db.Transition(ctx, id, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
				t.Fatal(err)
			}
			state, kind, reason := store.StateFailed, "task_failed", "build failed"
			if task.Seq == 4 {
				state, kind, reason = store.StateLost, "task_lost", "pane unavailable"
			}
			source := "worker"
			if task.Seq == 4 {
				source = "cli"
			}
			if err := db.Transition(ctx, id, store.StateWorking, state, source, reason); err != nil {
				t.Fatal(err)
			}
			if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: id, Kind: kind, Summary: task.Title + " lost its Rider"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	var output bytes.Buffer
	cli := testService(home, nil).CLI()
	cli.Out, cli.ErrOut = &output, &output
	run := func(args ...string) map[string]any {
		t.Helper()
		output.Reset()
		if code := cli.Run(append([]string{"--json"}, args...)); code != 0 {
			t.Fatalf("%v: %d %s", args, code, output.String())
		}
		var result map[string]any
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, args := range [][]string{{}, {"--full"}, {"roster"}, {"roster", "--all"}, {"show", "t3"}, {"show", "t3", "--full"}, {"decisions"}, {"project", "show"}, {"config", "schema"}, {"config", "show", "--effective"}, {"remuda"}} {
		result := run(args...)
		text, _ := json.Marshal(result)
		if matched := regexp.MustCompile(`"[A-Z][A-Za-z]*"\s*:`).Find(text); matched != nil {
			t.Errorf("%v: non-snake key %s", args, matched)
		}
		if strings.Contains(string(text), `"at":`+"1") || strings.Contains(string(text), `"created_at":`+"1") {
			t.Errorf("%v: raw timestamp %s", args, text)
		}
		switch strings.Join(args, " ") {
		case "roster":
			rows := result["tasks"].([]any)
			if rows[0].(map[string]any)["id"] != "t1" || rows[0].(map[string]any)["name"] != "duplicate" {
				t.Errorf("roster ids: %s", text)
			}
			if rows[2].(map[string]any)["reason"] != "build failed" || rows[3].(map[string]any)["reason"] != "pane unavailable" {
				t.Errorf("roster reason: %s", text)
			}
		case "decisions":
			decisions := result["decisions"].([]any)
			if len(decisions) != 2 || decisions[0].(map[string]any)["task_id"] != "t3" || !strings.Contains(decisions[0].(map[string]any)["question"].(string), "build failed") || decisions[1].(map[string]any)["task_id"] != "t4" || !strings.Contains(decisions[1].(map[string]any)["question"].(string), "pane unavailable") {
				t.Errorf("recovery Decision: %s", text)
			}
		case "show t3 --full":
			records := result["transitions"].([]any)
			row := records[0].(map[string]any)
			if _, ok := row["task_id"]; !ok {
				t.Errorf("transition keys: %s", text)
			}
			if _, ok := row["at"].(string); !ok {
				t.Errorf("transition time: %s", text)
			}
		case "":
			if rows, ok := result["needs_you"].([]any); !ok || len(rows) != 2 {
				t.Errorf("dashboard needs_you: %s", text)
			}
			if action, ok := result["next_action"].(string); ok && !strings.Contains(action, "t3") {
				t.Errorf("next_action missing ID: %s", action)
			}
		}
	}
	service := testService(home, nil)
	observer, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	for _, want := range []struct {
		id    string
		state store.State
	}{{"t3", store.StateFailed}, {"t4", store.StateLost}} {
		persisted, err := observer.Task(ctx, project.ID, want.id)
		if err != nil || persisted.State != want.state {
			t.Errorf("automatic discard or state change: %s %#v %v", want.id, persisted, err)
		}
	}
	if err := service.regenerateProjects(ctx, observer); err != nil {
		t.Fatal(err)
	}
	projectsMD, err := os.ReadFile(filepath.Join(home, "projects.md"))
	if err != nil || !strings.Contains(string(projectsMD), "shop/t3 (failed-work)") {
		t.Errorf("projects.md Task ID: %s, %v", projectsMD, err)
	}
	t.Chdir(root)
	for _, args := range [][]string{{}, {"roster", "--all"}} {
		result := run(args...)
		projects := result["projects"].([]any)
		tasks := projects[0].(map[string]any)["tasks"].([]any)
		if tasks[0].(map[string]any)["id"] != "shop/t1" {
			t.Errorf("cross-Project Task ID: %#v", tasks)
		}
	}
	t.Chdir(repo)
	output.Reset()
	if code := cli.Run([]string{"--json", "show", "duplicate"}); code != 1 {
		t.Fatalf("ambiguous name accepted: %d %s", code, output.String())
	}
	if !strings.Contains(output.String(), `"task_ambiguous"`) || !strings.Contains(output.String(), "t1") || !strings.Contains(output.String(), "t2") {
		t.Errorf("ambiguous: %s", output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"--json", "show", "t1"}); code != 0 {
		t.Errorf("ID not accepted: %d %s", code, output.String())
	}
}

// All registered command paths share the formatter, including nested commands.
// Real dashboard, roster and show outputs are covered above.
func TestEveryCommandUsesSharedOutputFormat(t *testing.T) {
	cli := testService(t.TempDir(), nil).CLI()
	var paths [][]string
	var collect func(*axi.Command, []string)
	collect = func(command *axi.Command, path []string) {
		if command.Handler != nil {
			paths = append(paths, append([]string(nil), path...))
			command.Handler = func(ctx *axi.Context, _ []string) error {
				return ctx.Print(map[string]any{"TaskID": "t1", "TransitionAt": int64(1735689600000)})
			}
		}
		for _, child := range command.Subcommands {
			collect(child, append(append([]string(nil), path...), child.Name))
		}
	}
	collect(cli.Root, nil)
	if len(paths) < 30 {
		t.Fatalf("only %d commands tested", len(paths))
	}
	for _, path := range paths {
		for _, format := range []string{"--json", ""} {
			var output bytes.Buffer
			cli.Out = &output
			args := append([]string(nil), path...)
			if format != "" {
				args = append(args, format)
			}
			if code := cli.Run(args); code != 0 || strings.Contains(output.String(), "TaskID") || strings.Contains(output.String(), "TransitionAt") || !strings.Contains(output.String(), "task_id") || !strings.Contains(output.String(), "transition_at") || strings.Contains(output.String(), "1735689600000") {
				t.Errorf("%v: code=%d output=%s", args, code, output.String())
			}
		}
	}
}

func TestDispatchPreviewSlugOnProfileRequired(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "home")
	config := "[profiles.one]\nkind = \"pi\"\n[profiles.two]\nkind = \"pi\"\n[[dispatch]]\ntype = \"ship\"\nuse = \"one\"\n[[dispatch]]\ntype = \"ship\"\nuse = \"two\"\n"
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(root, "brief.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: Fix unstable tests\ndone_when: Tests pass\n---\nRepair.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_ = project
	t.Chdir(repo)
	var output bytes.Buffer
	cli := testService(home, nil).CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"dispatch", "--brief", brief}); code != 1 || !strings.Contains(output.String(), "fix-unstable-tests") {
		t.Errorf("dispatch needs Profile and slug: %d %s", code, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"dispatch", "--brief", brief, "--profile", "one"}); code != 0 || !strings.Contains(output.String(), "fix-unstable-tests") {
		t.Errorf("dispatch override: %d %s", code, output.String())
	}
}
