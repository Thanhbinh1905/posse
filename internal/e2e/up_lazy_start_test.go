//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func appendedLogLines(contents, previous []byte) []string {
	if len(contents) < len(previous) {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(contents[len(previous):])), "\n")
}

func leadMemberCommands(path string, previous []byte, paneID, workspaceRoot, member string) bool {
	contents, err := os.ReadFile(path)
	if err != nil || len(contents) < len(previous) {
		return false
	}
	fetched, resolved := false, false
	for _, line := range appendedLogLines(contents, previous) {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 || fields[0] != paneID || !strings.Contains(fields[2], filepath.Join(workspaceRoot, member)) {
			continue
		}
		fetched = fetched || fields[1] == "fetch"
		resolved = resolved || fields[1] == "ls-remote"
	}
	return fetched && resolved
}

func assertNoCallsFromPane(t *testing.T, path, paneID, kind string) {
	t.Helper()
	calls, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if pane, _, found := strings.Cut(line, "\t"); found && pane == paneID {
			t.Fatalf("posse up ran %s in its caller pane: %s", kind, line)
		}
	}
}

func TestUpStartsRegisteredWorkspaceWithoutRepositoryOrForgeCalls(t *testing.T) {
	root, err := os.MkdirTemp("", "posse-e2e-a14-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove isolated E2E root: %v", err)
		}
	})

	env := isolatedE2EEnv(t, root)
	binDir := filepath.Join(root, "bin")
	for _, directory := range []string{binDir, filepath.Join(root, "posse"), filepath.Join(root, "claude", "skills"), filepath.Join(root, "codex")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	gitLog := filepath.Join(root, "git.log")
	gitCommandLog := filepath.Join(root, "git-commands.log")
	if err := os.WriteFile(gitCommandLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	forgeLog := filepath.Join(root, "forge.log")
	leadStarted := filepath.Join(root, "lead-started")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitScript := "#!/bin/sh\nset -eu\nprintf '%s\\t%s\\n' \"${HERDR_PANE_ID:-}\" \"$PWD $*\" >> \"$POSSE_TEST_GIT_COMMAND_LOG\"\nevent=\nfor arg in \"$@\"; do\n  case \"$arg\" in fetch|ls-remote) event=$arg; break ;; esac\ndone\nif [ -n \"$event\" ]; then\n  printf '%s\\t%s\\t%s\\n' \"${HERDR_PANE_ID:-}\" \"$event\" \"$PWD $*\" >> \"$POSSE_TEST_GIT_LOG\"\n  case \"$event:$*\" in\n    fetch:*offline*) exit 1 ;;\n    ls-remote:*offline*) exit 0 ;;\n  esac\nfi\nexec \"$POSSE_REAL_GIT\" \"$@\"\n"
	for name, script := range map[string]string{
		"git":    gitScript,
		"gh":     "#!/bin/sh\nprintf '%s\\tgh %s\\n' \"${HERDR_PANE_ID:-}\" \"$*\" >> \"$POSSE_TEST_FORGE_LOG\"\nexit 1\n",
		"glab":   "#!/bin/sh\nprintf '%s\\tglab %s\\n' \"${HERDR_PANE_ID:-}\" \"$*\" >> \"$POSSE_TEST_FORGE_LOG\"\nexit 1\n",
		"claude": "#!/bin/sh\nherdr pane report-agent \"$HERDR_PANE_ID\" --source posse.test --agent claude --state idle >/dev/null 2>&1 || true\ncase \"$PWD/\" in \"$POSSE_HOME/remuda/\"*) while IFS= read -r line; do herdr pane report-agent \"$HERDR_PANE_ID\" --source posse.test --agent claude --state working >/dev/null 2>&1 || true; done ;; *) printf 'started\\n' > \"$POSSE_TEST_LEAD_STARTED\"; while IFS= read -r line; do printf '%s\\n' \"$line\" >> \"$POSSE_TEST_ROOT/lead.log\"; done ;; esac\n",
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range map[string]string{
		"PATH":                       binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"POSSE_TEST_ROOT":            root,
		"POSSE_TEST_GIT_LOG":         gitLog,
		"POSSE_TEST_GIT_COMMAND_LOG": gitCommandLog,
		"POSSE_TEST_FORGE_LOG":       forgeLog,
		"POSSE_TEST_LEAD_STARTED":    leadStarted,
		"POSSE_REAL_GIT":             realGit,
		"GIT_AUTHOR_NAME":            "Posse E2E",
		"GIT_AUTHOR_EMAIL":           "posse-e2e@example.test",
		"GIT_COMMITTER_NAME":         "Posse E2E",
		"GIT_COMMITTER_EMAIL":        "posse-e2e@example.test",
	} {
		env = setEnv(env, key, value)
	}
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}

	workspaceRoot := filepath.Join(root, "stack")
	goodRepo := filepath.Join(workspaceRoot, "good")
	offlineRepo := filepath.Join(workspaceRoot, "offline")
	goodRemote := filepath.Join(root, "good-origin.git")
	initRepository(t, goodRepo, goodRemote, env)
	gitTest(t, env, goodRepo, "remote", "set-url", "origin", "https://github.com/acme/good.git")
	gitTest(t, env, goodRepo, "config", "url.file://"+goodRemote+".insteadOf", "https://github.com/acme/good.git")
	if err := os.MkdirAll(offlineRepo, 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, env, offlineRepo, "init", "-b", "main")
	gitTest(t, env, offlineRepo, "config", "user.name", "Posse E2E")
	gitTest(t, env, offlineRepo, "config", "user.email", "posse-e2e@example.test")
	if err := os.WriteFile(filepath.Join(offlineRepo, "README.md"), []byte("offline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, env, offlineRepo, "add", "README.md")
	gitTest(t, env, offlineRepo, "commit", "-m", "fixture")
	gitTest(t, env, offlineRepo, "remote", "add", "origin", "https://192.0.2.1/acme/offline.git")

	db, err := store.Open(filepath.Join(root, "posse"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.CreateWorkspaceProject(context.Background(), "stack", workspaceRoot, []store.ProjectRepo{
		{Name: "good", Path: "good", DefaultBranch: "main", OriginHost: "github.com"},
		{Name: "offline", Path: "offline", DefaultBranch: "main", OriginHost: "192.0.2.1"},
	})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "posse", "config.toml"), []byte("[lead]\nkind = \"claude\"\n\n[defaults]\nlanding_mode = \"pr\"\npr_poll = \"1ms\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	binary := filepath.Join(binDir, "posse")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = moduleRoot(t)
	build.Env = env
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse: %v\n%s", err, output)
	}

	client := herdr.NewWithEnv("herdr", env)
	startServer(t, client)
	if _, err := client.Run(context.Background(), "integration", "install", "claude"); err != nil {
		t.Fatal(err)
	}
	workspace, err := createWorkspace(client, workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	callerEnv := setEnv(env, "HERDR_ENV", "1")
	callerEnv = setEnv(callerEnv, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	callerEnv = setEnv(callerEnv, "HERDR_WORKSPACE_ID", workspace.Workspace.WorkspaceID)
	callerEnv = setEnv(callerEnv, "HERDR_TAB_ID", workspace.RootPane.TabID)
	commandsBeforeUp, err := os.ReadFile(gitCommandLog)
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now()
	if _, err := client.Run(context.Background(), "pane", "run", workspace.RootPane.PaneID, "posse up --name stack --yes"); err != nil {
		t.Fatalf("run posse up in isolated caller pane: %v", err)
	}
	if !waitForCondition(5*time.Second, func() bool {
		_, err := os.Stat(leadStarted)
		return err == nil
	}) {
		screen, _ := client.Run(context.Background(), "pane", "read", workspace.RootPane.PaneID, "--lines", "40")
		t.Fatalf("Lead did not start; pane output:\n%s", screen)
	}
	if elapsed := time.Since(startedAt); elapsed > 5*time.Second {
		t.Fatalf("posse up took %s, want under 5s", elapsed)
	} else {
		t.Logf("posse up started the Lead in %s", elapsed)
	}
	assertNoCallsFromPane(t, gitLog, workspace.RootPane.PaneID, "Git fetch")
	assertNoCallsFromPane(t, forgeLog, workspace.RootPane.PaneID, "forge CLI")
	commandsAfterUp, err := os.ReadFile(gitCommandLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range appendedLogLines(commandsAfterUp, commandsBeforeUp) {
		if pane, command, found := strings.Cut(line, "\t"); found && pane == workspace.RootPane.PaneID && (strings.Contains(command, goodRepo) || strings.Contains(command, offlineRepo)) {
			t.Fatalf("posse up inspected a Member repository: %s", line)
		}
	}
	t.Log("posse up made no Member Git, fetch, or forge CLI calls")

	db, err = store.Open(filepath.Join(root, "posse"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.ProjectByName(context.Background(), "stack")
	if err != nil {
		t.Fatal(err)
	}
	repos, err := db.ProjectRepos(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	originHosts := map[string]string{}
	for _, repo := range repos {
		originHosts[repo.Name] = repo.OriginHost
	}
	if originHosts["good"] != "github.com" || originHosts["offline"] != "192.0.2.1" {
		t.Fatalf("saved workspace Member origin hosts: %#v", originHosts)
	}
	if !waitForCondition(15*time.Second, func() bool {
		state, err := db.ProjectRepoWatchState(context.Background(), project.ID, "offline")
		return err == nil && state.CheckoutStatus == "root_behind" && state.CheckoutCheckedAt > 0
	}) {
		state, _ := db.ProjectRepoWatchState(context.Background(), project.ID, "offline")
		t.Fatalf("Lookout did not record the offline Member's checkout status: %+v", state)
	}
	show := runPosse(t, binary, workspaceRoot, env, "project", "show", "stack")
	if !strings.Contains(show, "root_behind") || !strings.Contains(show, "could not fetch origin") {
		t.Fatalf("Project show did not expose the background per-Member checkout status: %s", show)
	}
	notices, err := db.Notices(context.Background(), project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	foundOfflineNotice := false
	for _, notice := range notices {
		if notice.Kind == "root_behind" && strings.Contains(notice.Summary, "offline") {
			foundOfflineNotice = true
			break
		}
	}
	if !foundOfflineNotice {
		t.Fatalf("offline Member has no background checkout Notice: %#v", notices)
	}
	t.Log("Lookout recorded offline checkout root_behind with a Member-specific Notice")

	brief := filepath.Join(root, "good-member-ship.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: fix(good): verify lazy member fetch\ndone_when: the reachable Member Task starts\nrepos: [good]\n---\nChange only the reachable Member.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	leadEnv := setEnv(callerEnv, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	before, _ := os.ReadFile(gitLog)
	if output := runPosse(t, binary, workspaceRoot, leadEnv, "ride", "--brief", brief, "--name", "verify-lazy-member"); !strings.Contains(output, "task: t1") {
		t.Fatalf("Ship Task on the reachable Member did not start: %s", output)
	}
	if !waitForCondition(10*time.Second, func() bool {
		return leadMemberCommands(gitLog, before, workspace.RootPane.PaneID, workspaceRoot, "good")
	}) {
		calls, _ := os.ReadFile(gitLog)
		t.Fatalf("Ship Task did not fetch and resolve only its reachable Member: %s", calls)
	}
	calls, err := os.ReadFile(gitLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range appendedLogLines(calls, before) {
		if pane, event, found := strings.Cut(line, "\t"); found && pane == workspace.RootPane.PaneID && event == "fetch" && strings.Contains(line, "offline") {
			t.Fatalf("Ship Task touched the unrequested offline Member: %s", line)
		}
	}
	t.Log("Ship Task fetched and resolved only the requested reachable Member")
}
