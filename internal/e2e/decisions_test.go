//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestDecisionCLIFromLeadAndUserShellDeliversAnswerToLead(t *testing.T) {
	h := newIntentCLIHarness(t)
	f := h.newProject(t, "decisions")
	taskID, err := f.addShipTask(t, store.StateFailed)
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
	if _, err := db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, TaskID: f.taskDBID, Kind: "task_failed", Summary: "Rider failed"}); err != nil {
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
	code, output = f.run(t, "", "decide", fmt.Sprint(failedID), "relaunch", "--user-approved", "Please relaunch the Rider")
	if code != 0 {
		t.Fatalf("Lead quoted answer: %d %s", code, output)
	}
	if err := f.openDB(t, func(db *store.DB) error {
		d, e := db.Decision(context.Background(), f.project.ID, failedID)
		if e != nil {
			return e
		}
		if d.Answer != "relaunch" || d.UserQuote != "Please relaunch the Rider" {
			return fmt.Errorf("quoted answer: %#v", d)
		}
		notices, e := db.Notices(context.Background(), f.project.ID, false)
		if e != nil {
			return e
		}
		for _, n := range notices {
			if n.Kind == "decision_answered" && n.DeliveredAt > 0 {
				return nil
			}
		}
		return fmt.Errorf("decision_answered did not reach Lead: %#v", notices)
	}); err != nil {
		t.Fatal(err)
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
