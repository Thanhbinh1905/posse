//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFullPipelineWorkerRequestsReviewWithDoneFromCandidateCheckout(t *testing.T) {
	f := reviewIdentityFixture(t)
	stopBackgroundWatch(t, f)
	for _, setting := range [][2]string{{"defaults.merge_mode", "pr"}, {"defaults.review", "full-pipeline"}, {"review_loop.enabled", "true"}} {
		runPosse(t, f.binary, f.repo, f.env, "config", "set", setting[0], setting[1])
	}
	if code, output := startReviewIdentityTask(t, f, "Private review request", "author", ""); code != 0 {
		t.Fatalf("start Change Task: %d %s", code, output)
	}
	change := f.mustTask(t, "t1")
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "change.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, change.WorktreePath, "add", "change.txt")
	gitTest(t, f.env, change.WorktreePath, "commit", "-m", "add candidate change")
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "publish", "Candidate summary", "--verify", "go test ./... -> passed", "--proof", "Intent behavior verified", "--risk", "Risk: low\nRollback: revert"); code != 0 {
		t.Fatalf("submit candidate: %d %s", code, output)
	}
	candidate, err := f.db.LatestValidationCandidate(context.Background(), change.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	if code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, workerEnv, "signal", "needs-decision", "Request an independent Review of the submitted candidate"); code != 0 {
		t.Fatalf("needs-decision Signal: %d %s", code, output)
	}
	code, output := runReviewIdentityCLI(t, f, "review", "start", "t1", "--profile", "reviewer")
	if code == 0 || !strings.Contains(output, "review_target_incomplete") || !strings.Contains(output, "posse signal done") {
		t.Fatalf("Review of a needs-decision candidate must refuse and name the done request path: %d %s", code, output)
	}

	if code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, workerEnv, "signal", "working", "Request Review through the documented path"); code != 0 {
		t.Fatalf("resume Change Task: %d %s", code, output)
	}
	turn, err := f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, workerEnv, "signal", "done", "Candidate submitted; requesting a Review Round", "--turn", turn); code != 0 {
		t.Fatalf("request Review with done from the candidate checkout: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "review", "start", "t1", "--profile", "reviewer"); code != 0 {
		t.Fatalf("Review of a candidate requested with done: %d %s", code, output)
	}
	review := f.mustTask(t, "t2")
	targets, err := f.db.ReviewTargets(context.Background(), review.ID)
	if err != nil || len(targets) != 1 || targets[0].Commit != candidate.CandidateSHA {
		t.Fatalf("Review targets = %+v, %v; want candidate %s", targets, err, candidate.CandidateSHA)
	}
}

func TestFullPipelineWorkspaceWorkerRequestsReviewWithDoneBeforePromotion(t *testing.T) {
	f := reviewIdentityFixture(t)
	ws := newFullPipelineWorkspace(t, f)
	brief := filepath.Join(f.root, "workspace-review-request.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Workspace review request\ndone_when: checked\nrepos: [api, web]\n---\nChange both Members.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, ws.root, ws.leadEnv, "start", "--brief", brief, "--name", "workspace-review-request", "--profile", "author"); code != 0 {
		t.Fatalf("start workspace Change Task: code=%d %s", code, output)
	}
	change := mustProjectTask(t, f, ws.project.ID, "t1")
	repos, err := f.db.TaskRepos(context.Background(), change.ID)
	if err != nil || len(repos) != 2 {
		t.Fatalf("workspace Members: %+v %v", repos, err)
	}
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	candidates := map[string]string{}
	for _, repo := range repos {
		if err := os.WriteFile(filepath.Join(repo.WorktreePath, "change.txt"), []byte(repo.Repo+" change\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, f.env, repo.WorktreePath, "add", "change.txt")
		gitTest(t, f.env, repo.WorktreePath, "commit", "-m", "change "+repo.Repo)
		if code, output := runReleaseCLI(t, f.binary, repo.WorktreePath, workerEnv,
			"publish", "--repo", repo.Repo, repo.Repo+" candidate summary", "--verify", "go test ./... -> passed",
			"--proof", "Intent behavior verified", "--risk", "Risk: low\nRollback: revert"); code != 0 {
			t.Fatalf("submit %s private candidate: code=%d %s", repo.Repo, code, output)
		}
		candidate, err := f.db.LatestValidationCandidate(context.Background(), change.ID, repo.Repo)
		if err != nil {
			t.Fatal(err)
		}
		candidates[repo.Repo] = candidate.CandidateSHA
	}

	turn, err := f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "signal", "done", "Candidates submitted; requesting a Review Round", "--turn", turn); code != 0 {
		t.Fatalf("request Review with done before promotion: code=%d %s", code, output)
	}
	if code, output := runReleaseCLI(t, f.binary, ws.root, ws.leadEnv, "review", "start", "t1", "--profile", "reviewer"); code != 0 {
		t.Fatalf("Review of workspace candidates requested with done: code=%d %s", code, output)
	}
	review := mustProjectTask(t, f, ws.project.ID, "t2")
	targets, err := f.db.ReviewTargets(context.Background(), review.ID)
	if err != nil || len(targets) != 2 {
		t.Fatalf("Review targets = %+v, %v", targets, err)
	}
	for _, target := range targets {
		if target.Commit != candidates[target.Repository] {
			t.Fatalf("Review target %+v, want candidate %s", target, candidates[target.Repository])
		}
	}
}
