package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestOpenGitHubPRWatchDoesNotRejectAChangedTarget(t *testing.T) {
	task := store.Task{PRURL: "https://github.com/acme/shop/pull/17"}
	pull := &ghPullRequest{URL: task.PRURL, State: "OPEN", HeadRefOID: "head", BaseRefName: "staging"}
	if _, _, err := makePRObservation(1, task, pull, time.Now(), "main"); err != nil {
		t.Fatalf("open PR watch rejected a target changed outside Posse: %v", err)
	}
	pull.State = "MERGED"
	pull.MergeCommit = &struct {
		OID string `json:"oid"`
	}{OID: "merge"}
	if _, _, err := makePRObservation(1, task, pull, time.Now(), "main"); err == nil || !strings.Contains(err.Error(), `targets "staging", expected "main"`) {
		t.Fatalf("merged PR with a non-frozen target error = %v", err)
	}
}

func TestMergedLowerTaskRetargetsUpperPRAndRecordsChangeOnce(t *testing.T) {
	for _, alreadyRetargeted := range []bool{false, true} {
		name := "updates an open upper PR"
		if alreadyRetargeted {
			name = "accepts a forge-retargeted upper PR"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newPRLandingFixture(t, "pr", store.StateWorking)
			ctx := context.Background()
			lower := fixture.task
			lower.TargetBranch = "staging"
			const upperBranch = "posse/t2"
			const upperURL = "https://github.com/acme/shop/pull/18"
			upperID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{
				Seq: 2, Type: "ship", Title: "Upper Task", State: store.StateSpawning,
				LandingMode: "pr", Branch: upperBranch, BaseRef: "refs/remotes/origin/posse/t1",
				TargetBranch: lower.Branch, PRURL: upperURL,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.db.Transition(ctx, upperID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
				t.Fatal(err)
			}

			baseFile := filepath.Join(fixture.root, "upper-pr-base")
			initialBase := lower.Branch
			if alreadyRetargeted {
				initialBase = lower.TargetBranch
			}
			if err := os.WriteFile(baseFile, []byte(initialBase), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("POSSE_TEST_GH_BASE_BRANCH_FILE", baseFile)
			t.Setenv("POSSE_TEST_GH_VIEW_URL", upperURL)
			t.Setenv("POSSE_TEST_GH_HEAD_BRANCH", upperBranch)
			t.Setenv("POSSE_TEST_GH_VIEW_STATE", "OPEN")

			cfg, err := config.Load(fixture.home, fixture.project.Name)
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.service.enqueueStackedTargetChanges(ctx, fixture.db, fixture.project, lower, "", lower.TargetBranch); err != nil {
				t.Fatal(err)
			}
			if err := fixture.service.processPendingStackedTargetChanges(ctx, fixture.db, fixture.project, cfg, ""); err != nil {
				t.Fatal(err)
			}
			if err := fixture.service.enqueueStackedTargetChanges(ctx, fixture.db, fixture.project, lower, "", lower.TargetBranch); err != nil {
				t.Fatal(err)
			}
			if err := fixture.service.processPendingStackedTargetChanges(ctx, fixture.db, fixture.project, cfg, ""); err != nil {
				t.Fatal(err)
			}

			upper, err := fixture.db.TaskByID(ctx, fixture.project.ID, upperID)
			if err != nil || upper.TargetBranch != lower.TargetBranch {
				t.Fatalf("upper stored target = %q, %v; want %q", upper.TargetBranch, err, lower.TargetBranch)
			}
			gotBase, err := os.ReadFile(baseFile)
			if err != nil || string(gotBase) != lower.TargetBranch {
				t.Fatalf("upper PR target = %q, %v; want %q", gotBase, err, lower.TargetBranch)
			}
			changes, err := fixture.db.TaskTargetChanges(ctx, upperID)
			if err != nil || len(changes) != 1 || changes[0].Status != "complete" || changes[0].SourceTaskID != lower.ID {
				t.Fatalf("target change audit = %+v, %v", changes, err)
			}
			log, err := os.ReadFile(fixture.ghLog)
			if err != nil {
				t.Fatal(err)
			}
			editCount := strings.Count(string(log), "pr edit "+upperURL+" --base staging")
			wantEdits := 1
			if alreadyRetargeted {
				wantEdits = 0
			}
			if editCount != wantEdits {
				t.Fatalf("upper PR edit count = %d, want %d; gh log: %s", editCount, wantEdits, log)
			}
		})
	}
}
