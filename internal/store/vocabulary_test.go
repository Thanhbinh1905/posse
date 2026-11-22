package store

import (
	"encoding/json"
	"reflect"
	"testing"
)

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
