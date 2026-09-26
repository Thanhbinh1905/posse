package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestIdentityDoesNotAffectWorkerArtifactsOrSidebar(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, sequence, err := createTaskWithSequence(ctx, db, project, home, store.Task{Type: "ship", Title: "Stable output", ShortName: "stable-output", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, task.ID, store.StateSpawning, store.StateWorking, "cli", "Worker started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateDone, "worker", "Finished the requested change"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "task_done", Summary: "Stable output is ready", DataJSON: `{}`}); err != nil {
		t.Fatal(err)
	}

	type rendered struct {
		launch       []byte
		agentName    string
		paneLabel    string
		display      []byte
		notice       []byte
		home         []byte
		workerPrefix string
	}
	renders := make([]rendered, 0, 2)
	configs := []string{
		"[identity.lead]\nname = \"Marshal\"\npersona = \"nautical historian\"\nlanguage = \"vi\"\naddress_user = \"anh\"\n\n[identity.worker]\ndisplay_prefix = \"rider\"\n",
		"[identity.lead]\nname = \"Archivist\"\npersona = \"formal botanist\"\nlanguage = \"fr\"\naddress_user = \"madame\"\n\n[identity.worker]\ndisplay_prefix = \"acorn\"\n",
	}
	brief := dispatch.Brief{Type: "ship", Title: task.Title, DoneWhen: "the output is stable", Body: "Make the output stable."}
	launchPath := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(sequence), "launch.md")
	t.Chdir(repo)
	for _, text := range configs {
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(home, project.Name)
		if err != nil {
			t.Fatal(err)
		}
		contents := workerProtocol(project, task, brief, launchPath) + "\n\n" + brief.Body + "\n"
		if err := writeFile(launchPath, []byte(contents)); err != nil {
			t.Fatal(err)
		}
		launch, err := os.ReadFile(launchPath)
		if err != nil {
			t.Fatal(err)
		}
		notices, err := db.Notices(ctx, project.ID, true)
		if err != nil || len(notices) != 1 {
			t.Fatalf("open Notices = %#v, %v", notices, err)
		}
		notice, err := json.Marshal(notices[0])
		if err != nil {
			t.Fatal(err)
		}
		output := &bytes.Buffer{}
		cli := testService(home, nil).CLI()
		cli.Out = output
		if code := cli.Run(nil); code != 0 {
			t.Fatalf("home command failed: %s", output.String())
		}
		task.PaneID = "w1:p2"
		display, err := json.Marshal(map[string]any{"tab": workerTabLabel(task), "pane": workerDisplayMetadata(task, "pi")})
		if err != nil {
			t.Fatal(err)
		}
		renders = append(renders, rendered{launch: launch, agentName: agentName(project.Name, sequence, 1), paneLabel: task.PaneLabel, display: display, notice: notice, home: append([]byte(nil), output.Bytes()...), workerPrefix: cfg.Identity.Worker.DisplayPrefix})
	}
	if renders[0].workerPrefix == renders[1].workerPrefix {
		t.Fatal("Identity fixtures did not use distinct display prefixes")
	}
	for _, field := range []struct {
		name  string
		left  []byte
		right []byte
	}{{"launch.md", renders[0].launch, renders[1].launch}, {"agent name", []byte(renders[0].agentName), []byte(renders[1].agentName)}, {"pane label", []byte(renders[0].paneLabel), []byte(renders[1].paneLabel)}, {"Notice", renders[0].notice, renders[1].notice}, {"home output", renders[0].home, renders[1].home}} {
		if !bytes.Equal(field.left, field.right) {
			t.Errorf("%s changed with Identity:\nfirst:  %q\nsecond: %q", field.name, field.left, field.right)
		}
	}
	if renders[0].agentName != "posse-shop-t1-1" {
		t.Fatalf("canonical agent name = %q", renders[0].agentName)
	}
	if !bytes.Equal(renders[0].display, renders[1].display) {
		t.Fatalf("display_prefix changed Worker sidebar or pane metadata:\nfirst:  %s\nsecond: %s", renders[0].display, renders[1].display)
	}
	var display struct {
		Tab  string         `json:"tab"`
		Pane map[string]any `json:"pane"`
	}
	if err := json.Unmarshal(renders[0].display, &display); err != nil {
		t.Fatal(err)
	}
	if display.Tab != "stable-output" || display.Pane["title"] != "Stable output · stable-output · " || display.Pane["clear_display_agent"] != nil || display.Pane["display_agent"] != "pi" {
		t.Fatalf("Worker display metadata = %#v", display)
	}
	if got := leadAgentName("shop", 3); got != "posse-shop-lead-3" {
		t.Fatalf("Lead agent name = %q", got)
	}
}
