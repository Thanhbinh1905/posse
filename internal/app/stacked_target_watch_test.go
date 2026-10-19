package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestPRWatchRetargetsOpenStackedPRAfterLowerMerge(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	ctx := context.Background()
	gitTest(t, fixture.repo, "branch", "staging", "main")
	gitTest(t, fixture.repo, "push", "origin", "staging")
	lowerURL := "https://github.com/acme/shop/pull/17"
	upperURL := "https://github.com/acme/shop/pull/18"
	if err := fixture.db.Transition(ctx, fixture.task.ID, store.StateWorking, store.StateDone, "worker", "lower work complete"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, fixture.task.ID, store.StateDone, store.StateMerging, "cli", "lower PR opened"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET pr_url=?, target_branch='staging', gated_sha=? WHERE id=?`, lowerURL, fixture.headSHA, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	lower, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	upperID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{
		Seq: 2, Type: "change", Title: "Upper Task", State: store.StateSpawning,
		MergeMode: "pr", Branch: "posse/t2", BaseRef: "refs/remotes/origin/posse/t1",
		TargetBranch: lower.Branch, PRURL: upperURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, upperID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}

	baseFile := filepath.Join(fixture.root, "upper-pr-base")
	if err := os.WriteFile(baseFile, []byte(lower.Branch), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GH_BASE_BRANCH_FILE", baseFile)
	t.Setenv("POSSE_TEST_GH_VIEW_URL", upperURL)
	t.Setenv("POSSE_TEST_GH_VIEW_STATE", "OPEN")
	t.Setenv("POSSE_TEST_GH_HEAD_BRANCH", "posse/t2")
	response := map[string]any{"data": map[string]any{"repo1": map[string]any{
		"pr1": stackedWatchPull(lowerURL, "MERGED", fixture.headSHA, "staging", fixture.headSHA),
		"pr2": stackedWatchPull(upperURL, "OPEN", fixture.headSHA, lower.Branch, ""),
	}}}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.ghState, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	upper, err := fixture.db.TaskByID(ctx, fixture.project.ID, upperID)
	if err != nil || upper.TargetBranch != "staging" {
		t.Fatalf("upper stored target = %q, %v; want staging", upper.TargetBranch, err)
	}
	base, err := os.ReadFile(baseFile)
	if err != nil || string(base) != "staging" {
		t.Fatalf("upper forge target = %q, %v; want staging", base, err)
	}
	changes, err := fixture.db.TaskTargetChanges(ctx, upperID)
	if err != nil || len(changes) != 1 || changes[0].Status != "complete" {
		t.Fatalf("target change audit = %+v, %v", changes, err)
	}
	observation, err := fixture.db.LatestPRObservation(ctx, fixture.task.ID)
	if err != nil || observation.State != "MERGED" || observation.ObservedAt < time.Now().Add(-time.Minute).UnixMilli() {
		t.Fatalf("lower merge observation = %+v, %v", observation, err)
	}
}

func stackedWatchPull(url, state, head, base, mergeCommit string) map[string]any {
	var merge any
	if mergeCommit != "" {
		merge = map[string]any{"oid": mergeCommit}
	}
	return map[string]any{
		"url": url, "state": state, "headRefOid": head, "baseRefName": base,
		"mergeable": "MERGEABLE", "reviewDecision": "APPROVED", "mergeCommit": merge,
		"statusCheckRollup": map[string]any{"state": "SUCCESS", "contexts": map[string]any{"nodes": []any{}}},
	}
}
