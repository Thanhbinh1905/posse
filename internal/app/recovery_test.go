package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/runtime"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestRecoverRebuildMovesCorruptDatabaseAside(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 1, Type: "scout", Title: "Snapshot", LandingMode: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(home, "posse.db")
	if err := os.WriteFile(databasePath, []byte("corrupt database contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	service := testService(home, herdr.NewFake())
	output, errorsOut := &bytes.Buffer{}, &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	t.Setenv("HERDR_PANE_ID", "")
	t.Chdir(repo)
	if code := cli.Run([]string{"recover", "--rebuild"}); code != 0 {
		t.Fatalf("recover --rebuild failed: code=%d out=%s err=%s", code, output, errorsOut)
	}
	corruptCopies, err := filepath.Glob(databasePath + ".corrupt-*")
	if err != nil || len(corruptCopies) != 1 {
		t.Fatalf("corrupt database was not preserved aside: %v, %v", corruptCopies, err)
	}
	preserved, err := os.ReadFile(corruptCopies[0])
	if err != nil || string(preserved) != "corrupt database contents" {
		t.Fatalf("preserved corrupt database = %q, %v", preserved, err)
	}
	backups, err := filepath.Glob(filepath.Join(home, "backup", "posse-before-rebuild-*.db"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("pre-rebuild VACUUM backup missing: %v, %v", backups, err)
	}
	rebuilt, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	restored, err := rebuilt.Task(context.Background(), project.ID, "t1")
	if err != nil || restored.Title != "Snapshot" {
		t.Fatalf("snapshot Task was not restored: %#v, %v", restored, err)
	}
}

func TestRecoverRebuildWithHeldMountAndAcknowledgedDecisionNotices(t *testing.T) {
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
	shipID, err := db.CreateTask(ctx, project.ID, store.Task{
		Seq: 1, Type: "ship", ShortName: "ship", Title: "Ship with a held Mount", LandingMode: "pr",
		Branch: "posse/ship", PRURL: "https://example.test/pr/1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, shipID, store.StateSpawning, store.StateWorking, "cli", "Rider started"); err != nil {
		t.Fatal(err)
	}
	mount, err := db.AcquireMount(ctx, project.ID, shipID, filepath.Join(home, "remuda", "shop"))
	if err != nil {
		t.Fatal(err)
	}
	scoutID, err := db.CreateTask(ctx, project.ID, store.Task{
		Seq: 2, Type: "scout", ShortName: "scout", Title: "Scout report", LandingMode: "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, scoutID, store.StateSpawning, store.StateWorking, "cli", "Rider started"); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(home, "projects", project.Name, "tasks", "t2", "report.md")
	if err := os.WriteFile(reportPath, []byte("# Findings\n\nThe Project is healthy.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scout, err := db.TaskByID(ctx, project.ID, scoutID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := db.RecordWorkerSignal(ctx, scout, "done", "Report ready", nil, "task_done")
	if err != nil || state != store.StateReported {
		t.Fatalf("Scout done Signal state = %q, %v", state, err)
	}
	decision, err := db.RaiseDecision(ctx, store.DecisionRequest{
		ProjectID: project.ID, TaskID: shipID, Origin: "recover-rebuild-e2e", Kind: "rider_question",
		Question: "Should the Ship continue?", Options: []string{"continue", "stop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AnswerDecision(ctx, project.ID, decision.ID, "continue", "Continue the Ship."); err != nil {
		t.Fatal(err)
	}
	noticeID, err := db.CreateNotice(ctx, store.Notice{
		ProjectID: project.ID, TaskID: scoutID, Kind: "report_ready", Summary: "Scout report is ready",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AckNotices(ctx, project.ID, []string{"all"}); err != nil {
		t.Fatal(err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) < 2 {
		t.Fatalf("fixture Notices = %#v, %v", notices, err)
	}
	lastNoticeID := notices[len(notices)-1].ID
	if err := db.AdvanceDecisionNoticeCursor(ctx, project.ID, lastNoticeID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	service := testService(home, herdr.NewFake())
	output, errorsOut := &bytes.Buffer{}, &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	t.Setenv("HERDR_PANE_ID", "")
	t.Chdir(repo)
	if code := cli.Run([]string{"recover", "--rebuild"}); code != 0 {
		t.Fatalf("recover --rebuild failed: code=%d out=%s err=%s", code, output, errorsOut)
	}

	assertRestored := func() {
		rebuilt, err := store.Open(home)
		if err != nil {
			t.Fatal(err)
		}
		defer rebuilt.Close()
		foreignKeys, err := rebuilt.QueryContext(ctx, "PRAGMA foreign_key_check")
		if err != nil {
			t.Fatal(err)
		}
		defer foreignKeys.Close()
		if foreignKeys.Next() {
			var table string
			var rowID, parent string
			var foreignKeyID int
			if err := foreignKeys.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
				t.Fatal(err)
			}
			t.Fatalf("rebuilt database has foreign-key violation: table=%s row=%s parent=%s fk=%d", table, rowID, parent, foreignKeyID)
		}
		if err := foreignKeys.Err(); err != nil {
			t.Fatal(err)
		}
		restoredProject, err := rebuilt.ProjectByName(ctx, "shop")
		if err != nil || restoredProject.Root != repo {
			t.Fatalf("rebuilt Project = %#v, %v", restoredProject, err)
		}
		restoredShip, err := rebuilt.Task(ctx, project.ID, "t1")
		if err != nil || restoredShip.PRURL != "https://example.test/pr/1" || restoredShip.MountID != mount.ID {
			t.Fatalf("rebuilt Ship lost its PR or held Mount reference: %#v, %v", restoredShip, err)
		}
		restoredMount, err := rebuilt.MountByTask(ctx, shipID)
		if err != nil || restoredMount.State != "held" || restoredMount.Path != mount.Path {
			t.Fatalf("rebuilt held Mount = %#v, %v", restoredMount, err)
		}
		restoredScout, err := rebuilt.Task(ctx, project.ID, "t2")
		if err != nil || restoredScout.State != store.StateReported {
			t.Fatalf("rebuilt Scout = %#v, %v", restoredScout, err)
		}
		if _, err := os.Stat(reportPath); err != nil {
			t.Fatalf("Scout Report was lost: %v", err)
		}
		restoredDecision, err := rebuilt.Decisions(ctx, project.ID, false)
		if err != nil || len(restoredDecision) != 1 || restoredDecision[0].Answer != "continue" {
			t.Fatalf("rebuilt Decision = %#v, %v", restoredDecision, err)
		}
		restoredNotices, err := rebuilt.Notices(ctx, project.ID, false)
		if err != nil || len(restoredNotices) < 2 {
			t.Fatalf("rebuilt Notices = %#v, %v", restoredNotices, err)
		}
		acked := false
		for _, notice := range restoredNotices {
			if notice.ID == noticeID && notice.AckedAt != 0 {
				acked = true
			}
		}
		if !acked {
			t.Fatalf("acknowledged Notice %d was not restored: %#v", noticeID, restoredNotices)
		}
		var restoredCursor int64
		if err := rebuilt.QueryRowContext(ctx, `SELECT last_notice_id FROM decision_notice_cursors WHERE project_id=?`, project.ID).Scan(&restoredCursor); err != nil || restoredCursor != lastNoticeID {
			t.Fatalf("Decision Notice cursor = %d, %v; want %d", restoredCursor, err, lastNoticeID)
		}
	}
	assertRosterWorks := func() {
		output.Reset()
		errorsOut.Reset()
		cli := service.CLI()
		cli.Out, cli.ErrOut = output, errorsOut
		if code := cli.Run([]string{"config", "show", "--project", "shop"}); code != 0 || !strings.Contains(output.String(), "shop") {
			t.Fatalf("posse config show after rebuild failed: code=%d out=%s err=%s", code, output, errorsOut)
		}
	}
	assertRestored()
	assertRosterWorks()

	if err := os.Remove(filepath.Join(home, "posse.db")); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	errorsOut.Reset()
	cli = service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	if code := cli.Run([]string{"recover", "--rebuild"}); code != 0 {
		t.Fatalf("recover --rebuild after deleting posse.db failed: code=%d out=%s err=%s", code, output, errorsOut)
	}
	assertRestored()
	assertRosterWorks()
}

func TestRecoverRebuildFailureKeepsBeforeRebuildBackup(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "scout", Title: "Original state", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	snapshotPath := filepath.Join(home, "projects", project.Name, "tasks", "t1", "task.toml")
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot store.TaskSnapshot
	if _, err := toml.Decode(string(data), &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.LaunchIdentities = []store.TaskLaunchIdentity{{TaskID: taskID, LaunchNumber: 1}}
	var damaged bytes.Buffer
	if err := toml.NewEncoder(&damaged).Encode(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotPath, damaged.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	service := testService(home, herdr.NewFake())
	output, errorsOut := &bytes.Buffer{}, &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	t.Setenv("HERDR_PANE_ID", "")
	t.Chdir(repo)
	if code := cli.Run([]string{"recover", "--rebuild"}); code == 0 || !strings.Contains(output.String(), "invalid launch number") {
		t.Fatalf("damaged rebuild result: code=%d out=%s err=%s", code, output, errorsOut)
	}

	backups, err := filepath.Glob(filepath.Join(home, "backup", "posse-before-rebuild-*.db"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("pre-rebuild backup missing after interruption: %v, %v", backups, err)
	}
	backupHome := filepath.Join(root, "backup-check")
	if err := os.MkdirAll(backupHome, 0o700); err != nil {
		t.Fatal(err)
	}
	backupData, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupHome, "posse.db"), backupData, 0o600); err != nil {
		t.Fatal(err)
	}
	backupDB, err := store.OpenReadOnly(backupHome)
	if err != nil {
		t.Fatalf("pre-rebuild backup is not a readable database: %v", err)
	}
	backupTask, err := backupDB.Task(ctx, project.ID, "t1")
	if closeErr := backupDB.Close(); err == nil {
		err = closeErr
	}
	if err != nil || backupTask.Title != "Original state" {
		t.Fatalf("pre-rebuild backup lost its original Task: %#v, %v", backupTask, err)
	}
	untouched, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer untouched.Close()
	original, err := untouched.Task(ctx, project.ID, "t1")
	if err != nil || original.Title != "Original state" {
		t.Fatalf("failed rebuild changed the original database: %#v, %v", original, err)
	}
}

func TestRestartLeadPromptsWhenProfileArgsExistWithoutSystemPromptArg(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	configText := "[lead]\nkind = \"claude\"\n\n[lead.profiles]\nclaude = \"lead-profile\"\n\n[kinds.claude]\nsystem_prompt_args = []\n\n[profiles.lead-profile]\nkind = \"claude\"\nargs = [\"--profile-arg\"]\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err == nil {
		err = db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead")
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	project.HerdrWorkspaceID = "w1"
	project.LeadPaneID = "w1:p1"
	project.LeadLabel = "posse:shop:lead"
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: project.LeadLabel, Agent: "claude", AgentStatus: "working"}}}
	fake.Results["agent.get"] = []byte(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	service := testService(home, shellAfterInterruptAdapter{Fake: fake})
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := service.restartLead(ctx, db, home, project, cfg, fake.SnapshotValue); err != nil {
		t.Fatal(err)
	}
	found, restartedWithBypass := false, false
	for _, call := range fake.Calls {
		if call.Method == "agent.prompt" && call.Params["target"] == "w1:p1" && strings.Contains(call.Params["text"].(string), "posse lead") {
			found = true
		}
		if call.Method == "agent.start" {
			args := call.Params["args"].([]string)
			restartedWithBypass = len(args) >= 1 && args[0] == "--dangerously-skip-permissions"
		}
	}
	if !found {
		t.Fatalf("Profile args suppressed restart Lead instructions: %#v", fake.Calls)
	}
	if !restartedWithBypass {
		t.Fatalf("restarted Claude Lead omitted its default bypass argument: %#v", fake.Calls)
	}
	assertLeadMetadataCall(t, fake, "w1:p1", "claude")
	// Lookout failure must not strand the newly launched prompt-kind Lead.
	fake.Errors["tab.create"] = errors.New("Lookout unavailable")
	before := fake.CallCount("agent.prompt")
	if err := service.restartLead(ctx, db, home, project, cfg, fake.SnapshotValue); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("agent.prompt") <= before {
		t.Fatal("Lookout failure suppressed the Lead launch prompt")
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, notice := range notices {
		if notice.Kind == "pr_watch_failing" && strings.Contains(notice.Summary, "Lookout restart failed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Lookout failure produced no Notice: %#v", notices)
	}
}

func TestRestartLeadUsesDefaultAutoApproveArgsForCodexAndOpenCode(t *testing.T) {
	for _, kind := range []string{"codex", "opencode"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			initRepo(t, repo)
			home := filepath.Join(root, "posse")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			configText := "[lead]\nkind = \"" + kind + "\"\n"
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			project, err := db.CreateProject(ctx, "shop", repo, "main")
			if err == nil {
				err = db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead")
			}
			if closeErr := db.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				t.Fatal(err)
			}
			project.HerdrWorkspaceID = "w1"
			project.LeadPaneID = "w1:p1"
			project.LeadLabel = "posse:shop:lead"
			fake := herdr.NewFake()
			fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: project.LeadLabel, Agent: kind, AgentStatus: "working"}}}
			fake.Results["agent.get"] = []byte(`{"agent":{"agent_status":"working","launch_pending":true}}`)
			service := testService(home, shellAfterInterruptAdapter{Fake: fake})
			cfg, err := config.Load(home, project.Name)
			if err != nil {
				t.Fatal(err)
			}
			db, err = store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := service.restartLead(ctx, db, home, project, cfg, fake.SnapshotValue); err != nil {
				t.Fatal(err)
			}

			var args []string
			for _, call := range fake.Calls {
				if call.Method == "agent.start" {
					args = call.Params["args"].([]string)
					break
				}
			}
			if len(args) == 0 {
				t.Fatalf("restart did not start a %s Lead: %#v", kind, fake.Calls)
			}
			switch kind {
			case "codex":
				if len(args) < 4 || !equalStrings(args[:3], []string{"--dangerously-bypass-approvals-and-sandbox", "--sandbox", "danger-full-access"}) || args[len(args)-1] != codexOpeningPrompt {
					t.Fatalf("restarted Codex Lead argv = %#v", args)
				}
			case "opencode":
				if !equalStrings(args, []string{"--auto", "--prompt", codexOpeningPrompt}) {
					t.Fatalf("restarted OpenCode Lead argv = %#v", args)
				}
			}
		})
	}
}

func TestOpenCodeRestartExportsInlinePluginOnlyToLeadShell(t *testing.T) {
	fake := herdr.NewFake()
	service := testService(t.TempDir(), fake)
	value := `{"plugin":["file:///tmp/lead's-plugin.js"]}`
	if err := service.exportLeadEnvironment(context.Background(), "w1:p1", map[string]string{"OPENCODE_CONFIG_CONTENT": value}); err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls) != 1 || fake.Calls[0].Method != "pane.send_input" || fake.Calls[0].Params["pane_id"] != "w1:p1" || !strings.Contains(fake.Calls[0].Params["text"].(string), `lead'"'"'s-plugin`) {
		t.Fatalf("Lead shell export = %#v", fake.Calls)
	}
}

func TestStopLeadAgentUsesHarnessExitCommandAfterInterrupt(t *testing.T) {
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", Agent: "pi", AgentStatus: "working"}}}
	adapter := &promptExitAdapter{Fake: fake}
	service := testService(t.TempDir(), adapter)
	if err := service.stopLeadAgent(context.Background(), "w1:p1", "pi"); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("agent.send_keys") != 2 || fake.CallCount("agent.prompt") != 1 {
		t.Fatalf("agent exit did not interrupt, request a harness exit, and wait for shell: %#v", fake.Calls)
	}
	for _, call := range fake.Calls {
		if call.Method == "agent.prompt" && (call.Params["target"] != "w1:p1" || call.Params["text"] != "/quit") {
			t.Fatalf("Pi exit request = %#v", call)
		}
	}
}

type promptExitAdapter struct {
	*herdr.Fake
	exited bool
}

func (a *promptExitAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	result, err := a.Fake.Call(ctx, method, params)
	if method == "agent.prompt" {
		a.exited = true
	}
	return result, err
}

func (a *promptExitAdapter) Snapshot(ctx context.Context) (herdr.Snapshot, error) {
	snapshot, err := a.Fake.Snapshot(ctx)
	if err != nil || !a.exited {
		return snapshot, err
	}
	for index := range snapshot.Panes {
		snapshot.Panes[index].Agent = ""
		snapshot.Panes[index].AgentStatus = "unknown"
	}
	return snapshot, nil
}

type shellAfterInterruptAdapter struct{ *herdr.Fake }

func (a shellAfterInterruptAdapter) Snapshot(context.Context) (herdr.Snapshot, error) {
	snapshot := a.SnapshotValue
	for index := range snapshot.Panes {
		snapshot.Panes[index].Agent = ""
		snapshot.Panes[index].AgentStatus = "unknown"
	}
	return snapshot, nil
}

func TestRecoverAllContinuesAcrossProjectsAndRecordsGeneration(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	alphaRoot, betaRoot := filepath.Join(root, "alpha"), filepath.Join(root, "beta")
	initRepo(t, alphaRoot)
	initRepo(t, betaRoot)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	alpha, err := db.CreateProject(ctx, "alpha", alphaRoot, "main")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := db.CreateProject(ctx, "beta", betaRoot, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RememberProjectServerStartedAt(ctx, alpha.ID, "old-generation"); err != nil {
		t.Fatal(err)
	}
	if err := db.RememberProjectServerStartedAt(ctx, beta.ID, "new-generation"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "projects", "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "projects", "alpha", "config.toml"), []byte("[defaults\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := herdr.NewFake()
	adapter.SnapshotValue.ServerStartedAt = "new-generation"
	service := testService(home, adapter)
	output, errorsOut := &bytes.Buffer{}, &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, errorsOut
	t.Setenv("HERDR_PANE_ID", "")
	t.Chdir(alphaRoot)
	if code := cli.Run([]string{"recover", "--all"}); code != 0 {
		t.Fatalf("recover --all stopped at the first Project error: code=%d out=%s err=%s", code, output, errorsOut)
	}
	if !strings.Contains(output.String(), "attempted: 2") || !strings.Contains(output.String(), "alpha") || !strings.Contains(output.String(), "projects: 1") {
		t.Fatalf("recover --all did not isolate alpha's failure and reconcile beta: %s", output)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, project := range []store.Project{alpha, beta} {
		generation, err := db.ProjectServerStartedAt(ctx, project.ID)
		want := "new-generation"
		if project.ID == alpha.ID {
			want = "old-generation" // Invalid config must leave recovery pending.
		}
		if err != nil || generation != want {
			t.Fatalf("Project %s generation = %q, want %q: %v", project.Name, generation, want, err)
		}
	}
}

func TestFailedRecoveryLeavesGenerationPending(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", filepath.Join(home, "missing-repository"), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RememberProjectServerStartedAt(ctx, project.ID, "old-generation"); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue.ServerStartedAt = "new-generation"
	if _, err := testService(home, fake).recoverProject(ctx, db, home, project); err == nil {
		t.Fatal("recovery succeeded for a missing Project root")
	}
	generation, err := db.ProjectServerStartedAt(ctx, project.ID)
	if err != nil || generation != "old-generation" {
		t.Fatalf("failed recovery consumed restart generation: %q, %v", generation, err)
	}
}

func TestStartupRecoverySurvivesReconcileBeforeHook(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	configText := "[lead]\nkind = \"claude\"\n\n[profiles.deep]\nkind = \"claude\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{
		Seq: 1, Type: "ship", Title: "Recovery", Profile: "deep", LandingMode: "local",
		Branch: "posse/t1", BaseRef: "main", HerdrWorkspaceID: "w1", PaneID: "w1:p2", PaneLabel: "posse:shop:t1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "Worker ready"); err != nil {
		t.Fatal(err)
	}
	mount, err := db.AcquireMount(ctx, project.ID, taskID, filepath.Join(home, "projects", "shop", "remuda"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(mount.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "worktree", "add", "-b", "posse/t1", mount.Path, "main")
	launch, err := db.NextTaskLaunch(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTaskLaunch(ctx, taskID, mount.Path, "w1", "w1:p2", "posse:shop:t1", "posse-shop-t1-"+strconv.Itoa(launch)); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTaskObservation(ctx, taskID, "w1:p2", "w1", "", 0, 0, "old-generation"); err != nil {
		t.Fatal(err)
	}
	if err := db.RememberProjectServerStartedAt(ctx, project.ID, "old-generation"); err != nil {
		t.Fatal(err)
	}
	before, err := db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}

	lead := herdr.Pane{PaneID: "w1:p1", WorkspaceID: "w1", Label: project.LeadLabel, Agent: "claude", AgentStatus: "working"}
	worker := herdr.Pane{PaneID: "w1:p2", WorkspaceID: "w1", Label: before.PaneLabel, CWD: mount.Path, Agent: "claude", AgentStatus: "working"}
	// The plugin event sees the Worker missing in a new generation, recover --all
	// sees it present, and the next snapshot sees it missing again.
	snapshots := []herdr.Snapshot{
		{ServerStartedAt: "new-generation", Workspaces: []herdr.Workspace{{WorkspaceID: "w1", Root: repo}}, Panes: []herdr.Pane{lead}},
		{ServerStartedAt: "new-generation", Workspaces: []herdr.Workspace{{WorkspaceID: "w1", Root: repo}}, Panes: []herdr.Pane{{PaneID: lead.PaneID, WorkspaceID: lead.WorkspaceID, Label: lead.Label}, worker}},
		{ServerStartedAt: "new-generation", Workspaces: []herdr.Workspace{{WorkspaceID: "w1", Root: repo}}, Panes: []herdr.Pane{lead}},
	}
	adapter := &recoveryOrderingAdapter{Fake: herdr.NewFake(), snapshots: snapshots}
	adapter.Results["tab.create"] = json.RawMessage(`{"tab":{"tab_id":"w1:t3","workspace_id":"w1"},"root_pane":{"pane_id":"w1:p3","tab_id":"w1:t3"}}`)
	adapter.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	service := testService(home, adapter)

	if _, err := runtime.Run(ctx, db, adapter, project.ID, 0, 0, time.Now(), nil); err != nil {
		t.Fatalf("event reconciliation before startup hook: %v", err)
	}
	generation, err := db.ProjectServerStartedAt(ctx, project.ID)
	if err != nil || generation != "old-generation" {
		t.Fatalf("event reconciliation consumed the pending Herdr restart: generation=%q err=%v", generation, err)
	}
	observed, err := db.Task(ctx, project.ID, "t1")
	if err != nil || observed.AgentAbsentSince != 0 || observed.AgentServerStartedAt != "old-generation" {
		t.Fatalf("pre-hook reconcile changed an observation before recovery: task=%#v err=%v", observed, err)
	}
	if recovered, err := service.recoverProject(ctx, db, home, project); err != nil {
		t.Fatalf("startup recovery: %v", err)
	} else if recovered < 2 {
		t.Fatalf("startup recovery count = %d, want Worker and Lead recovery", recovered)
	}

	after, err := db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if after.State != store.StateWorking || after.Launches != before.Launches+1 || after.AgentName == before.AgentName || after.MountID != before.MountID || after.WorktreePath != before.WorktreePath || after.Branch != before.Branch {
		t.Fatalf("startup recovery did not relaunch Worker on the same Mount and branch: before=%#v after=%#v", before, after)
	}
	generation, err = db.ProjectServerStartedAt(ctx, project.ID)
	if err != nil || generation != "new-generation" {
		t.Fatalf("recovered Project generation = %q, %v", generation, err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	recoveryNotices := 0
	for _, notice := range notices {
		if notice.Kind == "recovery" && strings.Contains(notice.Summary, "Recovery") {
			recoveryNotices++
		}
	}
	if recoveryNotices != 1 {
		t.Fatalf("recovery digest Notice count = %d, want one: %#v", recoveryNotices, notices)
	}
	if adapter.CallCount("agent.start") != 2 {
		t.Fatalf("recovery started %d agents, want one Worker and one Lead: %#v", adapter.CallCount("agent.start"), adapter.Calls)
	}
}

type recoveryOrderingAdapter struct {
	*herdr.Fake
	snapshots []herdr.Snapshot
	next      int
}

func (adapter *recoveryOrderingAdapter) Snapshot(context.Context) (herdr.Snapshot, error) {
	index := adapter.next
	if index >= len(adapter.snapshots) {
		index = len(adapter.snapshots) - 1
	} else {
		adapter.next++
	}
	return adapter.snapshots[index], nil
}
