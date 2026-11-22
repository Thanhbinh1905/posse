package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	question := greenhousePartialDecisionQuestion("a1b2c3d4", result)
	for _, text := range []string{"a1b2c3d4", "internal/store/large.sql", "hunk windows exceed 65536 bytes", "context-heavy.go", "prompt exceeds model context (90000 tokens > 64000)", "internal/store/schema.sql", "reviewed by provenance", "tools/schema.go", "accept-coverage-at-exact-head", "hold"} {
		if !strings.Contains(question, text) {
			t.Errorf("partial Decision question %q does not contain %q", question, text)
		}
	}
	if strings.Contains(question, "schema.sql: uncovered") {
		t.Fatalf("generated path was presented as uncovered: %s", question)
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
	accepted, err := greenhouseAcceptHeadDisposition(ctx, f.db, f.task, head)
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
