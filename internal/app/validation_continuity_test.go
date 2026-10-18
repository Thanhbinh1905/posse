package app

import (
	"context"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func queueDeliveredValidationFix(t *testing.T, fixture *prLandingFixture) {
	t.Helper()
	result, err := fixture.db.QueueValidationFixInstruction(context.Background(), fixture.task.ID, "gate", fixture.headSHA, "gate:continuity", "Fix the failed Gate")
	if err != nil || !result.Queued {
		t.Fatalf("queue fix instruction = %#v, %v", result, err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE messages SET status='delivered',delivered_at=created_at WHERE id=?`, result.MessageID); err != nil {
		t.Fatal(err)
	}
}

func TestDoneRefusesTaskHeadThatDoesNotDescendFromAutomaticInstruction(t *testing.T) {
	fixture := newPRLandingFixture(t, "local", store.StateWorking)
	queueDeliveredValidationFix(t, fixture)
	gitTest(t, fixture.worktree, "commit", "--amend", "-m", "rewrite Task history")

	code, output, errOutput := fixture.run("holler", "done", "Finished the fix")
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	notices, noticeErr := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	var doneSignals int
	if queryErr := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM signals WHERE task_id=? AND verb='done'`, fixture.task.ID).Scan(&doneSignals); queryErr != nil {
		t.Fatal(queryErr)
	}
	actualHead := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
	var noticeContainsBothHeads bool
	for _, notice := range notices {
		if notice.Kind == "head_discontinuity" && strings.Contains(notice.Summary, fixture.headSHA) && strings.Contains(notice.Summary, actualHead) {
			noticeContainsBothHeads = true
		}
	}
	if code != 1 || !strings.Contains(output, "head_discontinuity") || err != nil || task.State != store.StateWorking || doneSignals != 0 || noticeErr != nil || countNoticeKind(notices, "head_discontinuity") != 1 || !noticeContainsBothHeads {
		t.Fatalf("rewritten Task head was accepted or not diagnosed: exit=%d output=%s stderr=%s task=%#v done_signals=%d notices=%#v errors=%v/%v", code, output, errOutput, task, doneSignals, notices, err, noticeErr)
	}
	if code, output, _ := fixture.run("holler", "done", "Retry after refusal"); code != 1 || !strings.Contains(output, "head_discontinuity") {
		t.Fatalf("repeated discontinuous done was not refused: exit=%d output=%s", code, output)
	}
	notices, err = fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "head_discontinuity") != 1 {
		t.Fatalf("repeated refusal duplicated the Notice: notices=%#v err=%v", notices, err)
	}
}

func TestDoneChecksContinuityBeforeValidatingPRHead(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	queueDeliveredValidationFix(t, fixture)
	gitTest(t, fixture.worktree, "commit", "--amend", "-m", "rewrite Task history")

	code, output, errOutput := fixture.run("holler", "done", "Finished the fix", "--pr", "https://github.com/acme/shop/pull/17")
	if code != 1 || !strings.Contains(output, "head_discontinuity") || strings.Contains(output, "pr_head_mismatch") {
		t.Fatalf("PR validation hid head discontinuity: exit=%d output=%s stderr=%s", code, output, errOutput)
	}
}

func TestDoneAcceptsTaskHeadThatDescendsFromAutomaticInstruction(t *testing.T) {
	fixture := newPRLandingFixture(t, "local", store.StateWorking)
	queueDeliveredValidationFix(t, fixture)
	gitTest(t, fixture.worktree, "commit", "--allow-empty", "-m", "fix the Gate")

	code, output, errOutput := fixture.run("holler", "done", "Finished the fix")
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if code != 0 || !strings.Contains(output, "state: done") || err != nil || task.State != store.StateDone {
		t.Fatalf("descendant Task head was refused: exit=%d output=%s stderr=%s task=%#v err=%v", code, output, errOutput, task, err)
	}
}

func TestGateAndCIFailuresForOneHeadDeliverAsOneInstruction(t *testing.T) {
	fixture := newPRLandingFixture(t, "local", store.StateWorking)
	ctx := context.Background()
	gate, err := fixture.db.QueueValidationFixInstruction(ctx, fixture.task.ID, "gate", fixture.headSHA, "gate:1", "Gate command failed with exit 1")
	if err != nil || !gate.Queued {
		t.Fatalf("queue Gate fix = %#v, %v", gate, err)
	}
	ci, err := fixture.db.QueueValidationFixInstruction(ctx, fixture.task.ID, "ci", fixture.headSHA, "ci:unit", "CI check unit failed")
	if err != nil || !ci.Queued || !ci.Coalesced || ci.MessageID != gate.MessageID {
		t.Fatalf("queue CI fix = %#v, %v", ci, err)
	}
	fake := fixture.service.Herdr.(*herdr.Fake)
	if err := fixture.service.deliverQueuedMessages(ctx, fixture.db, fixture.project, fake.SnapshotValue); err != nil {
		t.Fatalf("deliver coalesced instruction: %v", err)
	}
	var messageCount int
	var body, status, deliveredText string
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*),MIN(body),MIN(status) FROM messages WHERE task_id=?`, fixture.task.ID).Scan(&messageCount, &body, &status); err != nil {
		t.Fatal(err)
	}
	for _, call := range fake.Calls {
		if call.Method == "agent.prompt" {
			deliveredText, _ = call.Params["text"].(string)
		}
	}
	if messageCount != 1 || status != "delivered" || fake.CallCount("agent.prompt") != 1 || !strings.Contains(body, "Gate command failed") || !strings.Contains(body, "CI check unit failed") || !strings.Contains(deliveredText, "Gate command failed") || !strings.Contains(deliveredText, "CI check unit failed") {
		t.Fatalf("Gate and CI were not delivered together: messages=%d status=%q prompts=%d body=%q delivered=%q", messageCount, status, fake.CallCount("agent.prompt"), body, deliveredText)
	}
}
