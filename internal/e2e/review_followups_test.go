//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestReviewFollowupsResolveRecordsVerifiedOutcomes(t *testing.T) {
	f := reviewFollowupsFixture(t)
	const issue = "https://github.com/acme/shop/issues/301"
	if code, output := runReviewIdentityCLI(t, f, "review", "dispose", "t1", "F-3", "defer", "--issue", issue); code != 0 {
		t.Fatalf("link F-3 to an existing follow-up issue: %d %s", code, output)
	}
	const reason = "Verified on main at abc123"
	for _, tc := range []struct {
		id, outcome, reason, want string
	}{
		{"F-1", "fixed", reason, "review_disposition_refused"},
		{"F-2", "fixed", " ", "requires"},
		{"F-99", "obsolete", reason, "review_disposition_refused"},
		{"F-2", "fixed", reason, ""},
		{"F-3", "obsolete", reason, ""},
		{"F-2", "obsolete", reason, "review_disposition_refused"},
	} {
		code, output := runReviewIdentityCLI(t, f, "review", "followups", "resolve", "t1", tc.id, tc.outcome, "--reason", tc.reason)
		if tc.want != "" && (code == 0 || !strings.Contains(output, tc.want)) || tc.want == "" && code != 0 {
			t.Errorf("resolve %s as %s: %d %s", tc.id, tc.outcome, code, output)
		}
	}
	if t.Failed() {
		return
	}
	change := f.mustTask(t, "t1")
	for _, tc := range []struct {
		seq                 int
		state, event, actor string
	}{
		{1, "open", "reported", "reviewer"},
		{2, "fixed", "followup_fixed", "lead"},
		{3, "withdrawn", "followup_obsolete", "lead"},
	} {
		var state, event, actor, note, url string
		err := f.db.QueryRowContext(context.Background(), `SELECT f.state,e.event,e.actor,e.note,f.follow_up_url FROM review_findings f JOIN review_finding_events e ON e.finding_id=f.id WHERE f.ship_task_id=? AND f.seq=? ORDER BY e.id DESC LIMIT 1`, change.ID, tc.seq).Scan(&state, &event, &actor, &note, &url)
		if err != nil || state != tc.state || event != tc.event || actor != tc.actor || tc.seq != 1 && note != reason || tc.seq == 3 && url != issue {
			t.Fatalf("F-%d: state=%s event=%s actor=%s note=%q url=%s err=%v", tc.seq, state, event, actor, note, url, err)
		}
	}
	if code, output := runReviewIdentityCLI(t, f, "review", "followups", "defer", "--area", ".", "--issue", issue); code != 0 || !strings.Contains(output, "deferred: 0") {
		t.Fatalf("area deferral included a resolved finding: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "review", "t1", "--full"); code != 0 || !strings.Contains(output, "last_followup_resolution") || !strings.Contains(output, reason) || !strings.Contains(output, "obsolete") {
		t.Fatalf("resolution audit missing from review status: %d %s", code, output)
	}
}

func TestReviewFollowupsWorkerReadsAreScoped(t *testing.T) {
	f := reviewFollowupsFixture(t)
	ctx := context.Background()
	workerCommand := func(task store.Task, cwd string, args ...string) (int, string) {
		t.Helper()
		env := setEnv(f.env, "POSSE_WORKER_HOME", f.home)
		env = setEnv(env, "HERDR_PANE_ID", task.PaneID)
		env = setEnv(env, "CLAUDECODE", "1")
		code, output := runReleaseCLI(t, f.binary, cwd, env, args...)
		t.Logf("Worker posse %s: code=%d\n%s", strings.Join(args, " "), code, output)
		return code, output
	}
	deny := func(task store.Task, cwd string, args ...string) {
		t.Helper()
		if code, output := workerCommand(task, cwd, args...); code == 0 || !strings.Contains(output, "worker_forbidden") {
			t.Errorf("expected Worker refusal: %d %s", code, output)
		}
	}
	change, review := f.mustTask(t, "t1"), f.mustTask(t, "t2")
	if code, output := workerCommand(review, review.WorktreePath, "show", "t1"); code != 0 || !strings.Contains(output, change.Title) {
		t.Errorf("reviewer cannot inspect reviewed Change: %d %s", code, output)
	}
	deny(review, review.WorktreePath, "review", "followups")
	deny(change, change.WorktreePath, "show", "t2")
	deny(review, review.WorktreePath, "review", "t1")
	if _, err := os.Stat(filepath.Join(f.home, "projects", "shop", "tasks", "t1", "report.md")); !os.IsNotExist(err) {
		t.Fatalf("Change unexpectedly has a report.md: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.home, "projects", "shop", "tasks", "t2", "report.md")); err != nil {
		t.Fatalf("Review report missing: %v", err)
	}
	const assignedIssue = "https://github.com/acme/shop/issues/302"
	const otherIssue = "https://github.com/acme/shop/issues/303"
	for _, link := range [][2]string{{"F-2", assignedIssue}, {"F-3", otherIssue}} {
		if code, output := runReviewIdentityCLI(t, f, "review", "dispose", "t1", link[0], "defer", "--issue", link[1]); code != 0 {
			t.Fatalf("link %s to a follow-up issue: %d %s", link[0], code, output)
		}
	}
	brief := filepath.Join(f.root, "triage.md")
	const text = "---\ntype: research\ntitle: Verify assigned follow-up\ndone_when: verified\nfollowups: [t1/F-2]\n---\nVerify the finding on main.\n"
	if err := os.WriteFile(brief, []byte(strings.Replace(text, "t1/F-2", "t1/F-99", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runReviewIdentityCLI(t, f, "start", "--brief", brief, "--profile", "reviewer"); code == 0 || !strings.Contains(output, "brief_invalid") {
		t.Fatalf("admitted an unknown finding: %d %s", code, output)
	}
	if err := os.WriteFile(brief, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := runReviewIdentityCLI(t, f, "start", "--brief", brief, "--profile", "reviewer"); code != 0 {
		t.Fatalf("cannot admit a triage assignment: %d %s", code, output)
	}
	triage := f.mustTask(t, "t3")
	mutableBrief := filepath.Join(f.home, "projects", "shop", "tasks", "t3", "brief.md")
	if err := os.WriteFile(mutableBrief, []byte(strings.Replace(text, "t1/F-2", "t1/F-2, t1/F-3", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := workerCommand(triage, triage.WorktreePath, "review", "followups"); code != 0 || !strings.Contains(output, "F-2") || !strings.Contains(output, "unsafe operation") || !strings.Contains(output, assignedIssue) || strings.Contains(output, "F-1") || strings.Contains(output, "F-3") || strings.Contains(output, otherIssue) {
		t.Errorf("triage must read only its assigned finding, with ledger detail: %d %s", code, output)
	}
	deny(triage, triage.WorktreePath, "show", "t1")
	deny(review, review.WorktreePath, "show", "t3")
	deny(triage, triage.WorktreePath, "review", "followups", "resolve", "t1", "F-2", "fixed", "--reason", "Verified")
	deny(triage, triage.WorktreePath, "review", "followups", "defer", "--area", ".", "--issue", "https://github.com/acme/shop/issues/301")
	other := filepath.Join(f.root, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	project, err := f.db.CreateProject(ctx, "other", other, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Unrelated Change", MergeMode: "local"}); err != nil {
		t.Fatal(err)
	}
	deny(triage, other, "review", "followups")
	deny(review, other, "show", "t1")
}
