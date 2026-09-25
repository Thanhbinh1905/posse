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

// TestWorkspaceProjectRegistersRidesAndLandsAcrossMembers drives a folder shaped
// like a multi-repository stack through the real CLI in an isolated Herdr: `posse
// up` in the plain folder asks once and starts the Lead, one Task changes two
// members, and it Lands only after both are merged. A second unregistered
// single repository registers after one confirmation too.
func TestWorkspaceProjectRegistersRidesAndLandsAcrossMembers(t *testing.T) {
	root := newFixtureRoot(t, herdr.TestRootName())
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	posseBinary := filepath.Join(binDir, "posse")
	build := exec.Command("go", "build", "-o", posseBinary, "./cmd/posse")
	build.Dir = moduleRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse: %v\n%s", err, output)
	}
	stableDir := newFixtureRootAt(t, "/var/tmp", fixturePrefix("installed-"))
	stableBinary := filepath.Join(stableDir, "posse")
	contents, err := os.ReadFile(posseBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stableBinary, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	remuda := filepath.Join(root, "posse", "remuda")
	leadLog := filepath.Join(root, "lead.log")
	workerLog := filepath.Join(root, "worker.log")
	// The fake Worker commits in every member its launch file lists, then reports done.
	fakeAgent := `#!/bin/sh
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
case "$PWD/" in
  "$POSSE_E2E_REMUDA/"*)
    IFS= read -r prompt || exit 0
    launch=${prompt#Read }
    launch=${launch% and follow it.}
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
    for member in $(sed -n 's/^- ` + "`" + `\([^/]*\)\/` + "`" + `.*/\1/p' "$launch"); do
      (cd "$member" && printf 'change\n' > worker-change.txt && git add worker-change.txt && git commit -qm "worker change in $member") >> "$POSSE_E2E_WORKER_LOG" 2>&1
    done
    # posse moves the Task to working only after it confirms the prompt arrived.
    tries=0
    until posse holler done "Changed every listed member" >> "$POSSE_E2E_WORKER_LOG" 2>&1; do
      tries=$((tries + 1)); [ "$tries" -lt 100 ] || break; sleep 0.2
    done
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
    while IFS= read -r line; do :; done
    ;;
  *)
    printf 'lead %s in %s\n' "$*" "$PWD" >> "$POSSE_E2E_LEAD_LOG"
    while IFS= read -r line; do printf '%s\n' "$line" >> "$POSSE_E2E_LEAD_LOG"; done
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(fakeAgent), 0o700); err != nil {
		t.Fatal(err)
	}
	env := herdr.IsolatedTestEnvironment(root)
	for key, value := range map[string]string{
		"POSSE_TEST_ROOT": root, "POSSE_E2E_REMUDA": remuda, "POSSE_E2E_LEAD_LOG": leadLog, "POSSE_E2E_WORKER_LOG": workerLog,
		"PATH":            binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GIT_AUTHOR_NAME": "Posse E2E", "GIT_AUTHOR_EMAIL": "posse-e2e@example.test", "GIT_COMMITTER_NAME": "Posse E2E", "GIT_COMMITTER_EMAIL": "posse-e2e@example.test",
	} {
		env = setEnv(env, key, value)
	}
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(root, "posse"), filepath.Join(root, "claude", "skills"), filepath.Join(root, "codex"), filepath.Join(root, "home")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config := "[lead]\nkind = \"claude\"\n\n[defaults]\nlanding_mode = \"local\"\nauto_unsaddle = \"landed\"\n\n[profiles.deep]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"deep\"\n"
	if err := os.WriteFile(filepath.Join(root, "posse", "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	// A sample stack: shared context at the root, two
	// members with an origin, one without, and a folder of extra worktrees.
	workspace := filepath.Join(root, "stack")
	for _, member := range []string{"backend", "worker", "e2e-tool"} {
		repo := filepath.Join(workspace, member)
		if err := os.MkdirAll(repo, 0o700); err != nil {
			t.Fatal(err)
		}
		gitTest(t, env, repo, "init", "-q", "-b", "master")
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte(member+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, env, repo, "add", "README.md")
		gitTest(t, env, repo, "commit", "-qm", "initial "+member)
		if member != "e2e-tool" {
			remote := filepath.Join(root, "remotes", member+".git")
			gitTest(t, env, root, "init", "-q", "--bare", remote)
			gitTest(t, env, repo, "remote", "add", "origin", remote)
			gitTest(t, env, repo, "push", "-q", "-u", "origin", "master")
			gitTest(t, env, repo, "remote", "set-head", "origin", "master")
		}
	}
	gitTest(t, env, filepath.Join(workspace, "backend"), "worktree", "add", "-q", "--detach", filepath.Join(workspace, ".worktrees", "backend-spike"))
	if err := os.MkdirAll(filepath.Join(workspace, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, text := range map[string]string{"CLAUDE.md": "Backend changes often span worker.\n", "docs/overview.md": "overview\n", ".env": "TOKEN=1\n"} {
		if err := os.WriteFile(filepath.Join(workspace, path), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	single := filepath.Join(root, "single")
	if err := os.MkdirAll(single, 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, env, single, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(single, "README.md"), []byte("single\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, env, single, "add", "README.md")
	gitTest(t, env, single, "commit", "-qm", "initial")

	client := herdr.NewWithEnv("herdr", env)
	startServer(t, client)
	if _, err := client.Run(context.Background(), "integration", "install", "claude"); err != nil {
		t.Fatal(err)
	}
	runPosse(t, posseBinary, moduleRoot(t), env, "setup", "--binary", stableBinary)

	answerUp := func(dir, name string) workspaceResult {
		t.Helper()
		pane, err := createWorkspace(client, dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Run(context.Background(), "pane", "run", pane.RootPane.PaneID, "posse up"); err != nil {
			t.Fatal(err)
		}
		if output, err := client.Run(context.Background(), "pane", "wait-output", pane.RootPane.PaneID, "--match", "[Y/n]", "--timeout", "30000"); err != nil {
			t.Fatalf("posse up did not ask to register %s: %v %s", dir, err, output)
		}
		if _, err := client.Run(context.Background(), "pane", "send-text", pane.RootPane.PaneID, "y"); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Run(context.Background(), "pane", "send-keys", pane.RootPane.PaneID, "Enter"); err != nil {
			t.Fatal(err)
		}
		if !waitForCondition(30*time.Second, func() bool {
			log, _ := os.ReadFile(leadLog)
			return strings.Contains(string(log), "in "+dir)
		}) {
			screen, _ := client.Run(context.Background(), "pane", "read", pane.RootPane.PaneID, "--lines", "40")
			t.Fatalf("Lead for %s did not start:\n%s", name, screen)
		}
		return pane
	}
	leadPane := answerUp(workspace, "stack")
	screen, _ := client.Run(context.Background(), "pane", "read", leadPane.RootPane.PaneID, "--lines", "40")
	for _, want := range []string{"kind: workspace", "backend,master", "e2e-tool,master,none,local", "worker,master"} {
		if !strings.Contains(string(screen), want) {
			t.Fatalf("posse up detection lacks %q:\n%s", want, screen)
		}
	}
	answerUp(single, "single")

	db, err := store.Open(filepath.Join(root, "posse"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.ProjectByName(context.Background(), "stack")
	if err != nil || !project.IsWorkspace() {
		t.Fatalf("workspace Project = %#v, %v", project, err)
	}
	if repo, err := db.ProjectByName(context.Background(), "single"); err != nil || repo.IsWorkspace() {
		t.Fatalf("single repository Project = %#v, %v", repo, err)
	}
	members, err := db.ProjectRepos(context.Background(), project.ID)
	if err != nil || len(members) != 3 {
		t.Fatalf("members = %#v, %v", members, err)
	}
	if !waitForCondition(30*time.Second, func() bool {
		current, err := db.ProjectByID(context.Background(), project.ID)
		return err == nil && current.LeadPaneID == leadPane.RootPane.PaneID
	}) {
		t.Fatal("the calling pane did not become the workspace Lead")
	}

	leadEnv := setEnv(env, "HERDR_ENV", "1")
	leadEnv = setEnv(leadEnv, "HERDR_PANE_ID", leadPane.RootPane.PaneID)
	leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", leadPane.Workspace.WorkspaceID)
	brief := filepath.Join(root, "ship.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: Span backend and worker\ndone_when: both members have a commit\nrepos: [backend, worker]\n---\nChange both members.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, posseBinary, filepath.Join(workspace, "worker"), leadEnv, "ride", "--name", "span-backend-worker", "--brief", brief); !strings.Contains(output, "task: t1") {
		t.Fatalf("ride: %s", output)
	}
	var task store.Task
	if !waitForCondition(60*time.Second, func() bool {
		task, err = db.Task(context.Background(), project.ID, "t1")
		return err == nil && task.State == store.StateDone
	}) {
		log, _ := os.ReadFile(workerLog)
		t.Fatalf("Worker did not finish: state=%s log=%s", task.State, log)
	}
	for _, path := range []string{"CLAUDE.md", "docs/overview.md", "backend/worker-change.txt", "worker/worker-change.txt"} {
		if _, err := os.Stat(filepath.Join(task.WorktreePath, path)); err != nil {
			t.Fatalf("Mount lacks %s: %v", path, err)
		}
	}
	for _, absent := range []string{"e2e-tool", ".worktrees/backend-spike"} {
		if _, err := os.Stat(filepath.Join(task.WorktreePath, absent)); !os.IsNotExist(err) {
			t.Fatalf("Mount must not contain %s: %v", absent, err)
		}
	}
	if output := runPosse(t, posseBinary, workspace, leadEnv, "land", "t1"); !strings.Contains(output, "state: landing") || !strings.Contains(output, "backend,local,gated") || !strings.Contains(output, "worker,local,gated") {
		t.Fatalf("land: %s", output)
	}
	if output := runPosse(t, posseBinary, workspace, leadEnv, "land", "t1", "--merge", "--user-approved", "merge both"); !strings.Contains(output, "state: landed") || !strings.Contains(output, "teardown: torn-down") {
		t.Fatalf("land --merge: %s", output)
	}
	for _, member := range []string{"backend", "worker"} {
		subject := gitTest(t, env, filepath.Join(workspace, member), "log", "-1", "--format=%s", "refs/heads/master")
		if strings.TrimSpace(subject) != "worker change in "+member {
			t.Fatalf("%s master = %q", member, subject)
		}
		if branches := gitTest(t, env, filepath.Join(workspace, member), "branch", "--list", "posse/t1"); strings.TrimSpace(branches) != "" {
			t.Fatalf("%s kept the Task branch", member)
		}
	}
	task, err = db.Task(context.Background(), project.ID, "t1")
	if err != nil || task.State != store.StateTornDown {
		t.Fatalf("Task after Land = %s, %v", task.State, err)
	}
}
