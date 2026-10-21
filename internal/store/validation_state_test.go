package store

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidationStateSurvivesSnapshotRebuild(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	insertValidationStateFixture(t, f)
	if err := f.db.PersistTask(f.ctx, f.change); err != nil {
		t.Fatalf("persist validation state: %v", err)
	}
	before, err := f.db.validationStateForTask(f.ctx, f.change)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.ReviewRounds) != 1 || len(before.ReviewPatterns) != 1 || len(before.ReviewFindings) != 1 ||
		len(before.ReviewFindingEvents) != 1 || len(before.ReviewCarries) != 1 || len(before.ReviewDispositions) != 1 || len(before.ValidationFixRounds) != 2 {
		t.Fatalf("fixture did not cover every validation table: %#v", before)
	}
	if count, err := f.db.RebuildFromSnapshots(f.ctx, f.home); err != nil || count != 2 {
		t.Fatalf("rebuild count=%d err=%v", count, err)
	}
	after, err := f.db.validationStateForTask(f.ctx, f.change)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("validation state after rebuild = %#v, want %#v", after, before)
	}
}

func TestValidationInsertOnlyRowsRejectUpdates(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	insertValidationStateFixture(t, f)
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"finding event", `UPDATE review_finding_events SET note='changed' WHERE id=801`},
		{"carry", `UPDATE review_carries SET created_at=99 WHERE ship_task_id=?`},
		{"disposition", `UPDATE review_dispositions SET user_quote='changed' WHERE id=802`},
		{"fix round", `UPDATE validation_fix_rounds SET dedupe_key='changed' WHERE id=803`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if strings.Contains(tc.query, "ship_task_id=?") {
				_, err = f.db.ExecContext(f.ctx, tc.query, f.change)
			} else {
				_, err = f.db.ExecContext(f.ctx, tc.query)
			}
			if err == nil || !strings.Contains(err.Error(), "immutable") {
				t.Fatalf("insert-only update error = %v", err)
			}
		})
	}
}

func TestValidationRoundBindingsAreImmutableButStatusCanAdvance(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	insertValidationStateFixture(t, f)
	if _, err := f.db.ExecContext(f.ctx, `UPDATE review_rounds SET status='verdict',verdict='pass' WHERE ship_task_id=? AND review_task_id=?`, f.change, f.review); err != nil {
		t.Fatalf("record round verdict: %v", err)
	}
	if _, err := f.db.ExecContext(f.ctx, `UPDATE review_rounds SET head_sha=? WHERE ship_task_id=? AND review_task_id=?`, reviewShaB, f.change, f.review); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("round binding update error = %v", err)
	}
	if _, err := f.db.ExecContext(f.ctx, `UPDATE review_rounds SET verdict='changes_requested' WHERE ship_task_id=? AND review_task_id=?`, f.change, f.review); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("verdict rewrite error = %v", err)
	}
}

func TestVoidValidationRoundNumberCanBeReused(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	insertValidationStateFixture(t, f)
	if _, err := f.db.ExecContext(f.ctx, `UPDATE review_rounds SET status='void' WHERE ship_task_id=? AND review_task_id=?`, f.change, f.review); err != nil {
		t.Fatalf("void the first round: %v", err)
	}
	secondReview, err := f.db.CreateTask(f.ctx, f.task(t, f.change).ProjectID, Task{
		Seq: 3, Type: "review", ReviewsTaskID: f.change, Title: "Retry review", MergeMode: "local",
		ReviewTargets: []ReviewTarget{{Repository: "shop", Commit: reviewShaB}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.ctx, `INSERT INTO review_rounds(ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,verdict,created_at) VALUES(?,1,?,'shop','full',?,?,?,?,?,'[]','admitted',NULL,20)`,
		f.change, secondReview, reviewShaB, reviewShaB, reviewShaB, reviewShaB, reportHash); err != nil {
		t.Fatalf("reuse void round number: %v", err)
	}
	var count int
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM review_rounds WHERE ship_task_id=? AND round=1 AND repository='shop'`, f.change).Scan(&count); err != nil || count != 2 {
		t.Fatalf("round 1 history count=%d err=%v; want both void and retry rows", count, err)
	}
}

func TestReviewRoundAdmissionGuardsAndVoids(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	create := func(branch, name string, admission ReviewRoundAdmission) (int64, error) {
		changeTask := f.task(t, f.change)
		returnID, _, err := f.db.CreateReviewRoundTaskWithSequenceAndIntent(f.ctx, changeTask.ProjectID, "shop", Task{
			Type: "review", ReviewsTaskID: f.change, Title: name, ShortName: name, MergeMode: "local", Branch: branch,
			ReviewTargets: []ReviewTarget{{Repository: "shop", Commit: admission.Bindings[0].HeadSHA}},
		}, "start", 1, admission, func(int) (bool, error) { return false, nil })
		return returnID, err
	}
	admission := func(round int, head string, maxRounds int) ReviewRoundAdmission {
		return ReviewRoundAdmission{ChangeTaskID: f.change, Round: round, MaxRounds: maxRounds, Bindings: []ReviewRound{{
			ChangeTaskID: f.change, Round: round, Repository: "shop", ScopeKind: "full", ScopeBaseSHA: reviewShaA,
			HeadSHA: head, TargetCommitSHA: reviewShaA, BaselineTree: reviewShaA, DeltaSHA256: reportHash,
			ConflictedPaths: "[]", Status: "admitted",
		}}}
	}
	firstID, err := create("posse/t3", "round-one", admission(1, reviewShaA, 1))
	if err != nil {
		t.Fatalf("admit first round: %v", err)
	}
	if _, err := create("posse/t4", "active-retry", admission(1, reviewShaB, 1)); !errors.Is(err, ErrReviewRoundActive) {
		t.Fatalf("active round error = %v", err)
	}
	if err := f.db.Transition(f.ctx, firstID, StateSpawning, StateFailed, "cli", "failed before verdict"); err != nil {
		t.Fatal(err)
	}
	secondID, err := create("posse/t4", "round-one-retry", admission(1, reviewShaB, 1))
	if err != nil {
		t.Fatalf("admit reused void round number: %v", err)
	}
	rounds, err := f.db.ReviewRoundsFor(f.ctx, f.change)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 || rounds[0].Round != 1 || rounds[0].Status != "void" || rounds[1].Round != 1 || rounds[1].Status != "admitted" || rounds[1].ReviewTaskID != secondID {
		t.Fatalf("round history after void retry = %#v", rounds)
	}
	if _, err := f.db.ExecContext(f.ctx, `UPDATE review_rounds SET status='verdict',verdict='pass' WHERE review_task_id=?`, secondID); err != nil {
		t.Fatal(err)
	}
	if _, err := create("posse/t5", "unchanged", admission(2, reviewShaB, 1)); !errors.Is(err, ErrReviewRoundUnchanged) {
		t.Fatalf("unchanged head error = %v", err)
	}
	if _, err := create("posse/t5", "cap", admission(2, strings.Repeat("c", 40), 1)); !errors.Is(err, ErrReviewRoundCap) {
		t.Fatalf("cap error = %v", err)
	}
}

func TestReviewRoundAdmissionSharesRepairBudgetOnlyWhenEnabled(t *testing.T) {
	for _, shared := range []bool{false, true} {
		name := "separate"
		if shared {
			name = "shared"
		}
		t.Run(name, func(t *testing.T) {
			f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
			if _, err := f.db.QueueValidationFixInstruction(f.ctx, f.change, "gate", reviewShaA, "prior-gate-fix", "Prior Gate fix"); err != nil {
				t.Fatal(err)
			}
			admission := ReviewRoundAdmission{
				ChangeTaskID: f.change, Round: 1, MaxRounds: 1, SharedRepairBudget: shared,
				Bindings: []ReviewRound{{
					ChangeTaskID: f.change, Round: 1, Repository: "shop", ScopeKind: "full", ScopeBaseSHA: reviewShaA,
					HeadSHA: reviewShaB, TargetCommitSHA: reviewShaA, BaselineTree: reviewShaA, DeltaSHA256: reportHash,
					ConflictedPaths: "[]", Status: "admitted",
				}},
			}
			change := f.task(t, f.change)
			_, _, err := f.db.CreateReviewRoundTaskWithSequenceAndIntent(f.ctx, change.ProjectID, "shop", Task{
				Type: "review", ReviewsTaskID: f.change, Title: "round one", ShortName: "round-one", MergeMode: "local", Branch: "posse/t3",
				ReviewTargets: []ReviewTarget{{Repository: "shop", Commit: reviewShaB}},
			}, "start", 1, admission, func(int) (bool, error) { return false, nil })
			if shared && !errors.Is(err, ErrReviewRoundCap) {
				t.Fatalf("shared budget admission error = %v, want cap", err)
			}
			if !shared && err != nil {
				t.Fatalf("separate review-round budget was reduced by a Gate fix: %v", err)
			}
		})
	}
}

func TestReviewRoundAdmissionAllowsPartialWorkspaceDelta(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}, {Repository: "api", Commit: reviewShaB}})
	for _, row := range []struct {
		repository string
		head       string
	}{{"shop", reviewShaA}, {"api", reviewShaB}} {
		if _, err := f.db.ExecContext(f.ctx, `INSERT INTO review_rounds(ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,verdict,created_at) VALUES(?,1,?,?, 'full',?,?,?,?,?,'[]','verdict','pass',10)`,
			f.change, f.review, row.repository, reviewShaA, row.head, reviewShaA, row.head, reportHash); err != nil {
			t.Fatal(err)
		}
	}
	admission := ReviewRoundAdmission{ChangeTaskID: f.change, Round: 2, MaxRounds: 2, Bindings: []ReviewRound{
		{ChangeTaskID: f.change, Round: 2, Repository: "shop", ScopeKind: "delta", ScopeBaseSHA: reviewShaA, HeadSHA: reviewShaA, TargetCommitSHA: reviewShaA, BaselineTree: reviewShaA, DeltaSHA256: reportHash, ConflictedPaths: "[]", Status: "admitted"},
		{ChangeTaskID: f.change, Round: 2, Repository: "api", ScopeKind: "delta", ScopeBaseSHA: reviewShaB, HeadSHA: strings.Repeat("c", 40), TargetCommitSHA: reviewShaA, BaselineTree: reviewShaB, DeltaSHA256: reportHash, ConflictedPaths: "[]", Status: "admitted"},
	}}
	change := f.task(t, f.change)
	_, _, err := f.db.CreateReviewRoundTaskWithSequenceAndIntent(f.ctx, change.ProjectID, "shop", Task{
		Type: "review", ReviewsTaskID: f.change, Title: "partial workspace delta", ShortName: "partial-delta", MergeMode: "local", Branch: "posse/t3",
		ReviewTargets: []ReviewTarget{{Repository: "shop", Commit: reviewShaA}, {Repository: "api", Commit: strings.Repeat("c", 40)}},
	}, "start", 1, admission, func(int) (bool, error) { return false, nil })
	if err != nil {
		t.Fatalf("admit a round with one changed Member: %v", err)
	}
	rounds, err := f.db.ReviewRoundsFor(f.ctx, f.change)
	if err != nil || len(rounds) != 4 {
		t.Fatalf("partial workspace round rows = %#v, %v", rounds, err)
	}
}

func insertValidationStateFixture(t *testing.T, f reviewVerdictFixture) {
	t.Helper()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO review_rounds(ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,verdict,created_at) VALUES(?,1,?,'shop','full',?,?,?,?,?,'[]','admitted',NULL,10)`, []any{f.change, f.review, reviewShaA, reviewShaA, reviewShaA, reviewShaA, reportHash}},
		{`INSERT INTO review_patterns(id,ship_task_id,seq,key,title,rule,root_cause,fix_guidance,merged_into,created_round) VALUES(801,?,1,'unchecked-error','Dropped errors','Check all write errors','Callers ignore errors','Search all new writes',NULL,1)`, []any{f.change}},
		{`INSERT INTO review_findings(id,ship_task_id,seq,pattern_id,fingerprint,round,repository,path,symbol,excerpt,line_at_report,head_sha,severity,origin,in_scope,state,follow_up_url,inherited_from) VALUES(802,?,1,801,?,1,'shop','internal/store/example.go','save','db.Exec(ctx)',12,?,'P1','introduced',1,'open','',NULL)`, []any{f.change, reportHash, reviewShaA}},
		{`INSERT INTO review_finding_events(id,finding_id,round,event,actor,note,user_quote,at) VALUES(801,802,1,'reported','reviewer','new finding','',11)`, nil},
		{`INSERT INTO review_carries(ship_task_id,repository,from_head,to_head,target_commit_sha,tree,created_at) VALUES(?,'shop',?,?,?, ?,12)`, []any{f.change, reviewShaA, reviewShaB, reviewShaB, reviewShaB}},
		{`INSERT INTO review_dispositions(id,ship_task_id,head_sha,decision_id,user_quote,at) VALUES(802,?,?,NULL,'accepted by User',13)`, []any{f.change, reviewShaB}},
		{`INSERT INTO validation_fix_rounds(id,ship_task_id,kind,seq,expected_base_head,dedupe_key,message_id,created_at) VALUES(803,?,'gate',1,?,'gate:1',901,14)`, []any{f.change, reviewShaB}},
		{`INSERT INTO validation_fix_rounds(id,ship_task_id,kind,seq,expected_base_head,dedupe_key,message_id,created_at) VALUES(804,?,'ci',1,?,'ci:1',901,15)`, []any{f.change, reviewShaB}},
	}
	for _, statement := range statements {
		if _, err := f.db.ExecContext(f.ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("insert validation fixture %q: %v", statement.query, err)
		}
	}
}

func TestQueueValidationFixCoalescesKindsForOneHead(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	gate, err := f.db.QueueValidationFixInstruction(f.ctx, f.change, "gate", reviewShaA, "gate:1", "Gate command failed")
	if err != nil || !gate.Queued || gate.Coalesced || gate.MessageID == 0 {
		t.Fatalf("Gate fix queue = %#v, %v", gate, err)
	}
	ci, err := f.db.QueueValidationFixInstruction(f.ctx, f.change, "ci", reviewShaA, "ci:unit", "CI check failed")
	if err != nil || !ci.Queued || !ci.Coalesced || ci.MessageID != gate.MessageID {
		t.Fatalf("CI fix was not coalesced with Gate: %#v, %v", ci, err)
	}
	duplicate, err := f.db.QueueValidationFixInstruction(f.ctx, f.change, "gate", reviewShaA, "gate:1", "Gate command failed")
	if err != nil || duplicate.Queued || duplicate.MessageID != gate.MessageID {
		t.Fatalf("duplicate Gate observation = %#v, %v", duplicate, err)
	}

	var messageCount, roundCount int
	var body string
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*),MIN(body) FROM messages WHERE task_id=?`, f.change).Scan(&messageCount, &body); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND message_id=?`, f.change, gate.MessageID).Scan(&roundCount); err != nil {
		t.Fatal(err)
	}
	if messageCount != 1 || roundCount != 2 || !strings.Contains(body, "Gate command failed") || !strings.Contains(body, "CI check failed") {
		t.Fatalf("coalesced message count=%d rounds=%d body=%q", messageCount, roundCount, body)
	}
	heads, err := f.db.ValidationExpectedBaseHeads(f.ctx, f.change)
	if err != nil || !reflect.DeepEqual(heads, []string{reviewShaA}) {
		t.Fatalf("expected heads = %v, %v", heads, err)
	}

	other, err := f.db.QueueValidationFixInstruction(f.ctx, f.change, "ci", reviewShaB, "ci:integration", "Another head failed")
	if err != nil || !other.Queued || other.Coalesced || other.MessageID == gate.MessageID {
		t.Fatalf("different head was coalesced: %#v, %v", other, err)
	}
}

func TestValidationFixBudgetIsEnforcedAndIdempotent(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	first, err := f.db.QueueValidationFixInstructionWithinBudget(f.ctx, f.change, "gate", reviewShaA, "gate:first", "First Gate failure", 2)
	if err != nil || !first.Queued || first.Exhausted || first.Seq != 1 || first.RoundsUsed != 1 {
		t.Fatalf("first budgeted Gate round = %#v, %v", first, err)
	}
	duplicate, err := f.db.QueueValidationFixInstructionWithinBudget(f.ctx, f.change, "gate", reviewShaA, "gate:first", "Repeated observation", 2)
	if err != nil || duplicate.Queued || duplicate.Exhausted || duplicate.MessageID != first.MessageID {
		t.Fatalf("duplicate budgeted Gate round = %#v, %v", duplicate, err)
	}
	second, err := f.db.QueueValidationFixInstructionWithinBudget(f.ctx, f.change, "gate", reviewShaA, "gate:second", "Second Gate failure", 2)
	if err != nil || !second.Queued || second.Exhausted || second.Seq != 2 || second.RoundsUsed != 2 || second.MessageID != first.MessageID {
		t.Fatalf("second budgeted Gate round = %#v, %v", second, err)
	}
	exhausted, err := f.db.QueueValidationFixInstructionWithinBudget(f.ctx, f.change, "gate", reviewShaA, "gate:third", "Third Gate failure", 2)
	if err != nil || exhausted.Queued || !exhausted.Exhausted || exhausted.RoundsUsed != 2 {
		t.Fatalf("exhausted Gate budget = %#v, %v", exhausted, err)
	}
	var rounds, messages int
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='gate'`, f.change).Scan(&rounds); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM messages WHERE task_id=?`, f.change).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if rounds != 2 || messages != 1 {
		t.Fatalf("budgeted rounds/messages = %d/%d, want 2/1", rounds, messages)
	}
}

func TestConcurrentValidationFixBudgetCannotBeExceeded(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	type response struct {
		result QueueValidationFixResult
		err    error
	}
	responses := make(chan response, 2)
	var workers sync.WaitGroup
	for _, key := range []string{"gate:budget-a", "gate:budget-b"} {
		workers.Add(1)
		go func(key string) {
			defer workers.Done()
			result, err := f.db.QueueValidationFixInstructionWithinBudget(f.ctx, f.change, "gate", reviewShaA, key, key, 1)
			responses <- response{result: result, err: err}
		}(key)
	}
	workers.Wait()
	close(responses)
	queued, exhausted := 0, 0
	for result := range responses {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.result.Queued {
			queued++
		}
		if result.result.Exhausted {
			exhausted++
		}
	}
	var rounds int
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='gate'`, f.change).Scan(&rounds); err != nil {
		t.Fatal(err)
	}
	if queued != 1 || exhausted != 1 || rounds != 1 {
		t.Fatalf("concurrent budget results queued=%d exhausted=%d rounds=%d, want 1/1/1", queued, exhausted, rounds)
	}
}

func TestConcurrentValidationFixesForOneHeadShareAQueuedMessage(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	type response struct {
		result QueueValidationFixResult
		err    error
	}
	responses := make(chan response, 2)
	var workers sync.WaitGroup
	for _, item := range []struct{ kind, key, body string }{
		{"gate", "gate:parallel", "Gate failure"},
		{"ci", "ci:parallel", "CI failure"},
	} {
		workers.Add(1)
		go func(item struct{ kind, key, body string }) {
			defer workers.Done()
			result, err := f.db.QueueValidationFixInstruction(f.ctx, f.change, item.kind, reviewShaA, item.key, item.body)
			responses <- response{result: result, err: err}
		}(item)
	}
	workers.Wait()
	close(responses)
	var messageID int64
	coalesced := 0
	for result := range responses {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if messageID == 0 {
			messageID = result.result.MessageID
		} else if result.result.MessageID != messageID {
			t.Fatalf("concurrent fix instructions used different messages: %d and %d", messageID, result.result.MessageID)
		}
		if result.result.Coalesced {
			coalesced++
		}
	}
	var messageCount, roundCount int
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM messages WHERE task_id=?`, f.change).Scan(&messageCount); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND message_id=?`, f.change, messageID).Scan(&roundCount); err != nil {
		t.Fatal(err)
	}
	if messageCount != 1 || roundCount != 2 || coalesced != 1 {
		t.Fatalf("concurrent coalescing: messages=%d rounds=%d coalesced=%d", messageCount, roundCount, coalesced)
	}
}

func TestDoneSignalBindsValidationHeadsCheckedByCaller(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	if err := f.db.Transition(f.ctx, f.change, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	queued, err := f.db.QueueValidationFixInstruction(f.ctx, f.change, "gate", reviewShaA, "gate:continuity", "Fix the Gate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.ctx, `UPDATE messages SET status='delivered',delivered_at=? WHERE id=?`, time.Now().UnixMilli(), queued.MessageID); err != nil {
		t.Fatal(err)
	}
	task := f.task(t, f.change)
	checked, err := f.db.ValidationContinuitySnapshot(f.ctx, f.change)
	if err != nil || !reflect.DeepEqual(checked.Heads, []string{reviewShaA}) {
		t.Fatalf("checked heads = %v, %v", checked.Heads, err)
	}
	second, err := f.db.QueueValidationFixInstruction(f.ctx, f.change, "ci", reviewShaA, "ci:later", "Fix the CI check")
	if err != nil || !second.Queued || second.Coalesced {
		t.Fatalf("queue later same-head fix = %#v, %v", second, err)
	}
	if _, err := f.db.ExecContext(f.ctx, `UPDATE messages SET status='delivered',delivered_at=created_at WHERE id=?`, second.MessageID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.RecordWorkerSignalWithValidation(f.ctx, task, "done", "Finished", nil, "task_done", checked); !errors.Is(err, ErrValidationHeadsChanged) {
		t.Fatalf("done with stale continuity revision error = %v", err)
	}
	snapshot, err := f.db.ValidationContinuitySnapshot(f.ctx, f.change)
	if err != nil || !reflect.DeepEqual(snapshot.Heads, []string{reviewShaA}) || snapshot.LatestRoundID == checked.LatestRoundID {
		t.Fatalf("current continuity snapshot = %#v, checked %#v, err=%v", snapshot, checked, err)
	}
	if state, err := f.db.RecordWorkerSignalWithValidation(f.ctx, task, "done", "Finished", nil, "task_done", snapshot); err != nil || state != StateDone {
		t.Fatalf("done with checked head state=%s err=%v", state, err)
	}
	if heads, err := f.db.ValidationExpectedBaseHeads(f.ctx, f.change); err != nil || len(heads) != 0 {
		t.Fatalf("completed instruction remained active: heads=%v err=%v", heads, err)
	}
}
