package store

import (
	"errors"
	"reflect"
	"testing"
)

func TestValidationCandidateCustodySurvivesSnapshotRebuild(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	candidate, err := f.db.CreateValidationCandidate(f.ctx, ValidationCandidate{
		TaskID: f.change, SubmittedSHA: reviewShaA, CandidateSHA: reviewShaA, TargetSHA: reviewShaB,
		ExpectedRemoteSHA: reviewShaA, RefName: "refs/posse/candidate/t1", WorktreePath: "/home/test/posse/projects/shop/tasks/t1/candidates/shop/g1",
		Summary: "Candidate summary", Verification: "focused test -> passed", Proof: "test evidence", Risk: "Risk: none\nRollback: revert",
	})
	if err != nil {
		t.Fatalf("record candidate submission: %v", err)
	}
	if candidate.Generation != 1 || candidate.State != ValidationCandidatePreparing {
		t.Fatalf("new candidate = %+v", candidate)
	}
	if err := f.db.ActivateValidationCandidate(f.ctx, candidate.ID, reviewShaA); err != nil {
		t.Fatalf("activate candidate: %v", err)
	}
	if err := f.db.AdvanceValidationCandidate(f.ctx, candidate.ID, reviewShaA, reviewShaB); err != nil {
		t.Fatalf("advance candidate: %v", err)
	}
	if err := f.db.MarkValidationCandidateStale(f.ctx, candidate.ID, reviewShaC); err != nil {
		t.Fatalf("mark candidate stale: %v", err)
	}
	if err := f.db.SupersedeValidationCandidate(f.ctx, candidate.ID, ValidationCandidateStale, "refs/posse/candidate/archive/t1/1", reviewShaB); err != nil {
		t.Fatalf("archive candidate: %v", err)
	}
	second, err := f.db.CreateValidationCandidate(f.ctx, ValidationCandidate{
		TaskID: f.change, SubmittedSHA: reviewShaC, CandidateSHA: reviewShaC, TargetSHA: reviewShaB,
		RefName: "refs/posse/candidate/t1", WorktreePath: "/home/test/posse/projects/shop/tasks/t1/candidates/shop/g2", Summary: "Fresh candidate",
	})
	if err != nil {
		t.Fatalf("record resubmission: %v", err)
	}
	if second.Generation != 2 {
		t.Fatalf("fresh candidate generation = %d, want 2", second.Generation)
	}
	if err := f.db.ActivateValidationCandidate(f.ctx, second.ID, reviewShaC); err != nil {
		t.Fatalf("activate resubmission: %v", err)
	}
	if err := f.db.PersistTask(f.ctx, f.change); err != nil {
		t.Fatalf("persist candidate history: %v", err)
	}
	before, err := f.db.ValidationCandidates(f.ctx, f.change)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 || before[0].State != ValidationCandidateSuperseded || before[1].State != ValidationCandidateActive {
		t.Fatalf("candidate history before rebuild = %+v", before)
	}
	if count, err := f.db.RebuildFromSnapshots(f.ctx, f.home); err != nil || count != 2 {
		t.Fatalf("rebuild count=%d err=%v", count, err)
	}
	after, err := f.db.ValidationCandidates(f.ctx, f.change)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("candidate history after rebuild = %+v, want %+v", after, before)
	}
}

func TestValidationCandidateAllowsOnlyOneLiveCustodyLane(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	makeCandidate := func(summary string) (ValidationCandidate, error) {
		return f.db.CreateValidationCandidate(f.ctx, ValidationCandidate{
			TaskID: f.change, SubmittedSHA: reviewShaA, CandidateSHA: reviewShaA, TargetSHA: reviewShaB,
			RefName: "refs/posse/candidate/t1", WorktreePath: "/home/test/posse/projects/shop/tasks/t1/candidates/shop/g1", Summary: summary,
		})
	}
	first, err := makeCandidate("first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := makeCandidate("racing submission"); !errors.Is(err, ErrStateRace) {
		t.Fatalf("second live submission error = %v, want state race", err)
	}
	if err := f.db.ActivateValidationCandidate(f.ctx, first.ID, reviewShaA); err != nil {
		t.Fatal(err)
	}
	if err := f.db.MarkValidationCandidateStale(f.ctx, first.ID, reviewShaC); err != nil {
		t.Fatal(err)
	}
	if _, err := makeCandidate("must archive the stale candidate first"); !errors.Is(err, ErrStateRace) {
		t.Fatalf("submission over stale candidate error = %v, want state race", err)
	}
}
