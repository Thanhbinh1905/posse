package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestDecisionOnceWithAtomicAnswerAndNotice(t *testing.T) {
	ctx := context.Background()
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateProject(ctx, "other", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "A", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	request := DecisionRequest{ProjectID: project.ID, TaskID: id, Origin: "notice:42", Kind: "rider_question", Question: "Land A?", Options: []string{"land", "wait"}}
	decision, err := db.RaiseDecision(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if decision.ID < 1 || decision.CreatedAt == 0 || decision.Answer != "" {
		t.Fatalf("unrecorded Decision: %#v", decision)
	}
	again, err := db.RaiseDecision(ctx, request)
	if err != nil || again.ID != decision.ID {
		t.Fatalf("repeated origin: %#v %v", again, err)
	}
	changed := request
	changed.Question = "Different question"
	if _, err := db.RaiseDecision(ctx, changed); err == nil {
		t.Fatal("origin reused for a different question")
	}
	changed = request
	changed.Kind = "leftover"
	if _, err := db.RaiseDecision(ctx, changed); err == nil {
		t.Fatal("origin reused for a different kind")
	}
	changed = request
	changed.Origin = "other:42"
	changed.Kind = "unknown"
	if _, err := db.RaiseDecision(ctx, changed); err == nil {
		t.Fatal("unknown Decision kind accepted")
	}
	if _, err := db.RaiseDecision(ctx, DecisionRequest{ProjectID: other.ID, TaskID: id, Origin: "cross", Kind: "rider_question", Question: "A?", Options: []string{"yes", "no"}}); err == nil {
		t.Fatal("cross-Project Task accepted")
	}
	if _, err := db.AnswerDecision(ctx, other.ID, decision.ID, "land", "yes"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-Project answer: %v", err)
	}
	if _, err := db.AnswerDecision(ctx, project.ID, decision.ID, "invalid", "yes"); err == nil {
		t.Fatal("invalid option accepted")
	}
	if _, err := db.AnswerDecision(ctx, project.ID, decision.ID, "land", ""); err == nil {
		t.Fatal("empty quote accepted")
	}
	var wg sync.WaitGroup
	var successes int
	var mu sync.Mutex
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.AnswerDecision(ctx, project.ID, decision.ID, "land", "User said land")
			if err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("answered %d times", successes)
	}
	answered, err := db.Decision(ctx, project.ID, decision.ID)
	if err != nil || answered.Answer != "land" || answered.UserQuote != "User said land" || answered.AnsweredAt < answered.CreatedAt {
		t.Fatalf("bad answer: %#v %v", answered, err)
	}
	pending, err := db.Decisions(ctx, project.ID, true)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending: %#v %v", pending, err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "decision_answered" {
		t.Fatalf("answer Notices: %#v %v", notices, err)
	}
	for _, kind := range []string{"leftover", "pr_closed", "review", "land_ready"} {
		d, err := db.RaiseDecision(ctx, DecisionRequest{ProjectID: project.ID, TaskID: id, Origin: "opaque-" + kind, Kind: kind, Question: "What next?", Options: []string{"yes", "no"}})
		if err != nil || d.Kind != kind {
			t.Fatalf("explicit kind %q lost or inferred from origin: %#v %v", kind, d, err)
		}
	}
}
