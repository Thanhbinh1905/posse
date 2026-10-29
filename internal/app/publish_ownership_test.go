package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func promoteFullPipelineCandidateForTest(t *testing.T, fixture *prMergeFixture) string {
	t.Helper()
	gitTest(t, fixture.worktree, "push", "origin", "refs/heads/posse/t1:refs/heads/posse/t1")
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	recordCandidateReviewPass(t, fixture, candidate.CandidateSHA)
	recordCandidateStages(t, fixture)
	fixture.setGHHead(t, candidate.CandidateSHA)
	fixture.setGraphQLState(t, "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", candidate.CandidateSHA)
	if err := runCandidatePromotionForTest(t, fixture); err != nil {
		t.Fatalf("promote candidate: %v (%s)", err, fixture.output.String())
	}
	return candidate.CandidateSHA
}

func TestFullPipelinePublishRefreshRewritesPullRequestTextAfterPromotion(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	promoted := promoteFullPipelineCandidateForTest(t, fixture)
	marker, err := fixture.db.GetPRBodyMarker(context.Background(), fixture.task.ID, "")
	if err != nil || marker.PRURL != "https://github.com/acme/shop/pull/17" {
		t.Fatalf("promotion marker=%+v err=%v", marker, err)
	}
	t.Setenv("POSSE_TEST_GH_BODY", "Reviewer note\n\n"+managedPublishBody("## Summary\n\nCandidate summary\n\n## Verification\n\n- [x] wrong test count", marker.Token))

	code, output, stderr := fixture.run("publish", "Corrected summary", "--verify", "go test ./... -> 42 passed")
	if code != 0 || !strings.Contains(output, "already promoted") || !strings.Contains(output, "--refresh") {
		t.Fatalf("publish without --refresh after promotion: code=%d output=%s stderr=%s", code, output, stderr)
	}
	if err := os.WriteFile(fixture.ghLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, output, stderr = fixture.run("publish", "--refresh", "Corrected summary", "--verify", "go test ./... -> 42 passed", "--proof", "Corrected proof", "--risk", "Risk: low\nRollback: revert")
	if code != 0 || !strings.Contains(output, "refreshed: PR/MR text for promoted candidate t1") {
		t.Fatalf("publish --refresh after promotion: code=%d output=%s stderr=%s", code, output, stderr)
	}
	calls := readFileForPublishTest(t, fixture.ghLog)
	if strings.Contains(calls, "pr create") || !strings.Contains(calls, "pr edit https://github.com/acme/shop/pull/17") {
		t.Fatalf("refresh forge calls: %s", calls)
	}
	updated := readFileForPublishTest(t, os.Getenv("POSSE_TEST_GH_EDIT_BODY"))
	managed, ok := managedPublishSection(updated, marker.Token)
	if !strings.HasPrefix(updated, "Reviewer note\n\n") || !ok || !strings.Contains(managed, "Corrected summary") || !strings.Contains(managed, "42 passed") || strings.Contains(managed, "wrong test count") {
		t.Fatalf("refreshed body=%s", updated)
	}
	remoteHead := strings.TrimSpace(gitTest(t, fixture.remote, "show-ref", "--hash", "refs/heads/posse/t1"))
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil || remoteHead != promoted || candidate.State != store.ValidationCandidatePromoted || candidate.PromotedSHA != promoted {
		t.Fatalf("refresh changed custody: remote=%s candidate=%+v err=%v", remoteHead, candidate, err)
	}
}

func TestPublishAdoptsOnlyThisTasksOpenPullRequest(t *testing.T) {
	const prURL = "https://github.com/acme/shop/pull/17"
	for _, scenario := range []string{"discarded Task owner", "unknown owner", "own token"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newPRMergeFixture(t, "pr", store.StateWorking)
			ctx := context.Background()
			var token string
			switch scenario {
			case "discarded Task owner":
				otherID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 2, Type: "change", Title: "E2E PR title", State: store.StateSpawning, MergeMode: "pr", Branch: "posse/t2"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET state=? WHERE id=?`, store.StateTornDown, otherID); err != nil {
					t.Fatal(err)
				}
				if token, err = fixture.db.EnsurePRBodyMarker(ctx, otherID, "", prURL); err != nil {
					t.Fatal(err)
				}
			case "unknown owner":
				if _, err := fixture.db.EnsurePRBodyMarker(ctx, fixture.task.ID, "", "https://github.com/acme/shop/pull/9"); err != nil {
					t.Fatal(err)
				}
				token = strings.Repeat("ab", 32)
			case "own token":
				var err error
				if token, err = fixture.db.EnsurePRBodyMarker(ctx, fixture.task.ID, "", ""); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("POSSE_TEST_GH_BODY", managedPublishBody("## Summary\n\nEarlier attempt", token))
			openPR, err := json.Marshal([]map[string]string{{"url": prURL, "headRefName": "posse/t1", "headRefOid": fixture.headSHA}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fixture.ghOpenPRs, openPR, 0o600); err != nil {
				t.Fatal(err)
			}

			code, output, stderr := fixture.run("publish", "Second attempt", "--verify", "go test ./... -> passed")
			calls := readFileForPublishTest(t, fixture.ghLog)
			if scenario == "own token" {
				if code != 0 || !strings.Contains(calls, "pr edit "+prURL) {
					t.Fatalf("own PR was not adopted: code=%d output=%s stderr=%s calls=%s", code, output, stderr, calls)
				}
				return
			}
			if code == 0 || !strings.Contains(output+stderr, "pr_owner_mismatch") {
				t.Fatalf("publish adopted a PR of another Task: code=%d output=%s stderr=%s", code, output, stderr)
			}
			if scenario == "discarded Task owner" && !strings.Contains(output+stderr, "shop t2 (torn-down)") {
				t.Fatalf("refusal does not name the owning Task: %s %s", output, stderr)
			}
			if strings.Contains(calls, "pr edit") || strings.Contains(calls, "pr create") {
				t.Fatalf("refused publish edited or created a PR: %s", calls)
			}
		})
	}
}

func TestCandidateValidationAcceptsGitignoredLocalFilesFromSandbox(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	if err := os.WriteFile(filepath.Join(fixture.worktree, ".gitignore"), []byte("app.yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.worktree, "add", ".gitignore")
	gitTest(t, fixture.worktree, "commit", "-m", "ignore local configuration")
	if err := os.WriteFile(filepath.Join(fixture.worktree, "app.yaml"), []byte("secret: local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	installFullPipelinePlan(t, fixture)
	gitTest(t, fixture.worktree, "push", "origin", "refs/heads/posse/t1:refs/heads/posse/t1")
	output, _ := submitThroughCLI(t, fixture)
	if !strings.Contains(output, "copy the ones validation needs from the Sandbox") {
		t.Fatalf("submission does not explain gitignored files: %s", output)
	}
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(candidate.WorktreePath, "app.yaml")
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("fresh candidate checkout already has the gitignored file: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(fixture.worktree, "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	recordCandidateReviewPass(t, fixture, candidate.CandidateSHA)
	recordCandidateStages(t, fixture)
	fixture.setGHHead(t, candidate.CandidateSHA)
	fixture.setGraphQLState(t, "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", candidate.CandidateSHA)
	if err := runCandidatePromotionForTest(t, fixture); err != nil {
		t.Fatalf("promotion with a gitignored local file: %v (%s)", err, fixture.output.String())
	}
	if published := gitTest(t, fixture.remote, "ls-tree", "-r", "--name-only", "refs/heads/posse/t1"); strings.Contains(published, "app.yaml") {
		t.Fatalf("gitignored local file was published: %s", published)
	}
}
