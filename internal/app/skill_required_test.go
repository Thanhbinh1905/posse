package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func installRoleSkills(t *testing.T, fixture *prMergeFixture, skills ...string) {
	t.Helper()
	plan, err := fixture.task.AdmittedPlan()
	if err != nil || plan == nil {
		t.Fatalf("admitted plan: %v", err)
	}
	for _, skill := range skills {
		plan.Inputs = append(plan.Inputs, taskplan.Select("skill", skill, []byte("---\nname: "+skill+"\n---\n")))
	}
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`UPDATE tasks SET plan_json=? WHERE id=?`, `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=?`} {
		if _, err := fixture.db.ExecContext(context.Background(), query, encoded, fixture.task.ID); err != nil {
			t.Fatal(err)
		}
	}
	fixture.task, err = fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
}

func skillHookMarkerPath(fixture *prMergeFixture) string {
	return filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", skillHookMarker)
}

func TestValidationDocsAndLintRefuseWhileAConfiguredRoleSkillHasNoUses(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	installRoleSkills(t, fixture, "posse-tdd", "posse-deslop")
	if err := os.MkdirAll(filepath.Dir(skillHookMarkerPath(fixture)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillHookMarkerPath(fixture), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	recordCandidateReviewPass(t, fixture, candidate.CandidateSHA)
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent and E2E behavior verified"); code != 0 {
		t.Fatalf("test stage: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	docs := []string{"validate", "docs", "--no-comments", "passed", "--no-owner-doc-change", "No owner documentation changes are warranted", "--proof", "No unnecessary comments; complete PR description prepared"}
	lint := []string{"validate", "lint", "--proof", "Configured Gates passed", "--coverage", "No repository workflow jobs are configured locally"}
	for _, args := range [][]string{docs, lint} {
		code, output, _ := runCandidateCLI(t, fixture, candidate, args...)
		if code != 1 || !strings.Contains(output, "role_skills_unused") || !strings.Contains(output, "posse-tdd") || !strings.Contains(output, "posse-deslop") {
			t.Fatalf("%s with unused role skills: exit=%d output=%s", args[1], code, output)
		}
	}
	if err := fixture.db.RecordTaskSkillUse(context.Background(), fixture.task.ID, "posse-deslop"); err != nil {
		t.Fatal(err)
	}
	code, output, _ := runCandidateCLI(t, fixture, candidate, docs...)
	if code != 1 || !strings.Contains(output, "role_skills_unused") || !strings.Contains(output, "posse-tdd") || strings.Contains(output, "posse-deslop") {
		t.Fatalf("docs with one unused role skill: exit=%d output=%s", code, output)
	}
	if err := fixture.db.RecordTaskSkillUse(context.Background(), fixture.task.ID, "posse-tdd"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{docs, lint} {
		if code, output, stderr := runCandidateCLI(t, fixture, candidate, args...); code != 0 {
			t.Fatalf("%s with every role skill used: exit=%d output=%s stderr=%s", args[1], code, output, stderr)
		}
	}
}

func TestValidationIgnoresRoleSkillsWhenUsesAreNotRecorded(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	installRoleSkills(t, fixture, "posse-tdd")
	submitThroughCLI(t, fixture)
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	recordCandidateReviewPass(t, fixture, candidate.CandidateSHA)
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent and E2E behavior verified"); code != 0 {
		t.Fatalf("test stage: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if code, output, stderr := runCandidateCLI(t, fixture, candidate, "validate", "docs", "--no-comments", "passed", "--no-owner-doc-change", "No owner documentation changes are warranted", "--proof", "No unnecessary comments; complete PR description prepared"); code != 0 {
		t.Fatalf("docs for a Task launched without the skill hook: exit=%d output=%s stderr=%s", code, output, stderr)
	}
}

func TestRoleSkillsUnusedFailureNamesEverySkill(t *testing.T) {
	failure := roleSkillsUnusedFailure([]string{"posse-tdd", "go-style"})
	if failure.Code != "role_skills_unused" || !strings.Contains(failure.Message, "`posse-tdd`") || !strings.Contains(failure.Message, "`go-style`") {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestNoticeSummaryNamesRoleSkillsWithNoUses(t *testing.T) {
	if got := withUnusedRoleSkills("Task is ready to merge", nil); got != "Task is ready to merge" {
		t.Fatalf("summary without gaps = %q", got)
	}
	got := withUnusedRoleSkills("Task is ready to merge", []string{"posse-tdd", "posse-unslop"})
	if got != "Task is ready to merge; role skills with 0 recorded uses: `posse-tdd`, `posse-unslop`" {
		t.Fatalf("summary = %q", got)
	}
}
