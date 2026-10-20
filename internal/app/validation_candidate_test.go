package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func installFullPipelinePlan(t *testing.T, fixture *prMergeFixture) {
	t.Helper()
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief:     taskplan.Select("brief", "change.md", []byte("---\ntype: change\ntitle: Candidate\ndone_when: candidate is promoted\n---\nImplement the requested change.\n")),
		Execution: taskplan.Execution{Scope: "repo", Base: "main", Target: "main"},
		Launch:    taskplan.Launch{ProfileName: "author", Profile: config.Profile{Kind: "claude"}},
		Review:    "full-pipeline", Delivery: "pr", Autonomy: config.Autonomy{Review: "ask", Merge: "ask"},
		Paths: taskplan.Paths{Scratch: filepath.Join(fixture.home, "scratch", "shop", "t1"), Output: filepath.Join(fixture.home, "projects", "shop", "tasks", "t1")},
	})
	if err != nil {
		t.Fatalf("resolve full-pipeline plan: %v", err)
	}
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET plan_json=? WHERE id=?`, encoded, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `INSERT INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,1,?)`, fixture.task.ID, encoded); err != nil {
		t.Fatal(err)
	}
	fixture.task, err = fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
}

func submitCandidateForTest(t *testing.T, fixture *prMergeFixture) error {
	t.Helper()
	return fixture.service.submitPrivateCandidate(
		&axi.Context{Context: context.Background(), Out: &bytes.Buffer{}}, fixture.db, fixture.home,
		fixture.project, fixture.task, "", "Candidate summary", "go test ./... -> passed", "proof", "Risk: none\nRollback: revert", false,
	)
}

func runCandidatePromotionForTest(t *testing.T, fixture *prMergeFixture) error {
	t.Helper()
	ctx := context.Background()
	cfg, err := fixture.service.prepareProject(ctx, fixture.db, fixture.project)
	if err != nil {
		return err
	}
	fixture.output.Reset()
	return fixture.service.promotePrivateCandidate(
		&axi.Context{Context: ctx, Out: &fixture.output, ErrOut: &fixture.errOut}, fixture.db, fixture.home,
		fixture.project, fixture.task, "", cfg,
	)
}

func submitThroughCLI(t *testing.T, fixture *prMergeFixture) (string, string) {
	t.Helper()
	code, output, stderr := fixture.run("publish", "Candidate summary", "--verify", "go test ./... -> passed")
	if code != 0 {
		t.Fatalf("full-pipeline publish exit=%d output=%s stderr=%s", code, output, stderr)
	}
	return output, stderr
}

func addCandidateFix(t *testing.T, fixture *prMergeFixture) string {
	t.Helper()
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate.WorktreePath, "validation-fix.txt"), []byte("candidate-only fix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, candidate.WorktreePath, "add", "validation-fix.txt")
	gitTest(t, candidate.WorktreePath, "commit", "-m", "validation fix")
	return strings.TrimSpace(gitTest(t, candidate.WorktreePath, "rev-parse", "HEAD"))
}

func TestFullPipelineReviewCannotMergeBeforeVPC(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	installFullPipelinePlan(t, fixture)
	cfg, err := fixture.service.prepareProject(context.Background(), fixture.db, fixture.project)
	if err != nil {
		t.Fatal(err)
	}
	err = requireSelectedReview(context.Background(), fixture.db, fixture.project, cfg, fixture.task)
	if err == nil || !strings.Contains(err.Error(), "full-pipeline validation has not shipped") {
		t.Fatalf("full-pipeline merge check error=%v, want explicit VP-C block", err)
	}
}

func TestFullPipelinePublishSubmitsPrivateCandidateWithoutPushing(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	output, _ := submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.State != store.ValidationCandidateActive || !strings.Contains(output, candidate.CandidateSHA) {
		t.Fatalf("submission output=%s candidate=%+v", output, candidate)
	}
	if remote, err := remoteBranchHead(context.Background(), fixture.worktree, "refs/heads/posse/t1"); err != nil || remote != "" {
		t.Fatalf("submission pushed origin Task branch: head=%q err=%v", remote, err)
	}
	if log, err := os.ReadFile(fixture.ghLog); err == nil && strings.Contains(string(log), "pr create") {
		t.Fatalf("private submission opened a pull request: %s", log)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestCandidatePromotionFastForwardsTaskBranchAndOrigin(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	gitTest(t, fixture.worktree, "push", "origin", "refs/heads/posse/t1:refs/heads/posse/t1")
	submitted := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
	submitThroughCLI(t, fixture)
	candidateHead := addCandidateFix(t, fixture)
	fixture.setGHHead(t, candidateHead)
	fixture.setGraphQLState(t, "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", candidateHead)
	if err := runCandidatePromotionForTest(t, fixture); err != nil {
		t.Fatalf("promote candidate: %v (%s)", err, fixture.output.String())
	}
	localHead := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
	remoteHead := strings.TrimSpace(gitTest(t, fixture.remote, "show-ref", "--hash", "refs/heads/posse/t1"))
	if localHead != candidateHead || remoteHead != candidateHead || !gitAncestor(context.Background(), fixture.worktree, submitted, candidateHead) {
		t.Fatalf("promotion heads local=%s remote=%s candidate=%s submitted=%s", localHead, remoteHead, candidateHead, submitted)
	}
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil || candidate.State != store.ValidationCandidatePromoted || candidate.PromotedSHA != candidateHead {
		t.Fatalf("promoted candidate=%+v err=%v", candidate, err)
	}
}

func TestCandidatePromotionRefusesMovedRemoteAndDirtySandbox(t *testing.T) {
	for _, scenario := range []string{"remote moved", "dirty sandbox"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newPRMergeFixture(t, "pr", store.StateWorking)
			installFullPipelinePlan(t, fixture)
			gitTest(t, fixture.worktree, "push", "origin", "refs/heads/posse/t1:refs/heads/posse/t1")
			submitThroughCLI(t, fixture)
			addCandidateFix(t, fixture)
			candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "remote moved" {
				if err := os.WriteFile(filepath.Join(fixture.repo, "remote-only.txt"), []byte("external change\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				gitTest(t, fixture.repo, "add", "remote-only.txt")
				gitTest(t, fixture.repo, "commit", "-m", "external branch movement")
				diverged := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "HEAD"))
				gitTest(t, fixture.repo, "push", "origin", diverged+":refs/heads/posse/divergence-fixture")
				gitTest(t, fixture.remote, "update-ref", "refs/heads/posse/t1", diverged)
			} else {
				if err := os.WriteFile(filepath.Join(fixture.worktree, "uncommitted.txt"), []byte("keep local data\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
			remoteBefore := strings.TrimSpace(gitTest(t, fixture.remote, "show-ref", "--hash", "refs/heads/posse/t1"))
			err = runCandidatePromotionForTest(t, fixture)
			if err == nil {
				t.Fatal("promotion succeeded despite a moved remote or dirty Sandbox")
			}
			if scenario == "remote moved" && !strings.Contains(err.Error(), "origin Task branch moved after candidate submission") {
				t.Fatalf("remote movement error=%v", err)
			}
			after := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
			remoteAfter := strings.TrimSpace(gitTest(t, fixture.remote, "show-ref", "--hash", "refs/heads/posse/t1"))
			if before != after || remoteBefore != remoteAfter {
				t.Fatalf("refusal changed author/origin heads: local %s -> %s, remote %s -> %s, candidate=%s", before, after, remoteBefore, remoteAfter, candidate.CandidateSHA)
			}
		})
	}
}

func TestPrivateCandidateSubmissionStaysLocalAndRecoversCheckout(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	if err := submitCandidateForTest(t, fixture); err != nil {
		t.Fatalf("submit private candidate: %v", err)
	}
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.State != store.ValidationCandidateActive {
		t.Fatalf("candidate state = %s, want active", candidate.State)
	}
	if remote, err := remoteBranchHead(context.Background(), fixture.worktree, "refs/heads/posse/t1"); err != nil || remote != "" {
		t.Fatalf("Task branch on origin = %q, err=%v; submission must not push", remote, err)
	}
	refHead, found, err := readGitRef(context.Background(), fixture.worktree, candidate.RefName)
	if err != nil || !found || refHead != candidate.SubmittedSHA {
		t.Fatalf("private ref head=%q found=%v err=%v", refHead, found, err)
	}
	checkoutHead, err := gitOutput(context.Background(), candidate.WorktreePath, "rev-parse", "HEAD")
	if err != nil || checkoutHead != candidate.SubmittedSHA {
		t.Fatalf("candidate checkout head=%q err=%v", checkoutHead, err)
	}
	checkoutRef, err := gitOutput(context.Background(), candidate.WorktreePath, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || checkoutRef != candidate.RefName {
		t.Fatalf("candidate checkout ref=%q err=%v", checkoutRef, err)
	}
	if err := os.WriteFile(filepath.Join(candidate.WorktreePath, "validation-fix.txt"), []byte("candidate-only fix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, candidate.WorktreePath, "add", "validation-fix.txt")
	gitTest(t, candidate.WorktreePath, "commit", "-m", "validation fix")
	candidateHead := strings.TrimSpace(gitTest(t, candidate.WorktreePath, "rev-parse", "HEAD"))
	if _, err := gitOutput(context.Background(), fixture.worktree, "worktree", "remove", "--force", candidate.WorktreePath); err != nil {
		t.Fatalf("remove candidate checkout to simulate a crash: %v", err)
	}
	if err := fixture.service.ensureCandidateCheckout(context.Background(), fixture.db, fixture.home, fixture.task, candidate); err != nil {
		t.Fatalf("recover private candidate checkout: %v", err)
	}
	recoveredHead, err := gitOutput(context.Background(), candidate.WorktreePath, "rev-parse", "HEAD")
	if err != nil || recoveredHead != candidateHead {
		t.Fatalf("recovered candidate checkout head=%q, want %q, err=%v", recoveredHead, candidateHead, err)
	}
	updated, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil || updated.CandidateSHA != candidateHead {
		t.Fatalf("recovered candidate record=%+v err=%v", updated, err)
	}
}

func TestCandidateAuthorMovementRequiresResubmission(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.worktree, "author-followup.txt"), []byte("author movement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.worktree, "add", "author-followup.txt")
	gitTest(t, fixture.worktree, "commit", "-m", "author moved during validation")
	actual, err := gitOutput(context.Background(), fixture.worktree, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	code, output, stderr := fixture.run("signal", "working", "Still working")
	if code == 0 || !strings.Contains(output+stderr, candidate.SubmittedSHA) || !strings.Contains(output+stderr, actual) {
		t.Fatalf("movement Signal exit=%d output=%s stderr=%s; want submitted %s and current %s", code, output, stderr, candidate.SubmittedSHA, actual)
	}
	stale, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil || stale.State != store.ValidationCandidateStale {
		t.Fatalf("candidate after author movement=%+v err=%v", stale, err)
	}
	submitThroughCLI(t, fixture)
	fresh, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Generation != candidate.Generation+1 || fresh.State != store.ValidationCandidateActive || fresh.SubmittedSHA != actual {
		t.Fatalf("fresh candidate=%+v; want generation %d at %s", fresh, candidate.Generation+1, actual)
	}
	archivedHead, found, err := readGitRef(context.Background(), fixture.worktree, candidateArchiveRefName(fixture.task, stale))
	if err != nil || !found || archivedHead != stale.CandidateSHA {
		t.Fatalf("archived candidate head=%q found=%v err=%v", archivedHead, found, err)
	}
}
