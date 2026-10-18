package store

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidationStateSurvivesSnapshotRebuild(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	insertValidationStateFixture(t, f)
	if err := f.db.PersistTask(f.ctx, f.ship); err != nil {
		t.Fatalf("persist validation state: %v", err)
	}
	before, err := f.db.validationStateForTask(f.ctx, f.ship)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.ReviewRounds) != 1 || len(before.ReviewPatterns) != 1 || len(before.ReviewFindings) != 1 ||
		len(before.ReviewFindingEvents) != 1 || len(before.ReviewCarries) != 1 || len(before.ReviewDispositions) != 1 || len(before.ValidationFixRounds) != 1 {
		t.Fatalf("fixture did not cover every validation table: %#v", before)
	}
	if count, err := f.db.RebuildFromSnapshots(f.ctx, f.home); err != nil || count != 2 {
		t.Fatalf("rebuild count=%d err=%v", count, err)
	}
	after, err := f.db.validationStateForTask(f.ctx, f.ship)
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
				_, err = f.db.ExecContext(f.ctx, tc.query, f.ship)
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
	if _, err := f.db.ExecContext(f.ctx, `UPDATE review_rounds SET status='verdict',verdict='pass' WHERE ship_task_id=? AND review_task_id=?`, f.ship, f.review); err != nil {
		t.Fatalf("record round verdict: %v", err)
	}
	if _, err := f.db.ExecContext(f.ctx, `UPDATE review_rounds SET head_sha=? WHERE ship_task_id=? AND review_task_id=?`, reviewShaB, f.ship, f.review); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("round binding update error = %v", err)
	}
	if _, err := f.db.ExecContext(f.ctx, `UPDATE review_rounds SET verdict='changes_requested' WHERE ship_task_id=? AND review_task_id=?`, f.ship, f.review); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("verdict rewrite error = %v", err)
	}
}

func TestVoidValidationRoundNumberCanBeReused(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	insertValidationStateFixture(t, f)
	if _, err := f.db.ExecContext(f.ctx, `UPDATE review_rounds SET status='void' WHERE ship_task_id=? AND review_task_id=?`, f.ship, f.review); err != nil {
		t.Fatalf("void the first round: %v", err)
	}
	secondReview, err := f.db.CreateTask(f.ctx, f.task(t, f.ship).ProjectID, Task{
		Seq: 3, Type: "review", ReviewsTaskID: f.ship, Title: "Retry review", LandingMode: "local",
		ReviewTargets: []ReviewTarget{{Repository: "shop", Commit: reviewShaB}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.ctx, `INSERT INTO review_rounds(ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,verdict,created_at) VALUES(?,1,?,'shop','full',?,?,?,?,?,'[]','admitted',NULL,20)`,
		f.ship, secondReview, reviewShaB, reviewShaB, reviewShaB, reviewShaB, reportHash); err != nil {
		t.Fatalf("reuse void round number: %v", err)
	}
	var count int
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM review_rounds WHERE ship_task_id=? AND round=1 AND repository='shop'`, f.ship).Scan(&count); err != nil || count != 2 {
		t.Fatalf("round 1 history count=%d err=%v; want both void and retry rows", count, err)
	}
}

func insertValidationStateFixture(t *testing.T, f reviewVerdictFixture) {
	t.Helper()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO review_rounds(ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,verdict,created_at) VALUES(?,1,?,'shop','full',?,?,?,?,?,'[]','admitted',NULL,10)`, []any{f.ship, f.review, reviewShaA, reviewShaA, reviewShaA, reviewShaA, reportHash}},
		{`INSERT INTO review_patterns(id,ship_task_id,seq,key,title,rule,root_cause,fix_guidance,merged_into,created_round) VALUES(801,?,1,'unchecked-error','Dropped errors','Check all write errors','Callers ignore errors','Search all new writes',NULL,1)`, []any{f.ship}},
		{`INSERT INTO review_findings(id,ship_task_id,seq,pattern_id,fingerprint,round,repository,path,symbol,excerpt,line_at_report,head_sha,severity,origin,in_scope,state,follow_up_url,inherited_from) VALUES(802,?,1,801,?,1,'shop','internal/store/example.go','save','db.Exec(ctx)',12,?,'P1','introduced',1,'open','',NULL)`, []any{f.ship, reportHash, reviewShaA}},
		{`INSERT INTO review_finding_events(id,finding_id,round,event,actor,note,user_quote,at) VALUES(801,802,1,'reported','reviewer','new finding','',11)`, nil},
		{`INSERT INTO review_carries(ship_task_id,repository,from_head,to_head,target_commit_sha,tree,created_at) VALUES(?,'shop',?,?,?, ?,12)`, []any{f.ship, reviewShaA, reviewShaB, reviewShaB, reviewShaB}},
		{`INSERT INTO review_dispositions(id,ship_task_id,head_sha,decision_id,user_quote,at) VALUES(802,?,?,NULL,'accepted by User',13)`, []any{f.ship, reviewShaB}},
		{`INSERT INTO validation_fix_rounds(id,ship_task_id,kind,seq,expected_base_head,dedupe_key,message_id,created_at) VALUES(803,?,'gate',1,?,'gate:1',901,14)`, []any{f.ship, reviewShaB}},
	}
	for _, statement := range statements {
		if _, err := f.db.ExecContext(f.ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("insert validation fixture %q: %v", statement.query, err)
		}
	}
}
