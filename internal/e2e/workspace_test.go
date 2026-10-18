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
    mode=$(cat "$POSSE_TEST_ROOT/worker-mode" 2>/dev/null || true)
    rm -f "$POSSE_TEST_ROOT/worker-mode"
    case "$mode" in
      mixed-scout)
        if [ -e backend/.git ] && [ -e e2e-tool/.git ] && [ ! -e worker/.git ] && [ -r CLAUDE.md ] && [ ! -w CLAUDE.md ]; then
          printf 'mixed scope and read-only root verified\n' >> "$POSSE_E2E_WORKER_LOG"
        else
          printf 'mixed scope or read-only root mismatch\n' >> "$POSSE_E2E_WORKER_LOG"
        fi
        printf 'mixed workspace report\n' > "$TMPDIR/report.md"
        tries=0
        until posse holler done "Mixed-member Scout report ready" --report "$TMPDIR/report.md" >> "$POSSE_E2E_WORKER_LOG" 2>&1; do
          tries=$((tries + 1)); [ "$tries" -lt 100 ] || break; sleep 0.2
        done
        ;;
      root-only)
        member_worktree=false
        for member in */; do [ ! -e "$member/.git" ] || member_worktree=true; done
        if [ "$member_worktree" = false ] && [ -r CLAUDE.md ] && [ ! -w CLAUDE.md ]; then
          printf 'root-only Scout has no editable Members\n' >> "$POSSE_E2E_WORKER_LOG"
        else
          printf 'root-only Scout scope mismatch\n' >> "$POSSE_E2E_WORKER_LOG"
        fi
        printf 'root-only workspace report\n' > "$TMPDIR/report.md"
        tries=0
        until posse holler done "Root-only Scout report ready" --report "$TMPDIR/report.md" >> "$POSSE_E2E_WORKER_LOG" 2>&1; do
          tries=$((tries + 1)); [ "$tries" -lt 100 ] || break; sleep 0.2
        done
        ;;
      root-only-hold)
        trap ': > "$POSSE_TEST_ROOT/root-scout-interrupted"' INT
        trap 'posse holler failed "Rider stopped for approved discard" > "$POSSE_TEST_ROOT/root-scout-stop.log" 2>&1; exit 0' TERM
        member_worktree=false
        for member in */; do [ ! -e "$member/.git" ] || member_worktree=true; done
        if [ "$member_worktree" = false ] && [ -r CLAUDE.md ] && [ ! -w CLAUDE.md ]; then
          printf 'active root-only Scout has no editable Members\n' >> "$POSSE_E2E_WORKER_LOG"
        else
          printf 'active root-only workspace scope mismatch\n' >> "$POSSE_E2E_WORKER_LOG"
        fi
        : > "$POSSE_TEST_ROOT/root-scout-active"
        while :; do sleep 0.02; done
        ;;
      *)
        for member in $(sed -n 's/^- ` + "`" + `\([^/]*\)\/` + "`" + `.*/\1/p' "$launch"); do
          (cd "$member" && printf 'change\n' > worker-change.txt && git add worker-change.txt && git commit -qm "worker change in $member") >> "$POSSE_E2E_WORKER_LOG" 2>&1
        done
        # posse moves the Task to working only after it confirms the prompt arrived.
        tries=0
        until posse holler done "Changed every listed member" >> "$POSSE_E2E_WORKER_LOG" 2>&1; do
          tries=$((tries + 1)); [ "$tries" -lt 100 ] || break; sleep 0.2
        done
        ;;
    esac
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
	config := "[lead]\nkind = \"claude\"\n\n[defaults]\nlanding_mode = \"local\"\nauto_unsaddle = \"landed\"\n\n[review_loop]\nenabled = false\n\n[profiles.deep]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"deep\"\n"
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
	if err := os.WriteFile(filepath.Join(root, "worker-mode"), []byte("ship\n"), 0o600); err != nil {
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
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil || plan.Execution.Scope != "workspace" || plan.Execution.Mode != "mount" || len(plan.Execution.Members) != 2 {
		t.Fatalf("Workspace admission lost scope: %#v %v", plan, err)
	}
	defaultBranches := map[string]string{}
	for _, member := range members {
		defaultBranches[member.Name] = member.DefaultBranch
	}
	for index, name := range []string{"backend", "worker"} {
		member := plan.Execution.Members[index]
		branch := defaultBranches[name]
		if branch == "" || member.Name != name || member.Path != name || member.Base != "refs/remotes/origin/"+branch || member.Target != branch || member.Delivery != "local" {
			t.Fatalf("Workspace admission changed Member contract: %#v", member)
		}
	}
	legacyMount := task.WorktreePath
	for _, path := range []string{"backend/worker-change.txt", "worker/worker-change.txt"} {
		if _, err := os.Stat(filepath.Join(task.WorktreePath, path)); err != nil {
			t.Fatalf("Mount lacks %s: %v", path, err)
		}
	}
	for _, absent := range []string{"CLAUDE.md", "docs/overview.md", "e2e-tool", ".worktrees/backend-spike"} {
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
	if _, err := os.Stat(filepath.Join(legacyMount, "worker", ".git")); err != nil {
		t.Fatalf("legacy Mount did not retain its previous Member checkout: %v", err)
	}

	projectConfig := filepath.Join(root, "posse", "projects", "stack", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfig, []byte("[repositories.backend]\nlanding_mode = \"pr\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, env, filepath.Join(workspace, "worker"), "branch", "posse/inspect-mixed-scope")
	mixedBrief := filepath.Join(root, "mixed-scout.md")
	if err := os.WriteFile(mixedBrief, []byte("---\ntype: scout\ntitle: Inspect mixed scope\ndone_when: report is saved\nrepos: [backend, e2e-tool]\nroot_context: read-only\n---\nInspect only the selected Members.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "worker-mode"), []byte("mixed-scout\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, posseBinary, workspace, leadEnv, "ride", "--name", "inspect-mixed-scope", "--brief", mixedBrief); !strings.Contains(output, "task: t2") {
		t.Fatalf("ride mixed-member Scout: %s", output)
	}
	var mixedTask store.Task
	if !waitForCondition(60*time.Second, func() bool {
		mixedTask, err = db.Task(context.Background(), project.ID, "t2")
		return err == nil && mixedTask.State == store.StateReported
	}) {
		log, _ := os.ReadFile(workerLog)
		t.Fatalf("mixed-member Scout did not finish: state=%s log=%s", mixedTask.State, log)
	}
	mixedPlan, err := mixedTask.AdmittedPlan()
	if err != nil || mixedPlan == nil || mixedPlan.Execution.RootContext != "read-only" || len(mixedPlan.Execution.Members) != 2 {
		t.Fatalf("mixed-member plan = %#v, %v", mixedPlan, err)
	}
	mixedModes := map[string]string{}
	for _, member := range mixedPlan.Execution.Members {
		mixedModes[member.Name] = member.Delivery
	}
	if mixedModes["backend"] != "pr" || mixedModes["e2e-tool"] != "local" {
		t.Fatalf("mixed Member delivery modes = %#v", mixedModes)
	}
	if mixedTask.WorktreePath != legacyMount {
		t.Fatalf("legacy Mount was not reused: got %s, want %s", mixedTask.WorktreePath, legacyMount)
	}
	if _, err := os.Stat(filepath.Join(mixedTask.WorktreePath, "worker")); !os.IsNotExist(err) {
		t.Fatalf("unselected legacy Member was not drained: %v", err)
	}

	rootBrief := filepath.Join(root, "root-scout.md")
	if err := os.WriteFile(rootBrief, []byte("---\ntype: scout\ntitle: Inspect root context\ndone_when: report is saved\nroot_context: read-only\n---\nInspect shared-root context without Member checkouts.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "worker-mode"), []byte("root-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, posseBinary, workspace, leadEnv, "ride", "--name", "inspect-root-context", "--brief", rootBrief); !strings.Contains(output, "task: t3") {
		t.Fatalf("ride root-only Scout: %s", output)
	}
	var rootTask store.Task
	if !waitForCondition(60*time.Second, func() bool {
		rootTask, err = db.Task(context.Background(), project.ID, "t3")
		return err == nil && rootTask.State == store.StateReported
	}) {
		log, _ := os.ReadFile(workerLog)
		t.Fatalf("root-only Scout did not finish: state=%s log=%s", rootTask.State, log)
	}
	rootPlan, err := rootTask.AdmittedPlan()
	if err != nil || rootPlan == nil || rootPlan.Execution.RootContext != "read-only" || len(rootPlan.Execution.Members) != 0 {
		t.Fatalf("root-only workspace plan = %#v, %v", rootPlan, err)
	}
	for _, member := range []string{"backend", "worker", "e2e-tool"} {
		if _, err := os.Stat(filepath.Join(rootTask.WorktreePath, member, ".git")); !os.IsNotExist(err) {
			t.Fatalf("root-only Scout received Member %s: %v", member, err)
		}
	}
	projectConfig = filepath.Join(root, "posse", "projects", "stack", "config.toml")
	if err := os.WriteFile(projectConfig, []byte("[defaults]\npr_poll = \"1ms\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	activeBrief := filepath.Join(root, "active-root-scout.md")
	if err := os.WriteFile(activeBrief, []byte("---\ntype: scout\ntitle: Inspect active root context\ndone_when: approved cancellation succeeds\nroot_context: read-only\n---\nInspect shared-root context while PR polling continues.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "worker-mode"), []byte("root-only-hold\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, posseBinary, workspace, leadEnv, "ride", "--name", "active-root-scout", "--brief", activeBrief); !strings.Contains(output, "task: t4") {
		t.Fatalf("ride active root-only Scout: %s", output)
	}
	if !waitForCondition(30*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(root, "root-scout-active"))
		return err == nil
	}) {
		log, _ := os.ReadFile(workerLog)
		t.Fatalf("active root-only Scout did not start: %s", log)
	}
	activeTask, err := db.Task(context.Background(), project.ID, "t4")
	if err != nil || activeTask.State != store.StateWorking {
		t.Fatalf("active root-only Scout state = %s, %v", activeTask.State, err)
	}
	if repos, err := db.TaskRepos(context.Background(), activeTask.ID); err != nil || len(repos) != 0 {
		t.Fatalf("active root-only Scout member repositories = %#v, %v", repos, err)
	}
	var pollBefore int64
	if err := db.QueryRowContext(context.Background(), `SELECT COALESCE((SELECT pr_polled_at FROM project_watch_state WHERE project_id=?), 0)`, project.ID).Scan(&pollBefore); err != nil {
		t.Fatal(err)
	}
	// Make the Project poll due before each normal Lead command, matching the
	// reported failure where a memberless Scout aborted maintenance.
	time.Sleep(10 * time.Millisecond)
	for _, args := range [][]string{
		{},
		{"peek", "t4"},
		{"remuda"},
		{"send", "t4", "Queue a note for the active Scout", "--queue"},
	} {
		runPosse(t, posseBinary, workspace, leadEnv, args...)
		current, err := db.Task(context.Background(), project.ID, "t4")
		if err != nil || current.State != store.StateWorking {
			t.Fatalf("Lead command %v left active root-only Scout in %s: %v", args, current.State, err)
		}
	}
	if !waitForCondition(5*time.Second, func() bool {
		var lastPoll int64
		err := db.QueryRowContext(context.Background(), `SELECT COALESCE((SELECT pr_polled_at FROM project_watch_state WHERE project_id=?), 0)`, project.ID).Scan(&lastPoll)
		return err == nil && lastPoll > pollBefore
	}) {
		t.Fatal("workspace PR polling did not complete after the root-only Scout started")
	}
	discard := exec.Command(posseBinary, "unsaddle", "t4", "--discard", "--user-approved", "User approved discarding the active root-only Scout")
	discard.Dir, discard.Env = workspace, leadEnv
	discardOutput, discardErr := discard.CombinedOutput()
	if discardErr != nil || !strings.Contains(string(discardOutput), "state: torn-down") {
		current, taskErr := db.Task(context.Background(), project.ID, "t4")
		transitions, transitionErr := db.TaskTransitions(context.Background(), activeTask.ID, 20)
		t.Fatalf("discard active root-only Scout: err=%v output=%s current=%#v taskErr=%v transitions=%#v transitionErr=%v", discardErr, discardOutput, current, taskErr, transitions, transitionErr)
	}
	if current, err := db.Task(context.Background(), project.ID, "t4"); err != nil || current.State != store.StateTornDown {
		t.Fatalf("active root-only Scout after discard = %s, %v", current.State, err)
	}
	log, _ := os.ReadFile(workerLog)
	for _, proof := range []string{"mixed scope and read-only root verified", "root-only Scout has no editable Members", "active root-only Scout has no editable Members"} {
		if !strings.Contains(string(log), proof) {
			t.Fatalf("E2E Rider did not verify %q: %s", proof, log)
		}
	}
}
