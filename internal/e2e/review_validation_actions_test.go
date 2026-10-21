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
	for _, args := range [][]string{
		{"validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent behavior verified"},
		{"validate", "docs", "--no-comments", "passed", "--no-owner-doc-change", "No owner documentation changes are warranted", "--proof", "Complete PR description prepared"},
		{"validate", "lint", "--proof", "Configured Gates passed", "--coverage", "No local workflow jobs are configured"},
	} {
		if code, output := runReleaseCLI(t, f.binary, candidatePath, workerEnv, args...); code != 0 {
			t.Fatalf("record stage %v after User acceptance: %d %s", args, code, output)
		}
	}
	if code, output := runReleaseCLI(t, f.binary, candidatePath, workerEnv, "publish", "Candidate summary"); code != 0 || !strings.Contains(output, "promoted: candidate") {
		t.Fatalf("promote candidate with Review-only acceptance and required stages: %d %s", code, output)
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
}
