package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestPRWatchSerializesStaleFullPipelineCandidates(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	installValidationPlanForReview(t, fixture, "full-pipeline")
	first, err := fixture.db.Task(ctx, fixture.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	firstURL := "https://github.com/acme/shop/pull/17"
	if err := fixture.db.Transition(ctx, first.ID, store.StateDone, store.StateMerging, "cli", "PR opened"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET pr_url=?,gated_sha=? WHERE id=?`, firstURL, fixture.headSHA, first.ID); err != nil {
		t.Fatal(err)
	}

	secondID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 2, Type: "change", Title: "Second full-pipeline PR", State: store.StateSpawning, MergeMode: "pr", Branch: "posse/t2", BaseRef: "main", AutonomyMerge: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		from, to store.State
		source   string
	}{{store.StateSpawning, store.StateWorking, "cli"}, {store.StateWorking, store.StateDone, "worker"}, {store.StateDone, store.StateMerging, "cli"}} {
		if step.to == store.StateDone {
			if _, err := fixture.db.AddSignal(ctx, secondID, "done", "second Worker completion", nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := fixture.db.Transition(ctx, secondID, step.from, step.to, step.source, "second PR"); err != nil {
			t.Fatal(err)
		}
	}
	secondURL := "https://github.com/acme/shop/pull/18"
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET pr_url=?,gated_sha=?,plan_json=? WHERE id=?`, secondURL, fixture.headSHA, first.PlanJSON, secondID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `INSERT INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,1,?)`, secondID, first.PlanJSON); err != nil {
		t.Fatal(err)
	}

	for id, url := range map[int64]string{first.ID: firstURL, secondID: secondURL} {
		if _, err := fixture.db.ExecContext(ctx, `INSERT INTO pr_observations(project_id,task_id,pr_url,head_sha,state,checks,review,mergeable,observed_at,confirmed_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, fixture.project.ID, id, url, fixture.headSHA, "OPEN", `{"state":"SUCCESS"}`, "APPROVED", "MERGEABLE", time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	targetTip := fixture.advanceTarget(t, false, true)
	writeForgeResponse := func(firstState string, firstMerge string) {
		t.Helper()
		response := map[string]any{"data": map[string]any{"repo1": map[string]any{
			"pr1": stackedWatchPull(firstURL, firstState, fixture.headSHA, "main", firstMerge),
			"pr2": stackedWatchPull(secondURL, "OPEN", fixture.headSHA, "main", ""),
		}}}
		data, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixture.ghState, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeForgeResponse("OPEN", "")
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	poll := func() {
		t.Helper()
		if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	poll()

	var firstMessages, secondMessages int
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE task_id=? AND status='queued'`, first.ID).Scan(&firstMessages); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE task_id=? AND status='queued'`, secondID).Scan(&secondMessages); err != nil {
		t.Fatal(err)
	}
	if firstMessages != 1 || secondMessages != 0 {
		t.Fatalf("queued refresh messages: t1=%d t2=%d; want only queue head t1", firstMessages, secondMessages)
	}
	firstObservation, err := fixture.db.LatestPRObservation(ctx, first.ID)
	if err != nil || decodeChecks(firstObservation.Checks).State != "STALE" {
		t.Fatalf("queue head observation = %#v, err=%v; want stale checks", firstObservation, err)
	}
	secondObservation, err := fixture.db.LatestPRObservation(ctx, secondID)
	if err != nil || decodeChecks(secondObservation.Checks).State != "SUCCESS" {
		t.Fatalf("queued follower observation = %#v, err=%v; want retained green checks", secondObservation, err)
	}
	var body string
	if err := fixture.db.QueryRowContext(ctx, `SELECT body FROM messages WHERE task_id=? AND status='queued'`, first.ID).Scan(&body); err != nil || !strings.Contains(body, targetTip) {
		t.Fatalf("queue-head refresh did not name latest target %s: body=%q err=%v", targetTip, body, err)
	}

	writeForgeResponse("MERGED", fixture.headSHA)
	poll()
	secondObservation, err = fixture.db.LatestPRObservation(ctx, secondID)
	if err != nil || decodeChecks(secondObservation.Checks).State != "STALE" {
		t.Fatalf("promoted PR observation = %#v, err=%v; want stale checks after target refresh", secondObservation, err)
	}
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE task_id=? AND status='queued'`, secondID).Scan(&secondMessages); err != nil || secondMessages != 1 {
		t.Fatalf("promoted t2 refresh messages=%d err=%v; want one", secondMessages, err)
	}
	if err := fixture.db.QueryRowContext(ctx, `SELECT body FROM messages WHERE task_id=? AND status='queued'`, secondID).Scan(&body); err != nil || !strings.Contains(body, targetTip) {
		t.Fatalf("promoted t2 refresh did not bind latest target %s: body=%q err=%v", targetTip, body, err)
	}
}

func TestPRWatchDoesNotQueueTargetRefreshForActiveCandidate(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	fixture.requireCurrentTarget(t)
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, stderr)
	}
	targetTip := fixture.advanceTarget(t, false, true)
	if _, err := fixture.db.ExecContext(ctx, `INSERT INTO validation_candidates(task_id,repo,generation,submitted_sha,candidate_sha,target_sha,ref_name,worktree_path,state,summary,created_at,updated_at) VALUES(?,'',1,?,?,?,'refs/posse/test','/candidate','active','candidate',?,?)`, fixture.task.ID, fixture.headSHA, fixture.headSHA, targetTip, time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	writeForge := func() {
		t.Helper()
		fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
		cfg, err := config.Load(fixture.home, fixture.project.Name)
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	writeForge()
	writeForge()
	var messages int
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE task_id=? AND status='queued'`, fixture.task.ID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 0 {
		t.Fatalf("active candidate for target %s received %d stale-target refresh messages, want 0", targetTip, messages)
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE validation_candidates SET state='promoted',promoted_sha=candidate_sha WHERE task_id=?`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: fixture.project.ID, TaskID: fixture.task.ID, Kind: "review_cap", Origin: "test:review-cap", Question: "How should the review cap be handled?", Options: []string{"accept", "revise"}}); err != nil {
		t.Fatal(err)
	}
	writeForge()
	writeForge()
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE task_id=? AND status='queued'`, fixture.task.ID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 0 {
		t.Fatalf("open review_cap Decision received %d stale-target refresh messages, want 0", messages)
	}
}

func TestPRWatchRetainsGreenObservationForQueuedFullPipelineFollower(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	installValidationPlanForReview(t, fixture, "full-pipeline")
	first, err := fixture.db.Task(ctx, fixture.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	firstURL := "https://github.com/acme/shop/pull/17"
	if err := fixture.db.Transition(ctx, first.ID, store.StateDone, store.StateMerging, "cli", "PR opened"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET pr_url=?,gated_sha=? WHERE id=?`, firstURL, fixture.headSHA, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.EnterMergeQueue(ctx, first.ID, "", "main", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	secondID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 2, Type: "change", Title: "Second full-pipeline PR", State: store.StateSpawning, MergeMode: "pr", Branch: "posse/t2", BaseRef: "main", AutonomyMerge: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		from, to store.State
		source   string
	}{{store.StateSpawning, store.StateWorking, "cli"}, {store.StateWorking, store.StateDone, "worker"}, {store.StateDone, store.StateMerging, "cli"}} {
		if step.to == store.StateDone {
			if _, err := fixture.db.AddSignal(ctx, secondID, "done", "second Worker completion", nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := fixture.db.Transition(ctx, secondID, step.from, step.to, step.source, "second PR"); err != nil {
			t.Fatal(err)
		}
	}
	secondURL := "https://github.com/acme/shop/pull/18"
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET pr_url=?,gated_sha=?,plan_json=? WHERE id=?`, secondURL, fixture.headSHA, first.PlanJSON, secondID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `INSERT INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,1,?)`, secondID, first.PlanJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `INSERT INTO pr_observations(project_id,task_id,pr_url,head_sha,state,checks,review,mergeable,observed_at,confirmed_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, fixture.project.ID, secondID, secondURL, fixture.headSHA, "OPEN", `{"state":"SUCCESS"}`, "APPROVED", "MERGEABLE", time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	fixture.advanceTarget(t, false, true)
	response := map[string]any{"data": map[string]any{"repo1": map[string]any{
		"pr1": stackedWatchPull(firstURL, "OPEN", fixture.headSHA, "main", ""),
		"pr2": stackedWatchPull(secondURL, "OPEN", fixture.headSHA, "main", ""),
	}}}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.ghState, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	observation, err := fixture.db.LatestPRObservation(ctx, secondID)
	if err != nil || decodeChecks(observation.Checks).State != "SUCCESS" {
		t.Fatalf("queued follower observation = %#v, err=%v; want retained green checks", observation, err)
	}
}
