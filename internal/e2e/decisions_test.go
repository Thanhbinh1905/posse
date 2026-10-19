//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestDecisionCLIFromLeadAndUserShellDeliversAnswerToLead(t *testing.T) {
	h := newIntentCLIHarness(t)
	f := h.newProject(t, "decisions")
	taskID, err := f.addChangeTask(t, store.StateFailed)
	if err != nil {
		t.Fatal(err)
	}
	// The isolated Lead is idle and unfocused so the fake Herdr session can
	// receive the real CLI's Notice prompt.
	if _, apiErr := h.session.call("agent.start", map[string]any{"pane_id": f.project.LeadPaneID, "kind": "claude", "name": "e2e-lead"}); apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, TaskID: f.taskDBID, Kind: "task_failed", Summary: "Worker failed"}); err != nil {
		t.Fatal(err)
	}
	// A test-only trigger pins a valid in-flight answer delivery before
	// foreground or hidden Watch delivery can win it. Release it only after
	// the CLI commits the answer, then require the real Watch to deliver it.
	const deliveryOwner = "decision-answer-overlap"
	if _, err := db.ExecContext(context.Background(), `CREATE TRIGGER e2e_hold_answer_delivery AFTER INSERT ON notices WHEN NEW.kind='decision_answered' BEGIN UPDATE notices SET claim_token='decision-answer-overlap',claimed_at=NEW.created_at WHERE id=NEW.id; END`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	code, output := f.run(t, "", "decisions")
	if code != 0 || !strings.Contains(output, "relaunch") || !strings.Contains(output, "discard") {
		t.Fatalf("failed Task Decision: code=%d output=%s", code, output)
	}
	var failedID int64
	if err := f.openDB(t, func(db *store.DB) error {
		items, e := db.Decisions(context.Background(), f.project.ID, true)
		if e != nil {
			return e
		}
		if len(items) != 1 {
			return fmt.Errorf("want one failed Task Decision: %#v", items)
		}
		failedID = items[0].ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code, output = f.run(t, "", "decide", fmt.Sprint(failedID), "relaunch")
	if code == 0 || !strings.Contains(output, "user_only") {
		t.Fatalf("Lead answered without User quote: %d %s", code, output)
	}
	code, output = f.run(t, "", "decide", fmt.Sprint(failedID), "relaunch", "--user-approved", "Please relaunch the Worker")
	if code != 0 {
		t.Fatalf("Lead quoted answer: %d %s", code, output)
	}
	if err := f.openDB(t, func(db *store.DB) error {
		d, e := db.Decision(context.Background(), f.project.ID, failedID)
		if e != nil {
			return e
		}
		if d.Answer != "relaunch" || d.UserQuote != "Please relaunch the Worker" {
			return fmt.Errorf("quoted answer: %#v", d)
		}
		var noticeID int64
		var owner string
		if err := db.QueryRowContext(context.Background(), `SELECT id,claim_token FROM notices WHERE project_id=? AND kind='decision_answered' AND json_extract(data_json,'$.decision_id')=?`, f.project.ID, failedID).Scan(&noticeID, &owner); err != nil {
			return err
		}
		if owner != deliveryOwner {
			return fmt.Errorf("answer delivery did not remain in flight: owner=%q", owner)
		}
		if _, err := db.ExecContext(context.Background(), `DROP TRIGGER e2e_hold_answer_delivery`); err != nil {
			return err
		}
		return db.RollbackNoticeClaim(context.Background(), f.project.ID, []int64{noticeID}, deliveryOwner)
	}); err != nil {
		t.Fatal(err)
	}
	var deliveryErr error
	if !waitForCondition(10*time.Second, func() bool {
		deliveryErr = f.openDB(t, func(db *store.DB) error {
			notices, err := db.Notices(context.Background(), f.project.ID, false)
			if err != nil {
				return err
			}
			for _, notice := range notices {
				if notice.Kind == "decision_answered" && notice.DeliveredAt > 0 {
					return nil
				}
			}
			return fmt.Errorf("decision_answered did not reach Lead: %#v", notices)
		})
		return deliveryErr == nil
	}) {
		t.Fatal(deliveryErr)
	}
	h.session.mu.Lock()
	received := false
	for i, target := range h.session.promptTargets {
		if target == f.project.LeadPaneID && strings.Contains(h.session.prompts[i], fmt.Sprintf("Decision #%d answered", failedID)) {
			received = true
		}
	}
	h.session.mu.Unlock()
	if !received {
		t.Fatal("decision_answered Notice was not sent to the Lead pane")
	}
	code, output = f.run(t, "", "ask", taskID, "Keep the patch?", "--option", "keep", "--option", "discard")
	if code != 0 || !strings.Contains(output, "Keep the patch?") {
		t.Fatalf("Lead ask: %d %s", code, output)
	}
	// A real CLI process from the User's shell has no Herdr pane identity.
	env := append([]string(nil), f.env...)
	env = setEnv(env, "HERDR_PANE_ID", "")
	f.env = setEnv(env, "HERDR_ENV", "")
	// User shell may answer without --user-approved; its typed option is the quote.
	var shellID int64
	if err := f.openDB(t, func(db *store.DB) error {
		ds, e := db.Decisions(context.Background(), f.project.ID, true)
		if e != nil {
			return e
		}
		if len(ds) != 1 {
			return fmt.Errorf("pending: %#v", ds)
		}
		shellID = ds[0].ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code, output = f.run(t, "", "decide", fmt.Sprint(shellID), "keep")
	if code != 0 {
		t.Fatalf("User shell answer: %d %s", code, output)
	}
	if err := f.openDB(t, func(db *store.DB) error {
		d, e := db.Decision(context.Background(), f.project.ID, shellID)
		if e != nil {
			return e
		}
		if d.UserQuote != "keep" {
			return fmt.Errorf("User shell quote: %#v", d)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
