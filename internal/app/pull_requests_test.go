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
)

func TestPRLandingPushesGatedBranchAndOpensPullRequest(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	fixture.setGraphQLState(t, "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.task.GatedSHA)

	code, output, errOutput := fixture.run("land", "t1")
	if code != 0 {
		t.Fatalf("PR land exit=%d output=%s error=%s", code, output, errOutput)
	}

	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateLanding || task.GatedSHA == "" || task.PRURL != "https://github.com/acme/shop/pull/17" {
		t.Fatalf("PR Task was not recorded in landing: %#v, %v", task, err)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.remote, "show-ref", "--hash", "refs/heads/posse/t1")); got != task.GatedSHA {
		t.Fatalf("pushed PR head=%q want gated SHA %q", got, task.GatedSHA)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "pr_opened" {
		t.Fatalf("PR opened Notices = %#v, %v", notices, err)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil || !strings.Contains(string(log), "pr create") || !strings.Contains(string(log), "E2E intent") || !strings.Contains(string(log), "Worker completion summary") {
		t.Fatalf("gh pr create did not receive the Brief intent and Worker summary: %q, %v", log, err)
	}
	if !strings.Contains(string(log), "--title E2E Brief title") {
		t.Fatalf("PR title did not come from the Brief: %q", log)
	}
}

func TestSendToLandingWithoutFailureNoticeReturnsToWorking(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("PR land exit=%d output=%s error=%s", code, output, errOutput)
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
	if err != nil || before.State != store.StateLanding || before.GatedSHA == "" {
		t.Fatalf("PR Task was not gated before follow-up send: %#v, %v", before, err)
	}

	code, output, errOutput := fixture.run("send", "t1", "Add the expanded follow-up work.")
	if code != 0 || !strings.Contains(output, "delivered") {
		t.Fatalf("landing follow-up send exit=%d output=%s error=%s", code, output, errOutput)
	}
	after, err := fixture.db.Task(ctx, fixture.project.ID, "t1")
	if err != nil || after.State != store.StateWorking || after.GatedSHA != "" || after.PRURL != before.PRURL {
		t.Fatalf("landing follow-up did not return the same PR Task to working and clear its gated SHA: before=%#v after=%#v err=%v", before, after, err)
	}
}

func TestSendRefusesTerminalTasksWithNextSteps(t *testing.T) {
	cases := []struct {
		name     string
		state    store.State
		taskType string
		next     string
	}{
		{name: "landed", state: store.StateLanded, taskType: "ship", next: "Create a new Ship Task"},
		{name: "torn-down", state: store.StateTornDown, taskType: "ship", next: "Create a new Ship Task"},
		{name: "failed", state: store.StateFailed, taskType: "ship", next: "posse relaunch t1"},
		{name: "lost", state: store.StateLost, taskType: "ship", next: "posse relaunch t1"},
		{name: "reported", state: store.StateReported, taskType: "scout", next: "Create a new Ship Task"},
		{name: "done non-ship", state: store.StateDone, taskType: "scout", next: "Create a new Ship Task"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPRLandingFixture(t, "pr", store.StateDone)
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

func TestSendToLandingRefusesWhileMergeIntentRuns(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("PR land exit=%d output=%s error=%s", code, output, errOutput)
	}
	if err := fixture.db.StartIntent(context.Background(), fixture.project.ID, fixture.task.ID, "land --merge", "merge", `{}`, os.Getpid()); err != nil {
		t.Fatal(err)
	}

	code, output, errOutput := fixture.run("send", "t1", "Do not interrupt the merge.")
	if code != 1 || !strings.Contains(output, "intent_active") || !strings.Contains(output, "land --merge") || !strings.Contains(output, "finish") {
		t.Fatalf("send during merge exit=%d output=%s error=%s", code, output, errOutput)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	var queued int
	if queryErr := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE task_id=?`, fixture.task.ID).Scan(&queued); queryErr != nil {
		t.Fatal(queryErr)
	}
	if err != nil || task.State != store.StateLanding || task.GatedSHA == "" || queued != 0 {
		t.Fatalf("send interrupted or queued work during merge: task=%#v queued=%d err=%v", task, queued, err)
	}
}

func TestPRLandingRequiresCleanMountAndRunsTheConfiguredGate(t *testing.T) {
	dirty := newPRLandingFixture(t, "pr", store.StateDone)
	if err := os.WriteFile(filepath.Join(dirty.worktree, "uncommitted.txt"), []byte("uncommitted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output, _ := dirty.run("land", "t1"); code != 1 || !strings.Contains(output, "Task worktree is not clean") {
		t.Fatalf("PR landing did not reject a dirty Mount: exit=%d output=%s", code, output)
	}
	if _, err := os.Stat(filepath.Join(dirty.remote, "refs", "heads", "posse", "t1")); !os.IsNotExist(err) {
		t.Fatalf("dirty Mount was pushed to origin: %v", err)
	}

	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	configText := "[defaults]\nlanding_mode = \"pr\"\nmerge_method = \"squash\"\nauto_unsaddle = \"never\"\ngate = [\"touch " + filepath.Join(fixture.root, "gate-ran") + "\"]\n"
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output, errOutput := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("PR landing with a passing Gate exit=%d output=%s error=%s", code, output, errOutput)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "gate-ran")); err != nil {
		t.Fatalf("configured Gate did not run: %v", err)
	}
}

func TestPRLandingGateFailureDoesNotPushOrOpenPR(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte("[defaults]\nlanding_mode = \"pr\"\nmerge_method = \"squash\"\nauto_unsaddle = \"never\"\ngate = [\"exit 1\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output, _ := fixture.run("land", "t1")
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	notices, noticeErr := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	remotePRRefs := strings.TrimSpace(gitTest(t, fixture.remote, "for-each-ref", "--format=%(refname)", "refs/heads/posse/"))
	ghLog, readErr := os.ReadFile(fixture.ghLog)
	if code != 1 || !strings.Contains(output, "gate_failed") || err != nil || task.State != store.StateDone || task.GatedSHA == "" || noticeErr != nil || countNoticeKind(notices, "gate_failed") != 1 || remotePRRefs != "" || (readErr == nil && len(ghLog) != 0) || (readErr != nil && !os.IsNotExist(readErr)) {
		t.Fatalf("failed Gate did not leave the Task unpushed and done: exit=%d output=%s task=%#v notices=%#v refs=%q gh=%q errors=%v/%v/%v", code, output, task, notices, remotePRRefs, ghLog, err, noticeErr, readErr)
	}
}

func TestPRMergeRequiresApprovalAndPinsTheGatedHead(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("land", "t1"); code != 0 {
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
	}, store.PRObservationEffect{}); err != nil {
		t.Fatal(err)
	}

	if code, output, _ := fixture.run("land", "t1", "--merge"); code != 1 || !strings.Contains(output, "land_approval_required") {
		t.Fatalf("merge without approval exit=%d output=%s", code, output)
	}
	if code, output, errOutput := fixture.run("land", "t1", "--merge", "--user-approved", "User approved this PR"); code != 0 {
		t.Fatalf("approved PR merge exit=%d output=%s error=%s", code, output, errOutput)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil || !strings.Contains(string(log), "--squash") || !strings.Contains(string(log), "--match-head-commit "+task.GatedSHA) {
		t.Fatalf("gh pr merge did not pin the gated SHA and configured method: %q, %v", log, err)
	}
}

func TestPRMergeRequiresGreenObservationForGatedHead(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("land", "t1"); code != 0 {
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

	code, output, _ := fixture.run("land", "t1", "--merge")
	if code != 1 || !strings.Contains(output, "checks_not_green") {
		t.Fatalf("merge without an observation exit=%d output=%s", code, output)
	}
	pending := store.PRObservation{
		ProjectID: fixture.project.ID, TaskID: task.ID, PRURL: task.PRURL,
		HeadSHA: task.GatedSHA, State: "OPEN", Checks: marshalJSON(checkSnapshot{State: "PENDING"}),
		Review: "APPROVED", Mergeable: "MERGEABLE",
	}
	if _, err := fixture.db.RecordPRObservation(context.Background(), pending, store.PRObservationEffect{}); err != nil {
		t.Fatal(err)
	}

	code, output, _ = fixture.run("land", "t1", "--merge", "--user-approved", "User approved this PR")
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
	if _, err := fixture.db.RecordPRObservation(context.Background(), green, store.PRObservationEffect{}); err != nil {
		t.Fatal(err)
	}
	differentHead := green
	differentHead.HeadSHA = strings.Repeat("f", 40)
	if _, err := fixture.db.RecordPRObservation(context.Background(), differentHead, store.PRObservationEffect{}); err != nil {
		t.Fatal(err)
	}
	code, output, _ = fixture.run("land", "t1", "--merge", "--user-approved", "User approved this PR")
	if code != 1 || !strings.Contains(output, "checks_not_green") {
		t.Fatalf("merge with a green observation of a different head exit=%d output=%s", code, output)
	}
	log, err = os.ReadFile(fixture.ghLog)
	if err != nil || strings.Contains(string(log), "pr merge") {
		t.Fatalf("PR with a different latest head observation was merged: log=%q err=%v", log, err)
	}

	if _, err := fixture.db.RecordPRObservation(context.Background(), green, store.PRObservationEffect{}); err != nil {
		t.Fatal(err)
	}
	if code, output, errOutput := fixture.run("land", "t1", "--merge"); code != 0 {
		t.Fatalf("merge with green checks exit=%d output=%s error=%s", code, output, errOutput)
	}
	log, err = os.ReadFile(fixture.ghLog)
	if err != nil || !strings.Contains(string(log), "pr merge") || !strings.Contains(string(log), "--match-head-commit "+task.GatedSHA) {
		t.Fatalf("green PR was not merged at the gated SHA: log=%q err=%v", log, err)
	}
}

func TestPRMergeReturnsTaskToDoneWhenRemoteHeadMoved(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	taskBefore, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", taskBefore.GatedSHA)
	fixture.setGHHead(t, strings.Repeat("f", 40))

	code, output, _ := fixture.run("land", "t1", "--merge", "--user-approved", "User approved this PR")
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if code != 1 || !strings.Contains(output, "branch_moved") || err != nil || task.State != store.StateDone || task.GatedSHA != "" {
		t.Fatalf("moved PR head did not return Task to done: exit=%d output=%s task=%#v err=%v", code, output, task, err)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil || strings.Contains(string(log), "pr merge") {
		t.Fatalf("moved PR head was merged: log=%q err=%v", log, err)
	}
}

func TestNoMistakesDoneSignalRecordsPRForLand(t *testing.T) {
	fixture := newPRLandingFixture(t, "no-mistakes", store.StateWorking)
	fixture.installFakeNoMistakes(t, "status: initialized\n", 0)
	prURL := "https://github.com/acme/shop/pull/17"
	code, output, errOutput := fixture.run("holler", "done", "Worker completion summary", "--pr", prURL)
	if code != 0 {
		t.Fatalf("no-mistakes done Signal exit=%d output=%s error=%s", code, output, errOutput)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateDone || task.PRURL != prURL || task.GatedSHA != "" {
		t.Fatalf("no-mistakes PR was not recorded for Land: %#v, %v", task, err)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err == nil && (strings.Contains(string(log), "pr create") || strings.Contains(string(log), "git push")) {
		t.Fatalf("no-mistakes delivery unexpectedly opened or pushed a PR: %q, %v", log, err)
	}
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	code, output, errOutput = fixture.run("land", "t1")
	if code != 0 {
		t.Fatalf("no-mistakes Land exit=%d output=%s error=%s", code, output, errOutput)
	}
	task, err = fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateLanding || task.GatedSHA != fixture.headSHA {
		t.Fatalf("posse land did not start watching the no-mistakes PR: %#v, %v", task, err)
	}
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", task.GatedSHA)
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	code, output, errOutput = fixture.run("land", "t1", "--merge", "--user-approved", "User approved the no-mistakes PR")
	if code != 0 {
		t.Fatalf("no-mistakes PR merge exit=%d output=%s error=%s", code, output, errOutput)
	}
	log, err = os.ReadFile(fixture.ghLog)
	if err != nil || !strings.Contains(string(log), "pr merge") {
		t.Fatalf("no-mistakes merge did not use the PR merge path: %q, %v", log, err)
	}
}

func TestNoMistakesDoneRejectsPullRequestFromAnotherRepository(t *testing.T) {
	fixture := newPRLandingFixture(t, "no-mistakes", store.StateWorking)
	fixture.installFakeNoMistakes(t, "status: initialized\n", 0)
	prURL := "https://github.com/other/fork/pull/3"
	code, output, _ := fixture.run("holler", "done", "Worker completion summary", "--pr", prURL)
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if code != 1 || !strings.Contains(output, "pr_repository_mismatch") || !strings.Contains(output, "acme/shop") || err != nil || task.State != store.StateWorking || task.PRURL != "" {
		t.Fatalf("foreign PR URL was accepted: exit=%d output=%s task=%#v err=%v", code, output, task, err)
	}
}

func TestPRLandingDoesNotWriteBranchTrackingConfig(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("PR land exit=%d output=%s error=%s", code, output, errOutput)
	}
	config, err := os.ReadFile(filepath.Join(fixture.repo, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), `[branch "posse/t1"]`) || strings.Contains(string(config), "merge = refs/heads/posse/t1") {
		t.Fatalf("PR landing added branch tracking config to the User's Project: %s", config)
	}
}

func TestWorkerPublishesSignalsAndLeadGatesWatchesAndMerges(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateWorking)
	gate := filepath.Join(f.root, "gate-ran")
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("[defaults]\nlanding_mode = \"pr\"\nmerge_method = \"squash\"\nauto_unsaddle = \"never\"\ngate = [\"touch "+gate+"\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	url := "https://github.com/acme/shop/pull/17"
	if code, output, stderr := f.run("publish", "Worker completion summary"); code != 0 || !strings.Contains(output, url) {
		t.Fatalf("publish: %d %s %s", code, output, stderr)
	}
	if got := strings.TrimSpace(gitTest(t, f.remote, "rev-parse", "refs/heads/posse/t1")); got != f.headSHA {
		t.Fatalf("published %s instead of %s", got, f.headSHA)
	}
	if _, err := os.Stat(gate); !os.IsNotExist(err) {
		t.Fatalf("publish ran the Lead's gate: %v", err)
	}
	if code, output, _ := f.run("holler", "done", "Worker completion summary"); code != 1 || !strings.Contains(output, "requires --pr") {
		t.Fatalf("missing URL: %d %s", code, output)
	}
	if code, output, stderr := f.run("holler", "done", "Worker completion summary", "--pr", url); code != 0 {
		t.Fatalf("done: %d %s %s", code, output, stderr)
	}
	if code, output, stderr := f.run("land", "t1"); code != 0 {
		t.Fatalf("land: %d %s %s", code, output, stderr)
	}
	task, err := f.db.Task(context.Background(), f.project.ID, "t1")
	if err != nil || task.State != store.StateLanding || task.PRURL != url || task.GatedSHA != f.headSHA {
		t.Fatalf("not watching published PR: %+v %v", task, err)
	}
	if _, err := os.Stat(gate); err != nil {
		t.Fatalf("gate did not run: %v", err)
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
	if countNoticeKind(notices, "land_ready") != 1 {
		t.Fatalf("PR watch missed checks and review: %+v", notices)
	}
	if code, output, _ := f.run("land", "t1", "--merge"); code != 1 || !strings.Contains(output, "land_approval_required") {
		t.Fatalf("merge without approval: %d %s", code, output)
	}
	if code, output, stderr := f.run("land", "t1", "--merge", "--user-approved", "User approved the PR"); code != 0 {
		t.Fatalf("merge: %d %s %s", code, output, stderr)
	}
	log, _ = os.ReadFile(f.ghLog)
	if !strings.Contains(string(log), "--match-head-commit "+f.headSHA) {
		t.Fatalf("merge was not pinned: %s", log)
	}
}

func TestWorkerPublishAdoptsExistingPR(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateWorking)
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

func TestWorkerDoneAcceptsMergedPRAfterSourceBranchDeletion(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	mount := attachPRFixtureMount(t, fixture)
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(`[defaults]
landing_mode = "pr"
merge_method = "squash"
auto_unsaddle = "finished"
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

	if code, output, stderr := fixture.run("holler", "done", "Worker completion summary", "--pr", "https://github.com/acme/shop/pull/17"); code != 0 {
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
	fixture.test.Chdir(fixture.repo)
	if _, err := fixture.service.prepareProject(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatalf("reconcile merged PR after done Signal: %v", err)
	}
	task, err = fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateTornDown || task.LandedRef != mergeCommit {
		t.Fatalf("merged PR did not land and teardown: %#v, %v", task, err)
	}
	releasedMount, err := fixture.db.MountByTask(context.Background(), task.ID)
	if err == nil {
		t.Fatalf("auto-teardown left Mount held: %#v", releasedMount)
	}
	mounts, err := fixture.db.Mounts(context.Background(), fixture.project.ID)
	if err != nil || len(mounts) != 1 || mounts[0].ID != mount.ID || mounts[0].State != "idle" || mounts[0].TaskID != 0 {
		t.Fatalf("safe merged Task Mount was not released: %#v, %v", mounts, err)
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
			fixture := newPRLandingFixture(t, "pr", store.StateWorking)
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

			code, output, _ := fixture.run("holler", "done", "Worker completion summary", "--pr", "https://github.com/acme/shop/pull/17")
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
			f := newPRLandingFixture(t, "pr", store.StateWorking)
			if code, output, _ := f.run("publish", "Worker completion summary"); code != 0 {
				t.Fatalf("publish: %d %s", code, output)
			}
			if tc.source != "" {
				t.Setenv("POSSE_TEST_GH_SOURCE", tc.source)
			}
			if tc.head != "" {
				f.setGHHead(t, tc.head)
			}
			code, output, _ := f.run("holler", "done", "Worker completion summary", "--pr", tc.url)
			task, err := f.db.Task(context.Background(), f.project.ID, "t1")
			if code != 1 || !strings.Contains(output, tc.want) || err != nil || task.State != store.StateWorking || task.PRURL != "" {
				t.Fatalf("invalid PR accepted: %d %s %+v %v", code, output, task, err)
			}
		})
	}
}

func TestClosedWorkerPRMustBeReplacedBeforeRelanding(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateWorking)
	url := "https://github.com/acme/shop/pull/17"
	if code, out, _ := f.run("publish", "Worker completion summary"); code != 0 {
		t.Fatalf("publish: %d %s", code, out)
	}
	if code, out, _ := f.run("holler", "done", "Worker completion summary", "--pr", url); code != 0 {
		t.Fatalf("done: %d %s", code, out)
	}
	if code, out, _ := f.run("land", "t1"); code != 0 {
		t.Fatalf("land: %d %s", code, out)
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
	if code, out, _ := f.run("land", "t1"); code != 1 || !strings.Contains(out, "pr_head_mismatch") {
		t.Fatalf("relied on closed Worker PR: %d %s", code, out)
	}
	task, _ := f.db.Task(context.Background(), f.project.ID, "t1")
	if task.PRURL != url || task.State != store.StateDone {
		t.Fatalf("discarded closed PR URL: %+v", task)
	}
}

func TestWorkerPublishRestrictedToPRModeAndOwnBranch(t *testing.T) {
	for _, mode := range []string{"local", "no-mistakes"} {
		t.Run(mode, func(t *testing.T) {
			f := newPRLandingFixture(t, mode, store.StateWorking)
			if code, output, _ := f.run("publish", "Worker completion summary"); code != 1 || !strings.Contains(output, "publish_refused") {
				t.Fatalf("unexpected publish: %d %s", code, output)
			}
			if got := strings.TrimSpace(gitTest(t, f.remote, "for-each-ref", "--format=%(refname)", "refs/heads/posse/")); got != "" {
				t.Fatalf("published branch in %s mode: %s", mode, got)
			}
		})
	}
	f := newPRLandingFixture(t, "pr", store.StateWorking)
	gitTest(t, f.worktree, "checkout", "--detach")
	if code, output, _ := f.run("publish", "Worker completion summary"); code != 1 || !strings.Contains(output, "publish_refused") {
		t.Fatalf("published detached HEAD: %d %s", code, output)
	}
}

func TestLeadDoesNotGateMovedWorkerPR(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateWorking)
	if code, output, _ := f.run("publish", "Worker completion summary"); code != 0 {
		t.Fatalf("publish: %d %s", code, output)
	}
	if code, output, _ := f.run("holler", "done", "Worker completion summary", "--pr", "https://github.com/acme/shop/pull/17"); code != 0 {
		t.Fatalf("done: %d %s", code, output)
	}
	f.setGHHead(t, strings.Repeat("f", 40))
	if code, output, _ := f.run("land", "t1"); code != 1 || !strings.Contains(output, "pr_head_mismatch") {
		t.Fatalf("gated moved PR: %d %s", code, output)
	}
	task, _ := f.db.Task(context.Background(), f.project.ID, "t1")
	if task.State != store.StateDone || task.GatedSHA != "" {
		t.Fatalf("moved PR reached landing: %+v", task)
	}
}

func TestUpRefusesNoMistakesModeWhenCLIReportsUninitialized(t *testing.T) {
	fixture := newPRLandingFixture(t, "no-mistakes", store.StateWorking)
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
	fixture := newPRLandingFixture(t, "local", store.StateWorking)
	fixture.installFakeNoMistakes(t, "error: repo not initialized (run 'no-mistakes init' first)\n", 1)
	configPath := filepath.Join(fixture.home, "config.toml")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	code, output, _ := fixture.run("config", "set", "defaults.landing_mode", "no-mistakes")
	after, readErr := os.ReadFile(configPath)
	log, logErr := os.ReadFile(filepath.Join(fixture.root, "no-mistakes.log"))
	if code != 1 || !strings.Contains(output, "no_mistakes_uninitialized") || readErr != nil || string(after) != string(before) || logErr != nil || !strings.Contains(string(log), "axi status") {
		t.Fatalf("config set did not preserve the prior config on no-mistakes refusal: exit=%d output=%s before=%q after=%q log=%q errors=%v/%v", code, output, before, after, log, readErr, logErr)
	}
}

func TestLeadInstructionsExplainPRNoticeActions(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	code, output, errOutput := fixture.run("lead")
	if code != 0 {
		t.Fatalf("lead instructions exit=%d output=%s error=%s", code, output, errOutput)
	}
	for _, expected := range []string{"pr_opened", "pr_checks_failed", "pr_changes_requested", "pr_conflict", "land_ready", "pr_merged", "pr_closed", "pr_watch_failing", "root_behind", "Autonomy"} {
		if !strings.Contains(output, expected) {
			t.Errorf("Lead instructions omit PR handling for %q: %s", expected, output)
		}
	}
	for _, expected := range []string{"landing Ship Task also accepts a follow-up `posse send` before those Notices", "returns it to working until the next `posse land`", "merge `origin/<default>` into the Task branch", "--user-approved", "<User's words>", "posse retries automatically", "tell the User if it persists"} {
		if !strings.Contains(output, expected) {
			t.Errorf("Lead instructions omit recovery guidance %q: %s", expected, output)
		}
	}
}

type prLandingFixture struct {
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

func newPRLandingFixture(t *testing.T, mode string, state store.State) *prLandingFixture {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	remote := filepath.Join(root, "origin.git")
	worktree := filepath.Join(root, "mount")
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
	if err := os.WriteFile(filepath.Join(home, "projects", "shop", "tasks", "t1", "brief.md"), []byte("---\ntype: ship\ntitle: E2E Brief title\ndone_when: commit exists\n---\nE2E intent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nlanding_mode = \""+mode+"\"\nmerge_method = \"squash\"\nauto_unsaddle = \"never\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 1, Type: "ship", Title: "E2E PR title", State: store.StateSpawning, LandingMode: mode, Branch: "posse/t1", BaseRef: "main", WorktreePath: worktree, HerdrWorkspaceID: "w2", PaneID: "w2:p1", PaneLabel: "posse:shop:t1", AutonomyLand: "ask"})
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
	fixture := &prLandingFixture{
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

func (fixture *prLandingFixture) installFakeGH(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(fixture.bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$POSSE_TEST_GH_LOG"
if [ "${POSSE_TEST_GH_FAIL:-}" != "" ]; then printf 'fake gh unavailable\n' >&2; exit 1; fi
case "$1 $2" in
  "api graphql") cat "$POSSE_TEST_GH_STATE" ;;
  "pr list") cat "$POSSE_TEST_GH_OPEN_PRS" ;;
  "pr create") printf '%s\n' "$POSSE_TEST_GH_URL" ;;
  "pr view") printf '{"url":"%s","state":"%s","headRefOid":"%s","headRefName":"%s","baseRefName":"%s","headRepository":{"nameWithOwner":"%s"}}\n' "${POSSE_TEST_GH_VIEW_URL:-$POSSE_TEST_GH_URL}" "${POSSE_TEST_GH_VIEW_STATE:-OPEN}" "$(cat "$POSSE_TEST_GH_HEAD")" "${POSSE_TEST_GH_HEAD_BRANCH:-posse/t1}" "${POSSE_TEST_GH_BASE_BRANCH:-main}" "${POSSE_TEST_GH_SOURCE:-acme/shop}" ;;
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
	t.Setenv("POSSE_TEST_GH_FAIL", "")
	fixture.setGHHead(t, fixture.headSHA)
	if err := os.WriteFile(fixture.ghOpenPRs, []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
}

func (fixture *prLandingFixture) setGHHead(t *testing.T, head string) {
	t.Helper()
	if err := os.WriteFile(fixture.ghHead, []byte(head), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fixture *prLandingFixture) installFakeNoMistakes(t *testing.T, response string, exitCode int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fixture.bin, "no-mistakes"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$POSSE_TEST_NO_MISTAKES_LOG\"\nprintf '%s' \"$POSSE_TEST_NO_MISTAKES_RESPONSE\"\nexit \"$POSSE_TEST_NO_MISTAKES_EXIT\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_NO_MISTAKES_LOG", filepath.Join(fixture.root, "no-mistakes.log"))
	t.Setenv("POSSE_TEST_NO_MISTAKES_RESPONSE", response)
	t.Setenv("POSSE_TEST_NO_MISTAKES_EXIT", fmt.Sprint(exitCode))
}

func (fixture *prLandingFixture) setGraphQLState(t *testing.T, state, checks, review, mergeable, mergeCommit, head string) {
	t.Helper()
	contexts := []any{}
	if checks == "FAILURE" {
		contexts = append(contexts, map[string]any{"__typename": "CheckRun", "name": "unit", "status": "COMPLETED", "conclusion": "FAILURE", "detailsUrl": "https://checks.example/unit"})
	}
	response := map[string]any{"data": map[string]any{"repo1": map[string]any{"pr1": map[string]any{
		"url": "https://github.com/acme/shop/pull/17", "state": state, "headRefOid": head, "mergeable": mergeable, "reviewDecision": review,
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

func (fixture *prLandingFixture) run(args ...string) (int, string, string) {
	fixture.output.Reset()
	fixture.errOut.Reset()
	cli := fixture.service.CLI()
	cli.Out = &fixture.output
	cli.ErrOut = &fixture.errOut
	cwd := fixture.repo
	if len(args) > 0 && (args[0] == "holler" || args[0] == "publish") {
		cwd = fixture.worktree
	}
	fixture.test.Chdir(cwd)
	// The fixture has a Task worktree but no Remuda Mount row. Mark publish
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
