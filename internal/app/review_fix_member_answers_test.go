package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestReviewFixMemberAnswersFollowSupersedingCandidate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		descends    bool
		wantRefusal string
	}{
		{name: "newer candidate descending from the expected base is accepted", descends: true},
		{name: "newer candidate that does not descend from the expected base is refused", wantRefusal: "head_discontinuity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkspaceMergeFixture(t, "[defaults]\nreview = \"full-pipeline\"\n", []string{"worker"}, []string{"worker"})
			ctx := context.Background()
			task, err := f.db.TaskByID(ctx, f.project.ID, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			lane, err := resolveCandidateLane(ctx, f.db, f.project, task, "worker")
			if err != nil {
				t.Fatal(err)
			}
			worktree := lane.task.WorktreePath
			base := strings.TrimSpace(gitTest(t, worktree, "rev-parse", "HEAD"))
			candidateDir := func(generation string) string {
				return filepath.Join(f.home, "projects", "stack", "tasks", "t1", "candidates", "worker", generation)
			}
			create := func(head, generation string) store.ValidationCandidate {
				candidate, err := f.db.CreateValidationCandidate(ctx, store.ValidationCandidate{
					TaskID: task.ID, Repo: "worker", SubmittedSHA: head, CandidateSHA: head, TargetSHA: base,
					RefName: candidateRefName(lane.task), WorktreePath: candidateDir(generation), Summary: generation,
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.db.ActivateValidationCandidate(ctx, candidate.ID, head); err != nil {
					t.Fatal(err)
				}
				return candidate
			}
			first := create(base, "g1")
			if err := f.db.MarkValidationCandidateStale(ctx, first.ID, base); err != nil {
				t.Fatal(err)
			}
			if err := f.db.SupersedeValidationCandidate(ctx, first.ID, store.ValidationCandidateStale, candidateArchiveRefName(lane.task, first), base); err != nil {
				t.Fatal(err)
			}
			parent := base
			if !tc.descends {
				parent = strings.TrimSpace(gitTest(t, worktree, "rev-parse", "HEAD~1"))
			}
			newer := strings.TrimSpace(gitTest(t, worktree, "commit-tree", base+"^{tree}", "-p", parent, "-m", "resubmitted"))
			second := create(newer, "g2")
			round := store.ValidationFixRound{MemberHeads: []store.ValidationFixMemberHead{{Repository: "worker", ExpectedBaseHead: base, CandidateID: first.ID}}}
			answers, err := f.service.reviewFixMemberAnswers(ctx, f.db, f.home, f.project, task, round)
			if tc.wantRefusal != "" {
				var refusal *axi.Error
				if !errors.As(err, &refusal) || refusal.Code != tc.wantRefusal {
					t.Fatalf("answers = %v, %v; want %s", answers, err, tc.wantRefusal)
				}
				return
			}
			if err != nil {
				t.Fatalf("answers against the superseding candidate: %v", err)
			}
			if len(answers) != 1 || answers[0].CandidateID != second.ID || answers[0].ActualHead != newer {
				t.Fatalf("answers = %#v, want candidate %d at %s", answers, second.ID, newer)
			}
		})
	}
}
