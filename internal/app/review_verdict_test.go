package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type reviewVerdictFixture struct {
	*prMergeFixture
	review     store.Task
	reviewTree string
}

// newReviewVerdictFixture builds a completed Change Task (t1, author profile
// "author") and a working Review Task (t2, profile "reviewer") bound to it.
func newReviewVerdictFixture(t *testing.T) *reviewVerdictFixture {
	t.Helper()
	ctx := context.Background()
	base := newPRMergeFixture(t, "local", store.StateDone)
	if _, err := base.db.NextTaskLaunchWithIdentity(ctx, base.task.ID, "author", "model-a", true); err != nil {
		t.Fatal(err)
	}
	change, err := base.db.TaskByID(ctx, base.project.ID, base.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	base.task = change
	targets, err := changeCandidateTargets(ctx, base.db, base.project, change)
	if err != nil {
		t.Fatal(err)
	}
	reviewTree := filepath.Join(base.root, "review-sandbox")
	gitTest(t, base.repo, "worktree", "add", "-b", "posse/t2", reviewTree, "refs/heads/posse/t1")
	reviewID, err := base.db.CreateTask(ctx, base.project.ID, store.Task{Seq: 2, Type: "review", ReviewsTaskID: change.ID, Title: "Review t1", State: store.StateSpawning, MergeMode: "local", Branch: "posse/t2", BaseRef: "posse/t1", WorktreePath: reviewTree, HerdrWorkspaceID: "w3", PaneID: "w3:p1", PaneLabel: "posse:shop:t2", ReviewTargets: targets})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.db.NextTaskLaunchWithIdentity(ctx, reviewID, "reviewer", "model-b", true); err != nil {
		t.Fatal(err)
	}
	if err := base.db.Transition(ctx, reviewID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base.home, "projects", "shop", "tasks", "t2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reviewTree, "report.md"), []byte("Findings\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := base.service.Herdr.(*herdr.Fake)
	fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{PaneID: "w3:p1", WorkspaceID: "w3", Label: "posse:shop:t2", CWD: reviewTree, Agent: "claude", AgentStatus: "idle"})
	review, err := base.db.TaskByID(ctx, base.project.ID, reviewID)
	if err != nil {
		t.Fatal(err)
	}
	return &reviewVerdictFixture{prMergeFixture: base, review: review, reviewTree: reviewTree}
}

func (f *reviewVerdictFixture) signal(t *testing.T, pane string, args ...string) (int, string) {
	t.Helper()
	for key, value := range map[string]string{"HERDR_PANE_ID": pane, workerHomeEnv: f.home} {
		previous, present := os.LookupEnv(key)
		_ = os.Setenv(key, value)
		defer func(key, previous string, present bool) {
			if present {
				_ = os.Setenv(key, previous)
			} else {
				_ = os.Unsetenv(key)
			}
		}(key, previous, present)
	}
	tree := map[string]string{f.review.PaneID: f.reviewTree, "w4:p1": filepath.Join(f.root, "legacy-sandbox")}[pane]
	args = append([]string(nil), args...)
	for i, arg := range args {
		if arg == "report.md" && tree != "" {
			args[i] = filepath.Join(tree, arg)
		}
	}
	code, out, errOut := f.run(append([]string{"signal"}, args...)...)
	return code, out + errOut
}

func (f *reviewVerdictFixture) pass(t *testing.T) (store.ReviewVerdict, bool) {
	t.Helper()
	change, err := f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	verdict, ok, err := currentReviewPass(context.Background(), f.db, f.project, change)
	if err != nil {
		t.Fatal(err)
	}
	return verdict, ok
}

func (f *reviewVerdictFixture) commitToChange(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.worktree, "more.txt"), []byte("more\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.worktree, "add", "more.txt")
	gitTest(t, f.worktree, "commit", "-m", "later change")
}

func TestQuickReviewCarryTreatsUnavailableRemoteAsNoCarry(t *testing.T) {
	for _, remote := range []string{"missing", "unreachable"} {
		t.Run(remote, func(t *testing.T) {
			fixture := newPRMergeFixture(t, "local", store.StateDone)
			installGateValidationPlanForReview(t, fixture, 1, "quick")
			ctx := context.Background()
			change, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			fixture.task = change
			if remote == "missing" {
				gitTest(t, fixture.worktree, "remote", "remove", "origin")
			} else {
				gitTest(t, fixture.worktree, "remote", "set-url", "origin", filepath.Join(fixture.root, "missing-origin.git"))
			}
			head := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
			err = ensureReviewCarry(ctx, fixture.db, fixture.project, change, []store.ReviewTarget{{Repository: fixture.project.Name, Commit: head}})
			if err != nil {
				t.Fatalf("unavailable remote prevented local review handling: %v", err)
			}
			var carries int
			if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_carries WHERE ship_task_id=?`, change.ID).Scan(&carries); err != nil || carries != 0 {
				t.Fatalf("unavailable remote created %d carry rows, err=%v", carries, err)
			}
		})
	}
}

func TestReviewVerdictRecordedFromPosseRecords(t *testing.T) {
	f := newReviewVerdictFixture(t)
	code, out := f.signal(t, f.review.PaneID, "done", "Looks right", "--report", "report.md", "--verdict", "pass")
	if code != 0 {
		t.Fatalf("signal exit=%d %s", code, out)
	}
	verdict, ok := f.pass(t)
	if !ok {
		t.Fatal("pass verdict does not qualify the current candidate")
	}
	wantSHA := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "refs/heads/posse/t1"))
	digest, err := fileSHA256(filepath.Join(f.home, "projects", "shop", "tasks", "t2", "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(verdict.Targets) != 1 || verdict.Targets[0].Commit != wantSHA || verdict.ReportSHA256 != digest || verdict.Reviewer.Profile != "reviewer" || verdict.Reviewer.ConfiguredModel != "model-b" {
		t.Fatalf("verdict = %#v, want target %s digest %s", verdict, wantSHA, digest)
	}
	code, out, _ = f.run("show", "t2")
	if code != 0 || !strings.Contains(out, "review_verdict") || !strings.Contains(out, "current") {
		t.Fatalf("show omits verdict: %d %s", code, out)
	}
}

func TestReviewVerdictRefusedForWrongWorkerAndMalformedSignals(t *testing.T) {
	f := newReviewVerdictFixture(t)
	if err := f.db.Transition(context.Background(), f.task.ID, store.StateDone, store.StateWorking, "lead", "follow-up"); err != nil {
		t.Fatal(err)
	}
	// A Change Worker cannot record a verdict, whatever it names.
	if code, out := f.signal(t, f.task.PaneID, "done", "x", "--verdict", "pass"); code == 0 || !strings.Contains(out, "signal_invalid") {
		t.Fatalf("change Worker verdict: %d %s", code, out)
	}
	for _, args := range [][]string{
		{"done", "x", "--report", "report.md", "--verdict", "approved"},
		{"working", "x", "--verdict", "pass"},
		{"done", "x", "--report", "report.md", "--verdict"},
	} {
		if code, out := f.signal(t, f.review.PaneID, args...); code == 0 {
			t.Fatalf("malformed %v accepted: %s", args, out)
		}
	}
	// A Review Worker that omits the verdict records the Report but never qualifies.
	if code, out := f.signal(t, f.review.PaneID, "done", "x", "--report", "report.md"); code != 0 {
		t.Fatalf("done without verdict: %d %s", code, out)
	}
	if _, ok := f.pass(t); ok {
		t.Fatal("missing verdict qualified")
	}
	if _, err := f.db.ReviewVerdict(context.Background(), f.review.ID); err == nil {
		t.Fatal("missing verdict recorded a row")
	}
	change, err := f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil || change.State != store.StateWorking {
		t.Fatalf("change state after refusals = %s, %v", change.State, err)
	}
}

func TestReviewVerdictRefusedWithoutBoundTargetOrForOtherRevision(t *testing.T) {
	f := newReviewVerdictFixture(t)
	ctx := context.Background()
	// A Review created before targets existed cannot record a verdict.
	legacyTree := filepath.Join(f.root, "legacy-sandbox")
	gitTest(t, f.repo, "worktree", "add", "-b", "posse/t3", legacyTree, "refs/heads/posse/t1")
	legacyID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{Seq: 3, Type: "review", ReviewsTaskID: f.task.ID, Title: "Legacy", MergeMode: "local", Branch: "posse/t3", WorktreePath: legacyTree, HerdrWorkspaceID: "w4", PaneID: "w4:p1", PaneLabel: "posse:shop:t3"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, legacyID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	fake := f.service.Herdr.(*herdr.Fake)
	fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{PaneID: "w4:p1", WorkspaceID: "w4", Label: "posse:shop:t3", CWD: legacyTree, Agent: "claude", AgentStatus: "idle"})
	if err := os.WriteFile(filepath.Join(legacyTree, "report.md"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := f.signal(t, "w4:p1", "done", "x", "--report", "report.md", "--verdict", "pass"); code == 0 || !strings.Contains(out, "review_target_unbound") {
		t.Fatalf("legacy review verdict: %d %s", code, out)
	}
	// A Reviewer whose worktree never held the bound commit cannot record it.
	f.commitToChange(t)
	moved, err := changeCandidateTargets(ctx, f.db, f.project, f.task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `DELETE FROM review_targets WHERE task_id=?`, f.review.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `INSERT INTO review_targets(task_id,repository,commit_sha) VALUES(?,?,?)`, f.review.ID, moved[0].Repository, moved[0].Commit); err != nil {
		t.Fatal(err)
	}
	if code, out := f.signal(t, f.review.PaneID, "done", "x", "--report", "report.md", "--verdict", "pass"); code == 0 || !strings.Contains(out, "review_target_missing") {
		t.Fatalf("unseen revision verdict: %d %s", code, out)
	}
}

func TestReviewVerdictOnlyPassSatisfiesMergeAndMovementInvalidatesIt(t *testing.T) {
	for _, tc := range []struct {
		verdict string
		merge   bool
	}{{"pass", true}, {"changes_requested", false}, {"inconclusive", false}} {
		t.Run(tc.verdict, func(t *testing.T) {
			f := newReviewVerdictFixture(t)
			if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("[defaults]\nmerge_mode = \"local\"\nreview = \"quick\"\nauto_teardown = \"never\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if code, out, _ := f.run("merge", "t1"); code == 0 || !strings.Contains(out, "review_required") {
				t.Fatalf("merge without review: %d %s", code, out)
			}
			if code, out := f.signal(t, f.review.PaneID, "done", "x", "--report", "report.md", "--verdict", tc.verdict); code != 0 {
				t.Fatalf("signal: %d %s", code, out)
			}
			code, out, _ := f.run("merge", "t1")
			if got := !strings.Contains(out, "review_required"); got != tc.merge {
				t.Fatalf("merge satisfied by %s = %v: %d %s", tc.verdict, got, code, out)
			}
			if tc.verdict != "pass" {
				return
			}
			// The candidate moves: the old pass has no current standing and is not rebound.
			if err := f.db.Transition(context.Background(), f.task.ID, store.StateMerging, store.StateDone, "cli", "test"); err != nil && !strings.Contains(err.Error(), "state") {
				t.Fatal(err)
			}
			f.commitToChange(t)
			if _, ok := f.pass(t); ok {
				t.Fatal("old pass is current for a moved candidate")
			}
			if code, out, _ := f.run("merge", "t1"); code == 0 || !strings.Contains(out, "review_required") {
				t.Fatalf("merge after movement: %d %s", code, out)
			}
			targets, err := f.db.ReviewTargets(context.Background(), f.review.ID)
			if err != nil || len(targets) != 1 || targets[0].Commit == strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "refs/heads/posse/t1")) {
				t.Fatalf("old Review targets were rebound: %#v %v", targets, err)
			}
		})
	}
}

func TestReviewVerdictDuplicateCompletionAndTeardownRebuild(t *testing.T) {
	f := newReviewVerdictFixture(t)
	ctx := context.Background()
	if code, out := f.signal(t, f.review.PaneID, "done", "first", "--report", "report.md", "--verdict", "pass"); code != 0 {
		t.Fatalf("signal: %d %s", code, out)
	}
	if code, out := f.signal(t, f.review.PaneID, "done", "again", "--report", "report.md", "--verdict", "changes_requested"); code == 0 || !strings.Contains(out, "signal_state_invalid") {
		t.Fatalf("duplicate completion: %d %s", code, out)
	}
	if verdict, err := f.db.ReviewVerdict(ctx, f.review.ID); err != nil || verdict.Verdict != "pass" {
		t.Fatalf("duplicate replaced verdict: %#v %v", verdict, err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET state='torn-down' WHERE id=?`, f.review.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.pass(t); !ok {
		t.Fatal("Teardown removed the current pass")
	}
	if count, err := f.db.RebuildFromSnapshots(ctx, f.home); err != nil || count != 2 {
		t.Fatalf("rebuild = %d, %v", count, err)
	}
	if _, ok := f.pass(t); !ok {
		t.Fatal("rebuild lost the current pass")
	}
}

func TestReviewAdmissionRefusesIncompleteChangeAndPinsCompletedRevision(t *testing.T) {
	fixture := newStartFixture(t)
	configureReviewIdentityFixture(t, fixture, "[profiles.reviewer]\nkind = \"pi\"\nmodel = \"model-b\"\n")
	seedReviewAuthor(t, fixture)
	brief := filepath.Join(filepath.Dir(fixture.brief), "review-brief.md")
	if err := os.WriteFile(brief, []byte("---\ntype: review\ntitle: Review launch identity\ndone_when: the author change is independently reviewed\nreview_of: t1\n---\nReview the candidate.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func() (int, string) {
		var output strings.Builder
		cli := fixture.service.CLI()
		cli.Out, cli.ErrOut = &output, &output
		code := cli.Run([]string{"start", "--brief", brief, "--name", "review-launch-identity", "--profile", "reviewer"})
		return code, output.String()
	}
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), `UPDATE tasks SET state='working' WHERE seq=1`); err != nil {
		t.Fatal(err)
	}
	if code, out := run(); code == 0 || !strings.Contains(out, "review_target_incomplete") {
		t.Fatalf("review of a working Change: %d %s", code, out)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE tasks SET state='done' WHERE seq=1`); err != nil {
		t.Fatal(err)
	}
	if code, out := run(); code != 0 {
		t.Fatalf("review of a done Change: %d %s", code, out)
	}
	wantSHA := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/heads/posse/author"))
	var gotSHA string
	if err := db.QueryRowContext(context.Background(), `SELECT commit_sha FROM review_targets WHERE task_id=(SELECT id FROM tasks WHERE seq=2)`).Scan(&gotSHA); err != nil || gotSHA != wantSHA {
		t.Fatalf("pinned target = %q, %v; want %s", gotSHA, err, wantSHA)
	}
}
