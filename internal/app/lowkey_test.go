package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestNoticeWakeUsesRiderNameInsteadOfInternalTaskID(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 34, Type: "ship", Title: "Improve login", ShortName: "login-cleanup", Branch: "posse/t34"})
	if err != nil {
		t.Fatal(err)
	}
	notice := store.Notice{ID: 12, ProjectID: project.ID, TaskID: id, Kind: "pr_merged", Summary: "Improve login: pull request merged"}
	wake := noticeWakeMessage(ctx, db, []store.Notice{notice}, true)
	if !strings.Contains(wake, "Improve login: pull request merged") || strings.Contains(wake, "login-cleanup pr-merged") || strings.Contains(wake, "t34") {
		t.Fatalf("Notice wake exposes internal id: %s", wake)
	}
	task, err := db.TaskByID(ctx, project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if tab := workerTabLabel(task); tab != "login-cleanup" {
		t.Fatalf("legacy branch tab = %q", tab)
	}
	if metadata := workerDisplayMetadata(task); metadata["display_agent"] != nil || metadata["clear_display_agent"] != true {
		t.Fatalf("legacy branch overrides harness subtitle: %#v", metadata)
	}
}

func TestLowkeyToggleAndLookoutWakeWithoutRestart(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	t.Chdir(repo)
	home := filepath.Join(root, "home")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "working"}}}
	var out, errOut bytes.Buffer
	cli := testService(home, fake).CLI()
	cli.Out, cli.ErrOut = &out, &errOut
	run := func(args ...string) string {
		t.Helper()
		out.Reset()
		errOut.Reset()
		if code := cli.Run(args); code != 0 {
			t.Fatalf("posse %v failed: %s %s", args, out.String(), errOut.String())
		}
		return out.String()
	}
	if got := run("lowkey", "status"); !strings.Contains(got, "lowkey: off") {
		t.Fatal(got)
	}
	if got := run("config", "schema", "--json"); !strings.Contains(got, `"key":"lowkey.lead"`) || !strings.Contains(got, `"user_only":false`) {
		t.Fatalf("lowkey mode not in schema: %s", got)
	}
	if got := run("config", "set", "lowkey.lead", "true", "--project", "shop"); !strings.Contains(got, "lowkey.lead") {
		t.Fatal(got)
	}
	if got := run("lowkey", "status"); !strings.Contains(got, "lowkey: on") {
		t.Fatalf("config set not visible to lowkey: %s", got)
	}
	if got := run("lowkey", "on"); !strings.Contains(got, "lowkey: on") {
		t.Fatal(got)
	}
	if cfg, err := config.Load(home, "shop"); err != nil || !cfg.Lowkey.Lead {
		t.Fatalf("lowkey not persisted: %#v %v", cfg.Lowkey, err)
	}
	if got := run("lead"); !strings.Contains(got, "Lowkey mode on") || !strings.Contains(got, "lookout --ack") || !strings.Contains(got, "acknowledge silently") {
		t.Fatal(got)
	}
	if got := run(); !strings.Contains(got, "lowkey: true") || strings.Contains(got, "project{name,mode,autonomy,lead}") {
		t.Fatal(got)
	}
	if got := run("--full"); !strings.Contains(got, "project{name,mode,autonomy,lead}") {
		t.Fatal(got)
	}
	var complete map[string]any
	if err := json.Unmarshal([]byte(run("--json")), &complete); err != nil || complete["notices"] == nil || complete["tasks"] == nil {
		t.Fatalf("JSON dashboard: %#v %v", complete, err)
	}
	id, err := db.CreateNotice(context.Background(), store.Notice{ProjectID: project.ID, Kind: "needs_decision", Summary: "Choose  A\n or B"})
	if err != nil {
		t.Fatal(err)
	}
	var wake struct {
		Lowkey bool   `json:"lowkey"`
		Wake   string `json:"wake"`
	}
	if err := json.Unmarshal([]byte(run("lookout", "--json", "--timeout", "1000")), &wake); err != nil || !wake.Lowkey || !strings.Contains(wake.Wake, "project needs-decision - Choose A or B") || !strings.Contains(wake.Wake, "Notice ids: ") {
		t.Fatalf("wake: %#v %v", wake, err)
	}
	if got := run("lookout", "--ack", stringID(id), "--timeout", "1"); !strings.Contains(got, "timeout") {
		t.Fatalf("ack and wait: %s", got)
	}
	notices, err := db.Notices(context.Background(), project.ID, true)
	if err != nil || len(notices) != 0 {
		t.Fatalf("ack not persisted: %#v %v", notices, err)
	}
	if got := run("lowkey", "off"); !strings.Contains(got, "lowkey: off") {
		t.Fatal(got)
	}
	if _, err := db.CreateNotice(context.Background(), store.Notice{ProjectID: project.ID, Kind: "pr_opened", Summary: "opened"}); err != nil {
		t.Fatal(err)
	}
	if got := run("lookout", "--json", "--timeout", "1000"); !strings.Contains(got, "[posse | Posse -> Lead project | notice #") || !strings.Contains(got, `"lowkey":false`) {
		t.Fatal(got)
	}
	if data, err := os.ReadFile(config.ConfigPath(home, "shop")); err != nil || !strings.Contains(string(data), "lead = false") {
		t.Fatalf("lowkey off not persisted: %s %v", data, err)
	}
}

func TestLowkeyWakeIsBoundedAndSanitized(t *testing.T) {
	notices := []store.Notice{{Kind: "task_done", Summary: strings.Repeat("ab\n", 300)}}
	wake := noticeWakeMessage(context.Background(), nil, notices, true)
	if len(wake) > 1000 || strings.Count(wake, "\n") != 1 || !strings.Contains(wake, "done - ") {
		t.Fatal(wake)
	}
}
