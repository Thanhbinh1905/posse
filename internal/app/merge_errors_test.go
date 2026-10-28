package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestPRMergeBehindTargetReportsQueuedTargetSyncInstruction(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	fixture.requireCurrentTarget(t)
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("prepare Merge: exit=%d output=%s error=%s", code, output, stderr)
	}
	targetTip := fixture.advanceTarget(t, false, true)
	code, output, stderr := fixture.run("merge", "t1", "--execute", "--user-approved", "merge it")
	if code == 0 || !strings.Contains(output, "pr_branch_behind") || !strings.Contains(output, targetTip) || !strings.Contains(output, "Posse queued the Worker an instruction to merge origin/main") || !strings.Contains(output, "do not send a duplicate") {
		t.Fatalf("behind PR did not report the queued merge-target instruction: exit=%d output=%s error=%s", code, output, stderr)
	}
	// Later runs see the reopened Task without a gated head; they must still
	// explain the queued instruction instead of a generic state refusal.
	for attempt := 0; attempt < 2; attempt++ {
		code, output, stderr = fixture.run("merge", "t1", "--execute", "--user-approved", "merge it")
		if code == 0 || !strings.Contains(output, "pr_branch_behind") || !strings.Contains(output, targetTip) || !strings.Contains(output, "Posse queued the Worker an instruction") || strings.Contains(output, "must be done or merging") {
			t.Fatalf("retry %d after the queued instruction was misleading: exit=%d output=%s error=%s", attempt, code, output, stderr)
		}
	}
	var instructions int
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE task_id=? AND body LIKE '%missing current target commit%'`, fixture.task.ID).Scan(&instructions); err != nil || instructions != 1 {
		t.Fatalf("merge-target instructions = %d, %v; want one", instructions, err)
	}
}

func TestPRMergeExecuteRaceWithQueuedTargetSyncNamesTheInstruction(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	fixture.requireCurrentTarget(t)
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("prepare Merge: exit=%d output=%s error=%s", code, output, stderr)
	}
	stale, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	fixture.advanceTarget(t, false, true)
	if code, output, _ := fixture.run("merge", "t1", "--execute", "--user-approved", "merge it"); code == 0 || !strings.Contains(output, "pr_branch_behind") {
		t.Fatalf("behind PR was not refused: %s", output)
	}
	// The Worker reopened the Task and cleared its gated head after this Merge
	// command had read it in merging.
	stale.GatedSHA = ""
	err = fixture.service.mergePullRequest(&axi.Context{Context: context.Background()}, fixture.db, fixture.project, config.Config{Defaults: config.Defaults{RequireCurrentTarget: true}}, stale, "merge it", store.Intent{})
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "pr_branch_behind" || !strings.Contains(failure.Message, "Posse queued the Worker an instruction to merge origin/main") || strings.Contains(failure.Message, "no recorded pull request") {
		t.Fatalf("racing Merge error = %#v (%v)", failure, err)
	}
}

func TestPRMergeExecuteRaceWithTaskReopenIsRetryable(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("prepare Merge: exit=%d output=%s error=%s", code, output, stderr)
	}
	stale, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(context.Background(), stale.ID, store.StateMerging, store.StateWorking, "lead", "Lead submitted an instruction"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.ClearTaskGatedSHA(context.Background(), stale.ID); err != nil {
		t.Fatal(err)
	}
	stale.GatedSHA = ""
	err = fixture.service.mergePullRequest(&axi.Context{Context: context.Background()}, fixture.db, fixture.project, config.Config{}, stale, "merge it", store.Intent{})
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "task_state_changed" || !failure.Retryable || !strings.Contains(failure.Message, "t1") {
		t.Fatalf("reopened Task Merge error = %#v (%v)", failure, err)
	}
}

func TestStructureTaskStateRaceNamesPublicTaskID(t *testing.T) {
	task := store.Task{ID: 379, Seq: 346}
	raced := fmt.Errorf("%w: Task 379 is not in %q", store.ErrStateRace, store.StateDone)
	var failure *axi.Error
	if err := structureTaskStateRace(raced, task); !errors.As(err, &failure) || failure.Code != "task_state_changed" || !failure.Retryable || !strings.Contains(failure.Message, "t346") || strings.Contains(failure.Message, "379") {
		t.Fatalf("state race = %#v (%v)", failure, err)
	}
	refused := axi.Failure("merge_refused", "kept", false)
	if err := structureTaskStateRace(refused, task); err != refused {
		t.Fatalf("structured failure was replaced: %v", err)
	}
	other := errors.New("disk full")
	if err := structureTaskStateRace(other, task); err != other {
		t.Fatalf("unrelated error was replaced: %v", err)
	}
	if err := structureTaskStateRace(nil, task); err != nil {
		t.Fatalf("nil error became %v", err)
	}
}

func TestPRWatchCIFixWaitsForRunAndNamesEveryFailedShard(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	freezeCILoop(t, fixture, 3, 0)
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	shard := func(n int, status, conclusion string) map[string]any {
		return map[string]any{"__typename": "CheckRun", "name": fmt.Sprintf("e2e shard %d", n), "status": status, "conclusion": conclusion, "detailsUrl": fmt.Sprintf("https://checks.example/shard-%d", n)}
	}
	writeRollup := func(nodes ...map[string]any) {
		t.Helper()
		response := map[string]any{"data": map[string]any{"repo1": map[string]any{"pr1": map[string]any{
			"url": "https://github.com/acme/shop/pull/17", "state": "OPEN", "headRefOid": fixture.headSHA, "baseRefName": "main", "mergeable": "MERGEABLE", "reviewDecision": "REVIEW_REQUIRED",
			"mergeCommit": map[string]any{"oid": ""}, "statusCheckRollup": map[string]any{"state": "FAILURE", "contexts": map[string]any{"nodes": nodes}},
		}}}}
		if err := os.WriteFile(fixture.ghState, []byte(marshalJSON(response)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	poll := func() {
		t.Helper()
		if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	countRounds := func() int {
		t.Helper()
		var rounds int
		if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`, fixture.task.ID).Scan(&rounds); err != nil {
			t.Fatal(err)
		}
		return rounds
	}

	// GitHub already rolls up to FAILURE while the other shards still run.
	writeRollup(shard(1, "COMPLETED", "FAILURE"), shard(2, "IN_PROGRESS", ""), shard(3, "QUEUED", ""), shard(4, "IN_PROGRESS", ""))
	poll()
	if rounds := countRounds(); rounds != 0 {
		t.Fatalf("CI fix round queued before the run settled: %d", rounds)
	}

	writeRollup(shard(1, "COMPLETED", "FAILURE"), shard(2, "COMPLETED", "FAILURE"), shard(3, "COMPLETED", "FAILURE"), shard(4, "COMPLETED", "FAILURE"))
	poll()
	if rounds := countRounds(); rounds != 1 {
		t.Fatalf("CI fix rounds after the run settled = %d, want 1", rounds)
	}
	var body string
	if err := fixture.db.QueryRowContext(ctx, `SELECT body FROM messages WHERE task_id=? AND status='queued'`, fixture.task.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 4; n++ {
		if !strings.Contains(body, fmt.Sprintf("- Check: e2e shard %d", n)) {
			t.Errorf("CI fix instruction omitted e2e shard %d: %s", n, body)
		}
	}
	var summary string
	if err := fixture.db.QueryRowContext(ctx, `SELECT summary FROM notices WHERE task_id=? AND kind='ci_fix_queued'`, fixture.task.ID).Scan(&summary); err != nil || !strings.Contains(summary, "e2e shard 1, e2e shard 2, e2e shard 3, e2e shard 4") {
		t.Fatalf("ci_fix_queued summary = %q, %v", summary, err)
	}
}

func TestPRWatchRaisesOneChecksFailedNoticePerRunAndHead(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	freezeCILoop(t, fixture, 3, 0)
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	const shards = 4
	writeRun := func(failed int, running bool) {
		t.Helper()
		nodes := []map[string]any{}
		for n := 1; n <= shards; n++ {
			status, conclusion := "COMPLETED", "SUCCESS"
			switch {
			case n <= failed:
				conclusion = "FAILURE"
			case running:
				status, conclusion = "IN_PROGRESS", ""
			}
			nodes = append(nodes, map[string]any{"__typename": "CheckRun", "name": fmt.Sprintf("e2e shard %d", n), "status": status, "conclusion": conclusion, "detailsUrl": fmt.Sprintf("https://checks.example/shard-%d", n)})
		}
		response := map[string]any{"data": map[string]any{"repo1": map[string]any{"pr1": map[string]any{
			"url": "https://github.com/acme/shop/pull/17", "state": "OPEN", "headRefOid": fixture.headSHA, "baseRefName": "main", "mergeable": "MERGEABLE", "reviewDecision": "REVIEW_REQUIRED",
			"mergeCommit": map[string]any{"oid": ""}, "statusCheckRollup": map[string]any{"state": "FAILURE", "contexts": map[string]any{"nodes": nodes}},
		}}}}
		if err := os.WriteFile(fixture.ghState, []byte(marshalJSON(response)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	poll := func() {
		t.Helper()
		if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	noticeCount := func() int {
		t.Helper()
		notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		return countNoticeKind(notices, "pr_checks_failed")
	}

	for failed := 1; failed < shards; failed++ {
		writeRun(failed, true)
		poll()
		if got := noticeCount(); got != 1 {
			t.Fatalf("pr_checks_failed Notices with %d of %d jobs failed and the run still going = %d, want 1", failed, shards, got)
		}
	}

	// The last job passes, so the settled list equals the last quiet poll's
	// list while the first Notice named only one shard.
	writeRun(shards-1, false)
	poll()
	if got := noticeCount(); got != 2 {
		t.Fatalf("pr_checks_failed Notices after the run completed = %d, want the first plus one update", got)
	}
	writeRun(shards-1, false)
	poll()
	if got := noticeCount(); got != 2 {
		t.Fatalf("pr_checks_failed Notices after an unchanged poll = %d, want 2", got)
	}
}
