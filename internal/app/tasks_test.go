package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWorkerAgentArgsWithoutConfiguredArgumentsMarshalAsEmptyArray(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[profiles.pi]\nkind = \"pi\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	args := workerAgentArgs(cfg.Profiles["pi"], cfg.Kinds["pi"], "")
	if args == nil {
		t.Fatal("Worker args are nil")
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("Worker args JSON = %s, want []", encoded)
	}
}

func TestOpenCodeWorkerArgsAndResume(t *testing.T) {
	cfg, err := config.Load(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	profile := config.Profile{Kind: "opencode", Model: "provider/model", Args: []string{"--mini"}}
	if got := workerAgentArgs(profile, cfg.Kinds["opencode"], ""); !equalStrings(got, []string{"--auto", "--model", "provider/model", "--mini"}) {
		t.Fatalf("OpenCode Worker args = %#v", got)
	}
	if got := workerAgentArgs(profile, cfg.Kinds["opencode"], "ses_123"); !equalStrings(got, []string{"--auto", "--model", "provider/model", "--mini", "--session", "ses_123"}) {
		t.Fatalf("OpenCode resume args = %#v", got)
	}
}

func TestWorkerDisplayUsesSidebarFieldsWithoutChangingCanonicalIdentity(t *testing.T) {
	task := store.Task{Seq: 12, Title: "Stable output for workers", ShortName: "stable-output", Branch: "posse/stable-output", WorktreePath: "/tmp/remuda/shop/mount-2", PaneID: "w2:p1", PaneLabel: "posse:shop:t12"}
	if got := workerTabLabel(task); got != "stable-output" {
		t.Fatalf("Worker tab label = %q", got)
	}
	metadata := workerDisplayMetadata(task, "claude")
	if metadata["title"] != "Stable output for workers · stable-output · /tmp/remuda/shop/mount-2" || metadata["clear_display_agent"] != nil || metadata["display_agent"] != "claude" || metadata["pane_id"] != task.PaneID {
		t.Fatalf("Worker display metadata = %#v", metadata)
	}
	if got := taskPaneLabel.FindString(task.PaneLabel); got != task.PaneLabel {
		t.Fatalf("canonical pane label changed = %q", got)
	}
}

func TestWorkerDisplayMountContextDoesNotTruncateMidPath(t *testing.T) {
	mount := filepath.Join("/tmp", strings.Repeat("ci-run-", 18), "posse", "remuda", "shop", "mount-3")
	task := store.Task{Seq: 3, Title: "Show Worker worktrees", ShortName: "show-worktrees", Branch: "posse/show-worktrees", WorktreePath: mount, PaneID: "w2:p1"}
	metadata := workerDisplayMetadata(task, "codex")
	if metadata["clear_display_agent"] != nil || metadata["display_agent"] != "codex" {
		t.Fatalf("Worker harness subtitle does not match the detected agent: %#v", metadata)
	}
	tokens := metadata["tokens"].(map[string]string)
	if got := tokens["posse_mount"]; got != "mount-3" {
		t.Fatalf("Mount token = %q, want stable basename rather than Herdr's truncated path", got)
	}
	if got := metadata["title"].(string); !strings.Contains(got, mount) {
		t.Fatalf("full Mount path was not submitted as pane title: %q", got)
	}
}

func TestTaskTitleSlugNamesTheWork(t *testing.T) {
	for title, want := range map[string]string{
		"Hide tool calls in the Pi Lead while lowkey is on": "hide-tool-calls",
		"Span backend and worker":                           "span-backend-worker",
		"Fix the flaky login test":                          "fix-flaky-login",
	} {
		if got := taskTitleSlug(title); got != want {
			t.Errorf("slug for %q = %q, want %q", title, got, want)
		}
	}
}

func TestRideRequiresAValidWorkerName(t *testing.T) {
	for _, name := range []string{"Worker-tree", "worker_tree", "worker--tree", "-worker", "worker-", "t36", "worker-name-that-is-too-long-for-sidebar"} {
		t.Run(name, func(t *testing.T) {
			output := &bytes.Buffer{}
			cli := testService(t.TempDir(), nil).CLI()
			cli.Out, cli.ErrOut = output, output
			if code := cli.Run([]string{"ride", "--brief", "missing.md", "--name", name}); code != 1 || !strings.Contains(output.String(), "name_invalid") || !strings.Contains(output.String(), "invalid Rider name") {
				t.Fatalf("invalid Rider name exit=%d output=%s", code, output.String())
			}
		})
	}
	output := &bytes.Buffer{}
	cli := testService(t.TempDir(), nil).CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"ride", "--brief", "missing.md"}); code != 2 || !strings.Contains(output.String(), "--name <short>") {
		t.Fatalf("missing Worker name exit=%d output=%s", code, output.String())
	}
}

func TestRideReportsMountSetupFailureAndReleasesMount(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(filepath.Join(home, "projects", "shop"), 0o700); err != nil {
		t.Fatal(err)
	}
	globalConfig := "[defaults]\nlanding_mode = \"local\"\n\n[profiles.fast]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"fast\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(globalConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "projects", "shop", "config.toml"), []byte("[remuda]\nsetup = [\"false\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	briefPath := filepath.Join(root, "brief.md")
	if err := os.WriteFile(briefPath, []byte("---\ntype: ship\ntitle: Setup failure\ndone_when: setup passes\n---\nCreate a committed file.\n"), 0o600); err != nil {
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
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"}}}
	service := testService(home, fake)
	output := &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, output
	t.Chdir(repo)
	if code := cli.Run([]string{"ride", "--brief", briefPath, "--name", "setup-failure"}); code != 1 || !strings.Contains(output.String(), "mount_setup_failed") {
		t.Fatalf("ride setup failure code=%d output=%s", code, output.String())
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil || len(tasks) != 1 || tasks[0].State != store.StateFailed || tasks[0].MountID != 0 || tasks[0].ShortName != "setup-failure" {
		t.Fatalf("failed setup Task = %#v, %v", tasks, err)
	}
	output.Reset()
	if code := cli.Run([]string{"ride", "--brief", briefPath, "--name", "opaque"}); code != 1 || !strings.Contains(output.String(), "name_invalid") || !strings.Contains(output.String(), "Task title") {
		t.Fatalf("unrelated name exit=%d output=%s", code, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"ride", "--brief", briefPath, "--name", "setup-failure"}); code != 1 || !strings.Contains(output.String(), "branch_exists") || !strings.Contains(output.String(), "Rewrite the Task title") {
		t.Fatalf("reused name exit=%d output=%s", code, output.String())
	}
	if tasks, err := db.Tasks(ctx, project.ID, true); err != nil || len(tasks) != 1 {
		t.Fatalf("collision created a Task: %#v, %v", tasks, err)
	}
	mounts, err := db.Mounts(ctx, project.ID)
	if err != nil || len(mounts) != 1 || mounts[0].State != "idle" || mounts[0].TaskID != 0 {
		t.Fatalf("Mount after setup failure = %#v, %v", mounts, err)
	}
}

func TestRelaunchResumesWithFullProfileArgumentsInSameMount(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	mount := filepath.Join(root, "mount")
	gitTest(t, repo, "worktree", "add", "-b", "posse/t1", mount, "main")
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(filepath.Join(home, "projects", "shop", "tasks", "t1"), 0o700); err != nil {
		t.Fatal(err)
	}
	configText := "[kinds.claude]\nauto_approve_args = [\"--yes\"]\nmodel_args = [\"--model\", \"{model}\"]\neffort_args = [\"--effort\", \"{effort}\"]\nresume_args = [\"--resume\", \"{session}\"]\n\n[profiles.deep]\nkind = \"claude\"\nmodel = \"opus\"\neffort = \"high\"\nargs = [\"--profile-arg\"]\n\n[profiles.fast]\nkind = \"claude\"\nmodel = \"sonnet\"\neffort = \"low\"\nargs = [\"--fast-arg\"]\n\n[profiles.codex-fast]\nkind = \"codex\"\nmodel = \"gpt-5\"\neffort = \"high\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "projects", "shop", "tasks", "t1", "launch.md"), []byte("worker instructions\n"), 0o600); err != nil {
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
	if err := db.SetProjectLead(ctx, project.ID, "w3", "w3:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project.HerdrWorkspaceID, project.LeadPaneID, project.LeadLabel = "w3", "w3:p1", "posse:shop:lead"
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Resume", ShortName: "worker-tree", Profile: "deep", LandingMode: "local", Branch: "posse/t1", BaseRef: "main", WorktreePath: mount, HerdrWorkspaceID: "w2", PaneID: "w2:p1", PaneLabel: "posse:shop:t1", AgentSession: `{"session_id":"session-9"}`})
	if err != nil {
		t.Fatal(err)
	}
	created, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []struct {
		body      string
		createdAt int64
		status    string
	}{
		{body: "before the Brief", createdAt: created.CreatedAt - 1, status: "delivered"},
		{body: "first instruction", createdAt: created.CreatedAt + 1, status: "delivered"},
		{body: "queued instruction", createdAt: created.CreatedAt + 2, status: "queued"},
		{body: "later instruction supersedes the first", createdAt: created.CreatedAt + 3, status: "delivered"},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO messages(task_id,body,created_at,delivered_at,status,wait_for_idle) VALUES(?,?,?,?,?,0)`, taskID, message.body, message.createdAt, message.createdAt, message.status); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateWorking, store.StateLost, "cli", "lost"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, project.ID, mount, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, taskID, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	gitDir := strings.TrimSpace(gitTest(t, mount, "rev-parse", "--git-dir"))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(mount, gitDir)
	}
	indexLock := filepath.Join(gitDir, "index.lock")
	if err := os.WriteFile(indexLock, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	lead := herdr.Pane{PaneID: "w3:p1", WorkspaceID: "w3", TabID: "w3:t0", Label: "posse:shop:lead"}
	fake := herdr.NewFake()
	fake.SnapshotValue.Panes = []herdr.Pane{lead}
	fake.Results["tab.create"] = json.RawMessage(`{"tab":{"tab_id":"w3:t1","workspace_id":"w3"},"root_pane":{"pane_id":"w3:p2","tab_id":"w3:t1"}}`)
	fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	fake.BeforeCall = func(method string) {
		if method == "agent.start" {
			kind := "claude"
			if fake.CallCount("agent.start") > 0 {
				kind = "codex"
			}
			fake.SnapshotValue.Panes = []herdr.Pane{lead, {PaneID: "w3:p2", WorkspaceID: "w3", TabID: "w3:t1", Label: "posse:shop:t1", Agent: kind}}
		}
	}
	service := testService(home, fake)
	t.Chdir(repo)
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out = &output
	if code := cli.Run([]string{"relaunch", "t1", "--profile="}); code != 2 || !strings.Contains(output.String(), "non-empty Profile name") {
		t.Fatalf("relaunch accepted an empty Profile: code=%d output=%s", code, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"relaunch", "t1", "--profile", "missing"}); code != 1 || !strings.Contains(output.String(), "profile_unknown") {
		t.Fatalf("relaunch accepted an unknown Profile: code=%d output=%s", code, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"relaunch", "t1", "--profile", "fast"}); code != 0 || !strings.Contains(output.String(), "working") {
		t.Fatalf("relaunch failed: code=%d output=%s", code, output.String())
	}
	if _, err := os.Stat(indexLock); !os.IsNotExist(err) {
		t.Fatalf("stale Git index lock was not removed: %v", err)
	}
	started := false
	for _, call := range fake.Calls {
		if call.Method != "agent.start" {
			continue
		}
		started = call.Params["kind"] == "claude" && call.Params["pane_id"] == "w3:p2" && equalStrings(call.Params["args"].([]string), []string{"--yes", "--model", "sonnet", "--effort", "low", "--fast-arg", "--resume", "session-9"})
	}
	if !started {
		t.Fatalf("relaunch omitted the complete resume args: %#v", fake.Calls)
	}
	workspaceRenamed, tabRenamed, metadataReported := false, false, false
	startedIndex, metadataIndex := -1, -1
	for index, call := range fake.Calls {
		switch call.Method {
		case "workspace.rename":
			workspaceRenamed = true
		case "tab.rename":
			tabRenamed = call.Params["tab_id"] == "w3:t1" && call.Params["label"] == "worker-tree"
		case "agent.start":
			startedIndex = index
		case "pane.report_metadata":
			metadataIndex = index
			metadataReported = call.Params["clear_display_agent"] == nil && call.Params["display_agent"] == "claude" && strings.Contains(call.Params["title"].(string), mount)
		}
	}
	if workspaceRenamed || !tabRenamed || !metadataReported || startedIndex < 0 || metadataIndex <= startedIndex {
		t.Fatalf("relaunch renamed a workspace or missed a Worker name: workspace renamed=%v tab=%v metadata=%v calls=%#v", workspaceRenamed, tabRenamed, metadataReported, fake.Calls)
	}
	relaunchText, err := os.ReadFile(filepath.Join(home, "projects", "shop", "tasks", "t1", "relaunch.md"))
	if err != nil || !strings.Contains(string(relaunchText), "Re-read `launch.md`") || !strings.Contains(string(relaunchText), "posse holler") || !strings.Contains(string(relaunchText), "first instruction") || !strings.Contains(string(relaunchText), "later instruction supersedes the first") {
		t.Fatalf("relaunch instructions were not written: %q, %v", relaunchText, err)
	}
	if strings.Index(string(relaunchText), "first instruction") > strings.Index(string(relaunchText), "later instruction supersedes the first") || strings.Contains(string(relaunchText), "before the Brief") || strings.Contains(string(relaunchText), "queued instruction") || !strings.Contains(string(relaunchText), "Later messages supersede earlier ones") {
		t.Fatalf("relaunch Lead messages were not ordered and filtered correctly: %q", relaunchText)
	}
	observer, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	updated, err := observer.Task(ctx, project.ID, "t1")
	if err != nil || updated.State != store.StateWorking || updated.WorktreePath != mount || updated.HerdrWorkspaceID != "w3" || updated.PaneID != "w3:p2" || updated.Profile != "fast" || updated.DispatchRule != "relaunch --profile fast" {
		t.Fatalf("relaunch changed Task Mount/state incorrectly: %#v, %v", updated, err)
	}
	transitions, err := observer.TaskTransitions(ctx, taskID, 10)
	profileChangeRecorded := false
	for _, transition := range transitions {
		if transition.Note == "Profile changed from deep to fast" && transition.From == transition.To {
			profileChangeRecorded = true
		}
	}
	if err != nil || !profileChangeRecorded {
		t.Fatalf("Profile change history = %#v, %v", transitions, err)
	}
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex-home"))
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.relaunchTask(ctx, observer, home, project, cfg, updated, "codex-fast"); err != nil {
		t.Fatalf("relaunch with a different harness failed: %v", err)
	}
	freshStart := false
	for _, call := range fake.Calls {
		if call.Method == "agent.start" && call.Params["kind"] == "codex" {
			freshStart = equalStrings(call.Params["args"].([]string), []string{"--dangerously-bypass-approvals-and-sandbox", "-m", "gpt-5", "-c", "model_reasoning_effort=high"})
		}
	}
	if !freshStart {
		t.Fatalf("kind-changing relaunch resumed the old session or omitted Profile args: %#v", fake.Calls)
	}
	codexSubtitle := false
	for _, call := range fake.Calls[metadataIndex+1:] {
		if call.Method == "pane.report_metadata" && call.Params["display_agent"] == "codex" {
			codexSubtitle = true
		}
	}
	if !codexSubtitle {
		t.Fatalf("kind-changing relaunch kept the old harness subtitle: %#v", fake.Calls)
	}
	codexConfig, err := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"))
	if err != nil || !strings.Contains(string(codexConfig), `trust_level = "trusted"`) || !strings.Contains(string(codexConfig), mount) {
		t.Fatalf("Codex Mount preparation was not run: %q, %v", codexConfig, err)
	}
	updated, err = observer.Task(ctx, project.ID, "t1")
	if err != nil || updated.Profile != "codex-fast" || updated.DispatchRule != "relaunch --profile codex-fast" {
		t.Fatalf("kind-changing relaunch did not update the Task Profile: %#v, %v", updated, err)
	}
	transitions, err = observer.TaskTransitions(ctx, taskID, 10)
	foundProfileChanges := map[string]bool{}
	for _, transition := range transitions {
		foundProfileChanges[transition.Note] = true
	}
	if err != nil || !foundProfileChanges["Profile changed from deep to fast"] || !foundProfileChanges["Profile changed from fast to codex-fast"] {
		t.Fatalf("Profile history lost a change: %#v, %v", transitions, err)
	}
	if err := observer.StartIntent(ctx, project.ID, taskID, "relaunch", "done:agent.start", `{}`, deadIntentProcessID); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(home, project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.reconcileIntents(ctx, observer, project, cfg, fake.SnapshotValue); err != nil {
		t.Fatalf("interrupted relaunch did not recover: %v", err)
	}
	updated, err = observer.Task(ctx, project.ID, "t1")
	if err != nil || updated.State != store.StateWorking || updated.WorktreePath != mount || updated.Launches != 3 {
		t.Fatalf("recovered relaunch changed Task Mount/state incorrectly: %#v, %v", updated, err)
	}
	if _, err := observer.IntentByTask(ctx, taskID); !store.IsNotFound(err) {
		t.Fatalf("relaunch intent remains after recovery: %v", err)
	}
}

func TestRelaunchInstructionsBoundDeliveredLeadMessages(t *testing.T) {
	messages := make([]store.Message, maxRelaunchMessages+2)
	for index := range messages {
		messages[index] = store.Message{Body: fmt.Sprintf("message-%d: %s", index, strings.Repeat("x", maxRelaunchMessageBytes+20))}
	}
	instructions := string(relaunchInstructions("", messages))
	if len(instructions) > maxRelaunchMessages*(maxRelaunchMessageBytes+64)+2048 {
		t.Fatalf("relaunch prompt exceeded its message bound: %d bytes", len(instructions))
	}
	if !strings.Contains(instructions, "Earlier delivered Lead messages are omitted") || !strings.Contains(instructions, "message-2") || !strings.Contains(instructions, "message-33") || strings.Contains(instructions, "message-0") || !strings.Contains(instructions, "[truncated]") {
		t.Fatalf("relaunch prompt did not apply message bounds: %q", instructions)
	}
}

func TestRelaunchReportsInProgressGitStateWithoutChangingIt(t *testing.T) {
	for _, operation := range []string{"merge", "rebase", "git-process"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			initRepo(t, repo)
			mountPath := filepath.Join(root, "mount")
			gitTest(t, repo, "worktree", "add", "-b", "posse/t1", mountPath, "main")
			gitDirText := strings.TrimSpace(gitTest(t, mountPath, "rev-parse", "--git-dir"))
			gitDir := gitDirText
			if !filepath.IsAbs(gitDir) {
				gitDir = filepath.Join(mountPath, gitDir)
			}
			var marker string
			var process *exec.Cmd
			switch operation {
			case "merge":
				marker = filepath.Join(gitDir, "MERGE_HEAD")
				if err := os.WriteFile(marker, []byte(strings.TrimSpace(gitTest(t, repo, "rev-parse", "main"))+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "rebase":
				marker = filepath.Join(gitDir, "rebase-merge")
				if err := os.Mkdir(marker, 0o700); err != nil {
					t.Fatal(err)
				}
			case "git-process":
				marker = filepath.Join(gitDir, "index.lock")
				if err := os.WriteFile(marker, []byte("locked"), 0o600); err != nil {
					t.Fatal(err)
				}
				process = exec.Command("bash", "-c", "exec -a git sleep 30")
				process.Dir = mountPath
				if err := process.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
				deadline := time.Now().Add(2 * time.Second)
				for !processInMount(process.Process.Pid, mountPath) && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
			}
			warning, err := inspectMountGitState(context.Background(), mountPath)
			if err != nil {
				t.Fatalf("relaunch refused an in-progress Git operation: %v", err)
			}
			if warning == "" || !strings.Contains(string(relaunchInstructions(warning)), warning) {
				t.Fatalf("relaunch report omitted the in-progress Git state warning: %q", warning)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("relaunch changed in-progress Git state %s: %v", marker, err)
			}
		})
	}
}

type relaunchFixture struct {
	db      *store.DB
	fake    *herdr.Fake
	service *Service
	home    string
	project store.Project
	cfg     config.Config
	task    store.Task
}

func newRelaunchFixture(t *testing.T, initial store.State) relaunchFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	mountPath := filepath.Join(root, "mount")
	gitTest(t, repo, "worktree", "add", "-b", "posse/t1", mountPath, "main")
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(filepath.Join(home, "projects", "shop", "tasks", "t1"), 0o700); err != nil {
		t.Fatal(err)
	}
	configText := "[kinds.claude]\n\n[profiles.deep]\nkind = \"claude\"\nmodel = \"sonnet\"\n\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Resume attention state", Profile: "deep", LandingMode: "local", Branch: "posse/t1", BaseRef: "main", WorktreePath: mountPath, HerdrWorkspaceID: "w1", PaneID: "w1:p1", PaneLabel: "posse:shop:t1", AgentName: "posse-shop-t1-1"})
	if err == nil {
		err = db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started")
	}
	if err == nil {
		_, err = db.ExecContext(ctx, `UPDATE tasks SET state=? WHERE id=?`, string(initial), taskID)
	}
	if err == nil {
		_, err = db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, project.ID, mountPath, taskID)
	}
	if err == nil {
		_, err = db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, taskID, taskID)
	}
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "working"}}}
	fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	service := testService(home, fake)
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	return relaunchFixture{db: db, fake: fake, service: service, home: home, project: project, cfg: cfg, task: task}
}

func TestRelaunchRecoversNeedsDecisionAndBlockedWorkers(t *testing.T) {
	for _, initial := range []store.State{store.StateNeedsDecision, store.StateBlocked} {
		t.Run(string(initial), func(t *testing.T) {
			ctx := context.Background()
			fixture := newRelaunchFixture(t, initial)
			if _, err := fixture.service.relaunchTask(ctx, fixture.db, fixture.home, fixture.project, fixture.cfg, fixture.task, ""); err != nil {
				t.Fatalf("relaunch %s Worker: %v", initial, err)
			}
			updated, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
			if err != nil || updated.State != store.StateWorking || updated.MountID == 0 || updated.Launches != 1 {
				t.Fatalf("relaunch lost Task state or Mount: %#v, %v", updated, err)
			}
		})
	}
}

func TestRelaunchRecoversTaskMarkedLostWhileRelaunching(t *testing.T) {
	ctx := context.Background()
	fixture := newRelaunchFixture(t, store.StateWorking)
	marked := false
	fixture.fake.BeforeCall = func(method string) {
		if method != "agent.prompt" || marked {
			return
		}
		marked = true
		if err := fixture.db.Transition(ctx, fixture.task.ID, store.StateWorking, store.StateLost, "cli", "Worker pane and label are absent from the Herdr snapshot"); err != nil {
			t.Errorf("mark Task lost during relaunch: %v", err)
		}
	}
	if _, err := fixture.service.relaunchTask(ctx, fixture.db, fixture.home, fixture.project, fixture.cfg, fixture.task, ""); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	updated, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil || !marked || updated.State != store.StateWorking {
		t.Fatalf("relaunch left a Task marked lost mid-relaunch in %q (marked=%v, err=%v)", updated.State, marked, err)
	}
}

func TestTaskSequenceDoesNotDependOnLegacyBranch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.test"}} {
		if _, err := gitOutput(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "base"}, {"checkout", "-b", "posse/t1"}} {
		if _, err := gitOutput(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "unlanded.txt"), []byte("keep this commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, repo, "add", "unlanded.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, repo, "commit", "-m", "unlanded task work"); err != nil {
		t.Fatal(err)
	}
	wantCommit, err := gitOutput(ctx, repo, "rev-parse", "refs/heads/posse/t1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, repo, "checkout", "main"); err != nil {
		t.Fatal(err)
	}

	home := filepath.Join(root, ".posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	id, sequence, err := createTaskWithSequence(ctx, db, project, home, store.Task{Type: "ship", Title: "New work", ShortName: "new-work", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if sequence != 1 {
		t.Fatalf("allocated Task sequence = %d, want 1", sequence)
	}
	task, err := db.TaskByID(ctx, project.ID, id)
	if err != nil || task.Seq != 1 || task.Branch != "posse/new-work" {
		t.Fatalf("new Task = %#v, %v", task, err)
	}
	gotCommit, err := gitOutput(ctx, repo, "rev-parse", "refs/heads/posse/t1")
	if err != nil || gotCommit != wantCommit {
		t.Fatalf("stale posse/t1 moved from %s to %s: %v", wantCommit, gotCommit, err)
	}
}

func TestTaskBranchAvailabilityChecksLocalAndOrigin(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	db, err := store.Open(filepath.Join(root, "posse"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "branch", "posse/used-name")
	if err := taskBranchAvailable(ctx, db, project, "used-name"); err == nil || !strings.Contains(err.Error(), "posse/used-name") {
		t.Fatalf("local branch collision = %v", err)
	}
	remote := filepath.Join(root, "origin.git")
	gitTest(t, root, "init", "--bare", remote)
	gitTest(t, repo, "remote", "add", "origin", remote)
	gitTest(t, repo, "push", "origin", "refs/heads/main:refs/heads/posse/remote-name")
	if err := taskBranchAvailable(ctx, db, project, "remote-name"); err == nil || !strings.Contains(err.Error(), "posse/remote-name") {
		t.Fatalf("origin branch collision = %v", err)
	}
}

func TestFailSpawnDeliversTaskFailedNoticeToIdleLead(t *testing.T) {
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
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Broken spawn", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: project.LeadLabel, Agent: "claude", AgentStatus: "idle"}}}
	service := testService(home, fake)
	if err := service.failSpawn(ctx, db, project, taskID, "Broken spawn", "Mount setup failed"); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("agent.prompt") != 1 {
		t.Fatalf("task_failed Notice was not delivered after spawn failure: %#v", fake.Calls)
	}
	task, err := db.Task(ctx, project.ID, "t1")
	if err != nil || task.State != store.StateFailed {
		t.Fatalf("spawn Task state = %#v, %v", task, err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "task_failed" || notices[0].DeliveredAt == 0 {
		t.Fatalf("spawn failure Notice = %#v, %v", notices, err)
	}
}
