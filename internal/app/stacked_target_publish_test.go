package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestPublishReusesPRAfterStackedRetarget(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	prURL := "https://github.com/acme/shop/pull/17"
	upper, lower, targetFile := prepareStackedPublishFixture(t, fixture, prURL)
	openPRs, err := json.Marshal([]map[string]string{{"url": prURL, "headRefName": upper.Branch, "headRefOid": fixture.headSHA}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.ghOpenPRs, openPRs, 0o600); err != nil {
		t.Fatal(err)
	}

	if code, output, stderr := fixture.run("publish", "Republish after the lower PR merged"); code != 0 || !strings.Contains(output, prURL) {
		t.Fatalf("publish after stacked retarget: %d %s %s", code, output, stderr)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	calls := string(log)
	if !strings.Contains(calls, "pr list --state open --head "+upper.Branch) || strings.Contains(calls, "pr create") {
		t.Fatalf("publish did not find the retargeted PR without creating a duplicate: %s", calls)
	}
	if strings.Count(calls, "pr edit "+prURL+" --title") != 1 {
		t.Fatalf("publish did not refresh the existing PR metadata: %s", calls)
	}
	if got, err := os.ReadFile(targetFile); err != nil || string(got) != "staging" {
		t.Fatalf("forge target after publish = %q, %v; want staging", got, err)
	}
	stored, err := fixture.db.TaskByID(context.Background(), fixture.project.ID, upper.ID)
	if err != nil || stored.TargetBranch != lower.TargetBranch {
		t.Fatalf("stored target after publish = %q, %v; want %q", stored.TargetBranch, err, lower.TargetBranch)
	}
}

func TestPublishReusesMergeRequestAfterStackedRetarget(t *testing.T) {
	fixture := gitlabFixture(t, store.StateWorking)
	prURL := "https://git.example.com/group/sub/shop/-/merge_requests/17"
	upper, lower, _ := prepareStackedPublishFixture(t, fixture, prURL)
	list := []map[string]any{{
		"web_url": prURL, "state": "opened", "sha": fixture.headSHA,
		"source_branch": upper.Branch, "target_branch": lower.TargetBranch,
		"source_project_id": 7, "target_project_id": 7,
	}}
	encoded, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	listPath := filepath.Join(fixture.root, "retargeted-mrs.json")
	if err := os.WriteFile(listPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GLAB_LIST", listPath)

	if code, output, stderr := fixture.run("publish", "Republish after the lower MR merged"); code != 0 || !strings.Contains(output, prURL) {
		t.Fatalf("publish after stacked retarget: %d %s %s", code, output, stderr)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	calls := string(log)
	if !strings.Contains(calls, "source_branch=posse%2Ft1&per_page=100") || strings.Contains(calls, "target_branch=") || strings.Contains(calls, "mr create ") {
		t.Fatalf("publish did not find the retargeted MR without creating a duplicate: %s", calls)
	}
	if strings.Count(calls, "--method PUT") != 1 {
		t.Fatalf("publish did not refresh the existing MR metadata: %s", calls)
	}
	stored, err := fixture.db.TaskByID(context.Background(), fixture.project.ID, upper.ID)
	if err != nil || stored.TargetBranch != lower.TargetBranch {
		t.Fatalf("stored target after publish = %q, %v; want %q", stored.TargetBranch, err, lower.TargetBranch)
	}
	var request map[string]any
	state, err := os.ReadFile(fixture.ghState)
	if err != nil || json.Unmarshal(state, &request) != nil || request["target_branch"] != "staging" {
		t.Fatalf("MR target after publish = %s, %v; want staging", state, err)
	}
}

func prepareStackedPublishFixture(t *testing.T, fixture *prLandingFixture, prURL string) (upper, lower store.Task, targetFile string) {
	t.Helper()
	attachPRFixtureMount(t, fixture)
	ctx := context.Background()
	const lowerBranch = "posse/lower"
	const targetBranch = "staging"
	if fixture.project.Root != fixture.repo {
		t.Fatalf("fixture Project root = %q, want %q", fixture.project.Root, fixture.repo)
	}
	gitTest(t, fixture.repo, "branch", targetBranch, "main")
	gitTest(t, fixture.repo, "push", "origin", targetBranch)
	gitTest(t, fixture.repo, "branch", lowerBranch, "main")
	gitTest(t, fixture.repo, "push", "origin", lowerBranch)
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET target_branch=?,pr_url=? WHERE id=?`, lowerBranch, prURL, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	upper, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	lowerID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{
		Seq: 2, Type: "ship", Title: "Lower Task", LandingMode: "pr",
		Branch: lowerBranch, BaseRef: "refs/remotes/origin/main", TargetBranch: targetBranch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, lowerID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	lower, err = fixture.db.TaskByID(ctx, fixture.project.ID, lowerID)
	if err != nil {
		t.Fatal(err)
	}
	targetFile = filepath.Join(fixture.root, "upper-pr-base")
	if err := os.WriteFile(targetFile, []byte(lowerBranch), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GH_BASE_BRANCH_FILE", targetFile)
	t.Setenv("POSSE_TEST_GH_VIEW_URL", prURL)
	t.Setenv("POSSE_TEST_GH_HEAD_BRANCH", upper.Branch)
	t.Setenv("POSSE_TEST_GH_VIEW_STATE", "OPEN")
	if fixture.project.Kind == store.ProjectKindWorkspace {
		t.Fatal("stacked publish fixture must be a repository Project")
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	forge, err := forgeForRepository(ctx, fixture.repo, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if forge.Kind == "gitlab" {
		state, err := json.Marshal(map[string]any{
			"web_url": prURL, "state": "opened", "sha": fixture.headSHA,
			"source_branch": upper.Branch, "target_branch": lowerBranch,
			"source_project_id": 7, "target_project_id": 7,
			"detailed_merge_status": "mergeable", "merge_commit_sha": "",
			"title": "Old title", "description": "",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixture.ghState, state, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.service.enqueueStackedTargetChanges(ctx, fixture.db, fixture.project, lower, "", lower.TargetBranch); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.processPendingStackedTargetChanges(ctx, fixture.db, fixture.project, cfg, ""); err != nil {
		t.Fatal(err)
	}
	upper, err = fixture.db.TaskByID(ctx, fixture.project.ID, upper.ID)
	if err != nil || upper.TargetBranch != targetBranch {
		t.Fatalf("retargeted upper target = %q, %v; want %q", upper.TargetBranch, err, targetBranch)
	}
	return upper, lower, targetFile
}
