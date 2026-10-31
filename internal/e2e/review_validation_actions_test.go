//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func startPrivateReviewChange(t *testing.T, f *prLifecycleFixture, maxRounds string) (changePath, candidatePath string, changeSeq string) {
	t.Helper()
	stopBackgroundWatch(t, f)
	for _, setting := range [][2]string{{"defaults.merge_mode", "pr"}, {"defaults.review", "full-pipeline"}, {"review_loop.enabled", "true"}} {
		runPosse(t, f.binary, f.repo, f.env, "config", "set", setting[0], setting[1])
	}
	if maxRounds != "" {
		runPosse(t, f.binary, f.repo, f.env, "config", "set", "review_loop.max_rounds", maxRounds)
	}
	if code, output := startReviewIdentityTask(t, f, "Private review candidate", "author", ""); code != 0 {
		t.Fatalf("start Change Task: %d %s", code, output)
	}
	change := f.mustTask(t, "t1")
	path := filepath.Join(change.WorktreePath, "review-issue.txt")
	if err := os.WriteFile(path, []byte("unsafe operation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, change.WorktreePath, "add", "review-issue.txt")
	gitTest(t, f.env, change.WorktreePath, "commit", "-m", "add review issue")
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "publish", "Candidate summary", "--verify", "go test ./... -> passed", "--proof", "Intent behavior verified", "--risk", "Risk: low\nRollback: revert"); code != 0 {
		t.Fatalf("submit candidate: %d %s", code, output)
	}
	candidate, err := f.db.LatestValidationCandidate(context.Background(), change.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, workerEnv, "signal", "done", "Candidate submitted for validation", "--turn", turn); code != 0 {
		t.Fatalf("complete candidate submission turn: %d %s", code, output)
	}
	assertTaskBranchAbsentOnBareOrigin(t, f, change.Branch)
	return change.WorktreePath, candidate.WorktreePath, "t1"
}

func recordBlockingReviewFinding(t *testing.T, f *prLifecycleFixture, changeSeq string) {
	t.Helper()
	if code, output := runReviewIdentityCLI(t, f, "review", "start", changeSeq, "--profile", "reviewer"); code != 0 {
		t.Fatalf("start Review Round: %d %s", code, output)
	}
	review := f.mustTask(t, "t2")
	waitForReviewIdentityAgent(t, f, review.PaneID)
	report := filepath.Join(f.home, "scratch", "shop", "t2", "report.md")
	findings := filepath.Join(f.home, "scratch", "shop", "t2", "findings.json")
	if err := os.WriteFile(report, []byte("The candidate contains one blocking pattern.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := `{"schema":"posse.review-findings.v1","patterns":[{"key":"unsafe-operation","title":"Unsafe operation","rule":"Operations must be validated","root_cause":"The value is used without validation","fix_guidance":"Fix every occurrence of the unsafe operation","severity":"P1","origin":"introduced","instances":[{"repository":"shop","path":"review-issue.txt","line":1,"symbol":"operation","excerpt":"unsafe operation","description":"Unvalidated operation"}]}],"resolutions":[]}`
	if err := os.WriteFile(findings, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--findings", findings, "--verdict", "changes_requested")
}

func recordPassingReview(t *testing.T, f *prLifecycleFixture, changeSeq string) {
	t.Helper()
	if code, output := runReviewIdentityCLI(t, f, "review", "start", changeSeq, "--profile", "reviewer"); code != 0 {
		t.Fatalf("start Review Round: %d %s", code, output)
	}
	review := f.mustTask(t, "t2")
	waitForReviewIdentityAgent(t, f, review.PaneID)
	report := filepath.Join(f.home, "scratch", "shop", "t2", "report.md")
	findings := filepath.Join(f.home, "scratch", "shop", "t2", "findings.json")
	if err := os.WriteFile(report, []byte("The candidate passed review.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(findings, []byte(`{"schema":"posse.review-findings.v1","patterns":[],"resolutions":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--findings", findings, "--verdict", "pass")
}

func TestLeadCanDeferP2FindingWithIssue(t *testing.T) {
	f := reviewIdentityFixture(t)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "autonomy.review", "lead", "--project", "shop")
	_, _, changeSeq := startPrivateReviewChange(t, f, "3")
	if code, output := runReviewIdentityCLI(t, f, "review", "start", changeSeq, "--profile", "reviewer"); code != 0 {
		t.Fatalf("start Review Round: %d %s", code, output)
	}
	review := f.mustTask(t, "t2")
	waitForReviewIdentityAgent(t, f, review.PaneID)
	report := filepath.Join(f.home, "scratch", "shop", "t2", "report.md")
	findings := filepath.Join(f.home, "scratch", "shop", "t2", "findings.json")
	if err := os.WriteFile(report, []byte("One low-priority pattern requires follow-up.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := `{"schema":"posse.review-findings.v1","patterns":[{"key":"minor-operation","title":"Minor operation","rule":"Operations should be validated","root_cause":"The value lacks a low-risk validation","fix_guidance":"Validate the operation","severity":"P2","origin":"introduced","instances":[{"repository":"shop","path":"review-issue.txt","line":1,"symbol":"operation","excerpt":"unsafe operation","description":"Low-priority unvalidated operation"}]}],"resolutions":[]}`
	if err := os.WriteFile(findings, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--findings", findings, "--verdict", "pass")
	const issue = "https://github.com/acme/shop/issues/299"
	if code, output := runReviewIdentityCLI(t, f, "review", "dispose", changeSeq, "P-1", "defer", "--issue", issue); code != 0 {
		t.Fatalf("Lead could not defer P2 finding with issue: %d %s", code, output)
	}
	var severity, savedIssue, state, actor string
	if err := f.db.QueryRowContext(context.Background(), `SELECT f.severity,f.follow_up_url,f.state,e.actor FROM review_findings f JOIN review_finding_events e ON e.finding_id=f.id WHERE f.ship_task_id=(SELECT id FROM tasks WHERE seq=1 AND type='change') ORDER BY e.id DESC LIMIT 1`).Scan(&severity, &savedIssue, &state, &actor); err != nil {
		t.Fatal(err)
	}
	if severity != "P2" || savedIssue != issue || state != "deferred" || actor != "lead" {
		t.Fatalf("defer=(severity %s, issue %s, state %s, actor %s), want P2, %s, deferred, lead", severity, savedIssue, state, actor, issue)
	}
}

func TestReviewFollowupsDefersAnAreasNonBlockingFindingsToOneIssue(t *testing.T) {
	f := reviewIdentityFixture(t)
	_, _, changeSeq := startPrivateReviewChange(t, f, "3")
	if code, output := runReviewIdentityCLI(t, f, "review", "start", changeSeq, "--profile", "reviewer"); code != 0 {
		t.Fatalf("start Review Round: %d %s", code, output)
	}
	review := f.mustTask(t, "t2")
	waitForReviewIdentityAgent(t, f, review.PaneID)
	report := filepath.Join(f.home, "scratch", "shop", "t2", "report.md")
	findings := filepath.Join(f.home, "scratch", "shop", "t2", "findings.json")
	if err := os.WriteFile(report, []byte("One blocking and two low-priority patterns.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	instance := func(symbol string) string {
		return `{"repository":"shop","path":"review-issue.txt","line":1,"symbol":"` + symbol + `","excerpt":"unsafe operation","description":"` + symbol + `"}`
	}
	pattern := func(key, severity, symbol string) string {
		return `{"key":"` + key + `","title":"` + key + `","rule":"rule","root_cause":"cause","fix_guidance":"guidance","severity":"` + severity + `","origin":"introduced","instances":[` + instance(symbol) + `]}`
	}
	content := `{"schema":"posse.review-findings.v1","patterns":[` + pattern("blocking-operation", "P1", "blocking") + `,` + pattern("minor-operation", "P2", "minor") + `,` + pattern("style-operation", "P3", "style") + `],"resolutions":[]}`
	if err := os.WriteFile(findings, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--findings", findings, "--verdict", "changes_requested")
	code, output := runReviewIdentityCLI(t, f, "review", "followups")
	if code != 0 || !strings.Contains(output, "F-2") || !strings.Contains(output, "F-3") || strings.Contains(output, "F-1") {
		t.Fatalf("followups must list the unlinked P2 and P3 findings and not the blocking P1: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "review", "followups", "defer", "--area", ".", "--issue", ""); code == 0 {
		t.Fatalf("followups defer accepted an empty issue: %s", output)
	}
	const issue = "https://github.com/acme/shop/issues/301"
	if code, output := runReviewIdentityCLI(t, f, "review", "followups", "defer", "--area", ".", "--issue", issue); code != 0 || !strings.Contains(output, "deferred: 2") {
		t.Fatalf("defer the area's findings to one issue: %d %s", code, output)
	}
	rows, err := f.db.QueryContext(context.Background(), `SELECT seq,state,follow_up_url FROM review_findings WHERE ship_task_id=(SELECT id FROM tasks WHERE seq=1 AND type='change') ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var seq int
		var state, url string
		if err := rows.Scan(&seq, &state, &url); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("F-%d %s %s", seq, state, url))
	}
	want := []string{"F-1 open ", "F-2 deferred " + issue, "F-3 deferred " + issue}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ledger after area deferral:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if code, output := runReviewIdentityCLI(t, f, "review", "followups"); code != 0 || strings.Contains(output, "F-2") || strings.Contains(output, "F-3") {
		t.Fatalf("linked findings are still listed as unlinked: %d %s", code, output)
	}
}

func TestFullPipelineWholePatternFixRequiresPrivateCandidateResolutions(t *testing.T) {
	f := reviewIdentityFixture(t)
	_, candidatePath, changeSeq := startPrivateReviewChange(t, f, "3")
	recordBlockingReviewFinding(t, f, changeSeq)
	change := f.mustTask(t, changeSeq)
	if code, output := runReviewIdentityCLI(t, f, "review", "fix", changeSeq, "--pattern", "P-1"); code != 0 || !strings.Contains(output, "queued") {
		t.Fatalf("queue whole-pattern fix: %d %s", code, output)
	}
	if _, err := herdr.NewWithEnv("herdr", f.env).Run(context.Background(), "pane", "report-agent", change.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	if code, output := runReviewIdentityCLI(t, f, "show", changeSeq); code != 0 {
		t.Fatalf("deliver queued fix instruction: %d %s", code, output)
	}
	var messageID int64
	if err := f.db.QueryRowContext(context.Background(), `SELECT message_id FROM validation_fix_rounds WHERE ship_task_id=(SELECT id FROM tasks WHERE seq=1 AND type='change') AND kind='review'`).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	var body, messageStatus string
	if err := f.db.QueryRowContext(context.Background(), `SELECT body,status FROM messages WHERE id=?`, messageID).Scan(&body, &messageStatus); err != nil {
		t.Fatal(err)
	}
	if messageStatus != "delivered" || !strings.Contains(body, "Fix every instance") || !strings.Contains(body, "P-1") || !strings.Contains(body, "unsafe operation") {
		t.Fatalf("whole-pattern instruction missing or undelivered: status=%s body=%s", messageStatus, body)
	}
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	turn, err := f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, candidatePath, workerEnv, "signal", "done", "Missing answers", "--turn", turn); code != 1 || !strings.Contains(output, "review_resolution_missing") {
		t.Fatalf("done without whole-pattern answers was accepted: %d %s", code, output)
	}
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "signal", "done", "Wrong checkout", "--turn", turn); code != 1 || !strings.Contains(output, "candidate_checkout_required") {
		t.Fatalf("fix completed outside the private candidate: %d %s", code, output)
	}
	issue := filepath.Join(candidatePath, "review-issue.txt")
	if err := os.WriteFile(issue, []byte("validated operation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, candidatePath, "add", "review-issue.txt")
	gitTest(t, f.env, candidatePath, "commit", "-m", "fix unsafe operation")
	resolutions := filepath.Join(f.home, "scratch", "shop", "t1", "review-resolutions.json")
	payload, _ := json.Marshal(map[string]any{"schema": "posse.review-resolutions.v1", "patterns": []any{map[string]any{"pattern": "P-1", "status": "fixed", "note": "Fixed the complete pattern"}}})
	if err := os.WriteFile(resolutions, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	turn, err = f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, candidatePath, workerEnv, "signal", "done", "Fixed the complete pattern", "--turn", turn, "--resolutions", resolutions); code != 0 {
		t.Fatalf("record candidate-bound Review resolution: %d %s", code, output)
	}
	var state string
	if err := f.db.QueryRowContext(context.Background(), `SELECT state FROM review_findings WHERE ship_task_id=?`, change.ID).Scan(&state); err != nil || state != "fix_claimed" {
		t.Fatalf("resolution state=%q err=%v, want fix_claimed", state, err)
	}
	candidate, err := f.db.LatestValidationCandidate(context.Background(), change.ID, "")
	if err != nil || candidate.CandidateSHA == "" || candidate.CandidateSHA == candidate.SubmittedSHA {
		t.Fatalf("private candidate was not advanced with its fix: %#v %v", candidate, err)
	}
	assertTaskBranchAbsentOnBareOrigin(t, f, change.Branch)
}

func TestReviewPatternMergeAndDispositionRecordWholePatternEvents(t *testing.T) {
	f := reviewIdentityFixture(t)
	_, _, changeSeq := startPrivateReviewChange(t, f, "3")
	if code, output := runReviewIdentityCLI(t, f, "review", "start", changeSeq, "--profile", "reviewer"); code != 0 {
		t.Fatalf("start Review Round: %d %s", code, output)
	}
	review := f.mustTask(t, "t2")
	waitForReviewIdentityAgent(t, f, review.PaneID)
	report := filepath.Join(f.home, "scratch", "shop", "t2", "report.md")
	findings := filepath.Join(f.home, "scratch", "shop", "t2", "findings.json")
	if err := os.WriteFile(report, []byte("Two findings share one root cause.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := `{"schema":"posse.review-findings.v1","patterns":[{"key":"unsafe-operation","title":"Unsafe operation","rule":"Operations must be validated","root_cause":"The value is used without validation","fix_guidance":"Validate each operation","severity":"P1","origin":"introduced","instances":[{"repository":"shop","path":"review-issue.txt","line":1,"symbol":"operation","excerpt":"unsafe operation","description":"Unvalidated operation"}]},{"key":"unchecked-result","title":"Unchecked result","rule":"Results must be checked","root_cause":"The same value is used without validation","fix_guidance":"Validate each operation","severity":"P1","origin":"introduced","instances":[{"repository":"shop","path":"review-issue.txt","line":1,"symbol":"operation","excerpt":"unsafe operation","description":"Result is not checked"}]}],"resolutions":[]}`
	if err := os.WriteFile(findings, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--findings", findings, "--verdict", "changes_requested")
	change := f.mustTask(t, changeSeq)
	if code, output := runReviewIdentityCLI(t, f, "review", "merge", changeSeq, "P-1", "P-2"); code != 0 {
		t.Fatalf("merge duplicate patterns: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "review", "dispose", changeSeq, "P-1", "accept"); code != 1 || !strings.Contains(output, "review_disposition_requires_user") {
		t.Fatalf("accepted blocking pattern without User quote: %d %s", code, output)
	}
	const quote = "Accept both findings from this reviewed candidate."
	if code, output := runReviewIdentityCLI(t, f, "review", "dispose", changeSeq, "P-1", "accept", "--user-approved", quote); code != 0 {
		t.Fatalf("record User disposition: %d %s", code, output)
	}
	var accepted, merged int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM review_findings WHERE ship_task_id=? AND state='accepted'`, change.ID).Scan(&accepted); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM review_patterns WHERE ship_task_id=? AND merged_into IS NOT NULL`, change.ID).Scan(&merged); err != nil {
		t.Fatal(err)
	}
	if accepted != 2 || merged != 1 {
		t.Fatalf("accepted findings=%d merged patterns=%d; want two and one", accepted, merged)
	}
	var quoted int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM review_finding_events WHERE actor='lead' AND user_quote=?`, quote).Scan(&quoted); err != nil || quoted != 2 {
		t.Fatalf("quoted disposition events=%d err=%v", quoted, err)
	}
}

func TestFullPipelineReviewCarryIsReviewOnlyAtPublication(t *testing.T) {
	f := reviewIdentityFixture(t)
	changePath, candidatePath, changeSeq := startPrivateReviewChange(t, f, "3")
	recordPassingReview(t, f, changeSeq)
	change := f.mustTask(t, changeSeq)
	deliverReviewPassNextStep(t, f, f.leadEnv, f.repo, change)
	passed, err := f.db.LatestValidationCandidate(context.Background(), change.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	passedHead := passed.CandidateSHA
	// Advancing the frozen target with an empty commit changes its identity but
	// not its tree, so the candidate can carry review without carrying stages.
	gitTest(t, f.env, f.repo, "switch", "main")
	gitTest(t, f.env, f.repo, "commit", "--allow-empty", "-m", "advance target without tree changes")
	gitTest(t, f.env, f.repo, "push", "origin", "main")
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	if code, output := runReleaseCLI(t, f.binary, candidatePath, workerEnv, "signal", "working", "Refresh the private candidate for the new target"); code != 0 {
		t.Fatalf("resume Change Task: %d %s", code, output)
	}
	if code, output := runReleaseCLI(t, f.binary, changePath, workerEnv, "publish", "Refreshed candidate", "--verify", "go test ./... -> passed", "--proof", "Intent behavior verified", "--risk", "Risk: low\nRollback: revert"); code != 0 {
		t.Fatalf("submit candidate with refreshed target: %d %s", code, output)
	}
	refreshed, err := f.db.LatestValidationCandidate(context.Background(), change.ID, "")
	if err != nil || refreshed.CandidateSHA == passedHead {
		t.Fatalf("candidate was not refreshed: %+v %v", refreshed, err)
	}
	turn, err := f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, refreshed.WorktreePath, workerEnv, "signal", "done", "Candidate refreshed from clean target merge", "--turn", turn); code != 0 {
		t.Fatalf("auto-record Review carry on done Signal: %d %s", code, output)
	}
	var carryCount int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM review_carries WHERE ship_task_id=? AND from_head=? AND to_head=?`, change.ID, passedHead, refreshed.CandidateSHA).Scan(&carryCount); err != nil || carryCount != 1 {
		t.Fatalf("automatic carry rows=%d err=%v", carryCount, err)
	}
	if code, output := runReleaseCLI(t, f.binary, refreshed.WorktreePath, workerEnv, "signal", "working", "Continue candidate validation"); code != 0 {
		t.Fatalf("reopen for local stages: %d %s", code, output)
	}
	workerEnv = setEnv(workerEnv, "HERDR_PANE_ID", change.PaneID)
	if code, output := runReleaseCLI(t, f.binary, refreshed.WorktreePath, workerEnv, "validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent behavior verified"); code != 0 {
		t.Fatalf("carried Review evidence did not admit test stage: %d %s", code, output)
	}
	if code, output := runReleaseCLI(t, f.binary, refreshed.WorktreePath, workerEnv, "publish", "Refreshed candidate"); code == 0 || !strings.Contains(output, "validation_stage_required") {
		t.Fatalf("Review carry incorrectly carried docs/lint stages: %d %s", code, output)
	}
	assertTaskBranchAbsentOnBareOrigin(t, f, change.Branch)
}

func TestReviewCapAcceptHeadIsExactHeadEvidenceAndDoesNotSkipStages(t *testing.T) {
	f := reviewIdentityFixture(t)
	_, candidatePath, changeSeq := startPrivateReviewChange(t, f, "1")
	recordBlockingReviewFinding(t, f, changeSeq)
	change := f.mustTask(t, changeSeq)
	// The last allowed verdict is followed by a new author head. This is the
	// cap condition for a final-pass candidate with unreviewed code changes.
	if err := os.WriteFile(filepath.Join(candidatePath, "review-issue.txt"), []byte("changed after the final review\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, candidatePath, "add", "review-issue.txt")
	gitTest(t, f.env, candidatePath, "commit", "-m", "unreviewed final candidate")
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	if code, output := runReleaseCLI(t, f.binary, candidatePath, workerEnv, "signal", "working", "Submit the new candidate head"); code != 0 {
		t.Fatalf("reopen Change Task: %d %s", code, output)
	}
	turn, err := f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, candidatePath, workerEnv, "signal", "done", "Candidate changed after the final Review", "--turn", turn); code != 0 {
		t.Fatalf("record new private candidate head: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "decisions"); code != 0 || !strings.Contains(output, "review_cap") {
		t.Fatalf("reconcile Review cap Decision: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "review", changeSeq, "--full"); code != 0 || !strings.Contains(output, "review_rounds_used") || !strings.Contains(output, "P-1") {
		t.Fatalf("inspect Finding Ledger and cap usage: %d %s", code, output)
	}
	var decisionID int64
	var origin string
	if err := f.db.QueryRowContext(context.Background(), `SELECT id,origin FROM decisions WHERE task_id=? AND kind='review_cap' AND answered_at=0 AND obsolete_at=0`, change.ID).Scan(&decisionID, &origin); err != nil {
		t.Fatal(err)
	}
	candidate, err := f.db.LatestValidationCandidate(context.Background(), change.ID, "")
	if err != nil || !strings.HasSuffix(origin, candidate.CandidateSHA) {
		t.Fatalf("cap Decision is not bound to candidate head %s: origin=%s err=%v", candidate.CandidateSHA, origin, err)
	}
	if code, output := runReviewIdentityCLI(t, f, "decide", fmt.Sprint(decisionID), "accept-head"); code != 1 || !strings.Contains(output, "user_only") {
		t.Fatalf("Lead answered a User-only Decision without a User quote: %d %s", code, output)
	}
	const quote = "Accept this exact candidate head despite the blocking finding."
	if code, output := runReviewIdentityCLI(t, f, "decide", fmt.Sprint(decisionID), "accept-head", "--user-approved", quote); code != 0 {
		t.Fatalf("record User's cap choice: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "apply", fmt.Sprint(decisionID)); code != 0 || !strings.Contains(output, "accepted-head") {
		t.Fatalf("apply exact-head acceptance: %d %s", code, output)
	}
	var dispositionHead, recordedQuote string
	if err := f.db.QueryRowContext(context.Background(), `SELECT head_sha,user_quote FROM review_dispositions WHERE ship_task_id=? ORDER BY id DESC LIMIT 1`, change.ID).Scan(&dispositionHead, &recordedQuote); err != nil || dispositionHead != candidate.CandidateSHA || recordedQuote != quote {
		t.Fatalf("accepted-head disposition=(%s,%q), want (%s,%q), err=%v", dispositionHead, recordedQuote, candidate.CandidateSHA, quote, err)
	}
	if code, output := runReleaseCLI(t, f.binary, candidatePath, workerEnv, "signal", "working", "Resume local validation"); code != 0 {
		t.Fatalf("resume validation: %d %s", code, output)
	}
	if code, output := runReleaseCLI(t, f.binary, candidatePath, workerEnv, "publish", "Candidate summary"); code == 0 || !strings.Contains(output, "validation_stage_required") {
		t.Fatalf("accept-head incorrectly skipped local stages: %d %s", code, output)
	}
	assertTaskBranchAbsentOnBareOrigin(t, f, change.Branch)
	for _, args := range [][]string{
		{"validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent behavior verified"},
		{"validate", "docs", "--no-comments", "passed", "--no-owner-doc-change", "No owner documentation changes are warranted", "--proof", "Complete PR description prepared"},
		{"validate", "lint", "--proof", "Configured Gates passed", "--coverage", "No local workflow jobs are configured"},
	} {
		if code, output := runReleaseCLI(t, f.binary, candidatePath, workerEnv, args...); code != 0 {
			t.Fatalf("record stage %v after User acceptance: %d %s", args, code, output)
		}
		assertTaskBranchAbsentOnBareOrigin(t, f, change.Branch)
	}
	if code, output := runReleaseCLI(t, f.binary, candidatePath, workerEnv, "publish", "Candidate summary"); code != 0 || !strings.Contains(output, "promoted: candidate") {
		t.Fatalf("promote candidate with Review-only acceptance and required stages: %d %s", code, output)
	}
	if published := gitRef(t, f, f.remote, "refs/heads/"+change.Branch); published != candidate.CandidateSHA {
		t.Fatalf("bare origin published %s, want accepted candidate %s", published, candidate.CandidateSHA)
	}
}

func TestReviewCapBlockingFindingNeedsTheUsersCapDecision(t *testing.T) {
	f := reviewIdentityFixture(t)
	_, _, changeSeq := startPrivateReviewChange(t, f, "1")
	recordBlockingReviewFinding(t, f, changeSeq)
	change := f.mustTask(t, changeSeq)
	const quote = "ok làm hết"
	for _, args := range [][]string{
		{"accept", "--user-approved", quote},
		{"defer", "--issue", "https://github.com/acme/shop/issues/300", "--user-approved", quote},
		{"withdraw", "--reason", "not a real problem", "--user-approved", quote},
		{"reclassify", "--severity", "P2", "--user-approved", quote},
	} {
		command := append([]string{"review", "dispose", changeSeq, "F-1"}, args...)
		if code, output := runReviewIdentityCLI(t, f, command...); code != 1 || !strings.Contains(output, "review_cap_decision_required") || !strings.Contains(output, "review_cap") {
			t.Fatalf("%s cleared an introduced P1 at the spent cap without the User's cap Decision: %d %s", args[0], code, output)
		}
	}
	var state, severity string
	if err := f.db.QueryRowContext(context.Background(), `SELECT state,severity FROM review_findings WHERE ship_task_id=?`, change.ID).Scan(&state, &severity); err != nil || state != "open" || severity != "P1" {
		t.Fatalf("finding=(%s,%s) err=%v, want open P1", state, severity, err)
	}
}

func TestReviewCapRethinkStartsAChangeWithInheritedFindings(t *testing.T) {
	f := reviewIdentityFixture(t)
	_, _, changeSeq := startPrivateReviewChange(t, f, "1")
	recordBlockingReviewFinding(t, f, changeSeq)
	change := f.mustTask(t, changeSeq)
	var decisionID int64
	if err := f.db.QueryRowContext(context.Background(), `SELECT id FROM decisions WHERE task_id=? AND kind='review_cap' AND answered_at=0 AND obsolete_at=0`, change.ID).Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	const quote = "Rethink the approach and carry forward the finding history."
	if code, output := runReviewIdentityCLI(t, f, "decide", fmt.Sprint(decisionID), "rethink", "--user-approved", quote); code != 0 {
		t.Fatalf("answer rethink: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "apply", fmt.Sprint(decisionID)); code != 0 || !strings.Contains(output, "rethink") {
		t.Fatalf("apply rethink: %d %s", code, output)
	}
	brief := filepath.Join(f.home, "scratch", "shop", "rethink-followup.md")
	if err := os.MkdirAll(filepath.Dir(brief), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := []byte("---\ntype: change\ntitle: Rethink private candidate\ndone_when: revised approach is checked\nmerge_mode: pr\nreview: full-pipeline\nsupersedes: t1\n---\nRework the approach while preserving the User's decision context.\n")
	if err := os.WriteFile(brief, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runReviewIdentityCLI(t, f, "start", "--brief", brief, "--name", "rethink-followup"); code != 0 {
		t.Fatalf("start superseding Change Task: %d %s", code, output)
	}
	followup := f.mustTask(t, "t3")
	if code, output := runReviewIdentityCLI(t, f, "review", "t3", "--full"); code != 0 || !strings.Contains(output, "inherited") {
		t.Fatalf("inspect inherited Finding Ledger: %d %s", code, output)
	}
	var inherited int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM review_findings WHERE ship_task_id=? AND inherited_from IN (SELECT id FROM review_findings WHERE ship_task_id=?) AND state='deferred'`, followup.ID, change.ID).Scan(&inherited); err != nil || inherited != 1 {
		t.Fatalf("inherited findings=%d err=%v", inherited, err)
	}
	assertTaskBranchAbsentOnBareOrigin(t, f, change.Branch)
}

func TestReviewCapFinalFixRaisesANewDecisionForItsResultingHead(t *testing.T) {
	f := reviewIdentityFixture(t)
	_, candidatePath, changeSeq := startPrivateReviewChange(t, f, "1")
	recordBlockingReviewFinding(t, f, changeSeq)
	change := f.mustTask(t, changeSeq)
	if code, output := runReviewIdentityCLI(t, f, "decisions"); code != 0 || !strings.Contains(output, "review_cap") {
		t.Fatalf("last-round changes_requested did not raise its User-only cap Decision: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "review", "fix", changeSeq); code != 1 || !strings.Contains(output, "review_cap_reached") {
		t.Fatalf("cap did not stop an ordinary fix: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "decisions"); code != 0 {
		t.Fatalf("list Review cap Decision: %d %s", code, output)
	}
	var decisionID int64
	if err := f.db.QueryRowContext(context.Background(), `SELECT id FROM decisions WHERE task_id=? AND kind='review_cap' AND answered_at=0`, change.ID).Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	const quote = "Allow one final whole-pattern fix."
	if code, output := runReviewIdentityCLI(t, f, "decide", fmt.Sprint(decisionID), "final-fix", "--user-approved", quote); code != 0 {
		t.Fatalf("answer final-fix: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "apply", fmt.Sprint(decisionID)); code != 0 || !strings.Contains(output, "final-fix-queued") {
		t.Fatalf("apply final-fix: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "apply", fmt.Sprint(decisionID)); code == 0 || strings.Contains(output, "final-fix-queued") {
		t.Fatalf("duplicate final-fix must not report a newly queued instruction: %d %s", code, output)
	}
	if _, err := herdr.NewWithEnv("herdr", f.env).Run(context.Background(), "pane", "report-agent", change.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	if code, output := runReviewIdentityCLI(t, f, "show", changeSeq); code != 0 {
		t.Fatalf("deliver final-fix instruction: %d %s", code, output)
	}
	issue := filepath.Join(candidatePath, "review-issue.txt")
	if err := os.WriteFile(issue, []byte("validated operation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, candidatePath, "add", "review-issue.txt")
	gitTest(t, f.env, candidatePath, "commit", "-m", "apply final Review fix")
	resolutions := filepath.Join(f.home, "scratch", "shop", "t1", "final-fix-resolutions.json")
	contents := []byte(`{"schema":"posse.review-resolutions.v1","patterns":[{"pattern":"P-1","status":"fixed"}]}`)
	if err := os.WriteFile(resolutions, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	turn, err := f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	if code, output := runReleaseCLI(t, f.binary, candidatePath, workerEnv, "signal", "done", "Applied final whole-pattern fix", "--turn", turn, "--resolutions", resolutions); code != 0 {
		t.Fatalf("record final-fix resolution: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "decisions"); code != 0 {
		t.Fatalf("reconcile next-head cap Decision: %d %s", code, output)
	}
	var nextID int64
	var nextOrigin string
	if err := f.db.QueryRowContext(context.Background(), `SELECT id,origin FROM decisions WHERE task_id=? AND kind='review_cap' AND answered_at=0`, change.ID).Scan(&nextID, &nextOrigin); err != nil {
		t.Fatal(err)
	}
	candidate, err := f.db.LatestValidationCandidate(context.Background(), change.ID, "")
	if err != nil || nextID == decisionID || !strings.HasSuffix(nextOrigin, candidate.CandidateSHA) {
		t.Fatalf("next-head cap Decision id=%d origin=%s candidate=%s err=%v", nextID, nextOrigin, candidate.CandidateSHA, err)
	}
	assertTaskBranchAbsentOnBareOrigin(t, f, change.Branch)
}

// recordReviewRound admits the next Review Round of changeSeq and completes it
// with findings, returning the Review Task.
func recordReviewRound(t *testing.T, f *prLifecycleFixture, changeSeq, verdict, findings string) store.Task {
	t.Helper()
	if code, output := runReviewIdentityCLI(t, f, "review", "start", changeSeq, "--profile", "reviewer"); code != 0 {
		t.Fatalf("start Review Round: %d %s", code, output)
	}
	tasks, err := f.db.Tasks(context.Background(), f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	review := tasks[len(tasks)-1]
	waitForReviewIdentityAgent(t, f, review.PaneID)
	scratch := filepath.Join(f.home, "scratch", "shop", taskIDOf(review))
	report, findingsPath := filepath.Join(scratch, "report.md"), filepath.Join(scratch, "findings.json")
	if err := os.WriteFile(report, []byte("Review Round recorded "+verdict+".\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(findingsPath, []byte(findings), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--findings", findingsPath, "--verdict", verdict)
	return review
}

func blockingFindings(key, path, excerpt string) string {
	return `{"schema":"posse.review-findings.v1","patterns":[{"key":"` + key + `","title":"Unsafe operation","rule":"Operations must be validated","root_cause":"The value is used without validation","fix_guidance":"Validate the operation","severity":"P1","origin":"introduced","instances":[{"repository":"shop","path":"` + path + `","line":1,"symbol":"operation","excerpt":"` + excerpt + `","description":"Unvalidated operation"}]}],"resolutions":[]}`
}

func openReviewCapDecisions(t *testing.T, f *prLifecycleFixture, change store.Task) []int64 {
	t.Helper()
	if code, output := runReviewIdentityCLI(t, f, "decisions"); code != 0 {
		t.Fatalf("reconcile Decisions: %d %s", code, output)
	}
	rows, err := f.db.QueryContext(context.Background(), `SELECT id FROM decisions WHERE task_id=? AND kind='review_cap' AND answered_at=0 AND obsolete_at=0`, change.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestDefaultCapRoundThreeChangesRequestedRaisesOneUserOnlyDecision(t *testing.T) {
	f := reviewIdentityFixture(t)
	change := startQuickLoopChange(t, f, "false")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "autonomy.review", "lead", "--project", "shop")
	plan, err := change.AdmittedPlan()
	if err != nil || plan == nil || plan.ValidationLoops.MaxReviewRounds() != 3 {
		t.Fatalf("Change Task was not admitted with the default cap of three rounds: %#v %v", plan, err)
	}
	for round := 1; round <= 3; round++ {
		name := fmt.Sprintf("round-%d.txt", round)
		commitFile(t, f, change, name, "unsafe operation")
		resubmitChange(t, f, change, fmt.Sprintf("Candidate for round %d", round))
		recordReviewRound(t, f, "t1", "changes_requested", blockingFindings(fmt.Sprintf("unsafe-operation-%d", round), name, "unsafe operation"))
		decisions := openReviewCapDecisions(t, f, change)
		if round < 3 && len(decisions) != 0 {
			t.Fatalf("round %d of 3 raised a cap Decision: %v", round, decisions)
		}
		if round == 3 && len(decisions) != 1 {
			t.Fatalf("round 3 changes_requested raised %d cap Decisions, want one", len(decisions))
		}
	}
	decisionID := openReviewCapDecisions(t, f, change)[0]
	for _, option := range []string{"accept-head", "final-fix", "rethink", "discard"} {
		if code, output := runReviewIdentityCLI(t, f, "decide", fmt.Sprint(decisionID), option); code != 1 || !strings.Contains(output, "user_only") {
			t.Fatalf("Autonomy answered the cap Decision with %s: %d %s", option, code, output)
		}
	}
	if decisions := openReviewCapDecisions(t, f, change); len(decisions) != 1 || decisions[0] != decisionID {
		t.Fatalf("cap Decision changed without the User: %v", decisions)
	}
}

func TestRethinkSuccessorSuppressesInheritedFingerprints(t *testing.T) {
	f := reviewIdentityFixture(t)
	_, _, changeSeq := startPrivateReviewChange(t, f, "1")
	recordBlockingReviewFinding(t, f, changeSeq)
	change := f.mustTask(t, changeSeq)
	var decisionID int64
	if err := f.db.QueryRowContext(context.Background(), `SELECT id FROM decisions WHERE task_id=? AND kind='review_cap' AND answered_at=0 AND obsolete_at=0`, change.ID).Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	const quote = "Rethink the approach and carry forward the finding history."
	if code, output := runReviewIdentityCLI(t, f, "decide", fmt.Sprint(decisionID), "rethink", "--user-approved", quote); code != 0 {
		t.Fatalf("answer rethink: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "apply", fmt.Sprint(decisionID)); code != 0 {
		t.Fatalf("apply rethink: %d %s", code, output)
	}
	brief := filepath.Join(f.home, "scratch", "shop", "rethink-successor.md")
	contents := []byte("---\ntype: change\ntitle: Rethink private candidate\ndone_when: revised approach is checked\nmerge_mode: pr\nreview: full-pipeline\nsupersedes: t1\n---\nRework the approach.\n")
	if err := os.WriteFile(brief, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runReviewIdentityCLI(t, f, "start", "--brief", brief, "--name", "rethink-successor", "--profile", "author"); code != 0 {
		t.Fatalf("start superseding Change Task: %d %s", code, output)
	}
	successor := f.mustTask(t, "t3")
	// The successor still contains the line the predecessor's finding named.
	commitFile(t, f, successor, "review-issue.txt", "unsafe operation")
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", successor.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	if code, output := runReleaseCLI(t, f.binary, successor.WorktreePath, workerEnv, "publish", "Rethought candidate", "--verify", "go test ./... -> passed", "--proof", "Intent behavior verified", "--risk", "Risk: low\nRollback: revert"); code != 0 {
		t.Fatalf("submit successor candidate: %d %s", code, output)
	}
	candidate, err := f.db.LatestValidationCandidate(context.Background(), successor.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.db.TaskTurnToken(context.Background(), successor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, workerEnv, "signal", "done", "Successor candidate submitted", "--turn", turn); code != 0 {
		t.Fatalf("complete successor submission: %d %s", code, output)
	}
	// The Reviewer reports the predecessor's exact finding again.
	recordReviewRound(t, f, "t3", "pass", blockingFindings("unsafe-operation", "review-issue.txt", "unsafe operation"))
	var findings int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM review_findings WHERE ship_task_id=?`, successor.ID).Scan(&findings); err != nil || findings != 1 {
		t.Fatalf("successor findings=%d err=%v, want only the inherited one", findings, err)
	}
	var data string
	if err := f.db.QueryRowContext(context.Background(), `SELECT data_json FROM notices WHERE task_id=? AND kind='review_round_done' ORDER BY id DESC LIMIT 1`, successor.ID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var round struct {
		NewFindings       int `json:"new_findings"`
		SuppressedRepeats int `json:"suppressed_repeats"`
	}
	if err := json.Unmarshal([]byte(data), &round); err != nil || round.NewFindings != 0 || round.SuppressedRepeats != 1 {
		t.Fatalf("successor round=%+v err=%v, want the inherited fingerprint suppressed", round, err)
	}
	assertTaskBranchAbsentOnBareOrigin(t, f, change.Branch)
	assertTaskBranchAbsentOnBareOrigin(t, f, successor.Branch)
}

// TestFullPipelineWorkspaceWholePatternFixBindsEveryMemberCandidate proves
// that one Review fix instruction carries every selected pattern across the
// workspace Members, that the Worker applies it inside each Member's private
// candidate, and that nothing reaches a remote or target before validation.
func TestFullPipelineWorkspaceWholePatternFixBindsEveryMemberCandidate(t *testing.T) {
	f := reviewIdentityFixture(t)
	ws := newFullPipelineWorkspace(t, f)
	brief := filepath.Join(f.root, "workspace-fix.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Workspace fix\ndone_when: checked\nrepos: [api, web]\n---\nChange both Members safely.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, ws.root, ws.leadEnv, "start", "--brief", brief, "--name", "workspace-fix", "--profile", "author"); code != 0 {
		t.Fatalf("start workspace Change Task: code=%d %s", code, output)
	}
	change := mustProjectTask(t, f, ws.project.ID, "t1")
	repos, err := f.db.TaskRepos(context.Background(), change.ID)
	if err != nil || len(repos) != 2 {
		t.Fatalf("workspace Members: %+v %v", repos, err)
	}
	worktrees := map[string]string{}
	for _, repo := range repos {
		worktrees[repo.Repo] = repo.WorktreePath
		if err := os.WriteFile(filepath.Join(repo.WorktreePath, "change.txt"), []byte("unsafe "+repo.Repo+" operation\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, f.env, repo.WorktreePath, "add", "change.txt")
		gitTest(t, f.env, repo.WorktreePath, "commit", "-m", "change "+repo.Repo)
	}
	webTarget := gitRef(t, f, ws.web, "refs/heads/main")
	audit := func(stage string) {
		t.Helper()
		assertTaskBranchAbsentOnBareOrigin(t, f, change.Branch)
		if current := gitRef(t, f, ws.web, "refs/heads/main"); current != webTarget {
			t.Fatalf("web local target moved during %s", stage)
		}
	}
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	submitted := map[string]store.ValidationCandidate{}
	authorHeads := map[string]string{}
	for _, member := range []string{"api", "web"} {
		if code, output := runReleaseCLI(t, f.binary, worktrees[member], workerEnv,
			"publish", "--repo", member, member+" candidate summary", "--verify", "go test ./... -> passed",
			"--proof", "Intent behavior verified", "--risk", "Risk: low\nRollback: revert the "+member+" change"); code != 0 {
			t.Fatalf("submit %s private candidate: code=%d %s", member, code, output)
		}
		candidate, err := f.db.LatestValidationCandidate(context.Background(), change.ID, member)
		if err != nil {
			t.Fatal(err)
		}
		submitted[member] = candidate
		authorHeads[member] = gitRef(t, f, worktrees[member], "HEAD")
	}

	if code, output := runReleaseCLI(t, f.binary, ws.root, ws.leadEnv, "review", "start", "t1", "--profile", "reviewer"); code != 0 {
		t.Fatalf("start workspace Review Round: code=%d %s", code, output)
	}
	review := mustProjectTask(t, f, ws.project.ID, "t2")
	waitForReviewIdentityAgent(t, f, review.PaneID)
	scratch := filepath.Join(f.home, "scratch", "stack", "t2")
	report := filepath.Join(scratch, "report.md")
	findings := filepath.Join(scratch, "findings.json")
	if err := os.WriteFile(report, []byte("Each Member has one blocking pattern.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := `{"schema":"posse.review-findings.v1","patterns":[` +
		`{"key":"unsafe-api","title":"Unsafe api operation","rule":"API operations must be validated","root_cause":"The api value is used without validation","fix_guidance":"Validate every api operation","severity":"P1","origin":"introduced","instances":[{"repository":"api","path":"change.txt","line":1,"symbol":"operation","excerpt":"unsafe api operation","description":"Unvalidated api operation"}]},` +
		`{"key":"unsafe-web","title":"Unsafe web operation","rule":"Web operations must be validated","root_cause":"The web value is used without validation","fix_guidance":"Validate every web operation","severity":"P1","origin":"introduced","instances":[{"repository":"web","path":"change.txt","line":1,"symbol":"operation","excerpt":"unsafe web operation","description":"Unvalidated web operation"}]}` +
		`],"resolutions":[]}`
	if err := os.WriteFile(findings, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--findings", findings, "--verdict", "changes_requested")
	audit("review")

	if code, output := runReleaseCLI(t, f.binary, ws.root, ws.leadEnv, "review", "fix", "t1"); code != 0 || !strings.Contains(output, "queued") {
		t.Fatalf("queue workspace whole-pattern fix: code=%d %s", code, output)
	}
	if _, err := herdr.NewWithEnv("herdr", f.env).Run(context.Background(), "pane", "report-agent", change.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, ws.root, ws.leadEnv, "show", "t1"); code != 0 {
		t.Fatalf("deliver queued fix instruction: code=%d %s", code, output)
	}
	var instructions int
	var body, messageStatus string
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*),MAX(m.body),MAX(m.status) FROM validation_fix_rounds r JOIN messages m ON m.id=r.message_id WHERE r.ship_task_id=? AND r.kind='review'`, change.ID).Scan(&instructions, &body, &messageStatus); err != nil {
		t.Fatal(err)
	}
	if instructions != 1 || messageStatus != "delivered" {
		t.Fatalf("workspace fix instructions=%d status=%s, want one delivered instruction", instructions, messageStatus)
	}
	for _, want := range []string{"## P-1", "## P-2", "unsafe api operation", "unsafe web operation", "Fix every instance", "Do not publish directly",
		"Member api private candidate checkout: `" + submitted["api"].WorktreePath + "`", "Member web private candidate checkout: `" + submitted["web"].WorktreePath + "`"} {
		if !strings.Contains(body, want) {
			t.Fatalf("workspace fix instruction lacks %q:\n%s", want, body)
		}
	}

	turn, err := f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "signal", "done", "Missing answers", "--turn", turn); code != 1 || !strings.Contains(output, "review_resolution_missing") {
		t.Fatalf("workspace done without whole-pattern answers: code=%d %s", code, output)
	}
	if code, output := runReleaseCLI(t, f.binary, submitted["api"].WorktreePath, workerEnv, "publish", "--repo", "api", "Promote before fixing"); code == 0 || !strings.Contains(output, "review_findings_open") {
		t.Fatalf("api candidate published directly during a Review fix: code=%d %s", code, output)
	}
	audit("refused direct publication")
	for _, member := range []string{"api", "web"} {
		path := submitted[member].WorktreePath
		if err := os.WriteFile(filepath.Join(path, "change.txt"), []byte("validated "+member+" operation\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, f.env, path, "add", "change.txt")
		gitTest(t, f.env, path, "commit", "-m", "validate "+member+" operation")
	}
	resolutions := filepath.Join(f.home, "scratch", "stack", "t1", "review-resolutions.json")
	writeResolutions := func(patterns ...string) {
		t.Helper()
		var answers []any
		for _, pattern := range patterns {
			answers = append(answers, map[string]any{"pattern": pattern, "status": "fixed", "note": "Fixed every instance of " + pattern})
		}
		payload, _ := json.Marshal(map[string]any{"schema": "posse.review-resolutions.v1", "patterns": answers})
		if err := os.WriteFile(resolutions, payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeResolutions("P-1")
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "signal", "done", "One answer", "--turn", turn, "--resolutions", resolutions); code != 1 || !strings.Contains(output, "review_resolution_missing") {
		t.Fatalf("workspace done answering one of two patterns: code=%d %s", code, output)
	}
	writeResolutions("P-1", "P-2")
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "signal", "done", "Fixed both patterns", "--turn", turn, "--resolutions", resolutions); code != 0 {
		t.Fatalf("record workspace candidate-bound resolutions: code=%d %s", code, output)
	}
	var claimed int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM review_findings WHERE ship_task_id=? AND state='fix_claimed'`, change.ID).Scan(&claimed); err != nil || claimed != 2 {
		t.Fatalf("fix_claimed findings=%d err=%v, want 2", claimed, err)
	}
	for _, member := range []string{"api", "web"} {
		candidate, err := f.db.LatestValidationCandidate(context.Background(), change.ID, member)
		if err != nil || candidate.ID != submitted[member].ID || candidate.CandidateSHA == submitted[member].CandidateSHA {
			t.Fatalf("%s private candidate was not advanced by its fix: %#v %v", member, candidate, err)
		}
		if head := gitRef(t, f, worktrees[member], "HEAD"); head != authorHeads[member] {
			t.Fatalf("%s Task branch moved to %s during the private fix, want %s", member, head, authorHeads[member])
		}
	}
	audit("resolutions")

	// Round 2 verifies both claimed fixes on the fixed candidates; each
	// Member then validates and promotes exactly its fixed candidate.
	if code, output := runReleaseCLI(t, f.binary, ws.root, ws.leadEnv, "review", "start", "t1", "--profile", "reviewer"); code != 0 {
		t.Fatalf("start second workspace Review Round: code=%d %s", code, output)
	}
	verify := mustProjectTask(t, f, ws.project.ID, "t3")
	waitForReviewIdentityAgent(t, f, verify.PaneID)
	scratch = filepath.Join(f.home, "scratch", "stack", "t3")
	report = filepath.Join(scratch, "report.md")
	findings = filepath.Join(scratch, "findings.json")
	if err := os.WriteFile(report, []byte("Both fixes are verified.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(findings, []byte(`{"schema":"posse.review-findings.v1","patterns":[],"resolutions":[{"finding":"F-1","status":"resolved","note":"api validates"},{"finding":"F-2","status":"resolved","note":"web validates"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, verify, "--report", report, "--findings", findings, "--verdict", "pass")
	audit("verification round")
	deliverReviewPassNextStep(t, f, ws.leadEnv, ws.root, change)
	for _, member := range []string{"web", "api"} {
		candidate, err := f.db.LatestValidationCandidate(context.Background(), change.ID, member)
		if err != nil {
			t.Fatal(err)
		}
		recordCandidateStages(t, f, workerEnv, candidate, func(stage string) { audit(member + " " + stage) }, "--repo", member)
		if code, output := runReleaseCLI(t, f.binary, candidate.WorktreePath, workerEnv, "publish", "--repo", member, "Promote the fixed candidate"); code != 0 {
			t.Fatalf("promote fixed %s candidate: code=%d %s", member, code, output)
		}
		if member == "api" {
			if published := gitRef(t, f, f.remote, "refs/heads/"+change.Branch); published != candidate.CandidateSHA {
				t.Fatalf("api origin Task branch=%s, want fixed candidate %s", published, candidate.CandidateSHA)
			}
		}
	}
	turn, err = f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "signal", "done", "Both fixed Members validated", "--turn", turn); code != 0 {
		t.Fatalf("done Signal after promoting fixed candidates: code=%d %s", code, output)
	}
	var fixed int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM review_findings WHERE ship_task_id=? AND state='fixed'`, change.ID).Scan(&fixed); err != nil || fixed != 2 {
		t.Fatalf("fixed findings=%d err=%v, want 2", fixed, err)
	}
}

func TestReviewRoundLaunchNamesRealReviewDirWithoutGuidanceFiles(t *testing.T) {
	f := reviewIdentityFixture(t)
	_, _, changeSeq := startPrivateReviewChange(t, f, "3")
	if code, output := runReviewIdentityCLI(t, f, "review", "start", changeSeq, "--profile", "reviewer"); code != 0 {
		t.Fatalf("start Review Round: %d %s", code, output)
	}
	taskHome := filepath.Join(f.home, "projects", "shop", "tasks", "t2")
	launch, err := os.ReadFile(filepath.Join(taskHome, "launch.md"))
	if err != nil {
		t.Fatal(err)
	}
	reviewDir := filepath.Join(taskHome, "review")
	if !strings.Contains(string(launch), "`"+reviewDir+"/delta.patch`") {
		t.Fatalf("launch text does not name the real review directory %s:\n%s", reviewDir, launch)
	}
	if strings.Contains(string(launch), "{{review_dir}}") {
		t.Fatalf("launch text left the review_dir placeholder unresolved")
	}
	if !strings.Contains(string(launch), reviewDir+"/guidance/") {
		t.Fatalf("launch text does not mention review/guidance/")
	}
	for _, name := range []string{"delta.patch", "cumulative.patch", "round.json", "ledger.md", "conflicts.txt"} {
		if _, err := os.Stat(filepath.Join(reviewDir, name)); err != nil {
			t.Fatalf("review file %s named by the launch text is missing: %v", name, err)
		}
	}
	info, err := os.Stat(filepath.Join(reviewDir, "guidance"))
	if err != nil || !info.IsDir() {
		t.Fatalf("review/guidance must exist when the launch text mentions it: info=%v err=%v", info, err)
	}
}
