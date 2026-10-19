package store

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestVocabularyMigrationPreservesQuestionOptionsAndInterruptedWorkspaceMerge(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "upgrade", t.TempDir(), "main")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "User said Rider must land"})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	question, err := db.RaiseDecision(ctx, DecisionRequest{ProjectID: project.ID, TaskID: taskID, Kind: "worker_question", Origin: "question:1", Question: "Travel by land or sea?", Options: []string{"land", "sea"}})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE decisions SET kind='rider_question',answer='land',user_quote='land',answered_at=1 WHERE id=?`, question.ID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	payload := `{"user_quote":"Rider must land"}`
	if err := db.StartIntent(ctx, project.ID, taskID, "land --workspace", "done:landed_ref.record", payload, os.Getpid()); err != nil {
		db.Close()
		t.Fatal(err)
	}
	for _, statement := range []string{`DELETE FROM goose_db_version WHERE version_id=55`, `DELETE FROM posse_migration_checksums WHERE version=55`} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	question, err = db.Decision(ctx, project.ID, question.ID)
	if err != nil || question.Kind != "worker_question" || question.Answer != "land" || question.Options[0] != "land" || question.UserQuote != "land" {
		t.Fatalf("migration rewrote a User question: %#v %v", question, err)
	}
	intent, err := db.IntentByTask(ctx, taskID)
	if err != nil || intent.Command != "merge --workspace" || intent.Step != "done:merged_ref.record" || intent.PayloadJSON != payload {
		t.Fatalf("interrupted workspace Merge lost: %#v %v", intent, err)
	}
}

func TestHistoricalSnapshotVocabularyPreservesEvidenceAndOwnership(t *testing.T) {
	state := projectStateSnapshot{
		Notices:        []noticeSnapshot{{ID: 1, Kind: "rider_error", Summary: "User said Rider must ship", DataJSON: `{"quote":"land it"}`}, {ID: 2, Kind: "remuda_orphan"}, {ID: 3, Kind: "recovery_lookout_reattached"}},
		Decisions:      []decisionSnapshot{{ID: 9, Kind: "land_ready", Origin: "leftover:3", Options: []string{"land", "wait"}, Answer: "land", UserQuote: "land it now"}, {ID: 11, Kind: "rider_question", Options: []string{"land", "sea"}, Answer: "land", UserQuote: "land"}},
		ResourceLeases: []resourceLeaseSnapshot{{ID: 10, OwnerTaskID: 7, SourceKey: "mount:3", Owner: "upgrade mount-3", SandboxID: 3, ScopePath: "/existing/mount-3", CustodyToken: "unchanged"}, {ID: 11, Owner: "User mount-3", SourceKey: "manual", ScopePath: "/existing/mount-3"}},
	}
	normalizeProjectSnapshotVocabulary(&state)
	if state.Notices[0].Kind != "worker_error" || state.Notices[1].Kind != "pool_orphan" || state.Notices[2].Kind != "recovery_watch_reattached" {
		t.Fatalf("old Notice vocabulary survived: %#v", state.Notices)
	}
	if state.Notices[0].Summary != "User said Rider must ship" || state.Notices[0].DataJSON != `{"quote":"land it"}` {
		t.Fatal("migration rewrote evidence")
	}
	decision := state.Decisions[0]
	if decision.Kind != "merge_ready" || decision.Origin != "orphan:3" || decision.Answer != "merge" || !reflect.DeepEqual(decision.Options, []string{"merge", "wait"}) || decision.UserQuote != "land it now" {
		t.Fatalf("Decision migration=%#v", decision)
	}
	question := state.Decisions[1]
	if question.Kind != "worker_question" || question.Answer != "land" || question.Options[0] != "land" || question.UserQuote != "land" {
		t.Fatalf("migration rewrote a User question: %#v", question)
	}
	lease := state.ResourceLeases[0]
	if lease.SourceKey != "sandbox:3" || lease.Owner != "upgrade sandbox-3" || lease.ID != 10 || lease.OwnerTaskID != 7 || lease.ScopePath != "/existing/mount-3" || lease.CustodyToken != "unchanged" {
		t.Fatalf("lease migration=%#v", lease)
	}
	if lease := state.ResourceLeases[1]; lease.Owner != "User mount-3" || lease.SourceKey != "manual" {
		t.Fatalf("non-Sandbox lease label changed: %#v", lease)
	}
	before, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	normalizeProjectSnapshotVocabulary(&state)
	after, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("snapshot normalization is not idempotent")
	}
}
