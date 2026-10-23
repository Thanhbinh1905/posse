//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/atomicfile"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestHistoricalMultiIssuePublishKeepsClosingLinks(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	briefPath := filepath.Join(fixture.root, "change.md")
	initial := "---\ntype: change\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nCreate a committed change.\n"
	if err := os.WriteFile(briefPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.runStartWithStoreBusyRetry(t, briefPath, "pr-lifecycle-change")
	// This fixture represents an old Task, not a newly admitted contract.
	stopBackgroundWatch(t, fixture)
	task := fixture.mustTask(t, "t1")
	tx, err := fixture.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`UPDATE tasks SET plan_json='' WHERE id=?`,
		`DELETE FROM task_plan_revisions WHERE task_id=?`,
		`UPDATE task_launch_identities SET plan_revision=0 WHERE task_id=?`,
	} {
		if _, err := tx.ExecContext(context.Background(), statement, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.PersistTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}

	stored := filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "brief.md")
	historical := strings.Replace(initial, "done_when: committed change exists\n", "done_when: committed change exists\nissues: [12, 16]\nrefs: [18]\n", 1)
	if err := os.WriteFile(stored, []byte(historical), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "signal-gates", "t1"), []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.waitTaskState(t, "t1", store.StateDone)
	if after, err := os.ReadFile(stored); err != nil || !bytes.Equal(after, []byte(historical)) {
		t.Fatalf("historical Brief was rewritten: %s %v", after, err)
	}
	calls, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Closes #12", "Closes #16", "Refs #18"} {
		if !strings.Contains(string(calls), expected) {
			t.Errorf("historical publish lost %q: %s", expected, calls)
		}
	}
}

func TestPublishRefreshesReusedPRMetadata(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	brief := filepath.Join(fixture.root, "change.md")
	initialBrief := "---\ntype: change\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nCreate a committed change for the PR refresh E2E.\n"
	if err := os.WriteFile(brief, []byte(initialBrief), 0o600); err != nil {
		t.Fatal(err)
	}
	output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", brief, "--name", "pr-lifecycle-change")
	if !strings.Contains(output, "t1") {
		t.Fatalf("start did not return t1: %s", output)
	}
	task := fixture.mustTask(t, "t1")
	if !waitForCondition(15*time.Second, func() bool {
		count := strings.TrimSpace(gitTest(t, fixture.env, task.WorktreePath, "rev-list", "--count", task.BaseRef+".."+task.Branch))
		return count == "1"
	}) {
		t.Fatal("Worker did not commit its change before publish")
	}
	workerEnv := setEnv(fixture.leadEnv, "HERDR_PANE_ID", task.PaneID)
	workerEnv = setEnv(workerEnv, "HERDR_WORKSPACE_ID", task.HerdrWorkspaceID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", fixture.home)
	publish := func(env []string, args ...string) string {
		t.Helper()
		command := exec.Command(fixture.binary, append([]string{"publish"}, args...)...)
		command.Dir, command.Env = task.WorktreePath, env
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("posse publish %v: %v\n%s", args, err, output)
		}
		return string(output)
	}
	first := []string{"First summary", "--verify", "go test ./... -> pass", "--proof", "first proof", "--risk", "Risk: first\nRollback: first rollback"}
	publish(workerEnv, first...)
	legacyBody := stripManagedPublishedMarkers(readTestFile(t, fixture.ghBody))
	if err := os.WriteFile(fixture.ghBody, []byte("Maintainer note before.\n\n"+legacyBody+"\n\n## Maintainer notes\n\nKeep this paragraph.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "retitle", "t1", "feat: Updated PR title")
	second := []string{"Second summary", "--verify", "go test ./... -> second pass", "--proof", "second proof", "--risk", "Risk: second\nRollback: second rollback"}
	beforeConflict := readTestFile(t, fixture.ghBody)
	conflict := exec.Command(fixture.binary, append([]string{"publish"}, second...)...)
	conflict.Dir, conflict.Env = task.WorktreePath, workerEnv
	conflictOutput, conflictErr := conflict.CombinedOutput()
	if conflictErr == nil || !strings.Contains(string(conflictOutput), "pr_body_conflict") {
		t.Fatalf("legacy metadata refresh did not require --refresh: %v\n%s", conflictErr, conflictOutput)
	}
	if afterConflict := readTestFile(t, fixture.ghBody); afterConflict != beforeConflict {
		t.Fatalf("conflicting legacy PR was changed without explicit refresh: %s", afterConflict)
	}
	callsAfterConflict := readTestFile(t, fixture.ghLog)
	if strings.Count(callsAfterConflict, "pr create") != 1 || strings.Contains(callsAfterConflict, "pr edit") {
		t.Fatalf("legacy conflict created or edited a PR: %s", callsAfterConflict)
	}
	second = append([]string{"--refresh"}, second...)
	interruptedEnv := setEnv(workerEnv, "POSSE_TEST_GH_KILL_AFTER_EDIT", "1")
	interrupted := exec.Command(fixture.binary, append([]string{"publish"}, second...)...)
	interrupted.Dir, interrupted.Env = task.WorktreePath, interruptedEnv
	if output, err := interrupted.CombinedOutput(); err == nil {
		t.Fatalf("fake forge interruption did not terminate publish after applying its update: %s", output)
	}
	if !strings.Contains(readTestFile(t, fixture.ghBody), "Second summary") {
		t.Fatal("fake forge interruption happened before the metadata update")
	}
	publish(workerEnv, second...)
	body := readTestFile(t, fixture.ghBody)
	if !strings.HasPrefix(body, beforeConflict) {
		t.Fatalf("legacy PR description changed before the appended managed section: %s", body)
	}
	managed := managedPublishedSection(body)
	if managed == "" {
		t.Fatalf("re-published PR has no managed section: %s", body)
	}
	for _, expected := range []string{"Second summary", "go test ./... -> second pass", "second proof", "Risk: second", "Rollback: second rollback", "Maintainer note before.", "Keep this paragraph."} {
		if !strings.Contains(body, expected) {
			t.Errorf("re-published PR body omitted %q: %s", expected, body)
		}
	}
	for _, stale := range []string{"First summary", "go test ./... -> pass", "first proof", "Risk: first", "first rollback"} {
		if strings.Contains(managed, stale) {
			t.Errorf("re-published managed section retained stale %q: %s", stale, managed)
		}
	}
	if title := readTestFile(t, fixture.ghTitle); title != "feat: Updated PR title" {
		t.Errorf("re-published PR title=%q want %q", title, "feat: Updated PR title")
	}
	calls := readTestFile(t, fixture.ghLog)
	if strings.Count(calls, "pr create") != 1 || strings.Count(calls, "pr edit") != 1 {
		t.Errorf("interrupted refresh was not idempotently adopted: %s", calls)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "signal-gates", "t1"), []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.waitTaskState(t, "t1", store.StateDone)
	if err := fixture.db.Close(); err != nil {
		t.Fatal(err)
	}
	t205PRBodyMarkerCases(t)
	t208SnapshotRebuildCases(t)
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func managedPublishedSection(body string) string {
	const startPrefix = "<!-- posse:publish:start:"
	start := strings.Index(body, startPrefix)
	if start < 0 {
		return ""
	}
	lineEnd := strings.IndexByte(body[start:], '\n')
	if lineEnd < 0 {
		return ""
	}
	marker := strings.TrimSuffix(body[start:start+lineEnd], "\r")
	token := strings.TrimSuffix(strings.TrimPrefix(marker, startPrefix), " -->")
	if token == "" {
		return ""
	}
	endMarker := "<!-- posse:publish:end:" + token + " -->"
	end := strings.Index(body[start+lineEnd:], endMarker)
	if end < 0 {
		return ""
	}
	end += start + lineEnd
	return body[start : end+len(endMarker)]
}

func stripManagedPublishedMarkers(body string) string {
	section := managedPublishedSection(body)
	if section == "" {
		return body
	}
	lineEnd := strings.IndexByte(section, '\n')
	marker := strings.TrimSuffix(section[:lineEnd], "\r")
	const startPrefix = "<!-- posse:publish:start:"
	const endPrefix = "<!-- posse:publish:end:"
	token := strings.TrimSuffix(strings.TrimPrefix(marker, startPrefix), " -->")
	startMarker, endMarker := marker, endPrefix+token+" -->"
	body = strings.Replace(body, startMarker+"\n", "", 1)
	return strings.Replace(body, endMarker, "", 1)
}

func TestNativeTicketBlockersFailClosedBeforeWorkerDispatchE2E(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	if err := os.WriteFile(filepath.Join(fixture.root, "gh-blockers.json"), []byte(`[[{"number":27,"state":"open","repository_url":"https://api.github.com/repos/acme/shop","html_url":"https://github.com/acme/shop/issues/27"}]]`), 0o600); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(fixture.root, "ticket.md")
	if err := os.WriteFile(brief, []byte(`---
type: change
title: Blocked ticket
done_when: blocker is closed
ticket: 12
---
Do not dispatch while blocked.
`), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(fixture.binary, "start", "--brief", brief)
	command.Dir, command.Env = fixture.repo, fixture.leadEnv
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), `"ticket_blocked"`) {
		t.Fatalf("open native blocker did not prevent dispatch: err=%v output=%s", err, output)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "gh-blocker-fail"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	command = exec.Command(fixture.binary, "start", "--brief", brief)
	command.Dir, command.Env = fixture.repo, fixture.leadEnv
	output, err = command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), `"ticket_blockers_unavailable"`) {
		t.Fatalf("unavailable native blocker endpoint did not fail closed: err=%v output=%s", err, output)
	}
	tasks, err := fixture.db.Tasks(context.Background(), fixture.project.ID, true)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("blocked ticket allocated a Task: tasks=%#v err=%v", tasks, err)
	}
	sandboxes, err := fixture.db.Sandboxes(context.Background(), fixture.project.ID)
	if err != nil || len(sandboxes) != 0 {
		t.Fatalf("blocked ticket allocated a Sandbox: sandboxes=%#v err=%v", sandboxes, err)
	}
	log := string(readTestFile(t, fixture.ghLog))
	if !strings.Contains(log, "dependencies/blocked_by") {
		t.Fatalf("fake forge did not receive native blocker query: %s", log)
	}
}

func TestPRMergeLifecycleAndExternalMerge(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	gitTest(t, fixture.env, fixture.repo, "branch", "staging", "main")
	gitTest(t, fixture.env, fixture.repo, "push", "origin", "staging")
	baseMain := strings.TrimSpace(gitTest(t, fixture.env, fixture.repo, "rev-parse", "refs/heads/main"))
	if err := os.WriteFile(filepath.Join(fixture.root, "gh-base-branch"), []byte(`staging
`), 0o600); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(fixture.root, "change.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR lifecycle change\ndone_when: committed change exists\nbase: staging\ntarget: staging\n---\nCreate a committed change for the PR lifecycle E2E.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	first := fixture.startAndComplete(t, brief, "t1")
	if first.PRURL != "https://github.com/acme/shop/pull/17" || first.GatedSHA != "" || first.Branch != "posse/t1" || first.PaneLabel != "posse:shop:t1" || first.AgentName != "posse-shop-t1-1" || first.TargetBranch != "staging" || first.BaseRef != strings.TrimSpace(gitTest(t, fixture.env, fixture.repo, "rev-parse", "refs/remotes/origin/staging")) {
		t.Fatalf("Worker did not publish its PR before the Lead's Gate: %#v", first)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.env, fixture.remote, "show-ref", "--hash", "refs/heads/posse/t1")); got != strings.TrimSpace(gitTest(t, fixture.env, first.WorktreePath, "rev-parse", "HEAD")) {
		t.Fatalf("Worker did not push its own Task branch: %s", got)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t1"); !strings.Contains(output, "merging") {
		t.Fatalf("first PR did not open: %s", output)
	}
	first = fixture.mustTask(t, "t1")
	createdCalls, err := os.ReadFile(fixture.ghLog)
	if err != nil || !strings.Contains(string(createdCalls), "--title PR lifecycle change") || !strings.Contains(string(createdCalls), "--base staging") || strings.Contains(string(createdCalls), "--title t1") {
		t.Fatalf("forge title did not state the Task work: %q, %v", createdCalls, err)
	}
	if first.PRURL != "https://github.com/acme/shop/pull/17" || first.GatedSHA == "" {
		t.Fatalf("first gated PR was not recorded: %#v", first)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.env, fixture.remote, "show-ref", "--hash", "refs/heads/posse/t1")); got != first.GatedSHA {
		t.Fatalf("PR push head=%s, gated SHA=%s", got, first.GatedSHA)
	}

	// Make foreground CLI observation authoritative for the forge-state
	// assertions below, rather than racing an independent in-flight poll.
	stopBackgroundWatch(t, fixture)
	fixture.writeGraphQL(t, "pr1", "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", first.GatedSHA)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	fixture.requireNotice(t, "t1", "pr_checks_failed")
	fixture.requireNotice(t, "t1", "ci_fix_exhausted")
	var queued, rounds int
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE task_id=? AND status='queued'`, first.ID).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("disabled CI loop queued %d automatic instructions: %v", queued, err)
	}
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`, first.ID).Scan(&rounds); err != nil || rounds != 0 {
		t.Fatalf("disabled CI loop recorded %d fix rounds: %v", rounds, err)
	}

	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Fix the failing unit check and report done again."); !strings.Contains(output, "delivered") {
		t.Fatalf("PR fix instruction was not delivered: %s", output)
	}
	if err := os.WriteFile(filepath.Join(fixture.fixGate, "t1"), []byte("continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.waitTaskState(t, "t1", store.StateDone)
	first = fixture.mustTask(t, "t1")
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t1"); !strings.Contains(output, "merging") {
		t.Fatalf("fixed PR did not pass the Gate and return to merging: %s", output)
	}
	first = fixture.mustTask(t, "t1")
	fixture.writeGraphQL(t, "pr1", "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", first.GatedSHA)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	fixture.requireNotice(t, "t1", "merge_ready")
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t1", "--execute", "--user-approved", "User approved the PR merge"); !strings.Contains(output, "merging") {
		t.Fatalf("approved PR merge did not call GitHub: %s", output)
	}
	if _, err := os.Stat(fixture.ghMerge); err != nil {
		t.Fatalf("fake GitHub did not record the approved merge call: %v", err)
	}
	mergeOne := fixture.mergeOnLocalOrigin(t, first, "t1")
	fixture.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", mergeOne, first.GatedSHA)
	showAfterActiveTeardown(t, fixture, "t1")
	first = fixture.mustTask(t, "t1")
	if first.State != store.StateTornDown || first.MergedRef != mergeOne {
		t.Fatalf("merged PR did not merge and auto-teardown: %#v, merge=%s", first, mergeOne)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.env, fixture.repo, "rev-parse", "refs/heads/staging")); got != mergeOne {
		t.Fatalf("staging checkout head=%s, merge commit=%s", got, mergeOne)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.env, fixture.repo, "rev-parse", "refs/heads/main")); got != baseMain {
		t.Fatalf("syncing the frozen target changed the default branch: main=%s want=%s", got, baseMain)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "gh-base-branch"), []byte(`main
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", fixture.repo, "show-ref", "--verify", "refs/heads/"+first.Branch).CombinedOutput(); err == nil {
		t.Fatalf("merged PR Teardown kept Task branch %s: %s", first.Branch, output)
	}
	fixture.requireNotice(t, "t1", "pr_merged")

	secondBrief := filepath.Join(fixture.root, "change-second.md")
	if err := os.WriteFile(secondBrief, []byte("---\ntype: change\ntitle: External merge change\ndone_when: second committed change exists\n---\nCreate a second committed change for external merge observation.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secondPublished := fixture.startAndComplete(t, secondBrief, "t2")
	if secondPublished.PRURL != "https://github.com/acme/shop/pull/18" || secondPublished.GatedSHA != "" || secondPublished.Branch != "posse/t2" {
		t.Fatalf("second Worker did not publish its own PR: %#v", secondPublished)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t2"); !strings.Contains(output, "merging") {
		t.Fatalf("second PR did not open: %s", output)
	}
	second := fixture.mustTask(t, "t2")
	// The Watch remains stopped so only the offline observer can record this
	// merge; no live observer can advance merged to torn-down before the read.
	mergeTwo := fixture.mergeOnLocalOrigin(t, second, "t2")
	fixture.writeGraphQL(t, "pr2", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", mergeTwo, second.GatedSHA)
	time.Sleep(5 * time.Millisecond)
	offlineEnv := setEnv(fixture.leadEnv, "HERDR_SOCKET_PATH", filepath.Join(fixture.root, "offline-herdr.sock"))
	runPosse(t, fixture.binary, fixture.repo, offlineEnv, "show", "t2")
	fixture.waitTaskState(t, "t2", store.StateMerged)
	second = fixture.mustTask(t, "t2")
	if second.State != store.StateMerged || second.MergedRef != mergeTwo {
		t.Fatalf("merge observed without Herdr did not leave the Task merged: %#v, merge=%s", second, mergeTwo)
	}
	showAfterActiveTeardown(t, fixture, "t2")
	second = fixture.mustTask(t, "t2")
	if second.State != store.StateTornDown || second.MergedRef != mergeTwo {
		t.Fatalf("external PR merge was not observed and torn down: %#v, merge=%s", second, mergeTwo)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.env, fixture.repo, "rev-parse", "refs/heads/main")); got != mergeTwo {
		t.Fatalf("Project checkout was not synced after external merge: got=%s want=%s", got, mergeTwo)
	}
	if output, err := exec.Command("git", "-C", fixture.repo, "show-ref", "--verify", "refs/heads/"+second.Branch).CombinedOutput(); err == nil {
		t.Fatalf("externally merged PR Teardown kept Task branch %s: %s", second.Branch, output)
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

func TestExpiredQueuedMessageCrashRaisesNotice(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	// The fixture's poll-only Watch can deliver queued messages as soon as
	// the Worker becomes idle, racing the explicit crash-injected owner below.
	stopBackgroundWatch(t, fixture)
	defer fixture.db.Close()
	brief := filepath.Join(fixture.root, "queued-crash.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nCommit one change for queued message crash recovery.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.startAndComplete(t, brief, "t1")
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t1"); !strings.Contains(output, "merging") {
		t.Fatalf("Task did not enter merging: %s", output)
	}
	candidate := fixture.mustTask(t, "t1")
	fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", candidate.GatedSHA)

	client := herdr.NewWithEnv("herdr", fixture.env)
	awaitAgentState := func(want string) {
		t.Helper()
		if waitForCondition(10*time.Second, func() bool {
			snapshot, err := client.Snapshot(context.Background())
			if err != nil {
				return false
			}
			for _, pane := range snapshot.Panes {
				if pane.PaneID == candidate.PaneID {
					return pane.AgentStatus == want || want == "idle" && pane.AgentStatus == "done"
				}
			}
			return false
		}) {
			return
		}
		snapshot, err := client.Snapshot(context.Background())
		t.Fatalf("Worker did not reach %s: snapshot=%#v err=%v", want, snapshot, err)
	}
	awaitAgentState("idle")
	if _, err := client.Run(context.Background(), "pane", "report-agent", candidate.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "working"); err != nil {
		t.Fatal(err)
	}
	awaitAgentState("working")
	const body = "Check the queued crash recovery"
	queued := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", body, "--queue")
	if !strings.Contains(queued, "state: queued") {
		t.Fatalf("send did not finish with a queued instruction: %s", queued)
	}
	if _, err := client.Run(context.Background(), "pane", "report-agent", candidate.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	awaitAgentState("idle")
	var queuedStatus string
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT status FROM messages WHERE task_id=? AND body=?`, candidate.ID, body).Scan(&queuedStatus); err != nil || queuedStatus != "queued" {
		t.Fatalf("queued instruction changed before the controlled delivery owner: status=%q err=%v", queuedStatus, err)
	}

	owner := exec.Command(fixture.binary, "list")
	owner.Dir = fixture.repo
	owner.Env = setEnv(fixture.leadEnv, "POSSE_INTENT_CRASH_AT", "send:before:message.prompt")
	output, err := owner.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 86 {
		var status string
		_ = fixture.db.QueryRowContext(context.Background(), `SELECT status FROM messages WHERE task_id=? AND body=?`, candidate.ID, body).Scan(&status)
		snapshot, snapshotErr := client.Snapshot(context.Background())
		var paneState string
		var paneFocused bool
		for _, pane := range snapshot.Panes {
			if pane.PaneID == candidate.PaneID {
				paneState, paneFocused = pane.AgentStatus, pane.Focused
			}
		}
		t.Fatalf("delivery owner did not crash before agent.prompt: err=%v output=%s message_status=%s pane_status=%s focused=%t snapshot_err=%v", err, output, status, paneState, paneFocused, snapshotErr)
	}
	message, err := fixture.db.OldestQueuedMessage(context.Background(), candidate.ID)
	if !store.IsNotFound(err) {
		t.Fatalf("crashed message remained queued: %#v, %v", message, err)
	}
	var messageID int64
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT id FROM messages WHERE task_id=? AND body=? AND status='submitting'`, candidate.ID, body).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE messages SET claimed_at=? WHERE id=?`, time.Now().Add(-3*time.Minute).UnixMilli(), messageID); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "list")
	}
	show := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "--json", "show", "t1", "--full")
	for _, expected := range []string{"uncertain_messages", strconv.FormatInt(messageID, 10), body} {
		if !strings.Contains(show, expected) {
			t.Fatalf("Task inspection omitted uncertain instruction %q: %s", expected, show)
		}
	}
	watch := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "watch", "--timeout", "1")
	for _, expected := range []string{"message_delivery_uncertain", "#" + strconv.FormatInt(messageID, 10), body, "posse peek t1", "posse show t1 --full"} {
		if !strings.Contains(watch, expected) {
			t.Fatalf("Watch omitted actionable uncertainty %q: %s", expected, watch)
		}
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	wantInstructions := map[int64]string{messageID: body}
	uncertain := 0
	for _, notice := range notices {
		if notice.Kind != "message_delivery_uncertain" || notice.TaskID != candidate.ID {
			continue
		}
		uncertain++
		var data struct {
			MessageID   int64  `json:"message_id"`
			Instruction string `json:"instruction"`
		}
		if err := json.Unmarshal([]byte(notice.DataJSON), &data); err != nil {
			t.Fatalf("decode uncertainty Notice: %v", err)
		}
		if expected, ok := wantInstructions[data.MessageID]; !ok || expected != data.Instruction {
			t.Fatalf("uncertainty Notice identified the wrong instruction: %#v", notice)
		}
		delete(wantInstructions, data.MessageID)
	}
	if uncertain != 1 || len(wantInstructions) != 0 {
		t.Fatalf("uncertainty Notices = %d, missing instructions %#v", uncertain, wantInstructions)
	}
	var status string
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT status FROM messages WHERE id=?`, messageID).Scan(&status); err != nil || status != "submitting" {
		t.Fatalf("ambiguous submission %d was retried or lost: status=%q err=%v", messageID, status, err)
	}
	if _, err := os.Stat(filepath.Join(candidate.WorktreePath, "e2e-fix-t1.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crashed instruction was delivered despite the no-retry guarantee: err=%v", err)
	}
}

func TestPRMergeRefusesBranchBehindLatestTarget(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	projectConfig := filepath.Join(fixture.home, "projects", "shop", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfig, []byte("[autonomy]\nland = \"auto\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	firstBrief := filepath.Join(fixture.root, "migration-first.md")
	secondBrief := filepath.Join(fixture.root, "migration-second.md")
	for path, title := range map[string]string{firstBrief: "PR lifecycle change", secondBrief: "External merge change"} {
		brief := "---\ntype: change\ntitle: " + title + "\ndone_when: committed change exists\n---\nCreate a migration.\n"
		if err := os.WriteFile(path, []byte(brief), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	first := fixture.startAndComplete(t, firstBrief, "t1")
	second := fixture.startAndComplete(t, secondBrief, "t2")
	if first.AutonomyMerge != "auto" || second.AutonomyMerge != "auto" {
		t.Fatalf("Tasks did not inherit automatic Merge autonomy: %q, %q", first.AutonomyMerge, second.AutonomyMerge)
	}

	addVersion40 := func(task store.Task, name string) {
		t.Helper()
		relative := filepath.Join("internal", "store", "migrations", name)
		path := filepath.Join(task.WorktreePath, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		versionName := strings.TrimPrefix(strings.TrimSuffix(name, ".sql"), "00040_")
		migration := []byte("-- migration version 40\nCREATE TABLE migration_" + versionName + " (id INTEGER PRIMARY KEY);\n")
		if err := os.WriteFile(path, migration, 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, fixture.env, task.WorktreePath, "add", relative)
		gitTest(t, fixture.env, task.WorktreePath, "commit", "-m", "add "+name)
		gitTest(t, fixture.env, task.WorktreePath, "push", "origin", task.Branch)
	}
	addVersion40(first, "00040_first.sql")
	addVersion40(second, "00040_second.sql")

	for _, id := range []string{"t1", "t2"} {
		if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", id); !strings.Contains(output, "merging") {
			t.Fatalf("%s PR did not open: %s", id, output)
		}
	}
	first, second = fixture.mustTask(t, "t1"), fixture.mustTask(t, "t2")
	fixture.writeGraphQL(t, "pr1", "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", first.GatedSHA)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	fixture.writeGraphQL(t, "pr2", "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", second.GatedSHA)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t2")
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t1", "--execute"); !strings.Contains(output, "merging") {
		t.Fatalf("first PR did not merge: %s", output)
	}
	fixture.mergeOnLocalOrigin(t, first, "t1")
	if err := os.Remove(fixture.ghMerge); err != nil {
		t.Fatal(err)
	}

	// Keep the supervised watch active and let its stale-target instruction
	// reopen t2 before checking that Merge preserves the specific refusal.
	fixture.waitTaskState(t, "t2", store.StateWorking)
	beforeSecondMerge := fixture.mustTask(t, "t2")
	command := exec.Command(fixture.binary, "merge", "t2", "--execute")
	command.Dir, command.Env = fixture.repo, fixture.leadEnv
	output, err := command.CombinedOutput()
	if err == nil {
		merge := fixture.mergeOnLocalOrigin(t, second, "t2")
		checkout := filepath.Join(fixture.root, "external-merge-t2")
		files := strings.Fields(gitTest(t, fixture.env, checkout, "ls-tree", "-r", "--name-only", merge, "internal/store/migrations"))
		t.Fatalf("stale PR merged at %s and introduced duplicate migration versions: %v; command output: %s", merge, files, output)
	}
	targetTip := strings.TrimSpace(gitTest(t, fixture.env, fixture.remote, "rev-parse", "refs/heads/main"))
	if !strings.Contains(string(output), `"pr_branch_behind"`) || !strings.Contains(string(output), targetTip) {
		afterSecondMerge, taskErr := fixture.db.Task(context.Background(), fixture.project.ID, "t2")
		transitions, transitionErr := fixture.db.TaskTransitions(context.Background(), second.ID, 30)
		recovery, recoveryErr := fixture.db.ProjectRecovery(context.Background(), fixture.project.ID)
		watch, watchErr := fixture.db.WatchRuntime(context.Background(), fixture.project.ID)
		messageRows, messageErr := fixture.db.QueryContext(context.Background(), `SELECT id,status,body,created_at,delivered_at FROM messages WHERE task_id=? ORDER BY id`, second.ID)
		var messages []string
		if messageErr == nil {
			defer messageRows.Close()
			for messageRows.Next() {
				var id, createdAt int64
				var status, body string
				var deliveredAt sql.NullInt64
				if scanErr := messageRows.Scan(&id, &status, &body, &createdAt, &deliveredAt); scanErr != nil {
					messages = append(messages, "scan error: "+scanErr.Error())
					break
				}
				messages = append(messages, fmt.Sprintf("id=%d status=%s created=%d delivered=%v body=%q", id, status, createdAt, deliveredAt, body))
			}
			if rowsErr := messageRows.Err(); rowsErr != nil {
				messageErr = rowsErr
			}
		}
		t.Fatalf("stale PR refusal did not name the missing target commit: %v\n%s\nmissing target: %s\nTask state before/after: %q -> %q (err=%v)\ntransitions: %#v (err=%v)\nproject recovery: %#v (err=%v)\nWatch: %#v (err=%v)\nmessages: %#v (err=%v)", err, output, targetTip, beforeSecondMerge.State, afterSecondMerge.State, taskErr, transitions, transitionErr, recovery, recoveryErr, watch, watchErr, messages, messageErr)
	}
	var instruction string
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT body FROM messages WHERE task_id=? AND status IN ('queued','claimed','interrupting','submitting','delivered') ORDER BY id DESC LIMIT 1`, second.ID).Scan(&instruction); err != nil || !strings.Contains(instruction, targetTip) || !strings.Contains(instruction, "Merge origin/main") {
		t.Fatalf("Posse did not retain an actionable target-sync instruction: body=%q err=%v", instruction, err)
	}
	if _, err := os.Stat(fixture.ghMerge); !os.IsNotExist(err) {
		t.Fatalf("stale PR reached the forge merge command: %v", err)
	}
	targetFiles := strings.Fields(gitTest(t, fixture.env, fixture.remote, "ls-tree", "-r", "--name-only", "refs/heads/main", "internal/store/migrations"))
	if len(targetFiles) != 1 || targetFiles[0] != "internal/store/migrations/00040_first.sql" {
		t.Fatalf("target branch contains duplicate or unexpected migration versions: %v", targetFiles)
	}
}

func TestExternalMergeDuringFollowUpWithMovedTaskBranch(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	brief := filepath.Join(fixture.root, "follow-up.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR follow-up\ndone_when: committed change exists\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.startAndComplete(t, brief, "t1")
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t1")
	candidate := fixture.mustTask(t, "t1")
	if candidate.PRURL != "https://github.com/acme/shop/pull/17" || candidate.GatedSHA == "" {
		t.Fatalf("PR ownership was not recorded: %#v", candidate)
	}
	fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", candidate.GatedSHA)
	if err := os.WriteFile(filepath.Join(fixture.root, "pause-fix-commit"), []byte("wait"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Handle the follow-up.")
	if current := fixture.mustTask(t, "t1"); current.State != store.StateWorking || current.GatedSHA != "" || current.PRURL != candidate.PRURL {
		t.Fatalf("follow-up did not return to working: %#v", current)
	}
	merge := fixture.mergeOnLocalOrigin(t, candidate, "t1")
	// Other Tasks can Merge on main before the Worker merges origin/main.
	extra := filepath.Join(fixture.root, "external-merge-t1")
	if err := os.WriteFile(filepath.Join(extra, "other-main-work.txt"), []byte("another Task\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.env, extra, "add", "other-main-work.txt")
	gitTest(t, fixture.env, extra, "commit", "-m", "other Task on main")
	gitTest(t, fixture.env, extra, "push", "origin", "HEAD:refs/heads/main")
	// Keep this test ref isolated from origin/main, which Project sync may fetch concurrently.
	gitTest(t, fixture.env, candidate.WorktreePath, "fetch", "--no-write-fetch-head", "--refmap=", "origin", "main:refs/remotes/posse-test/main")
	gitTest(t, fixture.env, candidate.WorktreePath, "merge", "--no-edit", "refs/remotes/posse-test/main")
	movedTip := strings.TrimSpace(gitTest(t, fixture.env, candidate.WorktreePath, "rev-parse", "HEAD"))
	if movedTip == candidate.GatedSHA {
		t.Fatal("follow-up did not move the Task branch tip")
	}
	fixture.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, candidate.GatedSHA)
	showAfterActiveTeardown(t, fixture, "t1")
	current := fixture.mustTask(t, "t1")
	if verified, err := fixture.db.WasVerifiedPRHead(context.Background(), current.ID, current.PRURL, movedTip); err != nil || verified {
		t.Fatalf("moved local tip was recorded as verified PR head: %v %v", verified, err)
	}
	if current.State != store.StateTornDown || current.MergedRef != merge {
		status, _ := gitCommand(fixture.env, candidate.WorktreePath, "status", "--porcelain", "--untracked-files=all", "--ignored")
		diff, diffErr := gitCommand(fixture.env, candidate.WorktreePath, "diff", "--quiet", merge, "HEAD")
		t.Fatalf("externally merged PR was not Merged during follow-up: %#v status=%q diff=%q err=%v", current, status, diff, diffErr)
	}
	fixture.requireNotice(t, "t1", "pr_merged")
	if got := strings.TrimSpace(gitTest(t, fixture.env, fixture.repo, "rev-parse", "refs/heads/"+candidate.Branch)); got != movedTip {
		t.Fatalf("Task branch moved during teardown: got=%s want=%s", got, movedTip)
	}
	if current.PRURL != candidate.PRURL || current.Branch != candidate.Branch {
		t.Fatalf("merged PR identity changed: %#v", current)
	}
	for i := 0; i < 2; i++ {
		runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	}
	// A new CLI process opens the same database after the first observation.
	calls, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "pr create") != 1 || strings.Count(string(calls), "pr merge") != 0 {
		t.Fatalf("watcher created or merged a PR: %s", calls)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	merged := 0
	for _, notice := range notices {
		if notice.TaskID == current.ID && notice.Kind == "pr_merged" {
			merged++
		}
	}
	if merged != 1 {
		t.Fatalf("merge Notices=%d want 1", merged)
	}
}

// A missing old PR must not prevent a merged PR in the same GraphQL batch
// from merging and releasing its Worker, even when gh exits unsuccessfully.
func TestPRPollPartialGraphQLFailureDoesNotStarveMergedWorker(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	brief := filepath.Join(fixture.root, "partial-merge.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nObserve a merge despite an inaccessible old PR.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.startAndComplete(t, brief, "t1")
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t1"); !strings.Contains(output, "merging") {
		t.Fatalf("valid PR did not enter merging: %s", output)
	}
	valid := fixture.mustTask(t, "t1")
	badWorkerLabel := "posse:shop:t2"
	client := herdr.NewWithEnv("herdr", fixture.env)
	badWorker, err := createTab(client, fixture.project.HerdrWorkspaceID, fixture.repo, badWorkerLabel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "pane", "report-agent", badWorker.RootPane.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "working"); err != nil {
		t.Fatal(err)
	}
	badID, err := fixture.db.CreateTask(context.Background(), fixture.project.ID, store.Task{
		Seq: 2, Type: "change", Title: "inaccessible old PR", State: store.StateSpawning,
		MergeMode: "pr", Branch: "posse/old-pr", BaseRef: "main", HerdrWorkspaceID: fixture.project.HerdrWorkspaceID,
		PaneID: badWorker.RootPane.PaneID, PaneLabel: badWorkerLabel, AgentName: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct{ from, to store.State }{
		{store.StateSpawning, store.StateWorking}, {store.StateWorking, store.StateDone}, {store.StateDone, store.StateMerging},
	} {
		source := "cli"
		if transition.to == store.StateDone {
			source = "worker"
		}
		if err := fixture.db.Transition(context.Background(), badID, transition.from, transition.to, source, "old PR"); err != nil {
			t.Fatal(err)
		}
	}
	merge := fixture.mergeOnLocalOrigin(t, valid, "t1")
	fixture.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, valid.GatedSHA)
	var response map[string]any
	encoded, err := os.ReadFile(fixture.ghState)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	response["data"].(map[string]any)["repo1"].(map[string]any)["pr2"] = nil
	response["errors"] = []any{map[string]any{"message": "Could not resolve to a PullRequest with the number of 41.", "path": []string{"repo1", "pr2"}, "type": "NOT_FOUND"}}
	encoded, err = json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicfile.Write(fixture.ghState, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "gh-exit-nonzero"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Do not expose the old PR to Watch until its GraphQL response is ready.
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET pr_url=? WHERE id=?`, "https://github.com/acme/shop/pull/41", badID); err != nil {
		t.Fatal(err)
	}
	beforePolls, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{{"show", "t1"}, {"show", "t1"}, {"list"}} {
		time.Sleep(5 * time.Millisecond)
		if command[0] == "show" {
			showAfterActiveTeardown(t, fixture, "t1")
		} else {
			runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, command...)
		}
		merged := fixture.mustTask(t, "t1")
		if merged.State != store.StateTornDown || merged.MergedRef != merge {
			t.Fatalf("%v missed valid merge or safe teardown: %#v", command, merged)
		}
		sandboxes, err := fixture.db.Sandboxes(context.Background(), fixture.project.ID)
		if err != nil || len(sandboxes) != 1 || sandboxes[0].TaskID != 0 || sandboxes[0].State != "idle" {
			t.Fatalf("merged Worker still holds its Sandbox: %#v, %v", sandboxes, err)
		}
		fixture.requireNotice(t, "t1", "pr_merged")
		// Watch can claim a poll before the old URL is exposed, then read
		// only pr1. Allow the next poll to see the old URL before checking the
		// invalid PR Decision; it must still be raised, not skipped.
		var decisions []store.Decision
		if !waitForCondition(10*time.Second, func() bool {
			var err error
			decisions, err = fixture.db.Decisions(context.Background(), fixture.project.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			for _, decision := range decisions {
				if decision.TaskID == badID && decision.Kind == "pr_closed" {
					return true
				}
			}
			runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
			return false
		}) {
			calls, _ := os.ReadFile(fixture.ghLog)
			notices, _ := fixture.db.Notices(context.Background(), fixture.project.ID, false)
			watch, _ := fixture.db.ProjectWatchState(context.Background(), fixture.project.ID)
			t.Fatalf("%v: invalid PR has no User Decision: decisions=%#v notices=%#v watch=%#v ghCalls=%q", command, decisions, notices, watch, calls)
		}
		watch, err := fixture.db.ProjectWatchState(context.Background(), fixture.project.ID)
		if err != nil || watch.PRConsecutiveFailures != 0 {
			t.Fatalf("target-specific failure blocked healthy project polling: %#v, %v", watch, err)
		}
		bad := fixture.mustTask(t, "t2")
		if bad.State != store.StateMerging {
			t.Fatalf("bad PR was changed despite failed observation: %#v", bad)
		}
	}
	afterPolls, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture sets pr_poll=1ms. Its live Watch may legitimately poll
	// between these CLI calls, so an exact count races the background watch.
	if polls := strings.Count(string(afterPolls), "api graphql") - strings.Count(string(beforePolls), "api graphql"); polls < 3 {
		t.Fatalf("expected at least one poll per CLI restart, got %d", polls)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	mergeCount, failureCount := 0, 0
	for _, notice := range notices {
		if notice.TaskID == valid.ID && notice.Kind == "pr_merged" {
			mergeCount++
		}
		if notice.TaskID == badID && notice.Kind == "pr_watch_failing" {
			failureCount++
		}
	}
	if mergeCount != 1 || failureCount != 0 {
		t.Fatalf("repeated/restarted polls duplicated Notices: merge=%d failure=%d", mergeCount, failureCount)
	}
}

func TestWorkerExitedDoneTaskWithOpenPRCanBeDiscarded(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	brief := filepath.Join(fixture.root, "done-open-pr.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nCreate a completed Task with a recorded open PR.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := fixture.startAndComplete(t, brief, "t1")
	const prURL = "https://github.com/acme/shop/pull/17"
	if err := fixture.db.UpdateTaskMerge(context.Background(), task.ID, prURL, ""); err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(gitTest(t, fixture.env, task.WorktreePath, "rev-parse", task.Branch))
	fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", head)
	fixture.exitWorker(t, "t1")
	client := herdr.NewWithEnv("herdr", fixture.env)
	if _, err := client.Call(context.Background(), "pane.close", map[string]any{"pane_id": task.PaneID}); err != nil {
		t.Fatalf("remove exited Worker pane before observation: %v", err)
	}
	stopBackgroundWatch(t, fixture)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "list")

	current := fixture.mustTask(t, "t1")
	if current.State != store.StateDone || current.PRURL != prURL {
		t.Fatalf("fixture did not retain a done Task with its open PR: %#v", current)
	}
	decisions, err := fixture.db.Decisions(context.Background(), fixture.project.ID, true)
	if err != nil || len(decisions) != 1 || decisions[0].Kind != "worker_exited" || !slices.Contains(decisions[0].Options, "discard") {
		t.Fatalf("worker-exited discard Decision = %#v, %v", decisions, err)
	}
	const quote = "User approved discarding the completed Task with its open PR"
	decisionID := strconv.FormatInt(decisions[0].ID, 10)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "decide", decisionID, "discard", "--user-approved", quote)
	output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "apply", decisionID)
	if !strings.Contains(output, "torn-down") {
		t.Fatalf("worker-exited discard did not tear down the done Task: %s", output)
	}
	if current := fixture.mustTask(t, "t1"); current.State != store.StateTornDown || current.PRURL != prURL {
		t.Fatalf("discard did not preserve the Task's PR history: %#v", current)
	}
	if _, err := os.Stat(fixture.ghPRClosed); err != nil {
		t.Fatalf("discard did not close the recorded open PR: %v", err)
	}
	observation, err := fixture.db.LatestPRObservation(context.Background(), task.ID)
	if err != nil || observation.State != "CLOSED" || observation.PRURL != prURL {
		t.Fatalf("discard did not retain a verified PR closure observation: %#v %v", observation, err)
	}
	if _, err := fixture.db.SandboxByTask(context.Background(), task.ID); !store.IsNotFound(err) {
		t.Fatalf("discard did not release the completed Task's Sandbox: %v", err)
	}
}

func TestApprovedDiscardClosesAndVerifiesRecordedPR(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	task := prepareDiscardPRTask(t, fixture)
	client := herdr.NewWithEnv("herdr", fixture.env)
	if _, err := client.Call(context.Background(), "pane.close", map[string]any{"pane_id": task.PaneID}); err != nil {
		t.Fatalf("close the exited Task pane before discard: %v", err)
	}

	output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "teardown", "t1", "--discard", "--user-approved", "User approved discarding this Task")
	if !strings.Contains(output, "torn-down") {
		t.Fatalf("approved discard did not complete after closing the PR: %s", output)
	}
	if current := fixture.mustTask(t, "t1"); current.State != store.StateTornDown || current.PRURL != task.PRURL {
		t.Fatalf("discard did not preserve Task state and PR history: %#v", current)
	}
	if _, err := os.Stat(fixture.ghPRClosed); err != nil {
		t.Fatalf("recorded PR was not closed by the forge: %v", err)
	}
	calls, err := os.ReadFile(fixture.ghLog)
	if err != nil || !strings.Contains(string(calls), "pr close https://github.com/acme/shop/pull/17") {
		t.Fatalf("discard did not close the recorded PR through gh: %s %v", calls, err)
	}
	observation, err := fixture.db.LatestPRObservation(context.Background(), task.ID)
	if err != nil || observation.State != "CLOSED" || observation.PRURL != task.PRURL {
		t.Fatalf("discard did not preserve the verified closure observation: %#v %v", observation, err)
	}
	if _, err := fixture.db.SandboxByTask(context.Background(), task.ID); !store.IsNotFound(err) {
		t.Fatalf("verified discard did not release the Sandbox: %v", err)
	}
}

func TestWatchCloseObservationDuringDiscardTeardownIsObsoleted(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	task := prepareDiscardPRTask(t, fixture)
	client := herdr.NewWithEnv("herdr", fixture.env)
	if _, err := client.Call(context.Background(), "pane.close", map[string]any{"pane_id": task.PaneID}); err != nil {
		t.Fatalf("close the exited Task pane before discard: %v", err)
	}
	closeStarted := filepath.Join(fixture.root, "gh-close-started")
	closeRelease := filepath.Join(fixture.root, "gh-close-release")
	command := exec.Command(fixture.binary, "teardown", "t1", "--discard", "--user-approved", "User approved discarding this Task")
	command.Dir = fixture.repo
	command.Env = setEnv(fixture.leadEnv, "POSSE_TEST_GH_CLOSE_STARTED", closeStarted)
	command.Env = setEnv(command.Env, "POSSE_TEST_GH_CLOSE_RELEASE", closeRelease)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			_ = os.WriteFile(closeRelease, []byte("release\n"), 0o600)
			_ = command.Process.Kill()
		}
	})
	if !waitForCondition(15*time.Second, func() bool {
		_, err := os.Stat(closeStarted)
		return err == nil
	}) {
		t.Fatalf("discard did not reach the recorded PR close call: %s", output.String())
	}
	// This concurrent observation is the Watch side of the close/Teardown
	// race: it records the now-closed PR and raises its normal follow-up.
	fixture.writeGraphQL(t, "pr1", "CLOSED", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", task.GatedSHA)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	decisions, err := fixture.db.Decisions(context.Background(), fixture.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	foundClosedDecision := false
	for _, decision := range decisions {
		foundClosedDecision = foundClosedDecision || decision.TaskID == task.ID && decision.Kind == "pr_closed"
	}
	if !foundClosedDecision {
		t.Fatalf("concurrent Watch observation did not raise pr_closed: %#v", decisions)
	}
	if err := os.WriteFile(closeRelease, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	released = true
	if err := command.Wait(); err != nil {
		t.Fatalf("discard lost the close/Teardown race: %v output=%s", err, output.String())
	}
	if current := fixture.mustTask(t, "t1"); current.State != store.StateTornDown {
		t.Fatalf("concurrent Watch observation changed approved discard custody: %#v", current)
	}
	decisions, err = fixture.db.Decisions(context.Background(), fixture.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range decisions {
		if decision.TaskID == task.ID {
			t.Fatalf("closed-PR Decision remained pending after discard Teardown: %#v", decision)
		}
	}
}

func TestMergeReadyNoticeWhileTaskIsDoneRaisesDecision(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	task := prepareDiscardPRTask(t, fixture)
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET state='done' WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	fixture.writeGraphQL(t, "pr1", "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", task.GatedSHA)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	fixture.requireNotice(t, "t1", "merge_ready")
	decisions, err := fixture.db.Decisions(context.Background(), fixture.project.ID, true)
	if err != nil || len(decisions) != 1 || decisions[0].Kind != "merge_ready" {
		t.Fatalf("merge_ready Decision was not raised while the Task remained done: %#v, %v", decisions, err)
	}
}

func TestMergeReadyDecisionBackfillsAfterDeferredReconciliation(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	task := prepareDiscardPRTask(t, fixture)
	fixture.writeGraphQL(t, "pr1", "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", task.GatedSHA)
	writer, err := fixture.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(context.Background(), `UPDATE projects SET name=name WHERE id=?`, fixture.project.ID); err != nil {
		_ = writer.Rollback()
		t.Fatal(err)
	}
	command := exec.Command(fixture.binary, "show", "t1")
	command.Dir, command.Env = fixture.repo, fixture.leadEnv
	output, commandErr := command.CombinedOutput()
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	if commandErr != nil || !strings.Contains(string(output), "Project observations were deferred") {
		t.Fatalf("store contention did not defer reconciliation: err=%v output=%s", commandErr, output)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	var noticeID int64
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT id FROM notices WHERE task_id=? AND kind='merge_ready' ORDER BY id DESC LIMIT 1`, task.ID).Scan(&noticeID); err != nil {
		t.Fatalf("PR watch did not persist merge_ready while Task was merging: %v", err)
	}
	// Recreate the durable state of a deferred reconciliation that advanced the cursor
	// before saving its Decision, after the PR watcher persisted the real Notice.
	if _, err := fixture.db.ExecContext(context.Background(), `DELETE FROM decisions WHERE project_id=? AND origin=?`, fixture.project.ID, fmt.Sprintf("notice:%d", noticeID)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.AdvanceDecisionNoticeCursor(context.Background(), fixture.project.ID, noticeID); err != nil {
		t.Fatal(err)
	}
	output = []byte(runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "decisions"))
	decisions, err := fixture.db.Decisions(context.Background(), fixture.project.ID, true)
	if err != nil || len(decisions) != 1 || decisions[0].Kind != "merge_ready" {
		t.Fatalf("merge_ready Decision was not recovered after deferred reconciliation: decisions=%#v err=%v output=%s", decisions, err, output)
	}
}

func TestMergeReadyDecisionCanDiscardUnmergedPR(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	task := prepareDiscardPRTask(t, fixture)
	fixture.writeGraphQL(t, "pr1", "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", task.GatedSHA)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	fixture.requireNotice(t, "t1", "merge_ready")
	output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "decisions")
	if !strings.Contains(output, "discard") {
		t.Fatalf("merge_ready Decision omitted discard: %s", output)
	}
	decisions, err := fixture.db.Decisions(context.Background(), fixture.project.ID, true)
	if err != nil || len(decisions) != 1 || decisions[0].Kind != "merge_ready" || !slices.Contains(decisions[0].Options, "discard") {
		t.Fatalf("merge_ready Decision = %#v, %v", decisions, err)
	}
	const quote = "User approved discarding the ready but unmerged PR"
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "decide", strconv.FormatInt(decisions[0].ID, 10), "discard", "--user-approved", quote)
	output = runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "apply", strconv.FormatInt(decisions[0].ID, 10))
	if !strings.Contains(output, "torn-down") || fixture.mustTask(t, "t1").State != store.StateTornDown {
		t.Fatalf("merge_ready discard did not complete Teardown: %s task=%#v", output, fixture.mustTask(t, "t1"))
	}
	if _, err := os.Stat(fixture.ghPRClosed); err != nil {
		t.Fatalf("merge_ready discard did not close the recorded PR: %v", err)
	}
}

func TestApprovedDiscardKeepsSandboxHeldWhenPRCloseFails(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	task := prepareDiscardPRTask(t, fixture)
	if err := os.WriteFile(fixture.ghCloseFail, []byte("fail\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(fixture.binary, "teardown", "t1", "--discard", "--user-approved", "User approved discarding this Task")
	command.Dir, command.Env = fixture.repo, fixture.leadEnv
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "teardown_incomplete") || !strings.Contains(string(output), "keeps the Sandbox held") {
		t.Fatalf("failed forge close did not refuse discard: err=%v output=%s", err, output)
	}
	current := fixture.mustTask(t, "t1")
	sandbox, sandboxErr := fixture.db.SandboxByTask(context.Background(), task.ID)
	if current.State != store.StateMerging || sandboxErr != nil || sandbox.State != "held" {
		t.Fatalf("failed PR close changed Task custody: task=%#v sandbox=%#v err=%v", current, sandbox, sandboxErr)
	}
	if _, err := os.Stat(fixture.ghPRClosed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed close unexpectedly changed forge state: %v", err)
	}
	client := herdr.NewWithEnv("herdr", fixture.env)
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, pane := range snapshot.Panes {
		if pane.PaneID == task.PaneID {
			return
		}
	}
	t.Fatal("failed PR close proceeded to close the Worker pane")
}

func TestApprovedDiscardRefusesMergedPR(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	task := prepareDiscardPRTask(t, fixture)
	fixture.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", "merge-sha", task.GatedSHA)

	command := exec.Command(fixture.binary, "teardown", "t1", "--discard", "--user-approved", "User approved discarding this Task")
	command.Dir, command.Env = fixture.repo, fixture.leadEnv
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "pr_merged") {
		t.Fatalf("merged PR was not refused before discard: err=%v output=%s", err, output)
	}
	current := fixture.mustTask(t, "t1")
	sandbox, sandboxErr := fixture.db.SandboxByTask(context.Background(), task.ID)
	if current.State != store.StateMerging || sandboxErr != nil || sandbox.State != "held" {
		t.Fatalf("merged-PR refusal changed Task custody: task=%#v sandbox=%#v err=%v", current, sandbox, sandboxErr)
	}
	if _, err := os.Stat(fixture.ghPRClosed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("merged PR was sent a close request: %v", err)
	}
	calls, err := os.ReadFile(fixture.ghLog)
	if err != nil || strings.Contains(string(calls), "pr close") {
		t.Fatalf("merged PR was sent a close request: %s %v", calls, err)
	}
	var approvals int
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM approvals WHERE task_id=? AND action='discard'`, task.ID).Scan(&approvals); err != nil || approvals != 0 {
		t.Fatalf("merged PR refusal recorded %d discard approvals: %v", approvals, err)
	}
}

func prepareDiscardPRTask(t *testing.T, fixture *prLifecycleFixture) store.Task {
	t.Helper()
	brief := filepath.Join(fixture.root, "discard-pr.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nCreate a committed change for approved-discard PR closure.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.startAndComplete(t, brief, "t1")
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t1")
	task := fixture.mustTask(t, "t1")
	if task.State != store.StateMerging || task.PRURL == "" || task.GatedSHA == "" {
		t.Fatalf("fixture did not open a recorded PR: %#v", task)
	}
	fixture.exitWorker(t, "t1")
	stopBackgroundWatch(t, fixture)
	return task
}

func TestClosedPRDiscardRechecksForgeStateBeforeTeardown(t *testing.T) {
	fixture := newPRLifecycleFixtureWithManualWatchOwner(t)
	defer fixture.db.Close()
	ctx := context.Background()
	gitTest(t, fixture.env, fixture.repo, "branch", "posse/t1")
	gitTest(t, fixture.env, fixture.repo, "push", "origin", "posse/t1")
	taskID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{
		Seq: 1, Type: "change", Title: "Closed PR discard", ShortName: "closed-pr-discard", MergeMode: "pr", Branch: "posse/t1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, taskID, store.StateWorking, store.StateLost, "cli", "Worker exited"); err != nil {
		t.Fatal(err)
	}
	const prURL = "https://github.com/acme/shop/pull/17"
	if err := fixture.db.UpdateTaskMerge(ctx, taskID, prURL, ""); err != nil {
		t.Fatal(err)
	}
	fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", "head-open")
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	decisions, err := fixture.db.Decisions(ctx, fixture.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 0 {
		t.Fatalf("open PR unexpectedly created a close Decision: %#v", decisions)
	}
	fixture.writeGraphQL(t, "pr1", "CLOSED", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", "head-closed")
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	decisions, err = fixture.db.Decisions(ctx, fixture.project.ID, true)
	if err != nil || len(decisions) != 1 || decisions[0].Kind != "pr_closed" || decisions[0].TaskID != taskID {
		t.Fatalf("closed PR Decision = %#v, %v", decisions, err)
	}
	const quote = "User approved discarding the closed, unmerged Task"
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "decide", strconv.FormatInt(decisions[0].ID, 10), "discard", "--user-approved", quote)

	fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", "head-reopened")
	command := exec.Command(fixture.binary, "apply", strconv.FormatInt(decisions[0].ID, 10))
	command.Dir, command.Env = fixture.repo, fixture.leadEnv
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "decision_refused") || !strings.Contains(string(output), "no longer verified closed") {
		t.Fatalf("reopened PR was not refused before discard: err=%v output=%s", err, output)
	}
	if task := fixture.mustTask(t, "t1"); task.State != store.StateLost {
		t.Fatalf("reopened PR refusal changed Task state to %s", task.State)
	}
	var approvals int
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE task_id=? AND action='discard'`, taskID).Scan(&approvals); err != nil || approvals != 0 {
		t.Fatalf("reopened PR refusal recorded %d approvals: %v", approvals, err)
	}

	fixture.writeGraphQL(t, "pr1", "CLOSED", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", "head-closed")
	output = []byte(runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "apply", strconv.FormatInt(decisions[0].ID, 10)))
	if !strings.Contains(string(output), "torn-down") {
		t.Fatalf("verified closed PR discard did not complete: %s", output)
	}
	if task := fixture.mustTask(t, "t1"); task.State != store.StateTornDown {
		t.Fatalf("verified closed PR discard left Task in %s", task.State)
	}
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE task_id=? AND action='discard' AND user_quote=?`, taskID, quote).Scan(&approvals); err != nil || approvals != 1 {
		t.Fatalf("verified closed PR discard approval count = %d, %v", approvals, err)
	}
}

func TestMergedPRSnapshotsUnmergedFollowUp(t *testing.T) {
	for _, mode := range []string{"uncommitted", "committed"} {
		t.Run(mode, func(t *testing.T) {
			f := newPRLifecycleFixture(t)
			defer f.db.Close()
			brief := filepath.Join(f.root, "follow-up.md")
			if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR follow-up\ndone_when: committed change exists\n---\nMake a PR change.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			f.startAndComplete(t, brief, "t1")
			runPosse(t, f.binary, f.repo, f.leadEnv, "merge", "t1")
			candidate := f.mustTask(t, "t1")
			f.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", candidate.GatedSHA)
			if err := os.WriteFile(filepath.Join(f.root, "pause-fix-commit"), []byte("wait"), 0o600); err != nil {
				t.Fatal(err)
			}
			runPosse(t, f.binary, f.repo, f.leadEnv, "send", "t1", "Add the follow-up.")
			merge := f.mergeOnLocalOrigin(t, candidate, "t1")
			work := filepath.Join(candidate.WorktreePath, "follow-up-work.txt")
			if err := os.WriteFile(work, []byte("unmerged work\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if mode == "committed" {
				gitTest(t, f.env, candidate.WorktreePath, "add", "follow-up-work.txt")
				gitTest(t, f.env, candidate.WorktreePath, "commit", "-m", "unmerged follow-up")
			}
			f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, candidate.GatedSHA)
			showAfterActiveTeardown(t, f, "t1")
			if task := f.mustTask(t, "t1"); task.State != store.StateTornDown || task.MergedRef != merge {
				notices, _ := f.db.Notices(context.Background(), f.project.ID, false)
				t.Fatalf("merged Task did not release its Worker: %#v notices=%#v", task, notices)
			}
			assertSandboxUnlocked(t, f.env, f.repo, candidate.WorktreePath)
			f.requireNotice(t, "t1", "pr_merged")
			if content := gitTest(t, f.env, f.repo, "show", "refs/heads/posse/t1-orphan:follow-up-work.txt"); content != "unmerged work\n" {
				t.Fatalf("Orphan did not preserve %s work: %q", mode, content)
			}
			decisions, err := f.db.Decisions(context.Background(), f.project.ID, true)
			if err != nil || len(decisions) != 1 || decisions[0].Kind != "orphan" {
				t.Fatalf("Orphan Decision: %#v %v", decisions, err)
			}
			if mode == "committed" {
				// A squash merge has a different commit ID from the old Task.
				// The next PR must contain only the unmerged follow-up diff.
				if _, err := f.db.AnswerDecision(context.Background(), f.project.ID, decisions[0].ID, "open-task", "Open a follow-up Task"); err != nil {
					t.Fatal(err)
				}
				runPosse(t, f.binary, f.repo, f.leadEnv, "apply", strconv.FormatInt(decisions[0].ID, 10))
				gitTest(t, f.env, f.repo, "fetch", "origin")
				gitTest(t, f.env, f.repo, "merge", "--ff-only", "origin/main")
				followup := filepath.Join(f.root, "orphan-next.md")
				if err := os.WriteFile(followup, []byte("---\ntype: change\ntitle: From Orphan\ndone_when: follow-up change exists\n---\nKeep only the follow-up.\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runPosse(t, f.binary, f.repo, f.leadEnv, "start", "--brief", followup, "--name", "from-orphan", "--from-orphan", strconv.FormatInt(decisions[0].ID, 10))
				next := f.mustTask(t, "t2")
				diff := gitTest(t, f.env, next.WorktreePath, "diff", "--name-only", "origin/main...HEAD")
				if strings.Contains(diff, "e2e-worker-t1.txt") || !strings.Contains(diff, "follow-up-work.txt") {
					t.Fatalf("new PR includes old squash-merged work or loses the Orphan: %q", diff)
				}
			}
		})
	}
}

func TestMergedPRWithIgnoredArtifactStillMerges(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	brief := filepath.Join(f.root, "ignored-artifact.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.startAndComplete(t, brief, "t1")
	runPosse(t, f.binary, f.repo, f.leadEnv, "merge", "t1")
	candidate := f.mustTask(t, "t1")
	exclude := filepath.Join(f.root, "exclude")
	if err := os.WriteFile(exclude, []byte("build.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, candidate.WorktreePath, "config", "core.excludesFile", exclude)
	artifact := filepath.Join(candidate.WorktreePath, "build.log")
	if err := os.WriteFile(artifact, []byte("ignored build output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	merge := f.mergeOnLocalOrigin(t, candidate, "t1")
	f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, candidate.GatedSHA)
	showAfterActiveTeardown(t, f, "t1")
	if task := f.mustTask(t, "t1"); task.State != store.StateTornDown || task.MergedRef != merge {
		t.Fatalf("ignored artifact blocked normal merging: %#v", task)
	}
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("ignored artifact was discarded by default clean mode: %v", err)
	}
}

func TestMergedPRAutoTearsDownEvenWhenAutoTeardownIsNever(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	configPath := filepath.Join(f.home, "config.toml")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(strings.Replace(string(configData), "auto_teardown = \"finished\"", "auto_teardown = \"never\"", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(f.root, "late-work.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.startAndComplete(t, brief, "t1")
	runPosse(t, f.binary, f.repo, f.leadEnv, "merge", "t1")
	candidate := f.mustTask(t, "t1")
	merge := f.mergeOnLocalOrigin(t, candidate, "t1")
	f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, candidate.GatedSHA)
	showAfterActiveTeardown(t, f, "t1")
	if task := f.mustTask(t, "t1"); task.State != store.StateTornDown {
		t.Fatalf("merged PR did not tear down despite auto_teardown=never: %#v", task)
	}
	f.requireNotice(t, "t1", "pr_merged")
}

func TestPRMergeAcceptsFollowUpBeforeFailureNotice(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	// Test direct message delivery without a poll-only Watch racing to claim it.
	stopBackgroundWatch(t, fixture)
	brief := filepath.Join(fixture.root, "follow-up.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR follow-up\ndone_when: follow-up is committed\n---\nAllow follow-up work while the PR is merging.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	published := fixture.startAndComplete(t, brief, "t1")
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t1"); !strings.Contains(output, "merging") {
		t.Fatalf("PR did not enter merging: %s", output)
	}
	candidate := fixture.mustTask(t, "t1")
	fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", candidate.GatedSHA)

	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.TaskID == candidate.ID && (notice.Kind == "pr_checks_failed" || notice.Kind == "pr_changes_requested" || notice.Kind == "pr_conflict") {
			t.Fatalf("failure Notice exists before follow-up send: %#v", notice)
		}
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Add the expanded follow-up work."); !strings.Contains(output, "delivered") {
		t.Fatalf("follow-up send before a failure Notice was not delivered: %s", output)
	}
	working := fixture.mustTask(t, "t1")
	if working.State != store.StateWorking || working.GatedSHA != "" || working.PRURL != candidate.PRURL || published.PRURL != candidate.PRURL {
		t.Fatalf("follow-up did not return the same PR Task to working and clear its gate: published=%#v merging=%#v working=%#v", published, candidate, working)
	}

	if err := os.WriteFile(filepath.Join(fixture.fixGate, "t1"), []byte("continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.waitTaskState(t, "t1", store.StateDone)
	completed := fixture.mustTask(t, "t1")
	fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", strings.TrimSpace(gitTest(t, fixture.env, fixture.remote, "rev-parse", "refs/heads/posse/t1")))
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t1"); !strings.Contains(output, "merging") {
		t.Fatalf("follow-up could not be re-gated: %s", output)
	}
	remerged := fixture.mustTask(t, "t1")
	if reMergedState := remerged.State; reMergedState != store.StateMerging || remerged.GatedSHA == "" || remerged.PRURL != candidate.PRURL || completed.State != store.StateDone {
		t.Fatalf("Task did not return to merging after the follow-up: completed=%#v relanded=%#v", completed, remerged)
	}
}

func TestMergeReadyBeforeMergeDoesNotRaiseLatePROpened(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	brief := filepath.Join(fixture.root, "ready-before-merge.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nExercise the order of PR lifecycle Notices.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := fixture.startAndComplete(t, brief, "t1")
	head := strings.TrimSpace(gitTest(t, fixture.env, task.WorktreePath, "rev-parse", task.Branch))
	fixture.writeGraphQL(t, "pr1", "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", head)
	if !waitForCondition(30*time.Second, func() bool {
		notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
		if err != nil {
			return false
		}
		for _, notice := range notices {
			if notice.TaskID == task.ID && notice.Kind == "merge_ready" {
				return true
			}
		}
		return false
	}) {
		t.Fatal("PR watcher did not raise merge_ready")
	}
	var mergeOutput []byte
	var mergeErr error
	for attempt := 1; attempt <= 6; attempt++ {
		command := exec.Command(fixture.binary, "merge", "t1", "--execute", "--user-approved", "User approved the pull request")
		command.Dir, command.Env = fixture.repo, fixture.leadEnv
		mergeOutput, mergeErr = command.CombinedOutput()
		if mergeErr == nil || !strings.Contains(string(mergeOutput), `"pr_merge_busy"`) || !strings.Contains(string(mergeOutput), "another repository sync or pull request merge is in progress") {
			break
		}
		time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
	}
	if mergeErr != nil {
		t.Fatalf("retryable Merge contention did not clear: %v\n%s", mergeErr, mergeOutput)
	}

	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var opened, ready []store.Notice
	for _, notice := range notices {
		if notice.TaskID != task.ID {
			continue
		}
		switch notice.Kind {
		case "pr_opened":
			opened = append(opened, notice)
		case "merge_ready":
			ready = append(ready, notice)
		}
	}
	if len(opened) != 1 || len(ready) != 1 {
		t.Fatalf("expected one pr_opened and one merge_ready Notice, got opened=%#v ready=%#v", opened, ready)
	}
	if opened[0].ID >= ready[0].ID {
		t.Fatalf("pr_opened was raised after merge_ready: opened=%#v ready=%#v", opened[0], ready[0])
	}
	var openedData struct {
		URL  string `json:"url"`
		Head string `json:"head_sha"`
	}
	if err := json.Unmarshal([]byte(opened[0].DataJSON), &openedData); err != nil {
		t.Fatal(err)
	}
	if openedData.URL != task.PRURL || openedData.Head != head {
		t.Fatalf("pr_opened does not identify the observed PR head: %#v", openedData)
	}
}

func TestWorkerPublishRetriesLaggingOpenPRHead(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	brief := filepath.Join(fixture.root, "publish-follow-up.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR follow-up\ndone_when: committed change exists\n---\nExercise a stale PR head immediately after a push.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := fixture.startAndComplete(t, brief, "t1")
	oldHead := strings.TrimSpace(gitTest(t, fixture.env, task.WorktreePath, "rev-parse", task.Branch))
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t1")
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Add the publish consistency follow-up.")
	if !waitForCondition(30*time.Second, func() bool {
		current := strings.TrimSpace(gitTest(t, fixture.env, task.WorktreePath, "rev-parse", task.Branch))
		return current != oldHead
	}) {
		t.Fatal("follow-up Worker did not commit a new Task head")
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "gh-list-stale-head"), []byte(oldHead), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "gh-list-stale-head-duration-ms"), []byte("3500"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.fixGate, "t1"), []byte("continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	finished := waitForCondition(30*time.Second, func() bool {
		if fixture.mustTask(t, "t1").State == store.StateDone {
			return true
		}
		log, _ := os.ReadFile(filepath.Join(fixture.root, "worker-delivery.log"))
		return strings.Contains(string(log), "branch_moved") || strings.Contains(string(log), "pr_list_failed")
	})
	if !finished {
		log, _ := os.ReadFile(filepath.Join(fixture.root, "worker-delivery.log"))
		t.Fatalf("Worker did not finish or report publish failure: %s", log)
	}
	if current := fixture.mustTask(t, "t1"); current.State != store.StateDone {
		log, _ := os.ReadFile(filepath.Join(fixture.root, "worker-delivery.log"))
		t.Fatalf("publish rejected the temporarily stale PR head: task=%#v Worker delivery=%s", current, log)
	}
	startedText, err := os.ReadFile(filepath.Join(fixture.root, "gh-list-stale-head-started"))
	if err != nil {
		t.Fatalf("fake forge never began reporting its stale head: %v", err)
	}
	startedMillis, err := strconv.ParseInt(strings.TrimSpace(string(startedText)), 10, 64)
	if err != nil {
		t.Fatalf("invalid fake forge lag start time %q: %v", startedText, err)
	}
	if elapsed := time.Since(time.UnixMilli(startedMillis)); elapsed < 3500*time.Millisecond {
		t.Fatalf("fake forge lag lasted only %s; expected at least 3.5s", elapsed)
	}
	newHead := strings.TrimSpace(gitTest(t, fixture.env, task.WorktreePath, "rev-parse", task.Branch))
	remoteHead := strings.TrimSpace(gitTest(t, fixture.env, fixture.remote, "rev-parse", "refs/heads/"+task.Branch))
	if newHead == oldHead || remoteHead != newHead {
		t.Fatalf("publish did not push the new Task head: local=%s old=%s remote=%s", newHead, oldHead, remoteHead)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	openedHeads := map[string]int{}
	for _, notice := range notices {
		if notice.TaskID != task.ID || notice.Kind != "pr_opened" {
			continue
		}
		var data struct {
			Head string `json:"head_sha"`
		}
		if err := json.Unmarshal([]byte(notice.DataJSON), &data); err != nil {
			t.Fatal(err)
		}
		openedHeads[data.Head]++
	}
	if len(openedHeads) != 2 || openedHeads[oldHead] != 1 || openedHeads[newHead] != 1 {
		t.Fatalf("expected one pr_opened Notice per published head, got %#v", openedHeads)
	}
	calls, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "pr list") < 5 {
		t.Fatalf("publish did not re-read stale PR heads before the update: %s", calls)
	}
	if strings.Count(string(calls), "pr edit") != 1 {
		t.Fatalf("publish should refresh PR metadata exactly once after the head catches up: %s", calls)
	}
}

func TestWorkerPublishDistinguishesMovedHeadFromLagTimeout(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	brief := filepath.Join(fixture.root, "publish-head-race.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nExercise unexpected-head and timeout outcomes during publish.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := fixture.startAndComplete(t, brief, "t1")
	oldHead := strings.TrimSpace(gitTest(t, fixture.env, task.WorktreePath, "rev-parse", task.Branch))
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t1")
	if err := os.WriteFile(filepath.Join(fixture.root, "pause-fix-commit"), []byte("hold\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Add the publish consistency follow-up.")
	if err := os.WriteFile(filepath.Join(task.WorktreePath, "unexpected-follow-up.txt"), []byte("unexpected follow-up\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.env, task.WorktreePath, "add", "unexpected-follow-up.txt")
	gitTest(t, fixture.env, task.WorktreePath, "commit", "-m", "unexpected follow-up")
	if err := os.WriteFile(filepath.Join(fixture.root, "gh-list-stale-head"), []byte(strings.Repeat("f", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "gh-list-stale-head-duration-ms"), []byte("31000"), 0o600); err != nil {
		t.Fatal(err)
	}
	workerEnv := setEnv(fixture.leadEnv, "HERDR_PANE_ID", task.PaneID)
	workerEnv = setEnv(workerEnv, "HERDR_WORKSPACE_ID", task.HerdrWorkspaceID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", fixture.home)
	command := exec.Command(fixture.binary, "publish", "Worker unexpected-head follow-up")
	command.Dir, command.Env = task.WorktreePath, workerEnv
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), `"branch_moved"`) || !strings.Contains(string(output), `,false,`) {
		t.Fatalf("unexpected forge head did not fail non-retryable branch_moved: err=%v output=%s", err, output)
	}
	pushedHead := strings.TrimSpace(gitTest(t, fixture.env, fixture.remote, "rev-parse", "refs/heads/"+task.Branch))
	if pushedHead == oldHead {
		t.Fatalf("follow-up head was not pushed: remote=%s old=%s", pushedHead, oldHead)
	}
	localHead := strings.TrimSpace(gitTest(t, fixture.env, task.WorktreePath, "rev-parse", task.Branch))
	if pushedHead != localHead {
		t.Fatalf("publish did not push before rejecting the unexpected head: remote=%s local=%s", pushedHead, localHead)
	}

	if err := os.WriteFile(filepath.Join(task.WorktreePath, "timeout-follow-up.txt"), []byte("timeout follow-up\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.env, task.WorktreePath, "add", "timeout-follow-up.txt")
	gitTest(t, fixture.env, task.WorktreePath, "commit", "-m", "timeout follow-up")
	expectedHead := strings.TrimSpace(gitTest(t, fixture.env, task.WorktreePath, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(fixture.root, "gh-list-stale-head"), []byte(oldHead), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fixture.root, "gh-list-stale-head-started")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	command = exec.Command(fixture.binary, "publish", "Worker timeout follow-up")
	command.Dir, command.Env = task.WorktreePath, workerEnv
	started := time.Now()
	output, err = command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), `"pr_head_lag_timeout"`) || !strings.Contains(string(output), `,true,`) || !strings.Contains(string(output), "forge has not caught up") {
		t.Fatalf("stale forge head did not produce an accurately worded retryable timeout: err=%v output=%s", err, output)
	}
	if elapsed := time.Since(started); elapsed < 28*time.Second || elapsed > 40*time.Second {
		t.Fatalf("publish timeout took %s, expected about 30s", elapsed)
	}
	calls, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "pr edit") {
		t.Fatalf("publish edited PR metadata before the forge head caught up: %s", calls)
	}
	command = exec.Command(fixture.binary, "publish", "Worker timeout follow-up")
	command.Dir, command.Env = task.WorktreePath, workerEnv
	output, err = command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "https://github.com/acme/shop/pull/17") {
		t.Fatalf("retry after forge catch-up did not publish successfully: err=%v output=%s", err, output)
	}
	calls, err = os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "pr edit") != 1 {
		t.Fatalf("retry should edit PR metadata exactly once after the head catches up: %s", calls)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.env, fixture.remote, "rev-parse", "refs/heads/"+task.Branch)); got != expectedHead {
		t.Fatalf("timeout publish did not push the expected Task head: got=%s want=%s", got, expectedHead)
	}
}

func TestUpDefersInterruptedOpenPRRecoveryUntilLeadStarts(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	client := herdr.NewWithEnv("herdr", fixture.env)
	brief := filepath.Join(fixture.root, "deferred-pr-recovery.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR create recovery\ndone_when: committed change exists\n---\nExercise deferred recovery of an interrupted PR creation.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.startAndCommitLegacy(t, brief, "t1")

	// Keep the independent watch from racing the injected interrupted intent.
	stopBackgroundWatch(t, fixture)

	crashEnv := setEnv(fixture.leadEnv, "POSSE_INTENT_CRASH_AT", "merge --open-pr:after:pr.create")
	command := exec.Command(fixture.binary, "merge", "t1")
	command.Dir, command.Env = fixture.repo, crashEnv
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("Merge did not crash at the injected post-create boundary: err=%v output=%s", err, output)
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
	if _, err := os.Stat(filepath.Join(fixture.root, "gh.log")); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(fixture.root, "bin")
	ghPath := filepath.Join(binDir, "gh")
	if err := os.Rename(ghPath, filepath.Join(binDir, "gh-real")); err != nil {
		t.Fatal(err)
	}
	ghWrapper := "#!/bin/sh\nprintf '%s %s\\n' \"${HERDR_PANE_ID:-none}\" \"$*\" >> \"$POSSE_TEST_GH_LOG\"\nif [ \"$1 $2\" = 'pr list' ] && [ -e \"$POSSE_TEST_ROOT/slow-recovery\" ] && [ ! -e \"$POSSE_TEST_ROOT/lead-started\" ]; then\n  : > \"$POSSE_TEST_ROOT/pr-list-before-lead\"\n  sleep 30\nfi\nexec \"$POSSE_TEST_ROOT/bin/gh-real\" \"$@\"\n"
	if err := os.WriteFile(ghPath, []byte(ghWrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(binDir, "claude")
	if err := os.Rename(claudePath, filepath.Join(binDir, "claude-real")); err != nil {
		t.Fatal(err)
	}
	claudeWrapper := "#!/bin/sh\nprintf '%s\\n' \"$(date +%s%N)\" > \"$POSSE_TEST_ROOT/lead-started\"\nexec \"$POSSE_TEST_ROOT/bin/claude-real\" \"$@\"\n"
	if err := os.WriteFile(claudePath, []byte(claudeWrapper), 0o700); err != nil {
		t.Fatal(err)
	}

	newLead, err := createTab(client, fixture.project.HerdrWorkspaceID, fixture.repo, "replacement-shell")
	if err != nil {
		t.Fatal(err)
	}
	if oldTab := envValue(fixture.leadEnv, "HERDR_TAB_ID"); oldTab != "" {
		if _, err := client.Call(context.Background(), "tab.close", map[string]any{"tab_id": oldTab}); err != nil {
			t.Fatalf("close old fixture Lead tab: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "slow-recovery"), []byte("delay"), 0o600); err != nil {
		t.Fatal(err)
	}
	startupStarted := time.Now()
	if _, err := client.Run(context.Background(), "pane", "run", newLead.RootPane.PaneID, "posse up --name shop --yes"); err != nil {
		t.Fatalf("start Lead from replacement shell: %v", err)
	}
	if !waitForCondition(8*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(fixture.root, "lead-started"))
		return err == nil
	}) {
		screen, _ := client.Run(context.Background(), "pane", "read", newLead.RootPane.PaneID, "--lines", "60")
		t.Fatalf("Lead did not start promptly with an interrupted PR intent:\n%s", screen)
	}
	if elapsed := time.Since(startupStarted); elapsed > 5*time.Second {
		t.Fatalf("Lead startup took %s with deferred PR recovery, want under 5s", elapsed)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "pr-list-before-lead")); err == nil {
		t.Fatal("forge PR lookup ran before the Lead agent started")
	}
	if !waitForCondition(20*time.Second, func() bool {
		current := fixture.mustTask(t, "t1")
		_, intentErr := fixture.db.IntentByTask(context.Background(), current.ID)
		return current.PRURL == "https://github.com/acme/shop/pull/17" && store.IsNotFound(intentErr)
	}) {
		t.Fatalf("Watch did not recover the deferred PR intent after Lead startup: %#v", fixture.mustTask(t, "t1"))
	}
	ghLog, err := os.ReadFile(fixture.ghLog)
	if err != nil || !strings.Contains(string(ghLog), "pr list") {
		t.Fatalf("deferred recovery did not run its forge lookup after startup: %s %v", ghLog, err)
	}

	if _, err := client.Run(context.Background(), "pane", "send-keys", newLead.RootPane.PaneID, "ctrl+c"); err != nil {
		t.Errorf("stop replacement Lead agent: %v", err)
	}
}

func envValue(values []string, key string) string {
	for _, value := range values {
		name, content, found := strings.Cut(value, "=")
		if found && name == key {
			return content
		}
	}
	return ""
}

func TestPRCreateCrashRecoveryAdoptsOpenPullRequest(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	brief := filepath.Join(fixture.root, "crash-change.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR create recovery\ndone_when: committed change exists\n---\nExercise adoption of a PR created before Posse persisted its URL.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Simulate a Task completed before Workers published their own PRs.
	// The compatibility path still lets Merge recover a create interrupted
	// after the forge accepted it but before Posse persisted its URL.
	fixture.startAndCommitLegacy(t, brief, "t1")
	crashEnv := setEnv(fixture.leadEnv, "POSSE_INTENT_CRASH_AT", "merge --open-pr:after:pr.create")
	command := exec.Command(fixture.binary, "merge", "t1")
	command.Dir = fixture.repo
	command.Env = crashEnv
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf("Merge did not crash at the injected post-create boundary: err=%v output=%s", err, output)
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
	// The Herdr hook can claim this intent concurrently with show. Wait for its
	// state, URL and Notice to commit before checking the recovered result.
	if !waitForCondition(30*time.Second, func() bool {
		task := fixture.mustTask(t, "t1")
		if task.State != store.StateMerging || task.PRURL != "https://github.com/acme/shop/pull/17" {
			return false
		}
		notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
		if err != nil {
			return false
		}
		for _, notice := range notices {
			if notice.TaskID == task.ID && notice.Kind == "pr_opened" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("open PR was not fully adopted after the create crash: task=%#v", fixture.mustTask(t, "t1"))
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
	root        string
	repo        string
	remote      string
	home        string
	binary      string
	env         []string
	callerEnv   []string
	leadEnv     []string
	fixGate     string
	ghLog       string
	ghState     string
	ghOpenPRs   string
	ghMerge     string
	ghBody      string
	ghTitle     string
	ghPRClosed  string
	ghCloseFail string
	db          *store.DB
	project     store.Project
}

func newPRLifecycleFixture(t *testing.T) *prLifecycleFixture {
	return newPRLifecycleFixtureWithLeadAndShell(t, false, false)
}

func newPRLifecycleFixtureWithLead(t *testing.T, liveLead bool) *prLifecycleFixture {
	return newPRLifecycleFixtureWithLeadAndShell(t, liveLead, false)
}

func newPRLifecycleFixtureWithSlowLoginShell(t *testing.T) *prLifecycleFixture {
	return newPRLifecycleFixtureWithLeadAndShell(t, false, true)
}

// Use this fixture for tests that create synthetic Tasks and own their lifecycle.
func newPRLifecycleFixtureWithManualWatchOwner(t *testing.T) *prLifecycleFixture {
	return newPRLifecycleFixtureWithOptions(t, false, false, true)
}

func newPRLifecycleFixtureWithLeadAndShell(t *testing.T, liveLead, slowLoginShell bool) *prLifecycleFixture {
	return newPRLifecycleFixtureWithOptions(t, liveLead, slowLoginShell, false)
}

func newPRLifecycleFixtureWithOptions(t *testing.T, liveLead, slowLoginShell, manualWatchOwner bool) *prLifecycleFixture {
	t.Helper()
	root := newFixtureRoot(t, fixturePrefix("pr-"))
	binDir := filepath.Join(root, "bin")
	worktrees := filepath.Join(root, "posse", "pool")
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
	if manualWatchOwner {
		env = setEnv(env, "POSSE_WATCH_TOKEN", "e2e-manual-owner")
	}
	env = setEnv(env, "POSSE_E2E_WORKTREES", worktrees)
	env = setEnv(env, "POSSE_E2E_FIX_GATE", fixGate)
	env = setEnv(env, "POSSE_E2E_SIGNAL_GATE", filepath.Join(root, "signal-gates"))
	env = setEnv(env, "POSSE_E2E_POSSE_BIN", filepath.Join(binDir, "posse"))
	env = setEnv(env, "POSSE_TEST_GH_LOG", filepath.Join(root, "gh.log"))
	env = setEnv(env, "POSSE_TEST_GH_STATE", filepath.Join(root, "gh-state.json"))
	env = setEnv(env, "POSSE_TEST_GH_BLOCKERS", filepath.Join(root, "gh-blockers.json"))
	env = setEnv(env, "POSSE_TEST_GH_BLOCKER_FAIL", filepath.Join(root, "gh-blocker-fail"))
	env = setEnv(env, "POSSE_TEST_GH_OPEN_PRS", filepath.Join(root, "gh-open-prs.json"))
	env = setEnv(env, "POSSE_TEST_GH_MERGE", filepath.Join(root, "gh-merge-called"))
	env = setEnv(env, "POSSE_TEST_GH_BODY", filepath.Join(root, "gh-body.md"))
	env = setEnv(env, "POSSE_TEST_GH_TITLE", filepath.Join(root, "gh-title.txt"))
	env = setEnv(env, "POSSE_TEST_GH_BASE_BRANCH_FILE", filepath.Join(root, "gh-base-branch"))
	env = setEnv(env, "POSSE_TEST_GH_PR_CLOSED", filepath.Join(root, "gh-pr-closed"))
	env = setEnv(env, "POSSE_TEST_GH_CLOSE_FAIL", filepath.Join(root, "gh-close-fail"))
	env = setEnv(env, "POSSE_TEST_REMOTE", filepath.Join(root, "origin.git"))
	env = setEnv(env, "POSSE_TEST_GH_FAIL", "")
	env = setEnv(env, "PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for key, value := range map[string]string{
		"GIT_AUTHOR_NAME": "Posse E2E", "GIT_AUTHOR_EMAIL": "posse-e2e@example.test",
		"GIT_COMMITTER_NAME": "Posse E2E", "GIT_COMMITTER_EMAIL": "posse-e2e@example.test",
	} {
		env = setEnv(env, key, value)
	}
	if slowLoginShell {
		shell := filepath.Join(binDir, "slow-login-shell")
		marker := filepath.Join(root, "slow-login-enabled")
		starts := filepath.Join(root, "slow-login-starts")
		script := "#!/bin/sh\nif [ -e " + shellQuote(marker) + " ]; then printf 'started\\n' >> " + shellQuote(starts) + "; sleep 1.5; fi\nexec /bin/sh -l\n"
		if err := os.WriteFile(shell, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := herdr.WriteIsolatedConfigWithShell(root, shell); err != nil {
			t.Fatal(err)
		}
	} else if _, err := herdr.WriteIsolatedConfig(root); err != nil {
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
    : > "$POSSE_TEST_ROOT/worker-ready-$task_id"
    while [ ! -e "$POSSE_E2E_SIGNAL_GATE/$task_id" ]; do sleep 0.02; done
    case "$task_id" in t1) pr_url='https://github.com/acme/shop/pull/17' ;; t2) pr_url='https://github.com/acme/shop/pull/18' ;; esac
    "$POSSE_E2E_POSSE_BIN" publish "Worker committed $task_id" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
    "$POSSE_E2E_POSSE_BIN" signal done "Worker committed $task_id" --pr "$pr_url" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
    : > "$POSSE_TEST_ROOT/worker-idle-$task_id"
    while IFS= read -r instruction; do
      herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
      while [ -e "$POSSE_TEST_ROOT/pause-fix-commit" ]; do sleep 0.02; done
      printf 'fix change %s\n' "$task_id" > "e2e-fix-$task_id.txt"
      git add "e2e-fix-$task_id.txt"
      git commit -m "fix worker change $task_id" >/dev/null
      while [ ! -e "$POSSE_E2E_FIX_GATE/$task_id" ]; do sleep 0.02; done
      "$POSSE_E2E_POSSE_BIN" publish "Worker fixed $task_id" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
      turn_token=$(printf '%s\n' "$instruction" | sed -n 's/.*| turn \([^]]*\)].*/\1/p')
      "$POSSE_E2E_POSSE_BIN" signal done "Worker fixed $task_id" --pr "$pr_url" --turn "$turn_token" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
      herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
    done
    ;;
  *)
    while IFS= read -r line; do printf '%s\n' "$line" >> "$POSSE_TEST_ROOT/lead-prompts.log"; done
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
  "api graphql") cat "$POSSE_TEST_GH_STATE"; if [ -e "$POSSE_TEST_ROOT/gh-exit-nonzero" ]; then exit 1; fi ;;
  "api --hostname")
    case "$*" in *'dependencies/blocked_by'*)
      if [ -e "$POSSE_TEST_GH_BLOCKER_FAIL" ]; then printf 'permission denied\\n' >&2; exit 1; fi
      cat "$POSSE_TEST_GH_BLOCKERS" ;;
    *) exit 90 ;;
    esac ;; 
  "pr list")
    case " $* " in *' --head posse/t1 '*) branch=posse/t1; number=17 ;; *' --head posse/t2 '*) branch=posse/t2; number=18 ;; *) exit 90 ;; esac
    if grep -q "/pull/$number" "$POSSE_TEST_GH_OPEN_PRS"; then
      if [ -f "$POSSE_TEST_ROOT/gh-list-stale-head" ]; then
        if [ ! -f "$POSSE_TEST_ROOT/gh-list-stale-head-started" ]; then date +%s%3N > "$POSSE_TEST_ROOT/gh-list-stale-head-started"; fi
        now=$(date +%s%3N)
        started=$(cat "$POSSE_TEST_ROOT/gh-list-stale-head-started")
        duration=$(cat "$POSSE_TEST_ROOT/gh-list-stale-head-duration-ms")
        if [ "$((now - started))" -lt "$duration" ]; then
          head=$(cat "$POSSE_TEST_ROOT/gh-list-stale-head")
        else
          head=$(git --git-dir="$POSSE_TEST_REMOTE" rev-parse "refs/heads/$branch")
        fi
      else
        head=$(git --git-dir="$POSSE_TEST_REMOTE" rev-parse "refs/heads/$branch")
      fi
      printf '[{"url":"https://github.com/acme/shop/pull/%s","headRefName":"%s","headRefOid":"%s"}]\n' "$number" "$branch" "$head"
    else
      printf '[]\n'
    fi ;;
  "pr create")
    case " $* " in *' --head posse/t1 '*) branch=posse/t1; number=17 ;; *' --head posse/t2 '*) branch=posse/t2; number=18 ;; *) exit 90 ;; esac
    head=$(git --git-dir="$POSSE_TEST_REMOTE" rev-parse "refs/heads/$branch")
    url="https://github.com/acme/shop/pull/$number"
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --title) printf '%s' "$2" > "$POSSE_TEST_GH_TITLE"; shift 2 ;;
        --body) printf '%s' "$2" > "$POSSE_TEST_GH_BODY"; shift 2 ;;
        *) shift ;;
      esac
    done
    printf '[{"url":"%s","headRefName":"%s","headRefOid":"%s"}]\n' "$url" "$branch" "$head" > "$POSSE_TEST_GH_OPEN_PRS"
    printf '%s\n' "$url" ;;
  "pr edit")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --title) printf '%s' "$2" > "$POSSE_TEST_GH_TITLE"; shift 2 ;;
        --body) printf '%s' "$2" > "$POSSE_TEST_GH_BODY"; shift 2 ;;
        *) shift ;;
      esac
    done
    if [ "${POSSE_TEST_GH_KILL_AFTER_EDIT:-}" = 1 ]; then kill -9 "$PPID"; fi
    printf 'Edited\n' ;;
  "pr view")
    case "$*" in
      *'--json title,body'*) python3 -c 'import json,sys; print(json.dumps({"title":open(sys.argv[1]).read(),"body":open(sys.argv[2]).read()}))' "$POSSE_TEST_GH_TITLE" "$POSSE_TEST_GH_BODY" ;;
      *)
        case "$3" in */17) branch=posse/t1 ;; */18) branch=posse/t2 ;; *) exit 90 ;; esac
        head=
        if [ -n "$branch" ]; then head=$(git --git-dir="$POSSE_TEST_REMOTE" rev-parse "refs/heads/$branch"); fi
        state=OPEN
        base=main
        if [ -f "$POSSE_TEST_GH_BASE_BRANCH_FILE" ]; then base=$(cat "$POSSE_TEST_GH_BASE_BRANCH_FILE"); fi
        if grep -q '"state":"MERGED"' "$POSSE_TEST_GH_STATE"; then state=MERGED
        elif grep -q '"state":"CLOSED"' "$POSSE_TEST_GH_STATE" || [ -e "$POSSE_TEST_GH_PR_CLOSED" ]; then state=CLOSED
        fi
        printf '{"url":"%s","state":"%s","headRefOid":"%s","headRefName":"%s","baseRefName":"%s","headRepository":{"nameWithOwner":"acme/shop"}}\n' "$3" "$state" "$head" "$branch" "$base" ;;
    esac ;;
  "pr close")
    if [ -e "$POSSE_TEST_GH_CLOSE_FAIL" ]; then printf 'close failed\n' >&2; exit 1; fi
    if grep -q '"state":"MERGED"' "$POSSE_TEST_GH_STATE"; then printf 'already merged\n' >&2; exit 1; fi
    : > "$POSSE_TEST_GH_PR_CLOSED"
    if [ -n "${POSSE_TEST_GH_CLOSE_STARTED:-}" ]; then
      : > "$POSSE_TEST_GH_CLOSE_STARTED"
      while [ ! -e "$POSSE_TEST_GH_CLOSE_RELEASE" ]; do sleep 0.02; done
    fi
    printf 'Closed\n' ;;
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
	// Lifecycle tests exercise explicit Lead instructions; CI auto-fixes have fake-forge coverage elsewhere.
	configText := "[lead]\nkind = \"claude\"\n\n[defaults]\nmerge_mode = \"pr\"\nauto_teardown = \"finished\"\npr_poll = \"1ms\"\n\n[ci_loop]\nmax_fix_rounds = 0\nrerun_cancelled = 0\n\n[profiles.deep]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"deep\"\n"
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
	t.Cleanup(func() { stopLeadFinalizers(t, root, binary) })
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
	callerEnv = setEnv(callerEnv, "HERDR_SOCKET_PATH", herdr.SocketPath(env))
	if liveLead {
		// Run up inside the shell so its finalizer execs the fake Lead in
		// the actual pane. A detached CLI leaves no foreground agent to prompt.
		if _, err := client.Run(context.Background(), "pane", "run", workspace.RootPane.PaneID, "posse up --name shop --yes"); err != nil {
			t.Fatal(err)
		}
	} else {
		// This mode calls up outside the pane and has no live Lead to answer
		// its detached startup finalizer. Stop it now; it otherwise waits 30s
		// after a Herdr restart and outlives cleanup.
		runPRLifecycleUp(t, binary, repo, callerEnv)
		stopLeadFinalizers(t, root, binary)
	}
	opened, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	var project store.Project
	if !waitForCondition(15*time.Second, func() bool {
		project, err = opened.ProjectByName(context.Background(), "shop")
		return err == nil && project.LeadPaneID != ""
	}) {
		t.Fatalf("Lead did not start: %v", err)
	}
	leadEnv := setEnv(callerEnv, "HERDR_PANE_ID", project.LeadPaneID)
	leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", project.HerdrWorkspaceID)
	if liveLead {
		var processInfo json.RawMessage
		var processInfoErr error
		if !waitForCondition(15*time.Second, func() bool {
			processInfo, processInfoErr = client.Call(context.Background(), "pane.process_info", map[string]any{"pane_id": project.LeadPaneID})
			return processInfoErr == nil && paneForegroundMatches(processInfo, "claude")
		}) {
			t.Fatalf("Lead agent did not reach the pane foreground: %s (%v)", processInfo, processInfoErr)
		}
	}
	if _, err := client.Run(context.Background(), "pane", "report-agent", project.LeadPaneID, "--source", "posse.fake", "--agent", "claude", "--state", "working"); err != nil {
		t.Fatalf("mark isolated Lead busy: %v", err)
	}
	if manualWatchOwner {
		if pids := watchPIDs(root); len(pids) != 0 {
			t.Fatalf("manual Watch owner unexpectedly started hidden pollers: %v", pids)
		}
	}
	return &prLifecycleFixture{
		root: root, repo: repo, remote: remote, home: home, binary: binary,
		env: env, callerEnv: callerEnv, leadEnv: leadEnv, fixGate: fixGate, ghLog: filepath.Join(root, "gh.log"),
		ghState: filepath.Join(root, "gh-state.json"), ghOpenPRs: filepath.Join(root, "gh-open-prs.json"),
		ghMerge: filepath.Join(root, "gh-merge-called"), ghBody: filepath.Join(root, "gh-body.md"),
		ghTitle: filepath.Join(root, "gh-title.txt"), ghPRClosed: filepath.Join(root, "gh-pr-closed"),
		ghCloseFail: filepath.Join(root, "gh-close-fail"), db: opened, project: project,
	}
}

func runPRLifecycleUp(t *testing.T, binary, repo string, env []string) {
	t.Helper()
	for attempt := 1; attempt <= 6; attempt++ {
		command := exec.Command(binary, "up", "--name", "shop", "--yes")
		command.Dir, command.Env = repo, env
		output, err := command.CombinedOutput()
		if err == nil {
			return
		}
		// Open/migration contention happens before `up` can register a Project;
		// retry only that explicitly retryable, side-effect-free startup failure.
		if !strings.Contains(string(output), `"store_busy"`) || !strings.Contains(string(output), "while opening or migrating the database") {
			t.Fatalf("posse up --name shop --yes: %v\n%s", err, output)
		}
		t.Logf("retry posse up after retryable database-open contention (attempt %d/6): %s", attempt, strings.TrimSpace(string(output)))
		time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
	}
	t.Fatal("posse up remained blocked by retryable database-open contention after six attempts")
}

func paneForegroundMatches(raw json.RawMessage, want string) bool {
	var response struct {
		ProcessInfo struct {
			ForegroundProcesses []struct {
				Name    string   `json:"name"`
				Argv    []string `json:"argv"`
				Cmdline string   `json:"cmdline"`
			} `json:"foreground_processes"`
		} `json:"process_info"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return false
	}
	for _, process := range response.ProcessInfo.ForegroundProcesses {
		if filepath.Base(process.Name) == want || len(process.Argv) > 0 && filepath.Base(process.Argv[0]) == want {
			return true
		}
		if fields := strings.Fields(process.Cmdline); len(fields) > 0 && filepath.Base(fields[0]) == want {
			return true
		}
	}
	return false
}

// A detached Lead finalizer can outlive a failed fixture startup. Stop only
// finalizers matching this fixture's binary and isolated state paths.
func stopLeadFinalizers(t *testing.T, root, binary string) {
	t.Helper()
	pids, err := leadFinalizerPIDs(root, binary)
	if err != nil {
		t.Errorf("find isolated Lead finalizers: %v", err)
		return
	}
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("stop isolated Lead finalizer %d: %v", pid, err)
		}
	}
	if waitForCondition(2*time.Second, func() bool {
		pids, err = leadFinalizerPIDs(root, binary)
		return err == nil && len(pids) == 0
	}) {
		return
	}
	// A finalizer stuck in an RPC must not survive fixture shutdown.
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("kill isolated Lead finalizer %d: %v", pid, err)
		}
	}
	if !waitForCondition(2*time.Second, func() bool {
		pids, err = leadFinalizerPIDs(root, binary)
		return err == nil && len(pids) == 0
	}) {
		t.Errorf("isolated Lead finalizers remain after shutdown: pids=%v err=%v", pids, err)
	}
}

func leadFinalizerPIDs(root, binary string) ([]int, error) {
	if runtime.GOOS != "linux" {
		return nil, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(cmdline), "\x00")
		if len(args) < 2 || args[0] != binary || args[1] != "_finalize-lead" {
			continue
		}
		environ, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if err != nil {
			continue
		}
		values := "\x00" + string(environ)
		if strings.Contains(values, "\x00POSSE_TEST_ROOT="+root+"\x00") && strings.Contains(values, "\x00XDG_CONFIG_HOME="+filepath.Join(root, "xdg")+"\x00") {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func (fixture *prLifecycleFixture) exitWorker(t *testing.T, taskID string) {
	t.Helper()
	task := fixture.mustTask(t, taskID)
	client := herdr.NewWithEnv("herdr", fixture.env)
	if _, err := client.Run(context.Background(), "pane", "send-keys", task.PaneID, "ctrl+d"); err != nil {
		t.Fatalf("send EOF to fixture Worker %s: %v", taskID, err)
	}
	var lastProcessInfo json.RawMessage
	if !waitForCondition(5*time.Second, func() bool {
		raw, err := client.Call(context.Background(), "pane.process_info", map[string]any{"pane_id": task.PaneID})
		if err != nil {
			lastProcessInfo = json.RawMessage(err.Error())
			return false
		}
		lastProcessInfo = append(lastProcessInfo[:0], raw...)
		var info struct {
			ProcessInfo struct {
				ShellPID                 int `json:"shell_pid"`
				ForegroundProcessGroupID int `json:"foreground_process_group_id"`
			} `json:"process_info"`
		}
		return json.Unmarshal(raw, &info) == nil && info.ProcessInfo.ShellPID > 0 && (info.ProcessInfo.ForegroundProcessGroupID <= 1 || info.ProcessInfo.ForegroundProcessGroupID == info.ProcessInfo.ShellPID)
	}) {
		t.Fatalf("fixture Worker %s did not exit naturally: process_info=%s", taskID, lastProcessInfo)
	}
}

func (fixture *prLifecycleFixture) startAndComplete(t *testing.T, brief, taskID string) store.Task {
	t.Helper()
	name := "pr-lifecycle-change"
	if taskID == "t2" {
		name = "external-merge-change"
	} else if strings.Contains(brief, "follow-up") {
		name = "pr-follow-up"
	}
	output := fixture.runStartWithStoreBusyRetry(t, brief, name)
	if !strings.Contains(output, taskID) {
		t.Fatalf("start did not return %s: %s", taskID, output)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "signal-gates", taskID), []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.waitTaskState(t, taskID, store.StateDone)
	// The done Signal is persisted before the fake Worker's final Herdr status
	// update completes. Wait so teardown tests do not race that Sandbox child.
	idleMarker := filepath.Join(fixture.root, "worker-idle-"+taskID)
	if !waitForCondition(10*time.Second, func() bool {
		_, err := os.Stat(idleMarker)
		return err == nil
	}) {
		t.Fatalf("fake Worker %s did not finish its post-signal idle report", taskID)
	}
	return fixture.mustTask(t, taskID)
}

// A retryable observation lock can fail before start creates a Task. Retry that
// documented store_busy result, but never retry if the failed command created a Task.
func (fixture *prLifecycleFixture) runStartWithStoreBusyRetry(t *testing.T, brief, name string) string {
	t.Helper()
	const maxAttempts = 4
	var output []byte
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		command := exec.Command(fixture.binary, "start", "--brief", brief, "--name", name)
		command.Dir, command.Env = fixture.repo, fixture.leadEnv
		output, err = command.CombinedOutput()
		if err == nil {
			return string(output)
		}
		if !strings.Contains(string(output), `error{code,message,retryable,help}: "store_busy"`) {
			t.Fatalf("posse start: %v\n%s", err, output)
		}
		var count int
		if queryErr := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM tasks WHERE project_id=? AND short_name=?`, fixture.project.ID, name).Scan(&count); queryErr != nil || count != 0 {
			t.Fatalf("refusing to retry start after a partial Task creation: tasks=%d query_err=%v output=%s", count, queryErr, output)
		}
		if attempt+1 < maxAttempts {
			time.Sleep(time.Duration(25*(1<<attempt)) * time.Millisecond)
		}
	}
	t.Fatalf("posse start stayed retryable store_busy after %d attempts: %v\n%s", maxAttempts, err, output)
	return ""
}

func (fixture *prLifecycleFixture) startAndCommitLegacy(t *testing.T, brief, taskID string) {
	t.Helper()
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", brief, "--name", "pr-create-recovery")
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
	taskID := map[string]string{"pr1": "t1", "pr2": "t2"}[alias]
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	baseBranch := task.TargetBranch
	if baseBranch == "" {
		baseBranch = fixture.project.DefaultBranch
	}
	pull := map[string]any{
		"url":   "https://github.com/acme/shop/pull/" + map[string]string{"pr1": "17", "pr2": "18"}[alias],
		"state": state, "headRefOid": head, "baseRefName": baseBranch, "mergeable": mergeable, "reviewDecision": review,
		"mergeCommit":       map[string]any{"oid": mergeCommit},
		"statusCheckRollup": map[string]any{"state": checks, "contexts": map[string]any{"nodes": contexts}},
	}
	encoded, err := json.Marshal(map[string]any{"data": map[string]any{"repo1": map[string]any{alias: pull}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicfile.Write(fixture.ghState, encoded, 0o600); err != nil {
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
	targetBranch := task.TargetBranch
	if targetBranch == "" {
		targetBranch = fixture.project.DefaultBranch
	}
	gitTest(t, fixture.env, fixture.root, "clone", "--branch", targetBranch, fixture.remote, checkout)
	gitTest(t, fixture.env, checkout, "config", "user.name", "Posse E2E")
	gitTest(t, fixture.env, checkout, "config", "user.email", "posse-e2e@example.test")
	gitTest(t, fixture.env, checkout, "fetch", "origin", "refs/heads/"+task.Branch+":refs/remotes/origin/"+task.Branch)
	gitTest(t, fixture.env, checkout, "merge", "--squash", "refs/remotes/origin/"+task.Branch)
	gitTest(t, fixture.env, checkout, "commit", "-m", "external merge "+taskID)
	gitTest(t, fixture.env, checkout, "push", "origin", "HEAD:refs/heads/"+targetBranch)
	return strings.TrimSpace(gitTest(t, fixture.env, checkout, "rev-parse", "HEAD"))
}
