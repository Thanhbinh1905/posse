package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

const (
	reviewShaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reviewShaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	reportHash = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

type reviewVerdictFixture struct {
	ctx    context.Context
	home   string
	db     *DB
	change int64
	review int64
}

func newReviewVerdictFixture(t *testing.T, targets []ReviewTarget) reviewVerdictFixture {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	project, err := db.CreateProject(ctx, "shop", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	change, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Change", MergeMode: "local", Branch: "posse/change"})
	if err != nil {
		t.Fatal(err)
	}
	review, err := db.CreateTask(ctx, project.ID, Task{Seq: 2, Type: "review", ReviewsTaskID: change, Title: "Review", MergeMode: "local", ReviewTargets: targets})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(ctx, review, "reviewer", "model-b", true); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, review, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	return reviewVerdictFixture{ctx: ctx, home: home, db: db, change: change, review: review}
}

func (f reviewVerdictFixture) task(t *testing.T, id int64) Task {
	t.Helper()
	var projectID int64
	if err := f.db.QueryRowContext(f.ctx, `SELECT project_id FROM tasks WHERE id=?`, id).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	task, err := f.db.TaskByID(f.ctx, projectID, id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (f reviewVerdictFixture) signalCount(t *testing.T, id int64) int {
	t.Helper()
	var count int
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM signals WHERE task_id=?`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestReviewVerdictPersistsWithSignalAndReviewerIdentity(t *testing.T) {
	targets := []ReviewTarget{{Repository: "shop", Commit: reviewShaA}}
	f := newReviewVerdictFixture(t, targets)
	state, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, f.review), "done", "reviewed", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewPass, ReportSHA256: reportHash})
	if err != nil || state != StateReported {
		t.Fatalf("state=%s err=%v", state, err)
	}
	verdict, err := f.db.ReviewVerdict(f.ctx, f.review)
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Verdict != ReviewPass || verdict.ReportSHA256 != reportHash || !SameReviewTargets(verdict.Targets, targets) || verdict.SignalID == 0 {
		t.Fatalf("verdict = %#v", verdict)
	}
	if verdict.Reviewer.Profile != "reviewer" || verdict.Reviewer.ConfiguredModel != "model-b" || !verdict.Reviewer.ModelKnown || verdict.Reviewer.LaunchNumber != 1 {
		t.Fatalf("reviewer identity = %#v", verdict.Reviewer)
	}
}

func TestReviewVerdictRefusalLeavesNoSignalAndKeepsTaskWorking(t *testing.T) {
	f := newReviewVerdictFixture(t, nil)
	_, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, f.review), "done", "reviewed", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewPass, ReportSHA256: reportHash})
	if !errors.Is(err, ErrReviewTargetUnbound) {
		t.Fatalf("unbound review error = %v", err)
	}
	if got := f.task(t, f.review).State; got != StateWorking || f.signalCount(t, f.review) != 0 {
		t.Fatalf("refused verdict left state=%s signals=%d", got, f.signalCount(t, f.review))
	}
	for _, input := range []ReviewVerdictInput{{Verdict: "approve", ReportSHA256: reportHash}, {Verdict: ReviewPass, ReportSHA256: "short"}} {
		if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, f.review), "done", "x", map[string]string{}, "task_done", &input); err == nil {
			t.Fatalf("malformed verdict %#v was recorded", input)
		}
	}
	if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, f.review), "failed", "x", map[string]string{}, "task_failed", &ReviewVerdictInput{Verdict: ReviewPass, ReportSHA256: reportHash}); err == nil {
		t.Fatal("verdict on a non-done Signal was accepted")
	}
	if f.signalCount(t, f.review) != 0 || f.task(t, f.review).State != StateWorking {
		t.Fatal("malformed verdicts changed the Task")
	}
}

func TestReviewVerdictRefusedForNonReviewTasks(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	if err := f.db.Transition(f.ctx, f.change, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	_, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, f.change), "done", "x", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewPass, ReportSHA256: reportHash})
	if !errors.Is(err, ErrReviewVerdictType) {
		t.Fatalf("change verdict error = %v", err)
	}
	if f.task(t, f.change).State != StateWorking || f.signalCount(t, f.change) != 0 {
		t.Fatal("refused change verdict was partially recorded")
	}
	if _, err := f.db.CreateTask(f.ctx, f.task(t, f.change).ProjectID, Task{Seq: 3, Type: "research", Title: "Research", ReviewTargets: []ReviewTarget{{Repository: "shop", Commit: reviewShaA}}}); !errors.Is(err, ErrReviewVerdictType) {
		t.Fatalf("research with review targets error = %v", err)
	}
}

func TestReviewVerdictDuplicateCompletionKeepsFirstVerdict(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	task := f.task(t, f.review)
	if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, task, "done", "first", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewChangesRequested, ReportSHA256: reportHash}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, task, "done", "second", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewPass, ReportSHA256: reportHash}); err == nil {
		t.Fatal("duplicate completion was recorded")
	}
	verdict, err := f.db.ReviewVerdict(f.ctx, f.review)
	if err != nil || verdict.Verdict != ReviewChangesRequested || f.signalCount(t, f.review) != 1 {
		t.Fatalf("verdict=%#v signals=%d err=%v", verdict, f.signalCount(t, f.review), err)
	}
	if _, err := f.db.ExecContext(f.ctx, `INSERT INTO review_verdicts(task_id,signal_id,verdict,report_sha256,targets_json,launch_number,profile_name,configured_model,model_known,created_at) VALUES(?,1,'pass',?,'[]',1,'p','m',1,0)`, f.review, reportHash); err == nil {
		t.Fatal("second verdict row was accepted")
	}
}

func TestReviewTargetsAndVerdictsAreImmutable(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, f.review), "done", "x", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewPass, ReportSHA256: reportHash}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.ctx, `UPDATE review_targets SET commit_sha=? WHERE task_id=?`, reviewShaB, f.review); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("target rebind error = %v", err)
	}
	if _, err := f.db.ExecContext(f.ctx, `UPDATE review_verdicts SET verdict='pass', targets_json='[]' WHERE task_id=?`, f.review); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("verdict rewrite error = %v", err)
	}
}

func TestCurrentReviewPassRequiresPassOnCurrentCandidate(t *testing.T) {
	old := []ReviewTarget{{Repository: "shop", Commit: reviewShaA}}
	moved := []ReviewTarget{{Repository: "shop", Commit: reviewShaB}}
	for _, tc := range []struct {
		verdict   string
		candidate []ReviewTarget
		want      bool
	}{
		{ReviewPass, old, true},
		{ReviewPass, moved, false},
		{ReviewChangesRequested, old, false},
		{ReviewInconclusive, old, false},
	} {
		t.Run(tc.verdict+"/"+tc.candidate[0].Commit[:1], func(t *testing.T) {
			f := newReviewVerdictFixture(t, old)
			if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, f.review), "done", "x", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: tc.verdict, ReportSHA256: reportHash}); err != nil {
				t.Fatal(err)
			}
			_, got, err := f.db.CurrentReviewPass(f.ctx, f.change, tc.candidate, nil)
			if err != nil || got != tc.want {
				t.Fatalf("current pass = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	f := newReviewVerdictFixture(t, old)
	if _, _, err := f.db.CurrentReviewPass(f.ctx, f.change, old, nil); err != nil {
		t.Fatal(err)
	}
	if _, got, _ := f.db.CurrentReviewPass(f.ctx, f.change, old, nil); got {
		t.Fatal("a Review Task without a verdict qualified")
	}
}

func TestReviewTargetsAndVerdictSurviveRebuild(t *testing.T) {
	targets := []ReviewTarget{{Repository: "shop", Commit: reviewShaA}}
	f := newReviewVerdictFixture(t, targets)
	if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, f.review), "done", "x", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewPass, ReportSHA256: reportHash}); err != nil {
		t.Fatal(err)
	}
	before, err := f.db.ReviewVerdict(f.ctx, f.review)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := f.db.RebuildFromSnapshots(f.ctx, f.home); err != nil || count != 2 {
		t.Fatalf("rebuild = %d, %v", count, err)
	}
	after, err := f.db.ReviewVerdict(f.ctx, f.review)
	if err != nil {
		t.Fatal(err)
	}
	after.SignalID = before.SignalID
	if after.Verdict != before.Verdict || after.ReportSHA256 != before.ReportSHA256 || after.Reviewer != before.Reviewer || !SameReviewTargets(after.Targets, targets) {
		t.Fatalf("rebuilt verdict = %#v, want %#v", after, before)
	}
	rebuilt, err := f.db.ReviewTargets(f.ctx, f.review)
	if err != nil || !SameReviewTargets(rebuilt, targets) {
		t.Fatalf("rebuilt targets = %#v, %v", rebuilt, err)
	}
	if _, got, err := f.db.CurrentReviewPass(f.ctx, f.change, targets, nil); err != nil || !got {
		t.Fatalf("rebuilt pass current = %v, %v", got, err)
	}
}

func TestReviewEvidenceUsesOnePassForEveryWorkspaceMember(t *testing.T) {
	reviewed := []ReviewTarget{{Repository: "api", Commit: reviewShaA}, {Repository: "web", Commit: reviewShaB}}
	f := newReviewVerdictFixture(t, reviewed)
	if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, f.review), "done", "x", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewPass, ReportSHA256: reportHash}); err != nil {
		t.Fatal(err)
	}
	// A second round passed a different combination of the same Members.
	change := f.task(t, f.change)
	other, err := f.db.CreateTask(f.ctx, change.ProjectID, Task{Seq: 3, Type: "review", ReviewsTaskID: f.change, Title: "Second review", MergeMode: "local",
		ReviewTargets: []ReviewTarget{{Repository: "api", Commit: reviewShaB}, {Repository: "web", Commit: reviewShaA}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.NextTaskLaunchWithIdentity(f.ctx, other, "reviewer", "model-b", true); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(f.ctx, other, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, other), "done", "x", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewPass, ReportSHA256: reportHash}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		targets []ReviewTarget
		want    bool
	}{
		{"exact Member set", reviewed, true},
		{"Member subset of one pass", reviewed[:1], true},
		{"heads from different passes", []ReviewTarget{reviewed[0], {Repository: "web", Commit: reviewShaA}}, false},
		{"unreviewed Member", []ReviewTarget{{Repository: "docs", Commit: reviewShaA}}, false},
	} {
		evidence, err := f.db.ReviewEvidenceFor(f.ctx, f.change, tc.targets, nil)
		if err != nil || evidence.Complete != tc.want || evidence.Pass != tc.want {
			t.Fatalf("%s: evidence=%+v err=%v, want complete=%v", tc.name, evidence, err, tc.want)
		}
	}
}
