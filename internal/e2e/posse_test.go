//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	posseRuntime "github.com/thanhbinh1905/posse/internal/runtime"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestPosseSpawnNoticeLandTeardownAndRecovery(t *testing.T) {
	root := newFixtureRoot(t, herdr.TestRootName())
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	repoRoot := moduleRoot(t)
	posseBinary := filepath.Join(binDir, "posse")
	build := exec.Command("go", "build", "-o", posseBinary, "./cmd/posse")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse: %v\n%s", err, output)
	}
	stableBinaryDir := newFixtureRootAt(t, "/var/tmp", fixturePrefix("installed-"))
	stableBinary := filepath.Join(stableBinaryDir, "posse")
	binaryContents, err := os.ReadFile(posseBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stableBinary, binaryContents, 0o700); err != nil {
		t.Fatal(err)
	}
	worktrees := filepath.Join(root, "posse", "remuda")
	if err := os.MkdirAll(filepath.Join(root, "worker-signal-gates"), 0o700); err != nil {
		t.Fatal(err)
	}
	leadLog := filepath.Join(root, "lead-prompts.log")
	leadArgsLog := filepath.Join(root, "lead-args.log")
	workerLog := filepath.Join(root, "worker.log")
	workerArgsLog := filepath.Join(root, "worker-args.log")
	sleepPIDs := filepath.Join(root, "worker-sleep-pids")
	signalGateDir := filepath.Join(root, "worker-signal-gates")
	launchRead := filepath.Join(root, "worker-launch-delivered.md")
	claudeBinary := filepath.Join(binDir, "claude")
	fakeAgent := `#!/bin/sh
	case "$PWD/" in
	  "$POSSE_E2E_WORKTREES/"*)
	    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
	    IFS= read -r prompt || exit 0
	    launch_path=${prompt#Read }
	    launch_path=${launch_path% and follow it.}
	    task_id=$(basename "$(dirname "$launch_path")")
	    cat "$launch_path" > "$POSSE_E2E_LAUNCH_LOG"
	    printf 'start %s\n' "$*" >> "$POSSE_E2E_WORKER_ARGS_LOG"
	    sleep 300 >/dev/null 2>&1 &
	    echo $! >> "$POSSE_E2E_SLEEP_PIDS"
	    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1 &
    printf 'worker change %s\n' "$task_id" > "e2e-worker-$task_id.txt"
	    git add "e2e-worker-$task_id.txt" || { echo 'git add failed' >> "$POSSE_E2E_WORKER_LOG"; exit 0; }
	    # A relaunched Worker finds its change already committed and keeps running like a real agent.
	    git diff --cached --quiet || git commit -m "worker change $task_id" >> "$POSSE_E2E_WORKER_LOG" 2>&1 || { echo 'git commit failed' >> "$POSSE_E2E_WORKER_LOG"; exit 0; }
	    while [ ! -e "$POSSE_E2E_SIGNAL_GATE/$task_id" ]; do sleep 0.05; done
	    posse holler done 'E2E worker committed the change' >> "$POSSE_E2E_WORKER_LOG" 2>&1 || { echo 'posse holler failed' >> "$POSSE_E2E_WORKER_LOG"; exit 0; }
	    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1 &
	    while IFS= read -r line; do
	      printf '%s\n' "$line" >> "$POSSE_E2E_WORKER_LOG"
	      case "$line" in *'Queue after the current turn'*) posse holler done 'E2E worker committed the change' >> "$POSSE_E2E_WORKER_LOG" 2>&1 ;; esac
	    done
	    ;;
	  *)
	    printf 'start %s\n' "$*" >> "$POSSE_E2E_LEAD_ARGS_LOG"
	    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1 || exit 1
	    : > "$POSSE_TEST_ROOT/lead-initial-idle-reported"
	    while IFS= read -r line; do
	      if [ "$line" = '__posse_e2e_busy__' ]; then
	        herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1 || exit 1
	        : > "$POSSE_TEST_ROOT/lead-busy-reported"
	        continue
	      fi
	      printf '%s\n' "$line" >> "$POSSE_E2E_LEAD_LOG"
	    done
	    ;;
esac
`
	if err := os.WriteFile(claudeBinary, []byte(fakeAgent), 0o700); err != nil {
		t.Fatal(err)
	}
	env := isolatedE2EEnv(t, root)
	env = setEnv(env, "POSSE_TEST_ROOT", root)
	env = setEnv(env, "POSSE_E2E_WORKTREES", worktrees)
	env = setEnv(env, "POSSE_E2E_LEAD_LOG", leadLog)
	env = setEnv(env, "POSSE_E2E_LEAD_ARGS_LOG", leadArgsLog)
	env = setEnv(env, "POSSE_E2E_WORKER_LOG", workerLog)
	env = setEnv(env, "POSSE_E2E_WORKER_ARGS_LOG", workerArgsLog)
	env = setEnv(env, "POSSE_E2E_SLEEP_PIDS", sleepPIDs)
	env = setEnv(env, "POSSE_E2E_SIGNAL_GATE", signalGateDir)
	env = setEnv(env, "POSSE_E2E_LAUNCH_LOG", launchRead)
	env = setEnv(env, "HOME", filepath.Join(root, "home"))
	env = setEnv(env, "GIT_CONFIG_GLOBAL", "/dev/null")
	env = setEnv(env, "GIT_CONFIG_NOSYSTEM", "1")
	env = setEnv(env, "PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	env = setEnv(env, "GIT_AUTHOR_NAME", "Posse E2E")
	env = setEnv(env, "GIT_AUTHOR_EMAIL", "posse-e2e@example.test")
	env = setEnv(env, "GIT_COMMITTER_NAME", "Posse E2E")
	env = setEnv(env, "GIT_COMMITTER_EMAIL", "posse-e2e@example.test")
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "posse"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "claude", "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pi"), 0o700); err != nil {
		t.Fatal(err)
	}
	claudeSettings := `{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"foreign-claude"}]}],"PreToolUse":[{"hooks":[{"type":"command","command":"foreign-claude-tool"}]}]},"foreign":"preserve"}`
	codexHooks := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"foreign-codex"}]}],"Stop":[{"hooks":[{"type":"command","command":"foreign-codex-stop"}]}]},"foreign":"preserve"}`
	if err := os.WriteFile(filepath.Join(root, "claude", "settings.json"), []byte(claudeSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "codex", "hooks.json"), []byte(codexHooks), 0o600); err != nil {
		t.Fatal(err)
	}
	configText := "[lead]\nkind = \"claude\"\n\n[defaults]\nlanding_mode = \"local\"\nauto_unsaddle = \"never\"\nmax_workers = 4\n\n[remuda]\nkeep_idle = 1\n\n[profiles.deep]\nkind = \"claude\"\nmodel = \"sonnet\"\neffort = \"high\"\n\n[dispatch.default]\nuse = \"deep\"\n"
	if err := os.WriteFile(filepath.Join(root, "posse", "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, playbook := range []struct{ path, content string }{
		{"playbook/lead.md", "User lead marker: discuss the workflow before dispatch.\n"},
		{"playbook/rider.md", "User rider marker: verify from the User layer.\n"},
		{"projects/shop/playbook/lead.md", "Project lead marker: apply the Project workflow.\n"},
		{"projects/shop/playbook/rider.md", "Project rider marker: verify from the Project layer.\n"},
	} {
		path := filepath.Join(root, "posse", playbook.path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(playbook.content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repo := filepath.Join(root, "repo")
	remote := filepath.Join(root, "remote.git")
	initRepository(t, repo, remote, env)

	client := herdr.NewWithEnv("herdr", env)
	server := startServer(t, client)
	if err := client.CheckProtocol(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "integration", "install", "claude"); err != nil {
		t.Fatalf("prepare the isolated Claude integration: %v", err)
	}
	if _, err := client.Run(context.Background(), "integration", "install", "codex"); err != nil {
		t.Fatalf("prepare the isolated Codex integration: %v", err)
	}
	claudeSettingsBytes, err := os.ReadFile(filepath.Join(root, "claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	claudeSettings = string(claudeSettingsBytes)
	codexHooksBytes, err := os.ReadFile(filepath.Join(root, "codex", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	codexHooks = string(codexHooksBytes)
	workspace, err := createWorkspace(client, repo)
	if err != nil {
		t.Fatal(err)
	}
	pluginPath := filepath.Join(root, "posse", "plugin", "herdr-plugin.toml")
	if output := runPosse(t, posseBinary, repoRoot, env, "setup", "--binary", stableBinary); !strings.Contains(output, "plugin,"+pluginPath) || !strings.Contains(output, "Codex will ask once to trust the new hook") {
		t.Fatalf("setup did not link the isolated plugin: %s", output)
	}
	plugin, err := os.ReadFile(filepath.Join(root, "posse", "plugin", "herdr-plugin.toml"))
	if err != nil || !strings.Contains(string(plugin), `id = "posse.herdr"`) || !strings.Contains(string(plugin), stableBinary) || !strings.Contains(string(plugin), `"recover", "--all"`) {
		t.Fatalf("embedded plugin manifest is incomplete: %s, %v", plugin, err)
	}
	for _, fixture := range []struct {
		path     string
		original string
	}{
		{filepath.Join(root, "claude", "settings.json"), claudeSettings},
		{filepath.Join(root, "codex", "hooks.json"), codexHooks},
	} {
		data, err := os.ReadFile(fixture.path)
		if err != nil {
			t.Fatal(err)
		}
		want := []byte(fixture.original)
		contextCommand, _ := json.Marshal(shellQuote(stableBinary) + " _context")
		guardCommand, _ := json.Marshal(shellQuote(stableBinary) + " _guard")
		want = appendHookGroupFixture(want, "SessionStart", []byte(`{"matcher":"startup|resume|compact","hooks":[{"type":"command","command":`+string(contextCommand)+`,"timeout":5}]}`))
		want = appendHookGroupFixture(want, "PreToolUse", []byte(`{"hooks":[{"type":"command","command":`+string(guardCommand)+`,"timeout":5}]}`))
		if !bytes.Equal(data, want) {
			t.Fatalf("setup changed bytes outside the SessionStart and PreToolUse insertions in %s:\n got: %s\nwant: %s", fixture.path, data, want)
		}
	}
	for _, name := range []string{"posse", "posse-setup"} {
		skill := filepath.Join(root, "home", ".agents", "skills", name, "SKILL.md")
		link := filepath.Join(root, "claude", "skills", name)
		if _, err := os.Stat(skill); err != nil {
			t.Fatalf("embedded skill %s was not installed: %v", name, err)
		}
		if target, err := os.Readlink(link); err != nil || target != filepath.Dir(skill) {
			t.Fatalf("skill link %s = %q, %v", name, target, err)
		}
	}
	if guard, err := os.ReadFile(filepath.Join(root, "pi", "extensions", "posse-worker-guard.ts")); err != nil || !strings.Contains(string(guard), strconv.Quote(stableBinary)) {
		t.Fatalf("setup did not install the pi Worker guard: %s %v", guard, err)
	}
	if output := runPosse(t, posseBinary, repoRoot, env, "setup", "--binary", stableBinary, "--check"); !strings.Contains(output, "{step,target,action,note}:") || strings.Contains(output, "needs_apply") {
		t.Fatalf("setup check did not return a single action-bearing plan: %s", output)
	}
	if output := runPosse(t, posseBinary, repoRoot, env, "setup", "--binary", stableBinary); !strings.Contains(output, "changed: []") {
		t.Fatalf("second setup run was not idempotent: %s", output)
	}
	callerEnv := append([]string(nil), env...)
	callerEnv = setEnv(callerEnv, "HERDR_ENV", "1")
	callerEnv = setEnv(callerEnv, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	callerEnv = setEnv(callerEnv, "HERDR_WORKSPACE_ID", workspace.Workspace.WorkspaceID)
	callerEnv = setEnv(callerEnv, "HERDR_TAB_ID", workspace.RootPane.TabID)
	// Run through the real shell pane so ExecPendingLead replaces its foreground process.
	if _, err := client.Run(context.Background(), "pane", "run", workspace.RootPane.PaneID, "posse up --name shop --yes"); err != nil {
		status, statusErr := client.Status(context.Background())
		snapshot, snapshotErr := client.Snapshot(context.Background())
		logs, logErr := client.Call(context.Background(), "plugin.log.list", map[string]any{"plugin_id": "posse.herdr", "limit": 20})
		t.Fatalf("posse up in caller pane failed: %v; status=%#v %v; snapshot=%#v %v; logs=%s %v", err, status, statusErr, snapshot, snapshotErr, logs, logErr)
	}
	if output, err := client.Run(context.Background(), "pane", "wait-output", workspace.RootPane.PaneID, "--match", "shop", "--timeout", "30000"); err != nil {
		t.Fatalf("posse up did not report the Project from the caller pane: %v %s", err, output)
	}
	home := filepath.Join(root, "posse")
	setupDB, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(30*time.Second, func() bool {
		current, projectErr := setupDB.ProjectByName(context.Background(), "shop")
		return projectErr == nil && current.LeadPaneID == workspace.RootPane.PaneID
	}) {
		snapshot, _ := client.Snapshot(context.Background())
		pane, paneErr := client.Call(context.Background(), "pane.read", map[string]any{"pane_id": workspace.RootPane.PaneID, "source": "recent-unwrapped", "lines": 80})
		t.Fatalf("Lead finalizer did not record the caller pane: snapshot=%#v pane=%s paneErr=%v", snapshot, pane, paneErr)
	}
	setupProject, err := setupDB.ProjectByName(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	leadPaneID := setupProject.LeadPaneID
	leadEnv := setEnv(callerEnv, "HERDR_PANE_ID", leadPaneID)
	leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", setupProject.HerdrWorkspaceID)
	if err := setupDB.Close(); err != nil {
		t.Fatal(err)
	}
	if got := runPosse(t, posseBinary, repo, leadEnv, "lowkey", "on"); !strings.Contains(got, "lowkey: on") {
		t.Fatalf("isolated Lead did not enable lowkey mode: %s", got)
	}
	if got := runPosse(t, posseBinary, repo, leadEnv); !strings.Contains(got, "lowkey: true") || !strings.Contains(got, "undelivered_notices") {
		t.Fatalf("running Lead did not see lowkey dashboard: %s", got)
	}
	lowkeyDB, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	lowkeyNotice, err := lowkeyDB.CreateNotice(context.Background(), store.Notice{ProjectID: setupProject.ID, Kind: "needs_decision", Summary: "Pick a direction"})
	if err != nil {
		t.Fatal(err)
	}
	if err := lowkeyDB.Close(); err != nil {
		t.Fatal(err)
	}
	if got := runPosse(t, posseBinary, repo, leadEnv, "lookout", "--timeout", "1000"); !strings.Contains(got, "needs-decision - Pick a direction") || !strings.Contains(got, "lowkey: true") {
		t.Fatalf("running Lead did not receive Notice text and rule: %s", got)
	}
	if got := runPosse(t, posseBinary, repo, leadEnv, "lookout", "--ack", strconv.FormatInt(lowkeyNotice, 10), "--timeout", "1"); !strings.Contains(got, "timeout") {
		t.Fatalf("combined ack and lookout did not wait: %s", got)
	}
	if got := runPosse(t, posseBinary, repo, leadEnv, "lowkey", "off"); !strings.Contains(got, "lowkey: off") {
		t.Fatalf("isolated Lead did not disable lowkey mode: %s", got)
	}
	leadInstructions := runPosse(t, posseBinary, repo, leadEnv, "lead")
	userLeadIndex := strings.Index(leadInstructions, "User lead marker")
	projectLeadIndex := strings.Index(leadInstructions, "Project lead marker")
	if userLeadIndex < 0 || projectLeadIndex <= userLeadIndex {
		t.Fatalf("posse lead omitted or misordered layered Lead Playbooks: %s", leadInstructions)
	}
	leadPromptFile, err := os.ReadFile(filepath.Join(home, "projects", "shop", "lead.md"))
	if err != nil || !strings.Contains(string(leadPromptFile), "User lead marker") || !strings.Contains(string(leadPromptFile), "Project lead marker") {
		t.Fatalf("Lead startup instructions omitted its layered Playbook: %s, %v", leadPromptFile, err)
	}
	if err := os.WriteFile(filepath.Join(home, "projects", "shop", "playbook", "lead.md"), []byte("Updated Project lead marker.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	updatedLeadInstructions := runPosse(t, posseBinary, repo, leadEnv, "lead")
	if !strings.Contains(updatedLeadInstructions, "Updated Project lead marker") || strings.Contains(updatedLeadInstructions, "Project lead marker: apply") {
		t.Fatalf("posse lead did not reload the current Playbook: %s", updatedLeadInstructions)
	}
	playbookOutput := runPosse(t, posseBinary, repo, leadEnv, "playbook", "show")
	if !strings.Contains(playbookOutput, "User rider marker") || !strings.Contains(playbookOutput, "Project rider marker") || strings.Index(playbookOutput, "User rider marker") >= strings.Index(playbookOutput, "Project rider marker") {
		t.Fatalf("playbook show omitted or misordered effective sources: %s", playbookOutput)
	}
	pathOutput := runPosse(t, posseBinary, repo, leadEnv, "playbook", "path", "rider")
	if !strings.Contains(pathOutput, filepath.Join(home, "playbook", "rider.md")) || !strings.Contains(pathOutput, filepath.Join(home, "projects", "shop", "playbook", "rider.md")) {
		t.Fatalf("playbook path omitted its User or Project source: %s", pathOutput)
	}
	brief := filepath.Join(root, "ship.md")
	briefText := "---\ntype: ship\ntitle: E2E change\ndone_when: commit exists\n---\nCreate one committed file.\n"
	if err := os.WriteFile(brief, []byte(briefText), 0o600); err != nil {
		t.Fatal(err)
	}
	spawnCommand := exec.Command(posseBinary, "ride", "--brief", brief, "--name", "e2e-change")
	spawnCommand.Dir = repo
	spawnCommand.Env = leadEnv
	spawnOutput, spawnErr := spawnCommand.CombinedOutput()
	if spawnErr != nil {
		status, statusErr := client.Status(context.Background())
		pluginLogs, logErr := client.Call(context.Background(), "plugin.log.list", map[string]any{"plugin_id": "posse.herdr", "limit": 10})
		t.Fatalf("posse ride failed: %v %s; status=%#v %v; logs=%s %v", spawnErr, spawnOutput, status, statusErr, pluginLogs, logErr)
	}
	if !strings.Contains(string(spawnOutput), "t1") {
		t.Fatalf("posse ride returned no Task id: %s", spawnOutput)
	}
	if !waitForCondition(20*time.Second, func() bool {
		pending, openErr := store.Open(home)
		if openErr != nil {
			return false
		}
		defer pending.Close()
		task, taskErr := pending.Task(context.Background(), setupProject.ID, "t1")
		return taskErr == nil && task.State == store.StateWorking
	}) {
		t.Fatal("Worker was not working before mid-turn send")
	}
	if !waitForCondition(20*time.Second, func() bool {
		snapshot, snapshotErr := client.Snapshot(context.Background())
		if snapshotErr != nil {
			return false
		}
		for _, pane := range snapshot.Panes {
			if pane.Label == "posse:shop:t1" && pane.AgentStatus == "working" && !pane.Focused {
				return true
			}
		}
		return false
	}) {
		t.Fatal("Worker pane was not working and unfocused before mid-turn send")
	}
	assertLeadSidebarPresentation(t, client, setupProject.HerdrWorkspaceID, leadPaneID, "Lead:shop")
	assertWorkerIsolation(t, client, posseBinary, home, repo, root, callerEnv, setupProject.ID, leadPaneID)
	presentationDB, err := store.OpenReadOnly(home)
	if err != nil {
		t.Fatal(err)
	}
	launched, err := presentationDB.Task(context.Background(), setupProject.ID, "t1")
	_ = presentationDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	assertWorkerSidebarPresentation(t, client, launched, "Lead:shop")
	if output := runPosse(t, posseBinary, repo, leadEnv, "send", "t1", "Steer the current work"); !strings.Contains(output, "delivered") {
		t.Fatalf("mid-turn send did not deliver: %s", output)
	}
	if output := runPosse(t, posseBinary, repo, leadEnv, "send", "t1", "Queue after the current turn", "--queue"); !strings.Contains(output, "queued") {
		t.Fatalf("queued instruction was not held: %s", output)
	}
	// The fake Lead reports idle on startup. Have its input loop report busy
	// only after startup, so a late idle report cannot undo the working state.
	if !waitForCondition(15*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(root, "lead-initial-idle-reported"))
		return err == nil
	}) {
		t.Fatal("fake Lead never finished its initial idle report")
	}
	if _, err := client.Call(context.Background(), "agent.prompt", map[string]any{"target": leadPaneID, "text": "__posse_e2e_busy__"}); err != nil {
		t.Fatalf("ask fake Lead to start a busy turn: %v", err)
	}
	if !waitForCondition(15*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(root, "lead-busy-reported"))
		return err == nil
	}) {
		t.Fatal("fake Lead did not report its busy turn")
	}
	if err := os.WriteFile(filepath.Join(signalGateDir, "t1"), []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.ProjectByName(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	completed := waitForCondition(60*time.Second, func() bool {
		task, taskErr := db.Task(context.Background(), project.ID, "t1")
		return taskErr == nil && task.State == store.StateDone
	})
	if !completed {
		task, _ := db.Task(context.Background(), project.ID, "t1")
		pane, paneErr := client.Call(context.Background(), "pane.read", map[string]any{"pane_id": task.PaneID, "source": "recent_unwrapped", "lines": 80})
		logs, _ := client.Call(context.Background(), "plugin.log.list", map[string]any{"plugin_id": "posse.herdr", "limit": 20})
		worker, _ := os.ReadFile(workerLog)
		t.Fatalf("Worker did not signal done: task=%#v pane=%s paneErr=%v logs=%s workerLog=%s", task, pane, paneErr, logs, worker)
	}
	task, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if task.State != store.StateDone || task.WorktreePath == "" {
		t.Fatalf("Worker Signal did not complete its Task: %#v", task)
	}
	generatedLaunch, err := os.ReadFile(filepath.Join(home, "projects", "shop", "tasks", "t1", "launch.md"))
	if err != nil {
		t.Fatal(err)
	}
	deliveredLaunch, err := os.ReadFile(launchRead)
	if err != nil || !bytes.Equal(generatedLaunch, deliveredLaunch) || !strings.Contains(string(deliveredLaunch), "Write in English in a neutral voice.") || !strings.Contains(string(deliveredLaunch), "posse holler done") || !strings.Contains(string(deliveredLaunch), "as background commands and never poll them") || !strings.Contains(string(deliveredLaunch), "Do not wait for PR CI") || !strings.Contains(string(deliveredLaunch), "never touch panes, tabs or workspaces you did not create") || !strings.Contains(string(deliveredLaunch), "User rider marker") || strings.Index(string(deliveredLaunch), "Project rider marker") <= strings.Index(string(deliveredLaunch), "User rider marker") || strings.Contains(string(deliveredLaunch), "User lead marker") {
		t.Fatalf("Worker did not receive the generated launch.md contract: err=%v generated=%q delivered=%q", err, generatedLaunch, deliveredLaunch)
	}
	workerArgs, err := os.ReadFile(workerArgsLog)
	if err != nil || !hasArgPair(strings.Fields(string(workerArgs)), "--model", "sonnet") || !hasArgPair(strings.Fields(string(workerArgs)), "--effort", "high") {
		t.Fatalf("Profile model and effort were not rendered through kind templates: args=%q err=%v", workerArgs, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(30*time.Second, func() bool {
		pending, openErr := store.Open(home)
		if openErr != nil {
			return false
		}
		defer pending.Close()
		project, projectErr := pending.ProjectByName(context.Background(), "shop")
		if projectErr != nil {
			return false
		}
		notices, noticeErr := pending.UndeliveredNotices(context.Background(), project.ID)
		if noticeErr != nil || len(notices) != 2 {
			return false
		}
		for _, notice := range notices {
			if notice.TaskID != task.ID || notice.Kind != "task_done" {
				return false
			}
		}
		return true
	}) {
		snapshot, snapshotErr := client.Snapshot(context.Background())
		diagnosticDB, diagnosticErr := store.Open(home)
		var notices, undelivered []store.Notice
		var noticesErr, undeliveredErr error
		if diagnosticErr == nil {
			notices, noticesErr = diagnosticDB.Notices(context.Background(), project.ID, false)
			undelivered, undeliveredErr = diagnosticDB.UndeliveredNotices(context.Background(), project.ID)
			_ = diagnosticDB.Close()
		}
		leadPrompt, _ := os.ReadFile(leadLog)
		pluginLogs, _ := client.Call(context.Background(), "plugin.log.list", map[string]any{"plugin_id": "posse.herdr", "limit": 20})
		workerLogContents, _ := os.ReadFile(workerLog)
		t.Fatalf("busy Lead did not leave both Worker Notices pending: notices=%#v noticesErr=%v undelivered=%#v undeliveredErr=%v diagnosticErr=%v snapshot=%#v snapshotErr=%v leadLog=%q workerLog=%q pluginLogs=%s", notices, noticesErr, undelivered, undeliveredErr, diagnosticErr, snapshot, snapshotErr, leadPrompt, workerLogContents, pluginLogs)
	}
	leadSnapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	leadBusy := false
	for _, pane := range leadSnapshot.Panes {
		if pane.PaneID == project.LeadPaneID && pane.AgentStatus == "working" {
			leadBusy = true
		}
	}
	if !leadBusy {
		t.Fatalf("fake Lead was not working when the Worker finished: %#v", leadSnapshot.Panes)
	}
	if _, err := client.Run(context.Background(), "pane", "report-agent", project.LeadPaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatalf("fake Lead idle report failed: %v", err)
	}
	if _, err := client.Run(context.Background(), "agent", "focus", task.PaneID); err != nil {
		t.Fatalf("focus the Worker pane to exercise unfocused Lead prompt delivery: %v", err)
	}
	leadSnapshot, err = client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if leadSnapshot.FocusedPaneID == project.LeadPaneID {
		t.Fatalf("could not move focus away from the Lead before testing pane-prompt delivery: %#v", leadSnapshot.Panes)
	}
	if !waitForCondition(30*time.Second, func() bool {
		contents, readErr := os.ReadFile(leadLog)
		return readErr == nil && strings.Contains(string(contents), "[posse | Posse -> Lead project | notice")
	}) {
		contents, _ := os.ReadFile(leadLog)
		db, dbErr := store.Open(home)
		var notices any
		if dbErr == nil {
			project, _ := db.ProjectByName(context.Background(), "shop")
			notices, _ = db.UndeliveredNotices(context.Background(), project.ID)
			_ = db.Close()
		}
		snapshot, snapshotErr := client.Snapshot(context.Background())
		logs, _ := client.Call(context.Background(), "plugin.log.list", map[string]any{"plugin_id": "posse.herdr", "limit": 30})
		worker, _ := os.ReadFile(workerLog)
		t.Fatalf("unfocused Lead did not receive Notice after focus moved to the Worker pane: log=%q notices=%#v dbErr=%v snapshot=%#v snapshotErr=%v workerLog=%q pluginLogs=%s", contents, notices, dbErr, snapshot, snapshotErr, worker, logs)
	}
	if _, err := client.Run(context.Background(), "agent", "focus", leadPaneID); err != nil {
		t.Fatalf("unfocus Worker to release queued instruction: %v", err)
	}
	if !waitForCondition(15*time.Second, func() bool {
		contents, err := os.ReadFile(workerLog)
		return err == nil && strings.Count(string(contents), "[posse | Lead -> Rider t1 | instruction #") == 2 && strings.Contains(string(contents), `body: "Queue after the current turn"`)
	}) {
		contents, err := os.ReadFile(workerLog)
		t.Fatalf("Worker steer/queued instruction envelopes missing: %q %v", contents, err)
	}
	if !waitForCondition(15*time.Second, func() bool {
		pending, err := store.Open(home)
		if err != nil {
			return false
		}
		defer pending.Close()
		current, err := pending.Task(context.Background(), project.ID, "t1")
		contents, readErr := os.ReadFile(workerLog)
		return err == nil && readErr == nil && strings.Count(string(contents), "signal: done") >= 2 && current.State == store.StateDone
	}) {
		contents, _ := os.ReadFile(workerLog)
		t.Fatalf("Worker did not finish the queued instruction: log=%q", contents)
	}
	if output := runPosse(t, posseBinary, repo, leadEnv, "land", "t1", "--merge", "--user-approved", "User approved the local merge"); !strings.Contains(output, "landed") {
		t.Fatalf("local land failed: %s", output)
	}
	if output := runPosse(t, posseBinary, repo, leadEnv, "ack", "all"); !strings.Contains(output, "acknowledged") {
		t.Fatalf("could not acknowledge the first Task's Notices before the next Lookout check: %s", output)
	}
	if got, err := gitCommand(env, repo, "show", "main:e2e-worker-t1.txt"); err != nil || strings.TrimSpace(got) != "worker change t1" {
		t.Fatalf("Worker change was not merged: %q %v", got, err)
	}
	mountPath := task.WorktreePath
	if err := os.WriteFile(filepath.Join(mountPath, "README.md"), []byte("dirty tracked edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountPath, "untracked.marker"), []byte("remove me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mountPath, "node_modules"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountPath, "node_modules", "marker"), []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, posseBinary, repo, leadEnv, "unsaddle", "t1"); !strings.Contains(output, "torn-down") {
		t.Fatalf("teardown failed: %s", output)
	}
	if got, err := os.ReadFile(filepath.Join(mountPath, "README.md")); err != nil || string(got) != "fixture\n" {
		t.Fatalf("Teardown kept a tracked edit: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(mountPath, "untracked.marker")); !os.IsNotExist(err) {
		t.Fatalf("Teardown kept an untracked file: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(mountPath, "node_modules", "marker")); err != nil || string(got) != "keep me\n" {
		t.Fatalf("warm cleanup lost the ignored marker: %q %v", got, err)
	}
	assertWorkerSleepsStopped(t, sleepPIDs)

	// The next Task reuses the same clean Mount, including its ignored cache.
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: E2E second change\ndone_when: second commit exists\n---\nCreate one more committed file.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secondOutput := runPosse(t, posseBinary, repo, leadEnv, "ride", "--brief", brief, "--name", "e2e-second-change")
	if !strings.Contains(secondOutput, "t2") {
		t.Fatalf("second ride returned no t2 Task: %s", secondOutput)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	secondTask, err := db.Task(context.Background(), project.ID, "t2")
	if err != nil || secondTask.MountID != task.MountID || secondTask.WorktreePath != mountPath {
		t.Fatalf("second Task did not reuse Mount 1: first=%#v second=%#v err=%v", task, secondTask, err)
	}
	if got, err := os.ReadFile(filepath.Join(mountPath, "node_modules", "marker")); err != nil || string(got) != "keep me\n" {
		t.Fatalf("second Task did not retain the ignored marker: %q %v", got, err)
	}
	if _, err := client.Call(context.Background(), "agent.focus", map[string]any{"target": project.LeadPaneID}); err != nil {
		t.Fatalf("focus Lead before second Worker Signal: %v", err)
	}
	leadBeforeLookout, _ := os.ReadFile(leadLog)
	leadPromptCount := strings.Count(string(leadBeforeLookout), "[posse | Posse -> Lead ")
	lookoutEnv := setEnv(env, "HERDR_ENV", "1")
	lookoutEnv = setEnv(lookoutEnv, "HERDR_PANE_ID", project.LeadPaneID)
	lookoutEnv = setEnv(lookoutEnv, "HERDR_WORKSPACE_ID", project.HerdrWorkspaceID)
	lookout := exec.Command(posseBinary, "lookout", "--timeout", "60000")
	lookout.Dir = repo
	lookout.Env = lookoutEnv
	var lookoutOutput bytes.Buffer
	lookout.Stdout, lookout.Stderr = &lookoutOutput, &lookoutOutput
	if err := lookout.Start(); err != nil {
		t.Fatal(err)
	}
	lookoutDone := make(chan error, 1)
	go func() { lookoutDone <- lookout.Wait() }()
	if err := os.WriteFile(filepath.Join(signalGateDir, "t2"), []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-lookoutDone:
		if err != nil {
			t.Fatalf("background lookout failed: %v output=%s", err, lookoutOutput.String())
		}
	case <-time.After(75 * time.Second):
		_ = lookout.Process.Kill()
		<-lookoutDone
		t.Fatalf("background lookout did not return the Worker's Notice: %s", lookoutOutput.String())
	}
	if !strings.Contains(lookoutOutput.String(), "E2E worker committed the change") {
		currentTask, taskErr := db.Task(context.Background(), project.ID, "t2")
		pending, noticeErr := db.UndeliveredNotices(context.Background(), project.ID)
		worker, _ := os.ReadFile(workerLog)
		t.Fatalf("background lookout omitted the Worker completion digest: output=%s task=%#v taskErr=%v pending=%#v noticeErr=%v workerLog=%q", lookoutOutput.String(), currentTask, taskErr, pending, noticeErr, worker)
	}
	leadAfterLookout, _ := os.ReadFile(leadLog)
	if strings.Count(string(leadAfterLookout), "[posse | Posse -> Lead ") != leadPromptCount {
		t.Fatalf("focused Lead received typed input while lookout returned: before=%q after=%q", leadBeforeLookout, leadAfterLookout)
	}
	if !waitForCondition(60*time.Second, func() bool {
		current, taskErr := db.Task(context.Background(), project.ID, "t2")
		return taskErr == nil && current.State == store.StateDone
	}) {
		current, _ := db.Task(context.Background(), project.ID, "t2")
		worker, _ := os.ReadFile(workerLog)
		t.Fatalf("second Worker did not signal done: task=%#v workerLog=%q", current, worker)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, posseBinary, repo, env, "config", "set", "defaults.auto_unsaddle", "finished", "--project", "shop"); !strings.Contains(output, "finished") {
		t.Fatalf("could not enable automatic Teardown for the E2E landing case: %s", output)
	}
	if output := runPosse(t, posseBinary, repo, leadEnv, "land", "t2", "--merge", "--user-approved", "User approved the second local merge"); !strings.Contains(output, "landed") {
		t.Fatalf("second local land failed: %s", output)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	secondTask, err = db.Task(context.Background(), project.ID, "t2")
	if err != nil || secondTask.State != store.StateTornDown {
		t.Fatalf("land --merge did not auto-unsaddle the landed Task: %#v, %v", secondTask, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	assertWorkerSleepsStopped(t, sleepPIDs)

	// Pruning keeps the sole configured idle Mount and removes only the extra one.
	extraMount := filepath.Join(root, "posse", "remuda", "shop", "mount-2")
	if err := os.MkdirAll(filepath.Dir(extraMount), 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, env, repo, "worktree", "add", "--detach", extraMount, "main")
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `INSERT INTO mounts(project_id,n,path,state) VALUES(?,2,?,'idle')`, project.ID, extraMount); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	pruneDryRun := runPosse(t, posseBinary, repo, leadEnv, "remuda", "prune")
	if !strings.Contains(pruneDryRun, "mount-2") || !strings.Contains(pruneDryRun, "dry_run: true") {
		t.Fatalf("prune dry run did not select only the extra Mount: %s", pruneDryRun)
	}
	if _, err := os.Stat(extraMount); err != nil {
		t.Fatalf("dry run removed the extra Mount: %v", err)
	}
	pruneOutput := runPosse(t, posseBinary, repo, leadEnv, "remuda", "prune", "--yes")
	if !strings.Contains(pruneOutput, "mount-2") {
		t.Fatalf("prune --yes did not report the extra Mount: %s", pruneOutput)
	}
	if _, err := os.Stat(extraMount); !os.IsNotExist(err) {
		t.Fatalf("prune --yes kept the extra Mount: %v", err)
	}
	if _, err := os.Stat(mountPath); err != nil {
		t.Fatalf("prune removed the retained Mount: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(mountPath, "node_modules", "marker")); err != nil || string(got) != "keep me\n" {
		t.Fatalf("prune changed the retained Mount: %q %v", got, err)
	}
	oldLeadPaneID := project.LeadPaneID
	recoveryCaller, err := createTab(client, project.HerdrWorkspaceID, repo, "recovery-caller")
	if err != nil {
		t.Fatalf("create a shell pane for missing-Lead recovery: %v", err)
	}
	callerEnv = setEnv(callerEnv, "HERDR_PANE_ID", recoveryCaller.RootPane.PaneID)
	callerEnv = setEnv(callerEnv, "HERDR_TAB_ID", recoveryCaller.RootPane.TabID)
	if _, err := client.Call(context.Background(), "pane.close", map[string]any{"pane_id": oldLeadPaneID}); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, posseBinary, repo, callerEnv, "up", "--name", "shop"); !strings.Contains(output, "shop") {
		t.Fatalf("posse up did not replace the missing Lead: %s", output)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByName(context.Background(), "shop")
	if err != nil || project.LeadPaneID == oldLeadPaneID || project.LeadLabel != "posse:shop:lead" {
		t.Fatalf("missing Lead was not replaced: %#v, %v", project, err)
	}
	replacementSnapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	replacementFound := false
	for _, pane := range replacementSnapshot.Panes {
		if pane.PaneID == project.LeadPaneID && pane.Agent != "" {
			replacementFound = true
			break
		}
	}
	if !replacementFound {
		t.Fatalf("replacement Lead pane %s has no agent", project.LeadPaneID)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// A focused Lead must receive a desktop notification, never typed input.
	// Its background Lookout may claim the Notice even while the pane is focused.
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByName(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	previousPromptLog, _ := os.ReadFile(leadLog)
	previousPromptCount := strings.Count(string(previousPromptLog), "[posse | Posse -> Lead ")
	if _, err := db.CreateNotice(context.Background(), store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "focused guard check", DataJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(context.Background(), "agent.focus", map[string]any{"target": project.LeadPaneID}); err != nil {
		t.Fatal(err)
	}
	// Force the overlap that used to make the pending-Notice assertion flaky:
	// Lookout delivery is valid while the Lead is focused, but typed delivery is not.
	lookoutEnv = setEnv(lookoutEnv, "HERDR_PANE_ID", project.LeadPaneID)
	if output := runPosse(t, posseBinary, repo, lookoutEnv, "lookout", "--timeout", "3000"); !strings.Contains(output, "focused guard check") {
		t.Fatalf("focused Lead's Lookout did not receive the Notice: %s", output)
	}
	focusedEnv := setEnv(env, "HERDR_PLUGIN_EVENT_JSON", fmt.Sprintf(`{"event":"pane_focused","data":{"pane_id":%q}}`, project.LeadPaneID))
	runPosse(t, posseBinary, repo, focusedEnv, "_ingest")
	currentLog, _ := os.ReadFile(leadLog)
	if strings.Count(string(currentLog), "[posse | Posse -> Lead ") != previousPromptCount {
		t.Fatalf("focused Lead received unexpected typed input: %q", currentLog)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	undelivered, err := db.UndeliveredNotices(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range undelivered {
		if notice.Summary == "focused guard check" {
			t.Fatalf("Lookout did not mark the focused Lead Notice delivered: %#v", undelivered)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Panes) == 0 {
		t.Fatal("isolated Herdr snapshot had no panes")
	}

	// Startup recovery restarts an active Worker and its Lead after Herdr dies.
	workspaceID := project.HerdrWorkspaceID
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByName(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	leadEnv = setEnv(leadEnv, "HERDR_PANE_ID", project.LeadPaneID)
	leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", project.HerdrWorkspaceID)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: Recovery worker\ndone_when: recovery is verified\n---\nKeep working until the recovery E2E resumes you.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, posseBinary, repo, leadEnv, "ride", "--brief", brief, "--name", "recovery-worker"); !strings.Contains(output, "t3") {
		t.Fatalf("recovery Worker did not start: %s", output)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	thirdTask, err := db.Task(context.Background(), project.ID, "t3")
	if err != nil {
		t.Fatal(err)
	}
	if intents, intentErr := db.Intents(context.Background(), project.ID); intentErr != nil || len(intents) != 0 {
		t.Fatalf("successful ride left unfinished command intents: %#v, %v", intents, intentErr)
	}
	if thirdTask.MountID == 0 || thirdTask.WorktreePath == "" || thirdTask.Branch == "" {
		t.Fatalf("recovery Worker has no held Mount and branch: %#v", thirdTask)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	thirdLaunchPath := filepath.Join(home, "projects", "shop", "tasks", "t3", "launch.md")
	thirdLaunchSnapshot, err := os.ReadFile(thirdLaunchPath)
	if err != nil || !strings.Contains(string(thirdLaunchSnapshot), "User rider marker") || !strings.Contains(string(thirdLaunchSnapshot), "Project rider marker") {
		t.Fatalf("recovery Rider did not receive a layered Playbook snapshot: %s, %v", thirdLaunchSnapshot, err)
	}
	if err := os.WriteFile(filepath.Join(home, "playbook", "rider.md"), []byte("Updated User rider instructions.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "projects", "shop", "playbook", "rider.md"), []byte("Updated Project rider instructions.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(30*time.Second, func() bool {
		_, statErr := os.Stat(filepath.Join(thirdTask.WorktreePath, "e2e-worker-t3.txt"))
		return statErr == nil
	}) {
		t.Fatal("recovery Worker did not commit before the server restart")
	}
	workerArgsBefore, _ := os.ReadFile(workerArgsLog)
	leadArgsBefore, _ := os.ReadFile(leadArgsLog)
	leadPromptsBefore, _ := os.ReadFile(leadLog)
	workerStartsBefore := strings.Count(string(workerArgsBefore), "start ")
	leadStartsBefore := strings.Count(string(leadArgsBefore), "start ")
	leadPromptCountBefore := strings.Count(string(leadPromptsBefore), "[posse | Posse -> Lead ")
	if _, err := client.Call(context.Background(), "pane.focus", map[string]any{"pane_id": recoveryCaller.RootPane.PaneID}); err != nil {
		t.Fatalf("could not leave the Lead pane unfocused before startup recovery: %v", err)
	}
	var beforeRestart herdr.Snapshot
	if !waitForCondition(5*time.Second, func() bool {
		current, snapshotErr := client.Snapshot(context.Background())
		if snapshotErr != nil {
			return false
		}
		beforeRestart = current
		return current.ServerStartedAt != ""
	}) {
		t.Fatalf("isolated Herdr server generation was not observed: %#v", beforeRestart.ServerStartedAt)
	}
	killServer(t, server)
	startServer(t, client)
	var afterRestart herdr.Snapshot
	if !waitForCondition(30*time.Second, func() bool {
		current, snapshotErr := client.Snapshot(context.Background())
		if snapshotErr != nil {
			return false
		}
		afterRestart = current
		return current.ServerStartedAt != "" && current.ServerStartedAt != beforeRestart.ServerStartedAt
	}) {
		t.Fatalf("Herdr restart generation did not change: before=%q after=%q", beforeRestart.ServerStartedAt, afterRestart.ServerStartedAt)
	}
	recoveryOutput := "automatic Herdr startup recovery"
	if !waitForCondition(30*time.Second, func() bool {
		recoveryDB, openErr := store.OpenReadOnly(home)
		if openErr != nil {
			return false
		}
		defer recoveryDB.Close()
		generation, generationErr := recoveryDB.ProjectServerStartedAt(context.Background(), project.ID)
		if generationErr != nil {
			return false
		}
		currentTask, taskErr := recoveryDB.Task(context.Background(), project.ID, "t3")
		if taskErr != nil {
			return false
		}
		notices, noticesErr := recoveryDB.Notices(context.Background(), project.ID, false)
		if noticesErr != nil {
			return false
		}
		recoveryNotices := 0
		for _, notice := range notices {
			if notice.Kind == "recovery" && strings.Contains(notice.Summary, "recovery-worker") {
				recoveryNotices++
			}
		}
		workerArgsNow, _ := os.ReadFile(workerArgsLog)
		leadArgsNow, _ := os.ReadFile(leadArgsLog)
		leadPromptsNow, _ := os.ReadFile(leadLog)
		return generation == afterRestart.ServerStartedAt &&
			currentTask.State == store.StateWorking && currentTask.Launches > thirdTask.Launches && currentTask.AgentName != thirdTask.AgentName &&
			currentTask.MountID == thirdTask.MountID && currentTask.WorktreePath == thirdTask.WorktreePath && currentTask.Branch == thirdTask.Branch &&
			recoveryNotices == 1 && strings.Count(string(workerArgsNow), "start ") > workerStartsBefore &&
			strings.Count(string(leadArgsNow), "start ") > leadStartsBefore && strings.Count(string(leadPromptsNow), "[posse | Posse -> Lead ") > leadPromptCountBefore
	}) {
		debugDB, debugErr := store.Open(home)
		var intents []store.Intent
		var recovery store.ProjectRecovery
		var generation string
		var task store.Task
		var transitions string
		if debugErr == nil {
			intents, debugErr = debugDB.Intents(context.Background(), project.ID)
			recovery, _ = debugDB.ProjectRecovery(context.Background(), project.ID)
			generation, _ = debugDB.ProjectServerStartedAt(context.Background(), project.ID)
			task, _ = debugDB.Task(context.Background(), project.ID, "t3")
			transitions = taskTransitions(debugDB, task.ID)
			_ = debugDB.Close()
		}
		var panes []herdr.Pane
		if current, snapshotErr := client.Snapshot(context.Background()); snapshotErr == nil {
			panes = current.Panes
		}
		pluginLogs, pluginLogsErr := client.Run(context.Background(), "plugin", "log", "list", "--plugin", "posse.herdr")
		t.Fatalf("startup recovery did not finish: generation=%q want=%q recovery=%#v task=%#v transitions=%s panes=%#v intents=%#v dbErr=%v pluginLogs=%s pluginLogsErr=%v", generation, afterRestart.ServerStartedAt, recovery, task, transitions, panes, intents, debugErr, pluginLogs, pluginLogsErr)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce a plugin event that read the old server snapshot before recovery
	// but finishes reconciliation after the new server's recovery completes.
	if _, err := posseRuntime.ReconcileSnapshot(context.Background(), db, project.ID, beforeRestart, time.Now()); err != nil {
		t.Fatalf("reconcile delayed pre-restart event: %v", err)
	}
	recovered, err := db.Task(context.Background(), project.ID, "t3")
	if err != nil || recovered.State != store.StateWorking || recovered.MountID != thirdTask.MountID || recovered.WorktreePath != thirdTask.WorktreePath || recovered.Branch != thirdTask.Branch || recovered.AgentName == thirdTask.AgentName {
		transitions, _ := db.TaskTransitions(context.Background(), recovered.ID, 10)
		reconcileSnapshot, snapshotErr := client.Snapshot(context.Background())
		pluginLogs, pluginLogsErr := client.Run(context.Background(), "plugin", "log", "list", "--plugin", "posse.herdr")
		t.Fatalf("Worker was not relaunched on its original Mount and branch: before=%#v after=%#v transitions=%#v snapshot=%#v snapshotErr=%v recovery=%s err=%v pluginLogs=%s pluginLogsErr=%v", thirdTask, recovered, transitions, reconcileSnapshot, snapshotErr, recoveryOutput, err, pluginLogs, pluginLogsErr)
	}
	var recoverySnapshot herdr.Snapshot
	if !waitForCondition(5*time.Second, func() bool {
		snapshot, err := client.Snapshot(context.Background())
		if err != nil {
			return false
		}
		recoverySnapshot = snapshot
		for _, pane := range snapshot.Panes {
			if pane.PaneID == recovered.PaneID && pane.WorkspaceID == recovered.HerdrWorkspaceID {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("recovered Worker pane %s did not appear in Herdr: before=%#v after=%#v recovered=%#v", recovered.PaneID, beforeRestart.Panes, recoverySnapshot.Panes, recovered)
	}
	// Herdr renumbers pane ids on restore; recovery re-records the Lead found by its label.
	project, err = db.ProjectByName(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	assertLeadSidebarPresentation(t, client, project.HerdrWorkspaceID, project.LeadPaneID, "Lead:shop")
	assertWorkerSidebarPresentation(t, client, recovered, "Lead:shop")
	if _, err := db.ProjectServerStartedAt(context.Background(), project.ID); err != nil {
		t.Fatal(err)
	}
	notices, err := db.Notices(context.Background(), project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	recoveryNotices := 0
	for _, notice := range notices {
		if notice.Kind == "recovery" && strings.Contains(notice.Summary, "recovery-worker") {
			recoveryNotices++
		}
	}
	if recoveryNotices != 1 {
		t.Fatalf("recovery digest Notice count = %d, want one: %#v; recovery=%s", recoveryNotices, notices, recoveryOutput)
	}
	recoveredLaunchSnapshot, err := os.ReadFile(thirdLaunchPath)
	if err != nil || !bytes.Equal(recoveredLaunchSnapshot, thirdLaunchSnapshot) || strings.Contains(string(recoveredLaunchSnapshot), "Updated User rider instructions") || strings.Contains(string(recoveredLaunchSnapshot), "Updated Project rider instructions") {
		t.Fatalf("Rider Playbook snapshot changed during relaunch: before=%q after=%q err=%v", thirdLaunchSnapshot, recoveredLaunchSnapshot, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	workerArgsAfter, _ := os.ReadFile(workerArgsLog)
	workerStartsAfter := strings.Count(string(workerArgsAfter), "start ")
	if workerStartsAfter <= workerStartsBefore || !hasArgPair(strings.Fields(string(workerArgsAfter)), "--model", "sonnet") || !hasArgPair(strings.Fields(string(workerArgsAfter)), "--effort", "high") {
		t.Fatalf("recovered Worker did not start with its Profile arguments: before=%q after=%q", workerArgsBefore, workerArgsAfter)
	}
	leadArgsAfter, _ := os.ReadFile(leadArgsLog)
	if strings.Count(string(leadArgsAfter), "start ") <= leadStartsBefore || !strings.Contains(string(leadArgsAfter), "lead.md") {
		t.Fatalf("Lead was not restarted with its instruction file: before=%q after=%q", leadArgsBefore, leadArgsAfter)
	}
	leadPromptsAfter, _ := os.ReadFile(leadLog)
	if strings.Count(string(leadPromptsAfter), "[posse | Posse -> Lead ") <= leadPromptCountBefore {
		t.Fatalf("restarted Lead did not receive the recovery Notice: before=%q after=%q", leadPromptsBefore, leadPromptsAfter)
	}

	// A Worker pane missing by both id and label becomes lost without deleting its worktree.
	deathTab, err := createTab(client, workspaceID, repo, "worker-death")
	if err != nil {
		t.Fatal(err)
	}
	deathLabel := "posse:shop:t4"
	if _, err := client.Call(context.Background(), "pane.rename", map[string]any{"pane_id": deathTab.RootPane.PaneID, "label": deathLabel}); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	deathPath := filepath.Join(root, "preserved-worktree")
	deathID, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 4, Type: "ship", Title: "Death", LandingMode: "local", WorktreePath: deathPath, HerdrWorkspaceID: workspaceID, PaneID: deathTab.RootPane.PaneID, PaneLabel: deathLabel})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), deathID, store.StateSpawning, store.StateWorking, "cli", "test task ready"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(context.Background(), "pane.close", map[string]any{"pane_id": deathTab.RootPane.PaneID}); err != nil {
		t.Fatal(err)
	}
	runPosse(t, posseBinary, repo, env, "roster")
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	dead, err := db.Task(context.Background(), project.ID, "t4")
	if err != nil || dead.State != store.StateLost || dead.WorktreePath != deathPath {
		t.Fatalf("missing Worker did not become lost safely: %#v, %v", dead, err)
	}
	_ = db.Close()
	if output := runPosse(t, posseBinary, repoRoot, env, "setup", "--uninstall"); !strings.Contains(output, "uninstalled") {
		t.Fatalf("isolated setup uninstall failed: %s", output)
	}
	for _, fixture := range []struct {
		path     string
		original string
	}{{filepath.Join(root, "claude", "settings.json"), claudeSettings}, {filepath.Join(root, "codex", "hooks.json"), codexHooks}} {
		data, err := os.ReadFile(fixture.path)
		if err != nil || !bytes.Equal(data, []byte(fixture.original)) {
			t.Fatalf("uninstall did not preserve only foreign hooks in %s: %s, %v", fixture.path, data, err)
		}
	}
}

// appendHookGroupFixture returns original with group appended to the hooks
// array of event, or with a new event member when the event is absent.
func appendHookGroupFixture(original []byte, event string, group []byte) []byte {
	key := bytes.Index(original, []byte(`"`+event+`"`))
	if key < 0 {
		hooks := bytes.Index(original, []byte(`"hooks"`))
		if hooks < 0 {
			return nil
		}
		open := hooks + bytes.IndexByte(original[hooks:], '{')
		close := matchingBracket(original, open)
		if close < 0 {
			return nil
		}
		position := close
		for position > open+1 && (original[position-1] == ' ' || original[position-1] == '\n' || original[position-1] == '\r' || original[position-1] == '\t') {
			position--
		}
		member := []byte(`"` + event + `":[` + string(group) + `]`)
		body := original[open+1 : close]
		if first := bytes.IndexByte(body, '"'); first >= 0 && bytes.Contains(body[:first], []byte("\n")) {
			memberIndent := string(body[bytes.LastIndexByte(body[:first], '\n')+1 : first])
			closeIndent := string(original[bytes.LastIndexByte(original[:close], '\n')+1 : close])
			var formatted bytes.Buffer
			if err := json.Indent(&formatted, []byte(`[`+string(group)+`]`), memberIndent, strings.TrimPrefix(memberIndent, closeIndent)); err != nil {
				return nil
			}
			member = []byte("\n" + memberIndent + `"` + event + `": ` + formatted.String())
		}
		if len(bytes.TrimSpace(body)) > 0 {
			member = append([]byte{','}, member...)
		}
		return append(append(append([]byte(nil), original[:position]...), member...), original[position:]...)
	}
	openRelative := bytes.IndexByte(original[key:], '[')
	if openRelative < 0 {
		return nil
	}
	open := key + openRelative
	close := matchingBracket(original, open)
	if close < 0 {
		return nil
	}
	position := close
	for position > open+1 && (original[position-1] == ' ' || original[position-1] == '\n' || original[position-1] == '\r' || original[position-1] == '\t') {
		position--
	}
	addition := make([]byte, 0, len(group)+1)
	if len(bytes.TrimSpace(original[open+1:close])) > 0 {
		addition = append(addition, ',')
	}
	if itemIndent, indent, newline, pretty := fixtureArrayItemStyle(original, open); pretty {
		var formatted bytes.Buffer
		if err := json.Indent(&formatted, group, "", indent); err == nil {
			addition = append(addition, newline...)
			for index, line := range bytes.Split(formatted.Bytes(), []byte{'\n'}) {
				if index > 0 {
					addition = append(addition, newline...)
				}
				addition = append(addition, itemIndent...)
				addition = append(addition, line...)
			}
		} else {
			addition = append(addition, group...)
		}
	} else {
		addition = append(addition, group...)
	}
	updated := make([]byte, 0, len(original)+len(addition))
	updated = append(updated, original[:position]...)
	updated = append(updated, addition...)
	updated = append(updated, original[position:]...)
	return updated
}

// matchingBracket returns the index of the bracket that closes the one at open.
func matchingBracket(data []byte, open int) int {
	depth, quote, escape := 0, false, false
	for index := open; index < len(data); index++ {
		value := data[index]
		if quote {
			if escape {
				escape = false
			} else if value == '\\' {
				escape = true
			} else if value == '"' {
				quote = false
			}
			continue
		}
		switch value {
		case '"':
			quote = true
		case '[', '{':
			depth++
		case ']', '}':
			depth--
			if depth == 0 {
				return index
			}
		}
	}
	return -1
}

func fixtureArrayItemStyle(original []byte, open int) (string, string, string, bool) {
	itemStart := open + 1
	for itemStart < len(original) && (original[itemStart] == ' ' || original[itemStart] == '\n' || original[itemStart] == '\r' || original[itemStart] == '\t') {
		itemStart++
	}
	if itemStart >= len(original) || original[itemStart] != '{' {
		return "", "", "", false
	}
	memberStart := itemStart + 1
	for memberStart < len(original) && (original[memberStart] == ' ' || original[memberStart] == '\n' || original[memberStart] == '\r' || original[memberStart] == '\t') {
		memberStart++
	}
	if !bytes.Contains(original[itemStart+1:memberStart], []byte("\n")) {
		return "", "", "", false
	}
	itemLineStart := bytes.LastIndexByte(original[:itemStart], '\n') + 1
	memberLineStart := bytes.LastIndexByte(original[:memberStart], '\n') + 1
	itemIndent := string(original[itemLineStart:itemStart])
	memberIndent := string(original[memberLineStart:memberStart])
	if !strings.HasPrefix(memberIndent, itemIndent) || len(memberIndent) == len(itemIndent) {
		return "", "", "", false
	}
	newline := "\n"
	if bytes.Contains(original, []byte("\r\n")) {
		newline = "\r\n"
	}
	return itemIndent, memberIndent[len(itemIndent):], newline, true
}

// taskTransitions renders a Task's state history for failure messages.
func taskTransitions(db *store.DB, taskID int64) string {
	rows, err := db.QueryContext(context.Background(), `SELECT from_state, to_state, source, note, at FROM transitions WHERE task_id=? ORDER BY id`, taskID)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var from, to, source, note string
		var at int64
		if err := rows.Scan(&from, &to, &source, &note, &at); err != nil {
			return err.Error()
		}
		lines = append(lines, fmt.Sprintf("%d %s->%s by %s: %s", at, from, to, source, note))
	}
	return strings.Join(lines, "; ")
}

// The Lead's visible workspace label must not replace its canonical pane label.
func assertLeadSidebarPresentation(t *testing.T, client *herdr.Client, workspaceID, paneID, label string) {
	t.Helper()
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, workspace := range snapshot.Workspaces {
		if workspace.WorkspaceID == workspaceID {
			if workspace.Label != label {
				t.Fatalf("Lead workspace label = %q, want %q", workspace.Label, label)
			}
			for _, pane := range snapshot.Panes {
				if pane.PaneID == paneID && pane.Label == "posse:shop:lead" {
					if pane.Agent == "" || pane.DisplayAgent != pane.Agent || pane.Title != "Lead: shop" || pane.Tokens["posse_row"] != label {
						t.Fatalf("Lead must retain its role and display its detected harness: %#v", pane)
					}
					return
				}
			}
		}
	}
	t.Fatalf("Lead workspace/pane missing: workspace=%q pane=%q snapshot=%#v", workspaceID, paneID, snapshot)
}

// Assert the pane metadata and tab label against a live isolated Herdr snapshot.
func assertWorkerSidebarPresentation(t *testing.T, client *herdr.Client, task store.Task, workspaceLabel string) {
	t.Helper()
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var workspace herdr.Workspace
	for _, candidate := range snapshot.Workspaces {
		if candidate.WorkspaceID == task.HerdrWorkspaceID {
			workspace = candidate
		}
	}
	var pane herdr.Pane
	for _, candidate := range snapshot.Panes {
		if candidate.PaneID == task.PaneID {
			pane = candidate
		}
	}
	var tab herdr.Tab
	for _, candidate := range snapshot.Tabs {
		if candidate.TabID == pane.TabID {
			tab = candidate
		}
	}
	if workspace.Label != task.ShortName || workspace.Worktree.CheckoutPath != task.WorktreePath || !workspace.Worktree.IsLinkedWorktree ||
		pane.Label != task.PaneLabel || pane.CWD != task.WorktreePath || pane.Agent != "claude" || pane.DisplayAgent != pane.Agent ||
		!strings.HasPrefix(pane.Title, task.Title+" · "+task.ShortName) ||
		pane.Tokens["posse_title"] != task.Title || pane.Tokens["posse_mount"] != filepath.Base(task.WorktreePath) || pane.Tokens["posse_branch"] != task.ShortName ||
		tab.Label == task.ShortName {
		t.Fatalf("isolated Worker sidebar/cwd mismatch: task=%#v workspace=%#v tab=%#v pane=%#v", task, workspace, tab, pane)
	}
}

// assertWorkerIsolation checks the Worker guard and Mount identity. The group
// close replay runs in TestRidersAsGroupedChildrenRecoverAfterAnotherPrimaryClosesGroup.
func assertWorkerIsolation(t *testing.T, client *herdr.Client, posseBinary, home, repo, root string, callerEnv []string, projectID int64, leadPaneID string) {
	t.Helper()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.Task(context.Background(), projectID, "t1")
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	grouped := false
	for _, workspace := range snapshot.Workspaces {
		if workspace.WorkspaceID == task.HerdrWorkspaceID && workspace.Worktree.CheckoutPath == task.WorktreePath && workspace.Worktree.IsLinkedWorktree {
			grouped = true
		}
	}
	if !grouped {
		t.Fatalf("Worker is not a grouped child: %#v", snapshot.Workspaces)
	}
	if runtime.GOOS == "linux" {
		raw, err := client.Call(context.Background(), "pane.process_info", map[string]any{"pane_id": task.PaneID})
		if err != nil {
			t.Fatal(err)
		}
		var info struct {
			ProcessInfo struct {
				ShellPID int `json:"shell_pid"`
			} `json:"process_info"`
		}
		if err := json.Unmarshal(raw, &info); err != nil || info.ProcessInfo.ShellPID == 0 {
			t.Fatalf("Worker pane process info: %s %v", raw, err)
		}
		// Herdr's worktree.open does not accept per-pane env; the Mount cwd
		// and labeled pane identify this Rider to Posse's Worker mode.
	}

	workerEnv := setEnv(callerEnv, "HERDR_PANE_ID", task.PaneID)
	workerEnv = setEnv(workerEnv, "HERDR_WORKSPACE_ID", task.HerdrWorkspaceID)
	workerEnv = setEnv(workerEnv, "HERDR_SOCKET_PATH", herdr.SocketPath(callerEnv))
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", home)
	for _, args := range [][]string{{"project", "add", "--name", "stray"}, {"ride", "--brief", filepath.Join(root, "ship.md")}, {"config", "set", "defaults.max_workers", "2"}, {"roster"}} {
		command := exec.Command(posseBinary, args...)
		command.Dir = repo
		command.Env = workerEnv
		output, err := command.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "worker_forbidden") {
			t.Fatalf("Worker ran `posse %s` against its home: err=%v output=%s", strings.Join(args, " "), err, output)
		}
	}
	// The review's bypass: drop the marker and the pane id, leave the Mount. An
	// ancestor (the agent, here the outer sh) still carries the marker or still
	// runs inside the Mount, and /proc shows it.
	bypass := "cd / && env -u POSSE_WORKER_HOME -u HERDR_PANE_ID " + shellQuote(posseBinary) + " config set defaults.auto_unsaddle never; true"
	for _, ancestor := range []struct {
		name string
		env  []string
	}{
		{"marker", workerEnv},
		{"mount", setEnv(setEnv(workerEnv, "POSSE_WORKER_HOME", ""), "HERDR_PANE_ID", "")},
	} {
		command := exec.Command("sh", "-c", "sh -c "+shellQuote(bypass)+"; true")
		command.Dir = task.WorktreePath
		command.Env = ancestor.env
		output, _ := command.CombinedOutput()
		if !strings.Contains(string(output), "worker_forbidden") {
			t.Fatalf("a Worker (%s ancestor) shed its identity with env -u and cd: %s", ancestor.name, output)
		}
	}
	guard := exec.Command(posseBinary, "_guard")
	guard.Dir = task.WorktreePath
	guard.Env = workerEnv
	guard.Stdin = strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"herdr workspace close wGN --group"}}`)
	var guardErr bytes.Buffer
	guard.Stderr = &guardErr
	if err := guard.Run(); err == nil || guard.ProcessState.ExitCode() != 2 || !strings.Contains(guardErr.String(), "isolated Herdr server") {
		t.Fatalf("Worker guard allowed a Herdr group close: err=%v stderr=%s", err, guardErr.String())
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

type workspaceResult struct {
	Workspace struct {
		WorkspaceID string `json:"workspace_id"`
	} `json:"workspace"`
	RootPane struct {
		PaneID string `json:"pane_id"`
		TabID  string `json:"tab_id"`
	} `json:"root_pane"`
}

func createWorkspace(client *herdr.Client, root string) (workspaceResult, error) {
	raw, err := client.Call(context.Background(), "workspace.create", map[string]any{"cwd": root, "label": "posse-e2e", "focus": false})
	if err != nil {
		return workspaceResult{}, err
	}
	var result workspaceResult
	err = json.Unmarshal(raw, &result)
	return result, err
}

func createTab(client *herdr.Client, workspaceID, cwd, label string) (workspaceResult, error) {
	raw, err := client.Call(context.Background(), "tab.create", map[string]any{"workspace_id": workspaceID, "cwd": cwd, "label": label, "focus": false})
	if err != nil {
		return workspaceResult{}, err
	}
	var result workspaceResult
	err = json.Unmarshal(raw, &result)
	return result, err
}

func startServer(t *testing.T, client *herdr.Client) *herdr.ServerProcess {
	t.Helper()
	process, err := client.StartIsolatedServer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopServer(t, client, process) })
	var status herdr.Status
	var statusErr error
	if !waitForCondition(30*time.Second, func() bool {
		status, statusErr = client.Status(context.Background())
		return statusErr == nil && status.Running && status.Protocol >= herdr.MinimumProtocol
	}) {
		serverLog, _ := os.ReadFile(filepath.Join(filepath.Dir(status.Socket), "herdr-server.log"))
		if len(serverLog) > 4000 {
			serverLog = serverLog[len(serverLog)-4000:]
		}
		t.Fatalf("isolated Herdr server did not report running: status=%#v err=%v server log tail:\n%s", status, statusErr, serverLog)
	}
	var snapshot herdr.Snapshot
	var snapshotErr error
	if !waitForCondition(30*time.Second, func() bool {
		snapshot, snapshotErr = client.Snapshot(context.Background())
		return snapshotErr == nil && snapshot.ServerStartedAt != ""
	}) {
		t.Fatalf("isolated Herdr server generation was not observed: generation=%q err=%v", snapshot.ServerStartedAt, snapshotErr)
	}
	return process
}

func stopServer(t *testing.T, client *herdr.Client, process *herdr.ServerProcess) {
	t.Helper()
	if process == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = client.StopIsolatedServer(ctx)
	done := make(chan struct{})
	go func() {
		_ = process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = process.Kill()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
		}
	}
}

func killServer(t *testing.T, process *herdr.ServerProcess) {
	t.Helper()
	if process == nil {
		return
	}
	if err := process.Kill(); err != nil {
		t.Fatalf("kill isolated Herdr server: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("isolated Herdr server did not exit after SIGKILL")
	}
}

func runPosse(t *testing.T, binary, cwd string, env []string, args ...string) string {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Dir = cwd
	command.Env = env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("posse %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func hasArgPair(args []string, key, value string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == key && args[index+1] == value {
			return true
		}
	}
	return false
}

func initRepository(t *testing.T, root, remote string, env []string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, env, root, "init", "-b", "main")
	gitTest(t, env, root, "config", "user.name", "Posse E2E")
	gitTest(t, env, root, "config", "user.email", "posse-e2e@example.test")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, env, root, "add", "README.md", ".gitignore")
	gitTest(t, env, root, "commit", "-m", "fixture")
	if err := os.MkdirAll(remote, 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, env, remote, "init", "--bare")
	gitTest(t, env, root, "remote", "add", "origin", remote)
	gitTest(t, env, root, "push", "-u", "origin", "main")
	gitTest(t, env, root, "remote", "set-head", "origin", "main")
}

func assertWorkerSleepsStopped(t *testing.T, pidFile string) {
	t.Helper()
	contents, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read fake Worker sleep PIDs: %v", err)
	}
	pids := []int{}
	for _, value := range strings.Fields(string(contents)) {
		pid, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("invalid fake Worker sleep PID %q: %v", value, err)
		}
		pids = append(pids, pid)
	}
	if len(pids) == 0 {
		t.Fatal("fake Worker did not start a leftover sleep process")
	}
	if !waitForCondition(30*time.Second, func() bool {
		for _, pid := range pids {
			stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
			if err != nil {
				continue
			}
			closeParen := strings.LastIndexByte(string(stat), ')')
			fields := strings.Fields(string(stat[closeParen+1:]))
			if len(fields) > 0 && fields[0] != "Z" && fields[0] != "X" {
				return false
			}
		}
		return true
	}) {
		t.Fatalf("leftover Worker sleep processes are still running: %s", contents)
	}
}

func gitTest(t *testing.T, env []string, cwd string, args ...string) string {
	t.Helper()
	stdout, stderr, err := gitCommandOutput(env, cwd, args...)
	if err != nil {
		t.Fatalf("git %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout, stderr)
	}
	return stdout
}

func gitCommand(env []string, cwd string, args ...string) (string, error) {
	stdout, stderr, err := gitCommandOutput(env, cwd, args...)
	if err != nil {
		return stdout + stderr, err
	}
	return stdout, nil
}

func gitCommandOutput(env []string, cwd string, args ...string) (string, string, error) {
	command := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	command.Env = env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func waitForCondition(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return condition()
}

func setEnv(env []string, key, value string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name != key {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}

var e2eGoCache struct {
	sync.Once
	mod, build string
	err        error
}

// Herdr and Posse state must be isolated, but Go's download and build caches
// must not be recreated in every temporary test HOME. Go makes directories
// under the default module cache read-only, so RemoveAll would also leave
// hundreds of megabytes behind after each E2E run.
func isolatedE2EEnv(t *testing.T, root string) []string {
	t.Helper()
	e2eGoCache.Do(func() {
		output, err := exec.Command("go", "env", "-json", "GOMODCACHE", "GOCACHE").Output()
		if err != nil {
			e2eGoCache.err = err
			return
		}
		var caches struct {
			Mod   string `json:"GOMODCACHE"`
			Build string `json:"GOCACHE"`
		}
		if err := json.Unmarshal(output, &caches); err != nil {
			e2eGoCache.err = err
			return
		}
		e2eGoCache.mod, e2eGoCache.build = caches.Mod, caches.Build
	})
	if e2eGoCache.err != nil || e2eGoCache.mod == "" || e2eGoCache.build == "" {
		t.Fatalf("resolve shared Go caches: %v", e2eGoCache.err)
	}
	env := herdr.IsolatedTestEnvironment(root)
	env = setEnv(env, "GOMODCACHE", e2eGoCache.mod)
	return setEnv(env, "GOCACHE", e2eGoCache.build)
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate module source")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}
