package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/greenhouse"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestGreenhousePartialDecisionNamesUncoveredPathsAndGeneratedProvenance(t *testing.T) {
	result := greenhouse.HostResult{
		Uncovered: []greenhouse.UncoveredPath{
			{Path: "internal/store/large.sql", Reason: "hunk windows exceed 65536 bytes"},
			{Path: "context-heavy.go", Reason: "prompt exceeds model context (90000 tokens > 64000)"},
		},
		Generated: []greenhouse.GeneratedPath{{Path: "internal/store/schema.sql", Added: 1200, Removed: 30, Generators: []string{"tools/schema.go"}}},
	}
	question := greenhousePartialDecisionQuestion("worker", "a1b2c3d4", result)
	for _, text := range []string{"worker", "a1b2c3d4", "internal/store/large.sql", "hunk windows exceed 65536 bytes", "context-heavy.go", "prompt exceeds model context (90000 tokens > 64000)", "internal/store/schema.sql", "reviewed by provenance", "tools/schema.go", "accept-coverage-at-exact-head", "hold"} {
		if !strings.Contains(question, text) {
			t.Errorf("partial Decision question %q does not contain %q", question, text)
		}
	}
	if strings.Contains(question, "schema.sql: uncovered") {
		t.Fatalf("generated path was presented as uncovered: %s", question)
	}
}

func TestWorkspaceGreenhousePartialNoticesRaiseDecisionsPerMember(t *testing.T) {
	f := newWorkspaceMergeFixture(t, `review_provider = "greenhouse"
[defaults]
review = "full-pipeline"
merge_mode = "local"
`, []string{"worker", "e2e-tool"}, []string{"worker", "e2e-tool"})
	ctx := context.Background()
	repos := f.repos()
	head := strings.TrimSpace(gitTest(t, repos["worker"].WorktreePath, "rev-parse", "HEAD"))
	gitTest(t, repos["e2e-tool"].WorktreePath, "fetch", repos["worker"].WorktreePath, head)
	gitTest(t, repos["e2e-tool"].WorktreePath, "reset", "--hard", "FETCH_HEAD")
	toolHead := strings.TrimSpace(gitTest(t, repos["e2e-tool"].WorktreePath, "rev-parse", "HEAD"))
	if toolHead != head {
		t.Fatalf("workspace member heads differ: worker=%s e2e-tool=%s", head, toolHead)
	}
	members := []greenhousePendingMember{{Repository: "worker", Head: head}, {Repository: "e2e-tool", Head: head}}
	if _, err := f.db.AddSignal(ctx, f.task.ID, "done", "request partial reviews", map[string]any{"greenhouse_pending_members": members}); err != nil {
		t.Fatal(err)
	}
	for index, repository := range []string{"worker", "e2e-tool"} {
		grant := fmt.Sprintf("shop/t1/%s/r1", repository)
		verdict := store.Verdict{TaskID: f.task.ID, Repository: repository, Provider: store.GreenhouseReviewProvider, Protocol: "2", Grant: grant, Head: head, Target: head, ReviewBase: head, Outcome: "inconclusive", Mode: "gate"}
		if recorded, err := f.db.RecordGreenhouseVerdict(ctx, verdict, func(context.Context) (string, error) { return head, nil }); err != nil || !recorded {
			t.Fatalf("record %s partial Verdict: recorded=%t err=%v", repository, recorded, err)
		}
		requestedAt := time.Now().UnixMilli() + int64(index)
		if err := f.db.RecordGreenhouseReviewRequest(ctx, store.GreenhouseReviewRequest{ProjectID: f.project.ID, TaskID: f.task.ID, Repository: repository, RequestedHead: head, Provider: store.GreenhouseReviewProvider, RequestedAt: requestedAt, SettledAt: requestedAt + 1, Outcome: "verdict", Grant: grant}); err != nil {
			t.Fatalf("record %s partial review request: %v", repository, err)
		}
	}
	result := greenhouse.HostResult{Status: "partial-review", Verdict: "partial", Uncovered: []greenhouse.UncoveredPath{{Path: "large.sql", Reason: "hunk windows exceed 65536 bytes"}}}
	for _, repository := range []string{"worker", "e2e-tool"} {
		created, err := f.db.CreateGreenhousePartialNoticeOnce(ctx, f.project.ID, f.task.ID, repository, head, "Partial review", result)
		if err != nil || !created {
			t.Fatalf("create partial Notice for %s: created=%t err=%v", repository, created, err)
		}
	}
	if err := f.service.raiseNoticeDecisions(ctx, f.db, f.project); err != nil {
		t.Fatal(err)
	}
	decisions, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil || len(decisions) != 2 {
		t.Fatalf("workspace coverage Decisions = %#v, %v; want one per Member", decisions, err)
	}
	var workerDecision, toolDecision *store.Decision
	for index := range decisions {
		switch {
		case strings.Contains(decisions[index].Question, "Member worker"):
			workerDecision = &decisions[index]
		case strings.Contains(decisions[index].Question, "Member e2e-tool"):
			toolDecision = &decisions[index]
		}
	}
	if workerDecision == nil || toolDecision == nil {
		t.Fatalf("workspace coverage Decisions omit Member bindings: %#v", decisions)
	}
	if _, err := f.db.AnswerDecision(ctx, f.project.ID, workerDecision.ID, "accept-coverage-at-exact-head", "Accept coverage for worker only"); err != nil {
		t.Fatal(err)
	}
	if code := f.run("apply", fmt.Sprint(workerDecision.ID)); code != 0 {
		t.Fatalf("apply worker coverage Decision: %d %s", code, f.out.String())
	}
	acceptedWorker, err := greenhouseAcceptHeadDisposition(ctx, f.db, f.task, "worker", head)
	if err != nil || !acceptedWorker {
		t.Fatalf("worker coverage acceptance = %t, %v", acceptedWorker, err)
	}
	acceptedTool, err := greenhouseAcceptHeadDisposition(ctx, f.db, f.task, "e2e-tool", head)
	if err != nil || acceptedTool {
		t.Fatalf("e2e-tool inherited worker coverage acceptance = %t, %v", acceptedTool, err)
	}
	workerEvidence, err := f.db.ReviewEvidenceFor(ctx, f.task.ID, []store.ReviewTarget{{Repository: "worker", Commit: head}})
	if err != nil || !workerEvidence.Complete || !workerEvidence.Disposition {
		t.Fatalf("worker review evidence = %+v, %v", workerEvidence, err)
	}
	toolEvidence, err := f.db.ReviewEvidenceFor(ctx, f.task.ID, []store.ReviewTarget{{Repository: "e2e-tool", Commit: head}})
	if err != nil || toolEvidence.Complete || toolEvidence.Disposition {
		t.Fatalf("e2e-tool inherited worker review evidence = %+v, %v", toolEvidence, err)
	}
	workerCovered, err := f.db.ReviewTargetsCovered(ctx, f.task.ID, []store.ReviewTarget{{Repository: "worker", Commit: head}})
	if err != nil || !workerCovered {
		t.Fatalf("worker Review targets covered = %t, %v", workerCovered, err)
	}
	cfg := f.config()
	if err := f.service.reconcileGreenhouseWorkspacePrepublication(ctx, f.db, f.home, f.project, cfg); err != nil {
		t.Fatal(err)
	}
	messages, err := f.db.UndeliveredTaskMessages(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if strings.Contains(message.Body, "Publish each unchanged head") {
			t.Fatalf("one Member acceptance published the workspace: %+v", messages)
		}
	}
	toolCovered, err := f.db.ReviewTargetsCovered(ctx, f.task.ID, []store.ReviewTarget{{Repository: "e2e-tool", Commit: head}})
	if err != nil || toolCovered {
		t.Fatalf("e2e-tool inherited worker disposition = %t, %v", toolCovered, err)
	}
	if _, err := f.db.AnswerDecision(ctx, f.project.ID, toolDecision.ID, "accept-coverage-at-exact-head", "Accept coverage for e2e-tool only"); err != nil {
		t.Fatal(err)
	}
	if code := f.run("apply", fmt.Sprint(toolDecision.ID)); code != 0 {
		t.Fatalf("apply e2e-tool coverage Decision: %d %s", code, f.out.String())
	}
	acceptedTool, err = greenhouseAcceptHeadDisposition(ctx, f.db, f.task, "e2e-tool", head)
	if err != nil || !acceptedTool {
		t.Fatalf("e2e-tool coverage acceptance = %t, %v", acceptedTool, err)
	}
	if err := f.service.reconcileGreenhouseWorkspacePrepublication(ctx, f.db, f.home, f.project, cfg); err != nil {
		t.Fatal(err)
	}
	messages, err = f.db.UndeliveredTaskMessages(ctx, f.task.ID)
	if err != nil || len(messages) == 0 || !strings.Contains(messages[len(messages)-1].Body, "worker: User accepted coverage") || !strings.Contains(messages[len(messages)-1].Body, "e2e-tool: User accepted coverage") {
		t.Fatalf("publication did not require and name both Member acceptances: %+v, %v", messages, err)
	}
	for _, repository := range []string{"worker", "e2e-tool"} {
		covered, err := f.db.ReviewTargetsCovered(ctx, f.task.ID, []store.ReviewTarget{{Repository: repository, Commit: head}})
		if err != nil || !covered {
			t.Errorf("%s target not covered by its own acceptance: covered=%t err=%v", repository, covered, err)
		}
	}
}

func TestGreenhousePartialDecisionAcceptsOnlyItsExactHead(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
	defer f.db.Close()
	ctx := context.Background()
	head := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "refs/heads/"+f.task.Branch))
	result := greenhouse.HostResult{Status: "partial-review", Verdict: "partial", Uncovered: []greenhouse.UncoveredPath{{Path: "large.sql", Reason: "hunk windows exceed 65536 bytes"}}}
	if _, err := f.db.CreateGreenhousePartialNoticeOnce(ctx, f.project.ID, f.task.ID, f.project.Name, head, "Partial review", result); err != nil {
		t.Fatal(err)
	}
	if err := f.service.raiseNoticeDecisions(ctx, f.db, f.project); err != nil {
		t.Fatal(err)
	}
	decisions, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil || len(decisions) != 1 {
		t.Fatalf("partial review Decisions = %#v, %v", decisions, err)
	}
	decision := decisions[0]
	if decision.Kind != "greenhouse_coverage" || strings.Join(decision.Options, ",") != "accept-coverage-at-exact-head,hold" || !strings.Contains(decision.Question, "large.sql") {
		t.Fatalf("partial review Decision = %#v", decision)
	}
	if _, err := f.db.AnswerDecision(ctx, f.project.ID, decision.ID, "accept-coverage-at-exact-head", "Accept the reviewed coverage at this exact head"); err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := f.run("apply", fmt.Sprint(decision.ID)); code != 0 {
		t.Fatalf("apply partial coverage Decision: %s %s", output, stderr)
	}
	accepted, err := greenhouseAcceptHeadDisposition(ctx, f.db, f.task, f.project.Name, head)
	if err != nil || !accepted {
		t.Fatalf("exact-head coverage disposition = %t, %v", accepted, err)
	}
}

func TestGreenhousePartialDecisionRefusesMovedHead(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
	defer f.db.Close()
	ctx := context.Background()
	head := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "refs/heads/"+f.task.Branch))
	result := greenhouse.HostResult{Status: "partial-review", Verdict: "partial", Uncovered: []greenhouse.UncoveredPath{{Path: "large.sql", Reason: "prompt exceeds model context (90000 tokens > 64000)"}}}
	if _, err := f.db.CreateGreenhousePartialNoticeOnce(ctx, f.project.ID, f.task.ID, f.project.Name, head, "Partial review", result); err != nil {
		t.Fatal(err)
	}
	if err := f.service.raiseNoticeDecisions(ctx, f.db, f.project); err != nil {
		t.Fatal(err)
	}
	decisions, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil || len(decisions) != 1 {
		t.Fatalf("partial review Decisions = %#v, %v", decisions, err)
	}
	decision, err := f.db.AnswerDecision(ctx, f.project.ID, decisions[0].ID, "accept-coverage-at-exact-head", "Accept only the reviewed exact head")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.worktree, "moved-head.txt")
	if err := os.WriteFile(path, []byte("moved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.worktree, "add", "moved-head.txt")
	gitTest(t, f.worktree, "commit", "-m", "move Task head")
	if code, output, stderr := f.run("apply", fmt.Sprint(decision.ID)); code == 0 || !strings.Contains(output+stderr, "Task head changed after the partial Greenhouse review") {
		t.Fatalf("moved-head acceptance result = %d %s %s", code, output, stderr)
	}
}
