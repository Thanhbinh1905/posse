package store

import (
	"context"
	"sync"
	"testing"
)

func TestNeedsDecisionSignalCancelsActiveModelErrorNudgeAtomically(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Recover", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	task, err := db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	episode, observed, err := db.ObserveModelErrorEpisode(ctx, taskID, 1, "pi", "model_stream_error", "screen", 100)
	if err != nil || !observed {
		t.Fatalf("observe episode=%#v observed=%t err=%v", episode, observed, err)
	}
	if _, ok, err := db.ClaimModelErrorNudge(ctx, taskID, episode.Episode, episode.Fingerprint, 3, 100, 250); err != nil || !ok {
		t.Fatalf("claim nudge ok=%t err=%v", ok, err)
	}
	if state, err := db.RecordWorkerSignal(ctx, task, "needs-decision", "Waiting on Lead", nil, "needs_decision"); err != nil || state != StateNeedsDecision {
		t.Fatalf("commit needs-decision state=%q err=%v", state, err)
	}
	current, err := db.TaskModelErrorEpisode(ctx, taskID)
	if err != nil || current.Status != "blocked" || current.NextAttemptAt != 0 {
		t.Fatalf("needs-decision did not cancel stale retry: %#v err=%v", current, err)
	}
}

func TestIdenticalFailedTurnAfterSubmittedNudgeConsumesNextAttempt(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Recover", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	episode, observed, err := db.ObserveModelErrorEpisode(ctx, taskID, 1, "pi", "model_stream_error", "same-screen", 100)
	if err != nil || !observed {
		t.Fatalf("first turn episode=%#v observed=%t err=%v", episode, observed, err)
	}
	claimed, ok, err := db.ClaimModelErrorNudge(ctx, taskID, episode.Episode, episode.Fingerprint, 3, 100, 250)
	if err != nil || !ok {
		t.Fatalf("claim first retry=%#v ok=%t err=%v", claimed, ok, err)
	}
	if err := db.MarkModelErrorTurnStarted(ctx, taskID, 1, 200); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteModelErrorNudge(ctx, taskID, episode.Episode, 250, "stream disconnect", 350); err != nil {
		t.Fatal(err)
	}
	next, observed, err := db.ObserveModelErrorEpisode(ctx, taskID, 1, "pi", "model_stream_error", "same-screen", 400)
	if err != nil || !observed || next.Episode != episode.Episode || next.Attempts != 1 {
		t.Fatalf("failed turn reported after working was discarded by a stale idle observation: episode=%#v observed=%t err=%v", next, observed, err)
	}
}

func TestExpiredModelErrorNudgeIsInterruptedWithSpecificNotice(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Recover", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	episode, observed, err := db.ObserveModelErrorEpisode(ctx, taskID, 1, "pi", "model_stream_error", "screen", 100)
	if err != nil || !observed {
		t.Fatalf("observe error episode=%#v observed=%t err=%v", episode, observed, err)
	}
	claimed, ok, err := db.ClaimModelErrorNudge(ctx, taskID, episode.Episode, episode.Fingerprint, 3, 100, 250)
	if err != nil || !ok || claimed.NextAttemptAt != 350 {
		t.Fatalf("claim=%#v ok=%t err=%v", claimed, ok, err)
	}
	notice := Notice{ProjectID: project.ID, TaskID: taskID, Kind: "model_stream_error", Summary: "continue submission is uncertain"}
	created, err := db.InterruptExpiredModelErrorNudge(ctx, taskID, 5349, notice)
	if err != nil || created != nil {
		t.Fatalf("premature interruption=%#v err=%v", created, err)
	}
	created, err = db.InterruptExpiredModelErrorNudge(ctx, taskID, 5350, notice)
	if err != nil || created == nil {
		t.Fatalf("expired claim was not surfaced: notice=%#v err=%v", created, err)
	}
	current, err := db.TaskModelErrorEpisode(ctx, taskID)
	if err != nil || current.Status != "interrupted" || current.Attempts != 1 || current.NextAttemptAt != 0 {
		t.Fatalf("interrupted episode=%#v err=%v", current, err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "model_stream_error" {
		t.Fatalf("interruption Notice=%#v err=%v", notices, err)
	}
}

func TestModelErrorEpisodeDeduplicatesAndBoundsConcurrentNudges(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Recover", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}

	episode, observed, err := db.ObserveModelErrorEpisode(ctx, taskID, 1, "pi", "model_stream_error", "first", 100)
	if err != nil || !observed || episode.Episode != 1 || episode.Attempts != 0 {
		t.Fatalf("initial model error episode=%#v observed=%t err=%v", episode, observed, err)
	}
	if _, observed, err := db.ObserveModelErrorEpisode(ctx, taskID, 1, "pi", "model_stream_error", "first", 101); err != nil || observed {
		t.Fatalf("duplicate model error event observed=%t err=%v", observed, err)
	}
	episode, observed, err = db.ObserveModelErrorEpisode(ctx, taskID, 1, "pi", "model_stream_error", "second", 102)
	if err != nil || !observed || episode.Episode != 1 || episode.Fingerprint != "second" {
		t.Fatalf("same-episode retry observation=%#v observed=%t err=%v", episode, observed, err)
	}

	var successes int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for worker := 0; worker < 12; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			_, claimed, err := db.ClaimModelErrorNudge(ctx, taskID, 1, "second", 3, 300, 0)
			if err != nil {
				t.Errorf("claim model error nudge: %v", err)
				return
			}
			if claimed {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}(worker)
	}
	wg.Wait()
	if successes != 3 {
		t.Fatalf("concurrent nudge claims=%d, want exactly 3", successes)
	}
	episode, err = db.TaskModelErrorEpisode(ctx, taskID)
	if err != nil || episode.Attempts != 3 {
		t.Fatalf("persisted episode after claims=%#v err=%v", episode, err)
	}
	notice := Notice{ProjectID: project.ID, TaskID: taskID, Kind: "model_stream_error", Summary: "retries exhausted"}
	created, err := db.FinishModelErrorEpisode(ctx, taskID, 1, "exhausted", "retry budget exhausted", &notice, 300)
	if err != nil || created == nil {
		t.Fatalf("finish exhausted episode=%#v err=%v", created, err)
	}
	created, err = db.FinishModelErrorEpisode(ctx, taskID, 1, "exhausted", "retry budget exhausted", &notice, 301)
	if err != nil || created != nil {
		t.Fatalf("duplicate episode completion=%#v err=%v", created, err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "model_stream_error" {
		t.Fatalf("episode notices=%#v err=%v", notices, err)
	}

	episode, observed, err = db.ObserveModelErrorEpisode(ctx, taskID, 2, "pi", "model_stream_error", "third", 400)
	if err != nil || !observed || episode.Episode != 2 || episode.Attempts != 0 || episode.Status != "active" {
		t.Fatalf("new launch did not start a fresh bounded episode: %#v observed=%t err=%v", episode, observed, err)
	}
	if episode.Fingerprint != "third" {
		t.Fatalf("new episode fingerprint=%q, want third", episode.Fingerprint)
	}
}
