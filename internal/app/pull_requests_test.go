package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func TestPRMergeRefusesStaleHeadBeforeGate(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	gateMarker := filepath.Join(fixture.root, "gate-ran")
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte("[defaults]\nmerge_mode = \"pr\"\nmerge_method = \"squash\"\nauto_teardown = \"never\"\ngate = [\"touch "+gateMarker+"\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	targetTip := fixture.advanceTarget(t, false, true)
	code, output, stderr := fixture.run("merge", "t1")
	if code == 0 || !strings.Contains(output, "pr_branch_behind") || !strings.Contains(output, targetTip) {
		t.Fatalf("stale PR did not refuse before its Gate: exit=%d output=%s error=%s", code, output, stderr)
	}
	if _, err := os.Stat(gateMarker); !os.IsNotExist(err) {
		t.Fatalf("Gate ran for a branch that was already behind origin/main: %v", err)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateDone || task.GatedSHA != "" {
		t.Fatalf("stale branch acquired final Gate evidence: task=%#v err=%v", task, err)
	}
}

func TestNoMistakesMergeRefusesStaleHeadAndSkipsRebaseOnRecovery(t *testing.T) {
	fixture := newPRMergeFixture(t, "no-mistakes", store.StateDone)
	prURL := "https://github.com/acme/shop/pull/17"
	if err := fixture.db.UpdateTaskMerge(context.Background(), fixture.task.ID, prURL, ""); err != nil {
		t.Fatal(err)
	}
	targetTip := fixture.advanceTarget(t, false, true)
	code, output, stderr := fixture.run("merge", "t1")
	if code == 0 || !strings.Contains(output, "pr_branch_behind") || !strings.Contains(output, targetTip) {
		t.Fatalf("no-mistakes Merge accepted a stale head: exit=%d output=%s error=%s", code, output, stderr)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateDone || task.GatedSHA != "" {
		t.Fatalf("stale no-mistakes head acquired merging evidence: task=%#v err=%v", task, err)
	}
	var instruction string
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT body FROM messages WHERE task_id=? AND status='queued' ORDER BY id DESC LIMIT 1`, fixture.task.ID).Scan(&instruction); err != nil || !strings.Contains(instruction, "--skip=rebase") || !strings.Contains(instruction, targetTip) {
		t.Fatalf("no-mistakes freshness instruction does not preserve merge-based sync: body=%q err=%v", instruction, err)
	}
}

func TestPRMergePushesGatedBranchAndOpensPullRequest(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	fixture.setGraphQLState(t, "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.task.GatedSHA)

	code, output, errOutput := fixture.run("merge", "t1")
	if code != 0 {
		t.Fatalf("PR merge exit=%d output=%s error=%s", code, output, errOutput)
	}

	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateMerging || task.GatedSHA == "" || task.PRURL != "https://github.com/acme/shop/pull/17" {
		t.Fatalf("PR Task was not recorded in merging: %#v, %v", task, err)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.remote, "show-ref", "--hash", "refs/heads/posse/t1")); got != task.GatedSHA {
		t.Fatalf("pushed PR head=%q want gated SHA %q", got, task.GatedSHA)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "pr_opened" {
		t.Fatalf("PR opened Notices = %#v, %v", notices, err)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"pr create", "## Summary", "Worker completion summary", "## Changes", "- worker change", "## Documentation"} {
		if !strings.Contains(string(log), expected) {
			t.Fatalf("gh pr create omitted %q from the structured body: %q", expected, log)
		}
	}
	if !strings.Contains(string(log), "--title E2E Brief title") {
		t.Fatalf("PR title did not come from the Brief: %q", log)
	}
}

func TestPublishWritesSevenSectionPullRequestBody(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	brief := "---\ntype: change\ntitle: E2E Brief title\ndone_when: commit exists\nissues: [12]\nrefs: [14]\n---\nE2E intent\n"
	briefPath := filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "brief.md")
	if err := os.WriteFile(briefPath, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.worktree, "proof.md"), []byte("proof\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.worktree, "add", "proof.md")
	gitTest(t, fixture.worktree, "commit", "-m", "document proof template")
	fixture.headSHA = strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
	fixture.setGHHead(t, fixture.headSHA)

	verification := "go test ./internal/app -> pass\ngo vet ./... -> pass"
	proof := "![Updated settings](https://example.com/settings.png)\n\nTests passed in CI."
	risk := "Risk: low\nRollback: revert the merge commit"
	if code, output, stderr := fixture.run("publish", "Worker completion summary", "--verify", verification, "--proof", "## Proof\n\n"+proof, "--risk", "## Risk and rollback\n\n"+risk); code != 0 {
		t.Fatalf("publish: %d %s %s", code, output, stderr)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	expectedBody := "## Summary\n\nWorker completion summary\n\n" +
		"## Issue Link\n\nCloses #12\nRefs #14\n\n" +
		"## Changes\n\n- worker change\n- document proof template\n\n" +
		"## Verification\n\n- [x] go test ./internal/app -> pass\n- [x] go vet ./... -> pass\n\n" +
		"## Proof\n\n" + proof + "\n\n" +
		"## Risk And Rollback\n\n" + risk + "\n\n" +
		"## Documentation\n\n- [x] Documentation updated for this change\n- [ ] CLAUDE.md/AGENTS.md updated if needed\n"
	token := managedPublishTokenFromLog(string(log))
	if token == "" || !strings.Contains(string(log), "--body "+managedPublishBody(expectedBody, token)) {
		t.Fatalf("PR body did not match the tokenized seven-section template: %q", log)
	}
}

func TestPublishWithoutOptionalBodyFieldsWritesDefaults(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	if code, output, stderr := fixture.run("publish", "Worker completion summary"); code != 0 {
		t.Fatalf("publish: %d %s %s", code, output, stderr)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	expectedBody := "## Summary\n\nWorker completion summary\n\n" +
		"## Issue Link\n\n" +
		"## Changes\n\n- worker change\n\n" +
		"## Verification\n\n- [ ] Not run; reason: not supplied\n\n" +
		"## Proof\n\n_No proof supplied._\n\n" +
		"## Risk And Rollback\n\nRisk: not stated\nRollback: revert this PR\n\n" +
		"## Documentation\n\n- [ ] Documentation updated for this change\n- [ ] CLAUDE.md/AGENTS.md updated if needed\n"
	token := managedPublishTokenFromLog(string(log))
	if token == "" || !strings.Contains(string(log), "--body "+managedPublishBody(expectedBody, token)) {
		t.Fatalf("PR body did not include the tokenized optional-field defaults: %q", log)
	}
}

func TestPublishWritesClosingAndReferenceKeywordsToPullRequestBody(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	brief := "---\ntype: change\ntitle: E2E Brief title\ndone_when: commit exists\nticket: 12\nrefs: [18]\n---\nE2E intent\n"
	briefPath := filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "brief.md")
	if err := os.WriteFile(briefPath, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := fixture.run("publish", "Worker completion summary"); code != 0 {
		t.Fatalf("publish: %d %s %s", code, output, stderr)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, keyword := range []string{"Closes #12", "Refs #18"} {
		if !strings.Contains(string(log), keyword) {
			t.Fatalf("PR body omitted %q: %s", keyword, log)
		}
	}
	if strings.Contains(string(log), "Closes #18") {
		t.Fatalf("non-closing reference became a closing link: %s", log)
	}
}

func TestPublishAddsIssueLinksWhenReusingAnExistingPullRequest(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	briefPath := filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "brief.md")
	brief := "---\ntype: change\ntitle: E2E Brief title\ndone_when: commit exists\nissues: [12]\nrefs: [14]\n---\nE2E intent\n"
	if err := os.WriteFile(briefPath, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	_, currentBody, err := prDetails(context.Background(), fixture.db, fixture.project, fixture.task, fixture.service.homePath, "", "Worker completion summary", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/acme/shop/pull/17"
	token, err := fixture.db.EnsurePRBodyMarker(context.Background(), fixture.task.ID, "", prURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GH_BODY", "Maintainer intro\n\n"+managedPublishBody(currentBody, token)+"\n\nMaintainer footer")
	openPR := fmt.Sprintf(`[{"url":"https://github.com/acme/shop/pull/17","headRefName":"posse/t1","headRefOid":"%s"}]`, fixture.headSHA)
	if err := os.WriteFile(fixture.ghOpenPRs, []byte(openPR), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := fixture.run("publish", "Updated worker completion summary"); code != 0 {
		t.Fatalf("publish: %d %s %s", code, output, stderr)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"pr edit https://github.com/acme/shop/pull/17 --title", "Updated worker completion summary", "Maintainer intro", "Maintainer footer", "Closes #12", "Refs #14"} {
		if !strings.Contains(string(log), expected) {
			t.Fatalf("reused PR body update omitted %q: %s", expected, log)
		}
	}
}

func TestSendToMergeWithoutFailureNoticeReturnsToWorking(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("PR merge exit=%d output=%s error=%s", code, output, errOutput)
	}

	ctx := context.Background()
	notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "pr_checks_failed" || notice.Kind == "pr_changes_requested" || notice.Kind == "pr_conflict" {
			t.Fatalf("unexpected failure Notice before follow-up send: %#v", notice)
		}
	}
	before, err := fixture.db.Task(ctx, fixture.project.ID, "t1")
	if err != nil || before.State != store.StateMerging || before.GatedSHA == "" {
		t.Fatalf("PR Task was not gated before follow-up send: %#v, %v", before, err)
	}

	code, output, errOutput := fixture.run("send", "t1", "Add the expanded follow-up work.")
	if code != 0 || !strings.Contains(output, "delivered") {
		t.Fatalf("merging follow-up send exit=%d output=%s error=%s", code, output, errOutput)
	}
	after, err := fixture.db.Task(ctx, fixture.project.ID, "t1")
	if err != nil || after.State != store.StateWorking || after.GatedSHA != "" || after.PRURL != before.PRURL {
		t.Fatalf("merging follow-up did not return the same PR Task to working and clear its gated SHA: before=%#v after=%#v err=%v", before, after, err)
	}
}

func TestSendRefusesTerminalTasksWithNextSteps(t *testing.T) {
	cases := []struct {
		name     string
		state    store.State
		taskType string
		next     string
	}{
		{name: "merged", state: store.StateMerged, taskType: "change", next: "Create a new Change Task"},
		{name: "torn-down", state: store.StateTornDown, taskType: "change", next: "Create a new Change Task"},
		{name: "failed", state: store.StateFailed, taskType: "change", next: "posse relaunch t1"},
		{name: "lost", state: store.StateLost, taskType: "change", next: "posse relaunch t1"},
		{name: "reported", state: store.StateReported, taskType: "research", next: "Create a new Change Task"},
		{name: "done non-change", state: store.StateDone, taskType: "research", next: "Create a new Change Task"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPRMergeFixture(t, "pr", store.StateDone)
			if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET state=?,type=? WHERE id=?`, tc.state, tc.taskType, fixture.task.ID); err != nil {
				t.Fatal(err)
			}
			code, output, errOutput := fixture.run("send", "t1", "Follow up")
			if code != 1 || !strings.Contains(output, "message_refused") || !strings.Contains(output, "state "+string(tc.state)) || !strings.Contains(output, tc.next) {
				t.Fatalf("send refusal for %s exit=%d output=%s error=%s", tc.state, code, output, errOutput)
			}
		})
	}
}

func TestSendToMergeRefusesWhileMergeIntentRuns(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("PR merge exit=%d output=%s error=%s", code, output, errOutput)
	}
	if err := fixture.db.StartIntent(context.Background(), fixture.project.ID, fixture.task.ID, "merge --execute", "merge", `{}`, os.Getpid()); err != nil {
		t.Fatal(err)
	}

	code, output, errOutput := fixture.run("send", "t1", "Do not interrupt the merge.")
	if code != 1 || !strings.Contains(output, "intent_active") || !strings.Contains(output, "merge --execute") || !strings.Contains(output, "finish") {
		t.Fatalf("send during merge exit=%d output=%s error=%s", code, output, errOutput)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	var queued int
	if queryErr := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE task_id=?`, fixture.task.ID).Scan(&queued); queryErr != nil {
		t.Fatal(queryErr)
	}
	if err != nil || task.State != store.StateMerging || task.GatedSHA == "" || queued != 0 {
		t.Fatalf("send interrupted or queued work during merge: task=%#v queued=%d err=%v", task, queued, err)
	}
}

func TestPRMergeRequiresCleanSandboxAndDoesNotRunLegacyGates(t *testing.T) {
	dirty := newPRMergeFixture(t, "pr", store.StateDone)
	if err := os.WriteFile(filepath.Join(dirty.worktree, "uncommitted.txt"), []byte("uncommitted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output, _ := dirty.run("merge", "t1"); code != 1 || !strings.Contains(output, "Task worktree is not clean") {
		t.Fatalf("PR merging did not reject a dirty Sandbox: exit=%d output=%s", code, output)
	}
	if _, err := os.Stat(filepath.Join(dirty.remote, "refs", "heads", "posse", "t1")); !os.IsNotExist(err) {
		t.Fatalf("dirty Sandbox was pushed to origin: %v", err)
	}

	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	configText := "[defaults]\nmerge_mode = \"pr\"\nmerge_method = \"squash\"\nauto_teardown = \"never\"\ngate = [\"touch " + filepath.Join(fixture.root, "gate-ran") + "\"]\n"
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("PR merging with a passing Gate exit=%d output=%s error=%s", code, output, errOutput)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "gate-ran")); !os.IsNotExist(err) {
		t.Fatalf("removed Gate command ran: %v", err)
	}
}

func TestPRMergeIgnoresLegacyFailingGateAndOpensPR(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte("[defaults]\nmerge_mode = \"pr\"\nmerge_method = \"squash\"\nauto_teardown = \"never\"\ngate = [\"exit 1\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output, _ := fixture.run("merge", "t1")
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	notices, noticeErr := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	remotePRRefs := strings.TrimSpace(gitTest(t, fixture.remote, "for-each-ref", "--format=%(refname)", "refs/heads/posse/"))
	ghLog, readErr := os.ReadFile(fixture.ghLog)
	if code != 0 || err != nil || task.State != store.StateMerging || task.GatedSHA == "" || task.PRURL == "" || noticeErr != nil || countNoticeKind(notices, "gate_failed") != 0 || remotePRRefs == "" || readErr != nil || !strings.Contains(string(ghLog), "pr create") {
		t.Fatalf("legacy Gate blocked or altered PR delivery: exit=%d output=%s task=%#v notices=%#v refs=%q gh=%q errors=%v/%v/%v", code, output, task, notices, remotePRRefs, ghLog, err, noticeErr, readErr)
	}
}

func TestPRMergeRequiresApprovalAndPinsTheGatedHead(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", task.GatedSHA)
	fixture.setGHHead(t, task.GatedSHA)
	if _, err := fixture.db.RecordPRObservation(context.Background(), store.PRObservation{
		ProjectID: fixture.project.ID, TaskID: task.ID, PRURL: task.PRURL,
		HeadSHA: task.GatedSHA, State: "OPEN", Checks: marshalJSON(checkSnapshot{State: "SUCCESS"}),
		Review: "APPROVED", Mergeable: "MERGEABLE",
	}, store.PRObservationEffect{}, task); err != nil {
		t.Fatal(err)
	}

	if code, output, _ := fixture.run("merge", "t1", "--execute"); code != 1 || !strings.Contains(output, "merge_approval_required") {
		t.Fatalf("merge without approval exit=%d output=%s", code, output)
	}
	if code, output, errOutput := fixture.run("merge", "t1", "--execute", "--user-approved", "User approved this PR"); code != 0 {
		t.Fatalf("approved PR merge exit=%d output=%s error=%s", code, output, errOutput)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil || !strings.Contains(string(log), "--squash") || !strings.Contains(string(log), "--match-head-commit "+task.GatedSHA) {
		t.Fatalf("gh pr merge did not pin the gated SHA and configured method: %q, %v", log, err)
	}
}

func TestPRMergeRequiresGreenObservationForGatedHead(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET autonomy_land='auto' WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	fixture.setGHHead(t, task.GatedSHA)

	code, output, _ := fixture.run("merge", "t1", "--execute")
	if code != 1 || !strings.Contains(output, "checks_not_green") {
		t.Fatalf("merge without an observation exit=%d output=%s", code, output)
	}
	pending := store.PRObservation{
		ProjectID: fixture.project.ID, TaskID: task.ID, PRURL: task.PRURL,
		HeadSHA: task.GatedSHA, State: "OPEN", Checks: marshalJSON(checkSnapshot{State: "PENDING"}),
		Review: "APPROVED", Mergeable: "MERGEABLE",
	}
	if _, err := fixture.db.RecordPRObservation(context.Background(), pending, store.PRObservationEffect{}, task); err != nil {
		t.Fatal(err)
	}

	code, output, _ = fixture.run("merge", "t1", "--execute", "--user-approved", "User approved this PR")
	if code != 1 || !strings.Contains(output, "checks_not_green") {
		t.Fatalf("merge with pending checks exit=%d output=%s", code, output)
	}
	if approved, err := fixture.db.HasApproval(context.Background(), task.ID, "merge"); err != nil || approved {
		t.Fatalf("pending checks recorded merge approval: approved=%v err=%v", approved, err)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil || strings.Contains(string(log), "pr merge") {
		t.Fatalf("pending PR was merged: log=%q err=%v", log, err)
	}

	green := pending
	green.Checks = marshalJSON(checkSnapshot{State: "SUCCESS"})
	if _, err := fixture.db.RecordPRObservation(context.Background(), green, store.PRObservationEffect{}, task); err != nil {
		t.Fatal(err)
	}
	differentHead := green
	differentHead.HeadSHA = strings.Repeat("f", 40)
	if _, err := fixture.db.RecordPRObservation(context.Background(), differentHead, store.PRObservationEffect{}, task); err != nil {
		t.Fatal(err)
	}
	code, output, _ = fixture.run("merge", "t1", "--execute", "--user-approved", "User approved this PR")
	if code != 1 || !strings.Contains(output, "checks_not_green") {
		t.Fatalf("merge with a green observation of a different head exit=%d output=%s", code, output)
	}
	log, err = os.ReadFile(fixture.ghLog)
	if err != nil || strings.Contains(string(log), "pr merge") {
		t.Fatalf("PR with a different latest head observation was merged: log=%q err=%v", log, err)
	}

	if _, err := fixture.db.RecordPRObservation(context.Background(), green, store.PRObservationEffect{}, task); err != nil {
		t.Fatal(err)
	}
	if code, output, errOutput := fixture.run("merge", "t1", "--execute"); code != 0 {
		t.Fatalf("merge with green checks exit=%d output=%s error=%s", code, output, errOutput)
	}
	log, err = os.ReadFile(fixture.ghLog)
	if err != nil || !strings.Contains(string(log), "pr merge") || !strings.Contains(string(log), "--match-head-commit "+task.GatedSHA) {
		t.Fatalf("green PR was not merged at the gated SHA: log=%q err=%v", log, err)
	}
}

func TestPRMergeReturnsTaskToDoneWhenRemoteHeadMoved(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	taskBefore, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", taskBefore.GatedSHA)
	fixture.setGHHead(t, strings.Repeat("f", 40))

	code, output, _ := fixture.run("merge", "t1", "--execute", "--user-approved", "User approved this PR")
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if code != 1 || !strings.Contains(output, "branch_moved") || err != nil || task.State != store.StateDone || task.GatedSHA != "" {
		t.Fatalf("moved PR head did not return Task to done: exit=%d output=%s task=%#v err=%v", code, output, task, err)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil || strings.Contains(string(log), "pr merge") {
		t.Fatalf("moved PR head was merged: log=%q err=%v", log, err)
	}
}

func TestNoMistakesDoneSignalRecordsPRForMerge(t *testing.T) {
	fixture := newPRMergeFixture(t, "no-mistakes", store.StateWorking)
	fixture.installFakeNoMistakes(t, "status: initialized\n", 0)
	prURL := "https://github.com/acme/shop/pull/17"
	code, output, errOutput := fixture.run("signal", "done", "Worker completion summary", "--pr", prURL)
	if code != 0 {
		t.Fatalf("no-mistakes done Signal exit=%d output=%s error=%s", code, output, errOutput)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateDone || task.PRURL != prURL || task.GatedSHA != "" {
		t.Fatalf("no-mistakes PR was not recorded for Merge: %#v, %v", task, err)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err == nil && (strings.Contains(string(log), "pr create") || strings.Contains(string(log), "git push")) {
		t.Fatalf("no-mistakes delivery unexpectedly opened or pushed a PR: %q, %v", log, err)
	}
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	code, output, errOutput = fixture.run("merge", "t1")
	if code != 0 {
		t.Fatalf("no-mistakes Merge exit=%d output=%s error=%s", code, output, errOutput)
	}
	task, err = fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateMerging || task.GatedSHA != fixture.headSHA {
		t.Fatalf("posse merge did not start watching the no-mistakes PR: %#v, %v", task, err)
	}
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", task.GatedSHA)
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	code, output, errOutput = fixture.run("merge", "t1", "--execute", "--user-approved", "User approved the no-mistakes PR")
	if code != 0 {
		t.Fatalf("no-mistakes PR merge exit=%d output=%s error=%s", code, output, errOutput)
	}
	log, err = os.ReadFile(fixture.ghLog)
	if err != nil || !strings.Contains(string(log), "pr merge") {
		t.Fatalf("no-mistakes merge did not use the PR merge path: %q, %v", log, err)
	}
}

func TestTeardownRemovesExactMergedNoMistakesBranch(t *testing.T) {
	fixture := newPRMergeFixture(t, "no-mistakes", store.StateWorking)
	ctx := context.Background()

	sandboxBase := filepath.Join(fixture.home, "pool", fixture.project.Name)
	if err := os.MkdirAll(sandboxBase, 0o700); err != nil {
		t.Fatal(err)
	}
	sandbox, err := fixture.db.AcquireSandbox(ctx, fixture.project.ID, fixture.task.ID, sandboxBase)
	if err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.repo, "worktree", "move", fixture.worktree, sandbox.Path)
	if err := lockSandbox(ctx, fixture.repo, sandbox.Path, fixture.task.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, fixture.repo, "merge", "--squash", fixture.task.Branch); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.repo, "commit", "-m", "squash merge no-mistakes PR")
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET state='merged' WHERE id=?`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `INSERT INTO pr_observations(project_id,task_id,pr_url,head_sha,state,observed_at) VALUES(?,?,?,?,?,?)`, fixture.project.ID, fixture.task.ID, "", fixture.headSHA, "MERGED", 1); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.Herdr.(*herdr.Fake).SnapshotValue = herdr.Snapshot{}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.teardownTask(ctx, fixture.db, fixture.project, cfg, task, false, "")
	if err != nil || !result.BranchRemoved {
		t.Fatalf("Teardown did not remove the exact merged no-mistakes branch: result=%#v err=%v", result, err)
	}
	if _, err := gitOutput(ctx, fixture.repo, "rev-parse", "--verify", "refs/heads/"+task.Branch); !isMissingGitRef(err) {
		t.Fatalf("merged no-mistakes branch remains: %v", err)
	}
}

func TestTeardownPreservesAdvancedMergedNoMistakesBranch(t *testing.T) {
	fixture := newPRMergeFixture(t, "no-mistakes", store.StateWorking)
	ctx := context.Background()

	sandboxBase := filepath.Join(fixture.home, "pool", fixture.project.Name)
	if err := os.MkdirAll(sandboxBase, 0o700); err != nil {
		t.Fatal(err)
	}
	sandbox, err := fixture.db.AcquireSandbox(ctx, fixture.project.ID, fixture.task.ID, sandboxBase)
	if err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.repo, "worktree", "move", fixture.worktree, sandbox.Path)
	if err := lockSandbox(ctx, fixture.repo, sandbox.Path, fixture.task.Seq); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sandbox.Path, "follow-up.txt"), []byte("follow-up after merged head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, sandbox.Path, "add", "follow-up.txt")
	gitTest(t, sandbox.Path, "commit", "-m", "follow-up after merged no-mistakes PR")
	advancedSHA := strings.TrimSpace(gitTest(t, sandbox.Path, "rev-parse", "HEAD"))
	gitTest(t, fixture.repo, "cherry-pick", fixture.headSHA)
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET state='merged' WHERE id=?`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `INSERT INTO pr_observations(project_id,task_id,pr_url,head_sha,state,observed_at) VALUES(?,?,?,?,?,?)`, fixture.project.ID, fixture.task.ID, "", fixture.headSHA, "MERGED", 1); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.Herdr.(*herdr.Fake).SnapshotValue = herdr.Snapshot{}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.teardownTask(ctx, fixture.db, fixture.project, cfg, task, false, ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/heads/"+task.Branch)); got != advancedSHA {
		t.Fatalf("Teardown changed the advanced no-mistakes branch tip to %s, want %s", got, advancedSHA)
	}
}

func TestNoMistakesDoneRejectsPullRequestFromAnotherRepository(t *testing.T) {
	fixture := newPRMergeFixture(t, "no-mistakes", store.StateWorking)
	fixture.installFakeNoMistakes(t, "status: initialized\n", 0)
	prURL := "https://github.com/other/fork/pull/3"
	code, output, _ := fixture.run("signal", "done", "Worker completion summary", "--pr", prURL)
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if code != 1 || !strings.Contains(output, "pr_repository_mismatch") || !strings.Contains(output, "acme/shop") || err != nil || task.State != store.StateWorking || task.PRURL != "" {
		t.Fatalf("foreign PR URL was accepted: exit=%d output=%s task=%#v err=%v", code, output, task, err)
	}
}

func TestPRMergeDoesNotWriteBranchTrackingConfig(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("PR merge exit=%d output=%s error=%s", code, output, errOutput)
	}
	config, err := os.ReadFile(filepath.Join(fixture.repo, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), `[branch "posse/t1"]`) || strings.Contains(string(config), "merge = refs/heads/posse/t1") {
		t.Fatalf("PR merging added branch tracking config to the User's Project: %s", config)
	}
}

func TestPublishRefusesExistingPRToDifferentFrozenTarget(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateWorking)
	if err := os.WriteFile(filepath.Join(f.home, "projects", "shop", "config.toml"), []byte("[defaults]\nmerge_mode = \"pr\"\ntarget_branch = \"staging\"\nmerge_method = \"squash\"\nauto_teardown = \"never\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/acme/shop/pull/105"
	openPRs := fmt.Sprintf(`[{"url":%q,"headRefName":%q,"headRefOid":%q,"baseRefName":"staging","headRepository":{"nameWithOwner":"acme/shop"}}]`, prURL, f.task.Branch, f.headSHA)
	if err := os.WriteFile(f.ghOpenPRs, []byte(openPRs), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GH_FILTER_BASE", "1")

	code, output, stderr := f.run("publish", "Worker completion summary")
	if code != 1 || !strings.Contains(output, "pr_target_mismatch") || !strings.Contains(output, prURL) || !strings.Contains(output, "staging") || !strings.Contains(output, "main") {
		t.Fatalf("publish did not refuse the PR to a different target: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	log, err := os.ReadFile(f.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "pr create") {
		t.Fatalf("publish created a duplicate PR instead of refusing: %s", log)
	}
}

func TestWorkerPublishesPRToFrozenNonDefaultTarget(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateWorking)
	sandbox := attachPRFixtureSandbox(t, f)
	gitTest(t, f.repo, "branch", "staging", "main")
	gitTest(t, f.repo, "push", "origin", "staging")
	if _, err := f.db.ExecContext(context.Background(), `UPDATE tasks SET target_branch='staging' WHERE id=?`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.task.TargetBranch = "staging"
	t.Setenv("POSSE_TEST_GH_BASE_BRANCH", "staging")
	url := "https://github.com/acme/shop/pull/17"

	for i := 0; i < 2; i++ {
		if i == 1 {
			openPRs := fmt.Sprintf(`[{"url":%q,"headRefName":"posse/t1","headRefOid":%q}]`, url, f.headSHA)
			if err := os.WriteFile(f.ghOpenPRs, []byte(openPRs), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if code, output, stderr := f.run("publish", "Worker completion summary"); code != 0 || !strings.Contains(output, url) {
			t.Fatalf("publish attempt %d: %d %s %s", i+1, code, output, stderr)
		}
	}
	log, err := os.ReadFile(f.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), "pr create") != 1 || !strings.Contains(string(log), "--base staging") {
		t.Fatalf("publish did not reuse one PR with frozen target staging: %s", log)
	}

	if err := os.WriteFile(filepath.Join(f.home, "projects", "shop", "config.toml"), []byte("[defaults]\nmerge_mode = \"pr\"\ntarget_branch = \"main\"\nauto_teardown = \"finished\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := f.run("signal", "done", "Worker completion summary", "--pr", url); code != 0 {
		t.Fatalf("done against frozen target: %d %s %s", code, output, stderr)
	}
	if code, output, stderr := f.run("merge", "t1"); code != 0 {
		t.Fatalf("merge against frozen target: %d %s %s", code, output, stderr)
	}
	f.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", f.headSHA)
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.pollProjectPullRequests(context.Background(), f.db, f.project, cfg, true); err != nil {
		t.Fatalf("watch PR on frozen target: %v", err)
	}
	task, err := f.db.Task(context.Background(), f.project.ID, "t1")
	if err != nil || task.TargetBranch != "staging" || task.State != store.StateMerging {
		t.Fatalf("frozen target or merging state changed: %+v %v", task, err)
	}
	if code, output, stderr := f.run("merge", "t1", "--execute", "--user-approved", "User approved the PR"); code != 0 {
		t.Fatalf("merge: %d %s %s", code, output, stderr)
	}
	gitTest(t, f.worktree, "push", "origin", "posse/t1:refs/heads/staging")
	f.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", f.headSHA, f.headSHA)
	if err := f.service.pollProjectPullRequests(context.Background(), f.db, f.project, cfg, true); err != nil {
		t.Fatalf("watch merged PR: %v", err)
	}
	fake, ok := f.service.Herdr.(*herdr.Fake)
	if !ok {
		t.Fatal("fixture does not use the fake Herdr adapter")
	}
	fake.SnapshotValue.Agents = []herdr.Agent{{Name: "posse-shop-t1-1", PaneID: "w2:p1"}}
	adapter := &changingSnapshotAdapter{Fake: fake, snapshot: fake.SnapshotValue}
	f.service.Herdr = adapter
	removeFixtureTaskPane(f)
	t.Chdir(f.repo)
	if err := f.service.autoTeardownMergedTasks(context.Background(), f.db, f.project, cfg); err != nil {
		t.Fatalf("teardown merged Task: %v", err)
	}
	task, err = f.db.Task(context.Background(), f.project.ID, "t1")
	if err != nil || task.State != store.StateTornDown || task.TargetBranch != "staging" {
		notices, _ := f.db.Notices(context.Background(), f.project.ID, false)
		t.Fatalf("non-default target Task teardown = %+v, notices=%+v, panes=%+v calls=%+v, err=%v", task, notices, adapter.currentSnapshot().Panes, fake.Calls, err)
	}
	if _, err := f.db.SandboxByTask(context.Background(), task.ID); err == nil || sandbox.ID == 0 {
		t.Fatalf("Task sandbox was not released: sandbox=%+v err=%v", sandbox, err)
	}
}

func TestWorkerPublishesSignalsAndLeadGatesWatchesAndMerges(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateWorking)
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("[defaults]\nmerge_mode = \"pr\"\nmerge_method = \"squash\"\nauto_teardown = \"never\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	url := "https://github.com/acme/shop/pull/17"
	if code, output, stderr := f.run("publish", "Worker completion summary"); code != 0 || !strings.Contains(output, url) {
		t.Fatalf("publish: %d %s %s", code, output, stderr)
	}
	if got := strings.TrimSpace(gitTest(t, f.remote, "rev-parse", "refs/heads/posse/t1")); got != f.headSHA {
		t.Fatalf("published %s instead of %s", got, f.headSHA)
	}
	if code, output, _ := f.run("signal", "done", "Worker completion summary"); code != 1 || !strings.Contains(output, "requires --pr") {
		t.Fatalf("missing URL: %d %s", code, output)
	}
	if code, output, stderr := f.run("signal", "done", "Worker completion summary", "--pr", url); code != 0 {
		t.Fatalf("done: %d %s %s", code, output, stderr)
	}
	if code, output, stderr := f.run("merge", "t1"); code != 0 {
		t.Fatalf("merge: %d %s %s", code, output, stderr)
	}
	task, err := f.db.Task(context.Background(), f.project.ID, "t1")
	if err != nil || task.State != store.StateMerging || task.PRURL != url || task.GatedSHA != f.headSHA {
		t.Fatalf("not watching published PR: %+v %v", task, err)
	}
	log, _ := os.ReadFile(f.ghLog)
	if strings.Count(string(log), "pr create") != 1 {
		t.Fatalf("Lead recreated the Worker's PR: %s", log)
	}
	notices, _ := f.db.Notices(context.Background(), f.project.ID, false)
	if countNoticeKind(notices, "pr_opened") != 1 {
		t.Fatalf("missing PR notice: %+v", notices)
	}
	f.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", f.headSHA)
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.pollProjectPullRequests(context.Background(), f.db, f.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	notices, _ = f.db.Notices(context.Background(), f.project.ID, false)
	if countNoticeKind(notices, "merge_ready") != 1 {
		t.Fatalf("PR watch missed checks and review: %+v", notices)
	}
	if code, output, _ := f.run("merge", "t1", "--execute"); code != 1 || !strings.Contains(output, "merge_approval_required") {
		t.Fatalf("merge without approval: %d %s", code, output)
	}
	if code, output, stderr := f.run("merge", "t1", "--execute", "--user-approved", "User approved the PR"); code != 0 {
		t.Fatalf("merge: %d %s %s", code, output, stderr)
	}
	log, _ = os.ReadFile(f.ghLog)
	if !strings.Contains(string(log), "--match-head-commit "+f.headSHA) {
		t.Fatalf("merge was not pinned: %s", log)
	}
}

func TestWorkerPublishAdoptsExistingPR(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateWorking)
	gitTest(t, f.worktree, "push", "origin", "refs/heads/posse/t1:refs/heads/posse/t1")
	if err := os.WriteFile(f.ghOpenPRs, []byte(`[{"url":"https://github.com/acme/shop/pull/17","headRefName":"posse/t1","headRefOid":"`+f.headSHA+`"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := f.run("publish", "Worker completion summary"); code != 0 {
		t.Fatalf("adopt: %d %s %s", code, output, stderr)
	}
	log, _ := os.ReadFile(f.ghLog)
	if strings.Contains(string(log), "pr create") {
		t.Fatalf("duplicate PR created: %s", log)
	}
}

func TestWorkerPublishReportsUnexpectedPRHeadMismatch(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	gitTest(t, fixture.worktree, "push", "origin", "refs/heads/posse/t1:refs/heads/posse/t1")
	if err := os.WriteFile(filepath.Join(fixture.worktree, "follow-up.txt"), []byte("follow-up\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.worktree, "add", "follow-up.txt")
	gitTest(t, fixture.worktree, "commit", "-m", "follow-up commit")
	newHead := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
	unexpectedHead := strings.Repeat("f", 40)
	openPRs := fmt.Sprintf(`[{"url":"https://github.com/acme/shop/pull/17","headRefName":"posse/t1","headRefOid":"%s"}]`, unexpectedHead)
	if err := os.WriteFile(fixture.ghOpenPRs, []byte(openPRs), 0o600); err != nil {
		t.Fatal(err)
	}

	code, output, stderr := fixture.run("publish", "Worker follow-up")
	if code != 1 || !strings.Contains(output, unexpectedHead) || !strings.Contains(output, newHead) || !strings.Contains(output, "branch_moved") {
		t.Fatalf("unexpected PR head did not report branch_moved with both heads: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.remote, "rev-parse", "refs/heads/posse/t1")); got != newHead {
		t.Fatalf("publish did not push the expected Task head before checking the PR: got=%s want=%s", got, newHead)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), "pr list") != 1 {
		t.Fatalf("publish should reject an unexpected PR head without polling: %s", log)
	}
}

func TestWorkerDoneAcceptsMergedPRAfterSourceBranchDeletion(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	sandbox := attachPRFixtureSandbox(t, fixture)
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(`[defaults]
merge_mode = "pr"
merge_method = "squash"
auto_teardown = "finished"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := fixture.run("publish", "Worker completion summary"); code != 0 {
		t.Fatalf("publish: %d %s %s", code, output, stderr)
	}
	gitTest(t, fixture.worktree, "push", "origin", "--delete", "posse/t1")
	t.Setenv("POSSE_TEST_GH_VIEW_STATE", "MERGED")
	mergeCommit := fixture.headSHA
	fixture.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", mergeCommit, fixture.headSHA)

	if code, output, stderr := fixture.run("signal", "done", "Worker completion summary", "--pr", "https://github.com/acme/shop/pull/17"); code != 0 {
		t.Fatalf("done after merged PR source deletion: %d %s %s", code, output, stderr)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateDone || task.PRURL != "https://github.com/acme/shop/pull/17" {
		t.Fatalf("done Signal was not recorded: %#v, %v", task, err)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatalf("watch merged PR after done Signal: %v", err)
	}

	fake, ok := fixture.service.Herdr.(*herdr.Fake)
	if !ok {
		t.Fatal("fixture does not use the fake Herdr adapter")
	}
	fake.SnapshotValue.Agents = []herdr.Agent{{Name: "posse-shop-t1-1", PaneID: "w2:p1"}}
	fixture.service.Herdr = &changingSnapshotAdapter{Fake: fake, snapshot: fake.SnapshotValue}
	removeFixtureTaskPane(fixture)
	fixture.test.Chdir(fixture.repo)
	if _, err := fixture.service.prepareProject(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatalf("reconcile merged PR after done Signal: %v", err)
	}
	task, err = fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateTornDown || task.MergedRef != mergeCommit {
		t.Fatalf("merged PR did not merge and teardown: %#v, %v", task, err)
	}
	releasedSandbox, err := fixture.db.SandboxByTask(context.Background(), task.ID)
	if err == nil {
		t.Fatalf("auto-teardown left Sandbox held: %#v", releasedSandbox)
	}
	sandboxes, err := fixture.db.Sandboxes(context.Background(), fixture.project.ID)
	if err != nil || len(sandboxes) != 1 || sandboxes[0].ID != sandbox.ID || sandboxes[0].State != "idle" || sandboxes[0].TaskID != 0 {
		t.Fatalf("safe merged Task Sandbox was not released: %#v, %v", sandboxes, err)
	}
	observation, err := fixture.db.LatestPRObservation(context.Background(), task.ID)
	if err != nil || observation.State != "MERGED" || observation.MergeCommit != mergeCommit {
		t.Fatalf("merge observation did not record the merge commit: %#v, %v", observation, err)
	}
}

func TestWorkerDoneRejectsUnsafeMergedPRAfterSourceBranchDeletion(t *testing.T) {
	for _, tc := range []struct {
		name, state, source, branch, base, head, dirty, want string
	}{
		{name: "open PR", state: "OPEN", want: "pr_head_mismatch"},
		{name: "fork source", state: "MERGED", source: "other/shop", want: "pr_head_mismatch"},
		{name: "wrong task branch", state: "MERGED", branch: "posse/other", want: "pr_head_mismatch"},
		{name: "wrong target branch", state: "MERGED", base: "develop", want: "pr_head_mismatch"},
		{name: "mismatched PR head", state: "MERGED", head: strings.Repeat("f", 40), want: "pr_head_mismatch"},
		{name: "dirty worktree", state: "MERGED", dirty: "late change", want: "signal_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPRMergeFixture(t, "pr", store.StateWorking)
			if code, output, stderr := fixture.run("publish", "Worker completion summary"); code != 0 {
				t.Fatalf("publish: %d %s %s", code, output, stderr)
			}
			gitTest(t, fixture.worktree, "push", "origin", "--delete", "posse/t1")
			t.Setenv("POSSE_TEST_GH_VIEW_STATE", tc.state)
			if tc.source != "" {
				t.Setenv("POSSE_TEST_GH_SOURCE", tc.source)
			}
			if tc.branch != "" {
				t.Setenv("POSSE_TEST_GH_HEAD_BRANCH", tc.branch)
			}
			if tc.base != "" {
				t.Setenv("POSSE_TEST_GH_BASE_BRANCH", tc.base)
			}
			if tc.head != "" {
				fixture.setGHHead(t, tc.head)
			}
			if tc.dirty != "" {
				if err := os.WriteFile(filepath.Join(fixture.worktree, "late-change.txt"), []byte(tc.dirty), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			code, output, _ := fixture.run("signal", "done", "Worker completion summary", "--pr", "https://github.com/acme/shop/pull/17")
			task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
			if code != 1 || !strings.Contains(output, tc.want) || err != nil || task.State != store.StateWorking || task.PRURL != "" {
				t.Fatalf("unsafe PR accepted: %d %s %+v %v", code, output, task, err)
			}
		})
	}
}

func TestWorkerDoneRejectsForeignOrMovedPR(t *testing.T) {
	for _, tc := range []struct{ name, url, source, head, want string }{
		{"foreign URL", "https://github.com/other/shop/pull/17", "acme/shop", "", "pr_url_invalid"},
		{"fork source", "https://github.com/acme/shop/pull/17", "other/shop", "", "pr_head_mismatch"},
		{"moved head", "https://github.com/acme/shop/pull/17", "acme/shop", strings.Repeat("f", 40), "pr_head_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPRMergeFixture(t, "pr", store.StateWorking)
			if code, output, _ := f.run("publish", "Worker completion summary"); code != 0 {
				t.Fatalf("publish: %d %s", code, output)
			}
			if tc.source != "" {
				t.Setenv("POSSE_TEST_GH_SOURCE", tc.source)
			}
			if tc.head != "" {
				f.setGHHead(t, tc.head)
			}
			code, output, _ := f.run("signal", "done", "Worker completion summary", "--pr", tc.url)
			task, err := f.db.Task(context.Background(), f.project.ID, "t1")
			if code != 1 || !strings.Contains(output, tc.want) || err != nil || task.State != store.StateWorking || task.PRURL != "" {
				t.Fatalf("invalid PR accepted: %d %s %+v %v", code, output, task, err)
			}
		})
	}
}

func TestClosedWorkerPRMustBeReplacedBeforeRemerge(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateWorking)
	url := "https://github.com/acme/shop/pull/17"
	if code, out, _ := f.run("publish", "Worker completion summary"); code != 0 {
		t.Fatalf("publish: %d %s", code, out)
	}
	if code, out, _ := f.run("signal", "done", "Worker completion summary", "--pr", url); code != 0 {
		t.Fatalf("done: %d %s", code, out)
	}
	if code, out, _ := f.run("merge", "t1"); code != 0 {
		t.Fatalf("merge: %d %s", code, out)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	f.setGraphQLState(t, "CLOSED", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", f.headSHA)
	t.Setenv("POSSE_TEST_GH_VIEW_STATE", "CLOSED")
	if err := f.service.pollProjectPullRequests(context.Background(), f.db, f.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := f.run("merge", "t1"); code != 1 || !strings.Contains(out, "pr_closed") {
		t.Fatalf("relied on closed Worker PR: %d %s", code, out)
	}
	task, _ := f.db.Task(context.Background(), f.project.ID, "t1")
	if task.PRURL != url || task.State != store.StateMerging {
		t.Fatalf("discarded closed PR URL: %+v", task)
	}
}

func TestWorkerPublishRestrictedToPRModeAndOwnBranch(t *testing.T) {
	for _, mode := range []string{"local", "no-mistakes"} {
		t.Run(mode, func(t *testing.T) {
			f := newPRMergeFixture(t, mode, store.StateWorking)
			if code, output, _ := f.run("publish", "Worker completion summary"); code != 1 || !strings.Contains(output, "publish_refused") {
				t.Fatalf("unexpected publish: %d %s", code, output)
			}
			if got := strings.TrimSpace(gitTest(t, f.remote, "for-each-ref", "--format=%(refname)", "refs/heads/posse/")); got != "" {
				t.Fatalf("published branch in %s mode: %s", mode, got)
			}
		})
	}
	f := newPRMergeFixture(t, "pr", store.StateWorking)
	gitTest(t, f.worktree, "checkout", "--detach")
	if code, output, _ := f.run("publish", "Worker completion summary"); code != 1 || !strings.Contains(output, "publish_refused") {
		t.Fatalf("published detached HEAD: %d %s", code, output)
	}
}

func TestLeadDoesNotGateMovedWorkerPR(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateWorking)
	if code, output, _ := f.run("publish", "Worker completion summary"); code != 0 {
		t.Fatalf("publish: %d %s", code, output)
	}
	if code, output, _ := f.run("signal", "done", "Worker completion summary", "--pr", "https://github.com/acme/shop/pull/17"); code != 0 {
		t.Fatalf("done: %d %s", code, output)
	}
	f.setGHHead(t, strings.Repeat("f", 40))
	if code, output, _ := f.run("merge", "t1"); code != 1 || !strings.Contains(output, "pr_head_mismatch") {
		t.Fatalf("gated moved PR: %d %s", code, output)
	}
	task, _ := f.db.Task(context.Background(), f.project.ID, "t1")
	if task.State != store.StateDone || task.GatedSHA != "" {
		t.Fatalf("moved PR reached merging: %+v", task)
	}
}

func TestUpRefusesNoMistakesModeWhenCLIReportsUninitialized(t *testing.T) {
	fixture := newPRMergeFixture(t, "no-mistakes", store.StateWorking)
	fixture.installFakeNoMistakes(t, "error: repo not initialized (run 'no-mistakes init' first)\n", 1)
	if err := os.WriteFile(filepath.Join(fixture.repo, ".no-mistakes.yaml"), []byte("looks initialized\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.service.herdrContext = func() bool { return true }

	code, output, _ := fixture.run("up", "--claude")
	log, err := os.ReadFile(filepath.Join(fixture.root, "no-mistakes.log"))
	if code != 1 || !strings.Contains(output, "no_mistakes_uninitialized") || err != nil || !strings.Contains(string(log), "axi status") {
		t.Fatalf("up did not honor no-mistakes initialization status: exit=%d output=%s log=%q err=%v", code, output, log, err)
	}
}

func TestConfigSetRefusesNoMistakesModeWhenCLIReportsUninitialized(t *testing.T) {
	fixture := newPRMergeFixture(t, "local", store.StateWorking)
	fixture.installFakeNoMistakes(t, "error: repo not initialized (run 'no-mistakes init' first)\n", 1)
	configPath := filepath.Join(fixture.home, "config.toml")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	code, output, _ := fixture.run("config", "set", "defaults.merge_mode", "no-mistakes")
	after, readErr := os.ReadFile(configPath)
	log, logErr := os.ReadFile(filepath.Join(fixture.root, "no-mistakes.log"))
	if code != 1 || !strings.Contains(output, "no_mistakes_uninitialized") || readErr != nil || string(after) != string(before) || logErr != nil || !strings.Contains(string(log), "axi status") {
		t.Fatalf("config set did not preserve the prior config on no-mistakes refusal: exit=%d output=%s before=%q after=%q log=%q errors=%v/%v", code, output, before, after, log, readErr, logErr)
	}
}

func TestLeadInstructionsExplainPRAndCheckoutNoticeActions(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	code, output, errOutput := fixture.run("lead")
	if code != 0 {
		t.Fatalf("lead instructions exit=%d output=%s error=%s", code, output, errOutput)
	}
	for _, expected := range []string{"pr_opened", "pr_checks_failed", "pr_changes_requested", "pr_conflict", "merge_ready", "pr_merged", "pr_closed", "pr_watch_failing", "root_behind", "root_fetch_failing", "head_discontinuity", "Autonomy"} {
		if !strings.Contains(output, expected) {
			t.Errorf("Lead instructions omit PR handling for %q: %s", expected, output)
		}
	}
	for _, expected := range []string{"Done and merging Change Tasks accept follow-up instructions through `posse send <task> <message>`", "delivery returns them to working", "merging Task can receive it before those Notices", "After the Worker's next done Signal, run `posse merge` again", "merge `origin/<target>` into the Task branch", "--verify", "--proof", "--risk", "screenshots/images", "test/log evidence", "rollback details", "--user-approved", "<User's words>", "posse retries automatically", "tell the User if it persists", "report the affected repositories when the failure persists"} {
		if !strings.Contains(output, expected) {
			t.Errorf("Lead instructions omit recovery guidance %q: %s", expected, output)
		}
	}
}

type prMergeFixture struct {
	test      *testing.T
	root      string
	repo      string
	remote    string
	headSHA   string
	worktree  string
	home      string
	ghLog     string
	ghState   string
	ghOpenPRs string
	ghHead    string
	bin       string
	service   *Service
	db        *store.DB
	project   store.Project
	task      store.Task
	output    bytes.Buffer
	errOut    bytes.Buffer
}

func freezeCILoop(t *testing.T, fixture *prMergeFixture, maxFixRounds, rerunCancelled int) {
	freezeCILoopWithReview(t, fixture, maxFixRounds, rerunCancelled, "none")
}

func freezeCILoopWithReview(t *testing.T, fixture *prMergeFixture, maxFixRounds, rerunCancelled int, review string) {
	t.Helper()
	brief := []byte("---\ntype: change\ntitle: CI loop test\ndone_when: checks are resolved\n---\nUser intent (verbatim): Retain this exact request.\n")
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief:     taskplan.Select("brief", "change.md", brief),
		Execution: taskplan.Execution{Scope: "repo", Base: "main", Target: "main"},
		Launch:    taskplan.Launch{ProfileName: "author", Profile: config.Profile{Kind: "claude"}},
		Review:    review, Delivery: "pr", Autonomy: config.Autonomy{Review: "ask", Merge: "ask"},
		Paths: taskplan.Paths{Scratch: filepath.Join(fixture.home, "scratch", "shop", "t1"), Output: filepath.Join(fixture.home, "projects", "shop", "tasks", "t1")},
		ValidationLoops: &taskplan.ValidationLoops{
			Review: config.ReviewLoop{Enabled: true, MaxRounds: 3, BlockAt: "P1"},
			CI:     config.CILoop{MaxFixRounds: maxFixRounds, RerunCancelled: rerunCancelled},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET plan_json=?,gated_sha=? WHERE id=?`, encoded, fixture.headSHA, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `INSERT INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,1,?)`, fixture.task.ID, encoded); err != nil {
		t.Fatal(err)
	}
	fixture.task, err = fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeleteMergedRemoteBranchRequiresCurrentMergedPRHead(t *testing.T) {
	for _, scenario := range []string{"merged", "advanced", "open", "closed"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newPRMergeFixture(t, "pr", store.StateDone)
			fixture.task.PRURL = "https://github.com/acme/shop/pull/17"
			gitTest(t, fixture.root, "--git-dir", fixture.remote, "symbolic-ref", "HEAD", "refs/heads/main")
			gitTest(t, fixture.worktree, "push", "origin", "refs/heads/posse/t1:refs/heads/posse/t1")
			t.Setenv("POSSE_TEST_GH_VIEW_STATE", map[string]string{"merged": "MERGED", "advanced": "MERGED", "open": "OPEN", "closed": "CLOSED"}[scenario])
			if scenario == "advanced" {
				gitTest(t, fixture.worktree, "commit", "--allow-empty", "-m", "advance after merge")
				gitTest(t, fixture.worktree, "push", "origin", "refs/heads/posse/t1:refs/heads/posse/t1")
			}
			removed, err := deleteMergedRemoteBranch(context.Background(), fixture.project, fixture.task, config.Config{}, fixture.headSHA)
			if err != nil {
				t.Fatalf("deleteMergedRemoteBranch: %v", err)
			}
			wantRemoved := scenario == "merged"
			if removed != wantRemoved {
				t.Fatalf("removed = %t, want %t", removed, wantRemoved)
			}
			ref := "refs/heads/posse/t1"
			remote := gitTest(t, fixture.repo, "ls-remote", "--heads", "origin", ref)
			if wantRemoved && strings.TrimSpace(remote) != "" {
				t.Fatalf("merged remote branch remains: %s", remote)
			}
			if scenario == "advanced" && !strings.Contains(remote, strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))) {
				t.Fatalf("advanced branch was not preserved: %s", remote)
			}
			if (scenario == "open" || scenario == "closed") && strings.TrimSpace(remote) == "" {
				t.Fatalf("unmerged remote branch was deleted")
			}
		})
	}
}

func newPRMergeFixture(t *testing.T, mode string, state store.State) *prMergeFixture {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	remote := filepath.Join(root, "origin.git")
	worktree := filepath.Join(root, "sandbox")
	home := filepath.Join(root, "posse")
	initRepo(t, repo)
	gitTest(t, root, "init", "--bare", remote)
	gitTest(t, repo, "remote", "add", "origin", remote)
	gitTest(t, repo, "push", "-u", "origin", "main")
	gitTest(t, repo, "remote", "set-head", "origin", "main")
	gitTest(t, repo, "config", "remote.origin.url", "https://github.com/acme/shop.git")
	gitTest(t, repo, "config", "url.file://"+remote+".insteadOf", "https://github.com/acme/shop.git")
	gitTest(t, repo, "worktree", "add", "-b", "posse/t1", worktree, "refs/heads/main")
	if err := os.WriteFile(filepath.Join(worktree, "change.txt"), []byte("worker change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, worktree, "add", "change.txt")
	gitTest(t, worktree, "commit", "-m", "worker change")
	if err := os.MkdirAll(filepath.Join(home, "projects", "shop", "tasks", "t1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "projects", "shop", "tasks", "t1", "brief.md"), []byte("---\ntype: change\ntitle: E2E Brief title\ndone_when: commit exists\n---\nE2E intent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nmerge_mode = \""+mode+"\"\nmerge_method = \"squash\"\nauto_teardown = \"never\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close PR fixture database: %v", err)
		}
	})
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 1, Type: "change", Title: "E2E PR title", State: store.StateSpawning, MergeMode: mode, Branch: "posse/t1", BaseRef: "main", WorktreePath: worktree, HerdrWorkspaceID: "w2", PaneID: "w2:p1", PaneLabel: "posse:shop:t1", AutonomyMerge: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if state == store.StateDone {
		if _, err := db.AddSignal(context.Background(), taskID, "done", "Worker completion summary", nil); err != nil {
			t.Fatal(err)
		}
		if err := db.Transition(context.Background(), taskID, store.StateWorking, store.StateDone, "worker", "Worker completion summary"); err != nil {
			t.Fatal(err)
		}
	}
	fakeHerdr := herdr.NewFake()
	fakeHerdr.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"},
		{PaneID: "w2:p1", WorkspaceID: "w2", Label: "posse:shop:t1", CWD: worktree, Agent: "claude", AgentStatus: "idle"},
	}}
	service := testService(home, fakeHerdr)
	fixture := &prMergeFixture{
		test: t, root: root, repo: repo, remote: remote, worktree: worktree, home: home,
		ghLog: filepath.Join(root, "gh.log"), ghState: filepath.Join(root, "gh-state.json"), ghHead: filepath.Join(root, "gh-head"),
		ghOpenPRs: filepath.Join(root, "gh-open-prs.json"),
		bin:       filepath.Join(root, "bin"),
		service:   service, db: db, project: project,
	}
	fixture.headSHA = strings.TrimSpace(gitTest(t, worktree, "rev-parse", "HEAD"))
	fixture.task, err = db.Task(context.Background(), project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	fixture.installFakeGH(t)
	return fixture
}

func removeFixtureTaskPane(fixture *prMergeFixture) {
	remove := func(snapshot herdr.Snapshot) herdr.Snapshot {
		panes := snapshot.Panes[:0]
		for _, pane := range snapshot.Panes {
			if pane.PaneID != fixture.task.PaneID && pane.Label != fixture.task.PaneLabel {
				panes = append(panes, pane)
			}
		}
		snapshot.Panes = panes
		agents := snapshot.Agents[:0]
		for _, agent := range snapshot.Agents {
			if agent.PaneID != fixture.task.PaneID {
				agents = append(agents, agent)
			}
		}
		snapshot.Agents = agents
		return snapshot
	}
	switch adapter := fixture.service.Herdr.(type) {
	case *herdr.Fake:
		adapter.SnapshotValue = remove(adapter.SnapshotValue)
	case *changingSnapshotAdapter:
		adapter.mu.Lock()
		adapter.snapshot = remove(adapter.snapshot)
		adapter.mu.Unlock()
	}
}

func (fixture *prMergeFixture) installFakeGH(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(fixture.bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$POSSE_TEST_GH_LOG"
if [ "${POSSE_TEST_GH_FAIL:-}" != "" ]; then printf 'fake gh unavailable\n' >&2; exit 1; fi
case "$1 $2" in
  "run rerun") printf 'rerun requested\n' ;;
  "run view") printf 'unit failed log\n' ;;
  "api graphql")
    if [ -n "${POSSE_TEST_GH_ADVANCE_SHA:-}" ]; then
      git -C "$POSSE_TEST_GH_ADVANCE_REPO" push origin "$POSSE_TEST_GH_ADVANCE_SHA:refs/heads/${POSSE_TEST_GH_ADVANCE_BRANCH:-main}" >/dev/null
    fi
    cat "$POSSE_TEST_GH_STATE"
    # A .next state answers every later query, as after a merge.
    if [ -e "$POSSE_TEST_GH_STATE.next" ]; then mv "$POSSE_TEST_GH_STATE.next" "$POSSE_TEST_GH_STATE"; fi
    if [ -e "$POSSE_TEST_GH_STATE.exit" ]; then printf 'GraphQL request returned errors\n' >&2; exit 1; fi ;;
  "pr list")
    if [ "${POSSE_TEST_GH_FILTER_BASE:-}" = "1" ]; then
      case "$*" in
        *" --base "*) printf '[]\n' ;;
        *) cat "$POSSE_TEST_GH_OPEN_PRS" ;;
      esac
    else cat "$POSSE_TEST_GH_OPEN_PRS"; fi ;;
  "issue view")
    case "$3" in
      12) printf '{"state":"CLOSED"}\n' ;;
      13) printf 'issue not found\n' >&2; exit 1 ;;
      *) printf '{"state":"OPEN"}\n' ;;
    esac ;;
  "pr create") printf '%s\n' "$POSSE_TEST_GH_URL" ;;
  "pr edit")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --title)
          if [ -n "${POSSE_TEST_GH_EDIT_TITLE:-}" ]; then printf '%s' "$2" > "$POSSE_TEST_GH_EDIT_TITLE"; fi
          shift 2 ;;
        --body) printf '%s' "$2" > "$POSSE_TEST_GH_EDIT_BODY"; shift 2 ;;
        --base)
          if [ -n "${POSSE_TEST_GH_BASE_BRANCH_FILE:-}" ]; then printf '%s' "$2" > "$POSSE_TEST_GH_BASE_BRANCH_FILE"; fi
          shift 2 ;;
        *) shift ;;
      esac
    done
    printf 'Edited\n' ;;
  "pr view")
    base="${POSSE_TEST_GH_BASE_BRANCH:-main}"
    if [ -n "${POSSE_TEST_GH_BASE_BRANCH_FILE:-}" ] && [ -f "$POSSE_TEST_GH_BASE_BRANCH_FILE" ]; then base=$(cat "$POSSE_TEST_GH_BASE_BRANCH_FILE"); fi
    case "$*" in
      *"--json title,body"*) python3 -c 'import json,sys; print(json.dumps({"title":sys.argv[1],"body":sys.argv[2]}))' "E2E Brief title" "${POSSE_TEST_GH_BODY-}" ;;
      *"--json body"*) python3 -c 'import json,sys; print(json.dumps({"body":sys.argv[1]}))' "${POSSE_TEST_GH_BODY-}" ;;
      *) state=${POSSE_TEST_GH_VIEW_STATE:-}
         if [ -z "$state" ]; then
           state=OPEN
           if grep -q '\"state\":\"MERGED\"' "$POSSE_TEST_GH_STATE"; then state=MERGED
           elif grep -q '\"state\":\"CLOSED\"' "$POSSE_TEST_GH_STATE"; then state=CLOSED
           fi
         fi
         printf '{"url":"%s","state":"%s","headRefOid":"%s","headRefName":"%s","baseRefName":"%s","headRepository":{"nameWithOwner":"%s"}}\n' "${POSSE_TEST_GH_VIEW_URL:-$POSSE_TEST_GH_URL}" "$state" "$(cat "$POSSE_TEST_GH_HEAD")" "${POSSE_TEST_GH_HEAD_BRANCH:-posse/t1}" "$base" "${POSSE_TEST_GH_SOURCE:-acme/shop}" ;;
    esac ;;
  "pr merge") printf 'Merged\n' ;;
  *) printf 'unexpected fake gh command: %s\n' "$*" >&2; exit 90 ;;
esac
`
	if err := os.WriteFile(filepath.Join(fixture.bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fixture.bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GH_LOG", fixture.ghLog)
	t.Setenv("POSSE_TEST_GH_STATE", fixture.ghState)
	t.Setenv("POSSE_TEST_GH_OPEN_PRS", fixture.ghOpenPRs)
	t.Setenv("POSSE_TEST_GH_HEAD", fixture.ghHead)
	t.Setenv("POSSE_TEST_GH_URL", "https://github.com/acme/shop/pull/17")
	t.Setenv("POSSE_TEST_GH_BODY", "")
	t.Setenv("POSSE_TEST_GH_EDIT_BODY", filepath.Join(fixture.root, "gh-edited-body.md"))
	t.Setenv("POSSE_TEST_GH_FAIL", "")
	t.Setenv("POSSE_TEST_GH_CANCELLED", "")
	t.Setenv("POSSE_TEST_GH_FAILURE_NAME", "")
	t.Setenv("POSSE_TEST_GH_FAILURE_URL", "")
	t.Setenv("POSSE_TEST_GH_ADVANCE_SHA", "")
	t.Setenv("POSSE_TEST_GH_ADVANCE_REPO", fixture.repo)
	t.Setenv("POSSE_TEST_GH_ADVANCE_BRANCH", "main")
	fixture.setGHHead(t, fixture.headSHA)
	if err := os.WriteFile(fixture.ghOpenPRs, []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
}

func (fixture *prMergeFixture) setGHHead(t *testing.T, head string) {
	t.Helper()
	if err := os.WriteFile(fixture.ghHead, []byte(head), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fixture *prMergeFixture) installFakeNoMistakes(t *testing.T, response string, exitCode int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fixture.bin, "no-mistakes"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$POSSE_TEST_NO_MISTAKES_LOG\"\nprintf '%s' \"$POSSE_TEST_NO_MISTAKES_RESPONSE\"\nexit \"$POSSE_TEST_NO_MISTAKES_EXIT\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_NO_MISTAKES_LOG", filepath.Join(fixture.root, "no-mistakes.log"))
	t.Setenv("POSSE_TEST_NO_MISTAKES_RESPONSE", response)
	t.Setenv("POSSE_TEST_NO_MISTAKES_EXIT", fmt.Sprint(exitCode))
}

func (fixture *prMergeFixture) setGraphQLState(t *testing.T, state, checks, review, mergeable, mergeCommit, head string) {
	t.Helper()
	contexts := []any{}
	if checks == "FAILURE" {
		conclusion, detailsURL := "FAILURE", defaultValue(os.Getenv("POSSE_TEST_GH_FAILURE_URL"), "https://checks.example/unit")
		checkName := defaultValue(os.Getenv("POSSE_TEST_GH_FAILURE_NAME"), "unit")
		checkRun := map[string]any{"__typename": "CheckRun", "name": checkName, "status": "COMPLETED", "conclusion": conclusion, "detailsUrl": detailsURL}
		if os.Getenv("POSSE_TEST_GH_CANCELLED") == "1" {
			checkRun["conclusion"] = "CANCELLED"
			checkRun["detailsUrl"] = "https://github.com/acme/shop/actions/runs/123/job/456"
			checkRun["databaseId"] = 456
			checkRun["checkSuite"] = map[string]any{"workflowRun": map[string]any{"databaseId": 123}}
		}
		contexts = append(contexts, checkRun)
	}
	response := map[string]any{"data": map[string]any{"repo1": map[string]any{"pr1": map[string]any{
		"url": "https://github.com/acme/shop/pull/17", "state": state, "headRefOid": head, "baseRefName": defaultValue(os.Getenv("POSSE_TEST_GH_BASE_BRANCH"), "main"), "mergeable": mergeable, "reviewDecision": review,
		"mergeCommit": map[string]any{"oid": mergeCommit}, "statusCheckRollup": map[string]any{"state": checks, "contexts": map[string]any{"nodes": contexts}},
	}}}}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.ghState, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fixture *prMergeFixture) run(args ...string) (int, string, string) {
	fixture.output.Reset()
	fixture.errOut.Reset()
	cli := fixture.service.CLI()
	cli.Out = &fixture.output
	cli.ErrOut = &fixture.errOut
	cwd := fixture.repo
	if len(args) > 0 && (args[0] == "signal" || args[0] == "publish") {
		cwd = fixture.worktree
	}
	previousCWD, err := os.Getwd()
	if err != nil {
		fixture.test.Fatalf("get test working directory: %v", err)
	}
	previousPWD, hadPWD := os.LookupEnv("PWD")
	if err := os.Chdir(cwd); err != nil {
		fixture.test.Fatalf("change CLI working directory: %v", err)
	}
	_ = os.Setenv("PWD", cwd)
	defer func() {
		if err := os.Chdir(previousCWD); err != nil {
			fixture.test.Errorf("restore test working directory: %v", err)
		}
		if hadPWD {
			_ = os.Setenv("PWD", previousPWD)
		} else {
			_ = os.Unsetenv("PWD")
		}
	}()
	// The fixture has a Task worktree but no Pool Sandbox row. Mark publish
	// as a Worker invocation; restore before the next Lead command.
	if len(args) > 0 && args[0] == "publish" {
		previous, present := os.LookupEnv("POSSE_WORKER_HOME")
		_ = os.Setenv("POSSE_WORKER_HOME", fixture.home)
		defer func() {
			if present {
				_ = os.Setenv("POSSE_WORKER_HOME", previous)
			} else {
				_ = os.Unsetenv("POSSE_WORKER_HOME")
			}
		}()
	}
	code := cli.Run(args)
	return code, fixture.output.String(), fixture.errOut.String()
}
