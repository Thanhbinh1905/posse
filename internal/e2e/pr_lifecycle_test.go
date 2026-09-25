//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestPRLandingLifecycleAndExternalMerge(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	brief := filepath.Join(fixture.root, "ship.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nCreate a committed change for the PR lifecycle E2E.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	first := fixture.rideAndComplete(t, brief, "t1")
	if first.PRURL != "https://github.com/acme/shop/pull/17" || first.GatedSHA != "" || first.Branch != "posse/pr-lifecycle-change" || first.PaneLabel != "posse:shop:t1" || first.AgentName != "posse-shop-t1-1" {
		t.Fatalf("Worker did not publish its PR before the Lead's Gate: %#v", first)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.env, fixture.remote, "show-ref", "--hash", "refs/heads/posse/pr-lifecycle-change")); got != strings.TrimSpace(gitTest(t, fixture.env, first.WorktreePath, "rev-parse", "HEAD")) {
		t.Fatalf("Worker did not push its own Task branch: %s", got)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t1"); !strings.Contains(output, "landing") {
		t.Fatalf("first PR did not open: %s", output)
	}
	first = fixture.mustTask(t, "t1")
	createdCalls, err := os.ReadFile(fixture.ghLog)
	if err != nil || !strings.Contains(string(createdCalls), "--title PR lifecycle change") || strings.Contains(string(createdCalls), "--title t1") {
		t.Fatalf("forge title did not state the Task work: %q, %v", createdCalls, err)
	}
	if first.PRURL != "https://github.com/acme/shop/pull/17" || first.GatedSHA == "" {
		t.Fatalf("first gated PR was not recorded: %#v", first)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.env, fixture.remote, "show-ref", "--hash", "refs/heads/posse/pr-lifecycle-change")); got != first.GatedSHA {
		t.Fatalf("PR push head=%s, gated SHA=%s", got, first.GatedSHA)
	}

	fixture.writeGraphQL(t, "pr1", "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", first.GatedSHA)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	fixture.requireNotice(t, "t1", "pr_checks_failed")

	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Fix the failing unit check and report done again."); !strings.Contains(output, "delivered") {
		t.Fatalf("PR fix instruction was not delivered: %s", output)
	}
	if err := os.WriteFile(filepath.Join(fixture.fixGate, "t1"), []byte("continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.waitTaskState(t, "t1", store.StateDone)
	first = fixture.mustTask(t, "t1")
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t1"); !strings.Contains(output, "landing") {
		t.Fatalf("fixed PR did not pass the Gate and return to landing: %s", output)
	}
	first = fixture.mustTask(t, "t1")
	fixture.writeGraphQL(t, "pr1", "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", first.GatedSHA)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	fixture.requireNotice(t, "t1", "land_ready")
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t1", "--merge", "--user-approved", "User approved the PR merge"); !strings.Contains(output, "landing") {
		t.Fatalf("approved PR merge did not call GitHub: %s", output)
	}
	if _, err := os.Stat(fixture.ghMerge); err != nil {
		t.Fatalf("fake GitHub did not record the approved merge call: %v", err)
	}
	mergeOne := fixture.mergeOnLocalOrigin(t, first, "t1")
	fixture.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", mergeOne, first.GatedSHA)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	first = fixture.mustTask(t, "t1")
	if first.State != store.StateTornDown || first.LandedRef != mergeOne {
		t.Fatalf("merged PR did not land and auto-teardown: %#v, merge=%s", first, mergeOne)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.env, fixture.repo, "rev-parse", "refs/heads/main")); got != mergeOne {
		t.Fatalf("Project checkout head=%s, merge commit=%s", got, mergeOne)
	}
	fixture.requireNotice(t, "t1", "pr_merged")

	secondBrief := filepath.Join(fixture.root, "ship-second.md")
	if err := os.WriteFile(secondBrief, []byte("---\ntype: ship\ntitle: External merge change\ndone_when: second committed change exists\n---\nCreate a second committed change for external merge observation.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secondPublished := fixture.rideAndComplete(t, secondBrief, "t2")
	if secondPublished.PRURL != "https://github.com/acme/shop/pull/18" || secondPublished.GatedSHA != "" || secondPublished.Branch != "posse/external-merge-change" {
		t.Fatalf("second Worker did not publish its own PR: %#v", secondPublished)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t2"); !strings.Contains(output, "landing") {
		t.Fatalf("second PR did not open: %s", output)
	}
	second := fixture.mustTask(t, "t2")
	mergeTwo := fixture.mergeOnLocalOrigin(t, second, "t2")
	fixture.writeGraphQL(t, "pr2", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", mergeTwo, second.GatedSHA)
	time.Sleep(5 * time.Millisecond)
	offlineEnv := setEnv(fixture.leadEnv, "HERDR_SOCKET_PATH", filepath.Join(fixture.root, "offline-herdr.sock"))
	runPosse(t, fixture.binary, fixture.repo, offlineEnv, "show", "t2")
	second = fixture.mustTask(t, "t2")
	if second.State != store.StateLanded || second.LandedRef != mergeTwo {
		t.Fatalf("merge observed without Herdr did not leave the Task landed: %#v, merge=%s", second, mergeTwo)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t2")
	second = fixture.mustTask(t, "t2")
	if second.State != store.StateTornDown || second.LandedRef != mergeTwo {
		t.Fatalf("external PR merge was not observed and torn down: %#v, merge=%s", second, mergeTwo)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.env, fixture.repo, "rev-parse", "refs/heads/main")); got != mergeTwo {
		t.Fatalf("Project checkout was not synced after external merge: got=%s want=%s", got, mergeTwo)
	}
	fixture.requireNotice(t, "t2", "pr_merged")
	ghCalls, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(ghCalls), "pr create") != 2 || strings.Count(string(ghCalls), "pr merge") != 1 {
		t.Fatalf("unexpected fake GitHub call sequence: %s", ghCalls)
	}
	if !strings.Contains(string(ghCalls), "--match-head-commit "+first.GatedSHA) {
		t.Fatalf("PR merge was not pinned to the gated head: %s", ghCalls)
	}
	if err := fixture.db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPRLandingAcceptsFollowUpBeforeFailureNotice(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	brief := filepath.Join(fixture.root, "follow-up.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR follow-up\ndone_when: follow-up is committed\n---\nAllow follow-up work while the PR is landing.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	published := fixture.rideAndComplete(t, brief, "t1")
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t1"); !strings.Contains(output, "landing") {
		t.Fatalf("PR did not enter landing: %s", output)
	}
	landing := fixture.mustTask(t, "t1")
	fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", landing.GatedSHA)

	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.TaskID == landing.ID && (notice.Kind == "pr_checks_failed" || notice.Kind == "pr_changes_requested" || notice.Kind == "pr_conflict") {
			t.Fatalf("failure Notice exists before follow-up send: %#v", notice)
		}
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Add the expanded follow-up work."); !strings.Contains(output, "delivered") {
		t.Fatalf("follow-up send before a failure Notice was not delivered: %s", output)
	}
	working := fixture.mustTask(t, "t1")
	if working.State != store.StateWorking || working.GatedSHA != "" || working.PRURL != landing.PRURL || published.PRURL != landing.PRURL {
		t.Fatalf("follow-up did not return the same PR Task to working and clear its gate: published=%#v landing=%#v working=%#v", published, landing, working)
	}

	if err := os.WriteFile(filepath.Join(fixture.fixGate, "t1"), []byte("continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.waitTaskState(t, "t1", store.StateDone)
	completed := fixture.mustTask(t, "t1")
	fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", strings.TrimSpace(gitTest(t, fixture.env, fixture.remote, "rev-parse", "refs/heads/posse/pr-follow-up")))
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t1"); !strings.Contains(output, "landing") {
		t.Fatalf("follow-up could not be re-gated: %s", output)
	}
	relanded := fixture.mustTask(t, "t1")
	if reLandedState := relanded.State; reLandedState != store.StateLanding || relanded.GatedSHA == "" || relanded.PRURL != landing.PRURL || completed.State != store.StateDone {
		t.Fatalf("Task did not return to landing after the follow-up: completed=%#v relanded=%#v", completed, relanded)
	}
}

func TestPRCreateCrashRecoveryAdoptsOpenPullRequest(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	brief := filepath.Join(fixture.root, "crash-ship.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR create recovery\ndone_when: committed change exists\n---\nExercise adoption of a PR created before Posse persisted its URL.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Simulate a Task completed before Workers published their own PRs.
	// The compatibility path still lets Land recover a create interrupted
	// after the forge accepted it but before Posse persisted its URL.
	fixture.rideAndCommitLegacy(t, brief, "t1")
	crashEnv := setEnv(fixture.leadEnv, "POSSE_INTENT_CRASH_AT", "land --open-pr:after:pr.create")
	command := exec.Command(fixture.binary, "land", "t1")
	command.Dir = fixture.repo
	command.Env = crashEnv
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("Land did not crash at the injected post-create boundary: err=%v output=%s", err, output)
	}
	task := fixture.mustTask(t, "t1")
	openPRs, err := json.Marshal([]map[string]string{{
		"url": "https://github.com/acme/shop/pull/17", "headRefName": task.Branch, "headRefOid": task.GatedSHA,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.ghOpenPRs, openPRs, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", task.GatedSHA)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	recovered := fixture.mustTask(t, "t1")
	if recovered.State != store.StateLanding || recovered.PRURL != "https://github.com/acme/shop/pull/17" {
		t.Fatalf("open PR was not adopted after the create crash: %#v", recovered)
	}
	calls, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "pr create") != 1 || strings.Count(string(calls), "pr list") < 2 {
		t.Fatalf("unexpected lookup/create recovery calls: %s", calls)
	}
	fixture.requireNotice(t, "t1", "pr_opened")
	if err := fixture.db.Close(); err != nil {
		t.Fatal(err)
	}
}

type prLifecycleFixture struct {
	root      string
	repo      string
	remote    string
	home      string
	binary    string
	env       []string
	leadEnv   []string
	fixGate   string
	ghLog     string
	ghState   string
	ghOpenPRs string
	ghMerge   string
	db        *store.DB
	project   store.Project
}

func newPRLifecycleFixture(t *testing.T) *prLifecycleFixture {
	t.Helper()
	root := newFixtureRoot(t, fixturePrefix("pr-"))
	binDir := filepath.Join(root, "bin")
	worktrees := filepath.Join(root, "posse", "remuda")
	fixGate := filepath.Join(root, "fix-gates")
	for _, directory := range []string{binDir, worktrees, fixGate} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, directory := range []string{filepath.Join(root, "claude", "skills"), filepath.Join(root, "codex")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := isolatedE2EEnv(t, root)
	env = setEnv(env, "POSSE_TEST_ROOT", root)
	env = setEnv(env, "POSSE_E2E_WORKTREES", worktrees)
	env = setEnv(env, "POSSE_E2E_FIX_GATE", fixGate)
	env = setEnv(env, "POSSE_E2E_SIGNAL_GATE", filepath.Join(root, "signal-gates"))
	env = setEnv(env, "POSSE_E2E_POSSE_BIN", filepath.Join(binDir, "posse"))
	env = setEnv(env, "POSSE_TEST_GH_LOG", filepath.Join(root, "gh.log"))
	env = setEnv(env, "POSSE_TEST_GH_STATE", filepath.Join(root, "gh-state.json"))
	env = setEnv(env, "POSSE_TEST_GH_OPEN_PRS", filepath.Join(root, "gh-open-prs.json"))
	env = setEnv(env, "POSSE_TEST_GH_MERGE", filepath.Join(root, "gh-merge-called"))
	env = setEnv(env, "POSSE_TEST_REMOTE", filepath.Join(root, "origin.git"))
	env = setEnv(env, "POSSE_TEST_GH_FAIL", "")
	env = setEnv(env, "PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for key, value := range map[string]string{
		"GIT_AUTHOR_NAME": "Posse E2E", "GIT_AUTHOR_EMAIL": "posse-e2e@example.test",
		"GIT_COMMITTER_NAME": "Posse E2E", "GIT_COMMITTER_EMAIL": "posse-e2e@example.test",
	} {
		env = setEnv(env, key, value)
	}
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}

	module := moduleRoot(t)
	binary := filepath.Join(binDir, "posse")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = module
	build.Env = env // isolatedE2EEnv shares the parent Go caches.
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse for PR E2E: %v\n%s", err, output)
	}
	claude := filepath.Join(binDir, "claude")
	agentScript := `#!/bin/sh
set -eu
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
case "$PWD/" in
  "$POSSE_E2E_WORKTREES/"*)
    IFS= read -r prompt || exit 0
    launch_path=${prompt#Read }
    launch_path=${launch_path% and follow it.}
    task_id=$(basename "$(dirname "$launch_path")")
    printf 'worker change %s\n' "$task_id" > "e2e-worker-$task_id.txt"
    git add "e2e-worker-$task_id.txt"
    git commit -m "worker change $task_id" >/dev/null
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
    while [ ! -e "$POSSE_E2E_SIGNAL_GATE/$task_id" ]; do sleep 0.02; done
    case "$task_id" in t1) pr_url='https://github.com/acme/shop/pull/17' ;; t2) pr_url='https://github.com/acme/shop/pull/18' ;; esac
    "$POSSE_E2E_POSSE_BIN" publish "Worker committed $task_id" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
    "$POSSE_E2E_POSSE_BIN" holler done "Worker committed $task_id" --pr "$pr_url" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
    while IFS= read -r instruction; do
      herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
      printf 'fix change %s\n' "$task_id" > "e2e-fix-$task_id.txt"
      git add "e2e-fix-$task_id.txt"
      git commit -m "fix worker change $task_id" >/dev/null
      while [ ! -e "$POSSE_E2E_FIX_GATE/$task_id" ]; do sleep 0.02; done
      "$POSSE_E2E_POSSE_BIN" publish "Worker fixed $task_id" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
      "$POSSE_E2E_POSSE_BIN" holler done "Worker fixed $task_id" --pr "$pr_url" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
      herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
    done
    ;;
  *)
    while IFS= read -r line; do :; done
    ;;
esac
`
	if err := os.WriteFile(claude, []byte(agentScript), 0o700); err != nil {
		t.Fatal(err)
	}
	gh := filepath.Join(binDir, "gh")
	ghScript := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$POSSE_TEST_GH_LOG"
case "$1 $2" in
  "api graphql") cat "$POSSE_TEST_GH_STATE" ;;
  "pr list")
    case " $* " in *' --head posse/pr-lifecycle-change '*) branch=posse/pr-lifecycle-change; number=17 ;; *' --head posse/pr-follow-up '*) branch=posse/pr-follow-up; number=17 ;; *' --head posse/pr-create-recovery '*) branch=posse/pr-create-recovery; number=17 ;; *' --head posse/external-merge-change '*) branch=posse/external-merge-change; number=18 ;; *) exit 90 ;; esac
    if grep -q "/pull/$number" "$POSSE_TEST_GH_OPEN_PRS"; then
      head=$(git --git-dir="$POSSE_TEST_REMOTE" rev-parse "refs/heads/$branch")
      printf '[{"url":"https://github.com/acme/shop/pull/%s","headRefName":"%s","headRefOid":"%s"}]\n' "$number" "$branch" "$head"
    else
      printf '[]\n'
    fi ;;
  "pr create")
    case " $* " in *' --head posse/pr-lifecycle-change '*) branch=posse/pr-lifecycle-change; number=17 ;; *' --head posse/pr-follow-up '*) branch=posse/pr-follow-up; number=17 ;; *' --head posse/pr-create-recovery '*) branch=posse/pr-create-recovery; number=17 ;; *' --head posse/external-merge-change '*) branch=posse/external-merge-change; number=18 ;; *) exit 90 ;; esac
    head=$(git --git-dir="$POSSE_TEST_REMOTE" rev-parse "refs/heads/$branch")
    url="https://github.com/acme/shop/pull/$number"
    printf '[{"url":"%s","headRefName":"%s","headRefOid":"%s"}]\n' "$url" "$branch" "$head" > "$POSSE_TEST_GH_OPEN_PRS"
    printf '%s\n' "$url" ;;
  "pr view")
    case "$3" in */17) branch=$(git --git-dir="$POSSE_TEST_REMOTE" for-each-ref --format='%(refname:short)' 'refs/heads/posse/pr-*' | head -1) ;; */18) branch=posse/external-merge-change ;; *) exit 90 ;; esac
    head=$(git --git-dir="$POSSE_TEST_REMOTE" rev-parse "refs/heads/$branch")
    printf '{"url":"%s","state":"OPEN","headRefOid":"%s","headRefName":"%s","baseRefName":"main","headRepository":{"nameWithOwner":"acme/shop"}}\n' "$3" "$head" "$branch" ;;
  "pr merge") : > "$POSSE_TEST_GH_MERGE"; printf 'Merged\n' ;;
  *) printf 'unexpected fake gh command: %s\n' "$*" >&2; exit 90 ;;
esac
`
	if err := os.WriteFile(gh, []byte(ghScript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "gh-open-prs.json"), []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "gh-state.json"), []byte(`{"data":{"repo1":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	home := filepath.Join(root, "posse")
	configText := "[lead]\nkind = \"claude\"\n\n[defaults]\nlanding_mode = \"pr\"\nauto_unsaddle = \"finished\"\npr_poll = \"1ms\"\n\n[profiles.deep]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"deep\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	remote := filepath.Join(root, "origin.git")
	initRepository(t, repo, remote, env)
	gitTest(t, env, repo, "remote", "set-url", "origin", "https://github.com/acme/shop.git")
	gitTest(t, env, repo, "config", "url.file://"+remote+".insteadOf", "https://github.com/acme/shop.git")
	if err := os.MkdirAll(filepath.Join(root, "signal-gates"), 0o700); err != nil {
		t.Fatal(err)
	}
	client := herdr.NewWithEnv("herdr", env)
	startServer(t, client)
	if err := client.CheckProtocol(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "integration", "install", "claude"); err != nil {
		t.Fatalf("install Claude integration in isolated Herdr: %v", err)
	}
	workspace, err := createWorkspace(client, repo)
	if err != nil {
		t.Fatal(err)
	}
	callerEnv := setEnv(env, "HERDR_ENV", "1")
	callerEnv = setEnv(callerEnv, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	callerEnv = setEnv(callerEnv, "HERDR_WORKSPACE_ID", workspace.Workspace.WorkspaceID)
	callerEnv = setEnv(callerEnv, "HERDR_TAB_ID", workspace.RootPane.TabID)
	runPosse(t, binary, repo, callerEnv, "up", "--name", "shop", "--yes")
	opened, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := opened.ProjectByName(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	leadEnv := setEnv(callerEnv, "HERDR_PANE_ID", project.LeadPaneID)
	leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", project.HerdrWorkspaceID)
	if _, err := client.Run(context.Background(), "pane", "report-agent", project.LeadPaneID, "--source", "posse.fake", "--agent", "claude", "--state", "working"); err != nil {
		t.Fatalf("mark isolated Lead busy: %v", err)
	}
	return &prLifecycleFixture{
		root: root, repo: repo, remote: remote, home: home, binary: binary,
		env: env, leadEnv: leadEnv, fixGate: fixGate, ghLog: filepath.Join(root, "gh.log"),
		ghState: filepath.Join(root, "gh-state.json"), ghOpenPRs: filepath.Join(root, "gh-open-prs.json"),
		ghMerge: filepath.Join(root, "gh-merge-called"), db: opened, project: project,
	}
}

func (fixture *prLifecycleFixture) rideAndComplete(t *testing.T, brief, taskID string) store.Task {
	t.Helper()
	name := "pr-lifecycle-change"
	if taskID == "t2" {
		name = "external-merge-change"
	} else if strings.Contains(brief, "follow-up") {
		name = "pr-follow-up"
	}
	output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "ride", "--brief", brief, "--name", name)
	if !strings.Contains(output, taskID) {
		t.Fatalf("ride did not return %s: %s", taskID, output)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "signal-gates", taskID), []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.waitTaskState(t, taskID, store.StateDone)
	return fixture.mustTask(t, taskID)
}

func (fixture *prLifecycleFixture) rideAndCommitLegacy(t *testing.T, brief, taskID string) {
	t.Helper()
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "ride", "--brief", brief, "--name", "pr-create-recovery")
	if !waitForCondition(30*time.Second, func() bool {
		task := fixture.mustTask(t, taskID)
		if task.WorktreePath == "" {
			return false
		}
		output, err := exec.Command("git", "-C", task.WorktreePath, "rev-list", "--count", task.BaseRef+".."+task.Branch).CombinedOutput()
		return err == nil && strings.TrimSpace(string(output)) == "1"
	}) {
		t.Fatal("legacy Worker did not commit its change")
	}
	task := fixture.mustTask(t, taskID)
	if _, err := fixture.db.AddSignal(context.Background(), task.ID, "done", "Legacy Worker committed "+taskID, nil); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(context.Background(), task.ID, store.StateWorking, store.StateDone, "worker", "Legacy Worker committed "+taskID); err != nil {
		t.Fatal(err)
	}
}

func (fixture *prLifecycleFixture) mustTask(t *testing.T, taskID string) store.Task {
	t.Helper()
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, taskID)
	if err != nil {
		t.Fatalf("load %s: %v", taskID, err)
	}
	return task
}

func (fixture *prLifecycleFixture) waitTaskState(t *testing.T, taskID string, want store.State) {
	t.Helper()
	if !waitForCondition(30*time.Second, func() bool {
		task, err := fixture.db.Task(context.Background(), fixture.project.ID, taskID)
		return err == nil && task.State == want
	}) {
		task, _ := fixture.db.Task(context.Background(), fixture.project.ID, taskID)
		log, _ := os.ReadFile(filepath.Join(fixture.root, "worker-delivery.log"))
		t.Fatalf("Task %s did not reach %s: %#v, Worker delivery: %s", taskID, want, task, log)
	}
}

func (fixture *prLifecycleFixture) writeGraphQL(t *testing.T, alias, state, checks, review, mergeable, mergeCommit, head string) {
	t.Helper()
	contexts := []any{}
	if checks == "FAILURE" {
		contexts = append(contexts, map[string]any{"__typename": "CheckRun", "name": "unit", "status": "COMPLETED", "conclusion": "FAILURE", "detailsUrl": "https://checks.example/unit"})
	}
	pull := map[string]any{
		"url":   "https://github.com/acme/shop/pull/" + map[string]string{"pr1": "17", "pr2": "18"}[alias],
		"state": state, "headRefOid": head, "mergeable": mergeable, "reviewDecision": review,
		"mergeCommit":       map[string]any{"oid": mergeCommit},
		"statusCheckRollup": map[string]any{"state": checks, "contexts": map[string]any{"nodes": contexts}},
	}
	encoded, err := json.Marshal(map[string]any{"data": map[string]any{"repo1": map[string]any{alias: pull}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.ghState, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fixture *prLifecycleFixture) requireNotice(t *testing.T, taskID, kind string) {
	t.Helper()
	task := fixture.mustTask(t, taskID)
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.TaskID == task.ID && notice.Kind == kind {
			return
		}
	}
	t.Fatalf("Task %s has no %s Notice: %#v", taskID, kind, notices)
}

func (fixture *prLifecycleFixture) mergeOnLocalOrigin(t *testing.T, task store.Task, taskID string) string {
	t.Helper()
	checkout := filepath.Join(fixture.root, "external-merge-"+taskID)
	gitTest(t, fixture.env, fixture.root, "clone", "--branch", "main", fixture.remote, checkout)
	gitTest(t, fixture.env, checkout, "config", "user.name", "Posse E2E")
	gitTest(t, fixture.env, checkout, "config", "user.email", "posse-e2e@example.test")
	gitTest(t, fixture.env, checkout, "fetch", "origin", "refs/heads/"+task.Branch+":refs/remotes/origin/"+task.Branch)
	gitTest(t, fixture.env, checkout, "merge", "--squash", "refs/remotes/origin/"+task.Branch)
	gitTest(t, fixture.env, checkout, "commit", "-m", "external merge "+taskID)
	gitTest(t, fixture.env, checkout, "push", "origin", "HEAD:refs/heads/main")
	return strings.TrimSpace(gitTest(t, fixture.env, checkout, "rev-parse", "HEAD"))
}
