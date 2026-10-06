package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// Every observed merged PR must finish Teardown or retain exactly one
// deduplicated, visible reason. A terminal Rider state never drops the watch.
func TestMergedPRNeverSilentlySkipsTask(t *testing.T) {
	states := []store.State{store.StateWorking, store.StateNeedsDecision, store.StateBlocked, store.StateStalled, store.StateFailed, store.StateLost, store.StateDone, store.StateLanding}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			f := newPRLandingFixture(t, "pr", store.StateWorking)
			defer f.db.Close()
			ctx := context.Background()
			if state == store.StateLanding {
				if err := f.db.Transition(ctx, f.task.ID, store.StateWorking, store.StateDone, "worker", "done"); err != nil {
					t.Fatal(err)
				}
				if err := f.db.Transition(ctx, f.task.ID, store.StateDone, store.StateLanding, "cli", "landing"); err != nil {
					t.Fatal(err)
				}
			} else if state != store.StateWorking {
				source := "cli"
				if state == store.StateDone || state == store.StateFailed || state == store.StateNeedsDecision {
					source = "worker"
				}
				if state == store.StateBlocked {
					source = "herdr"
				}
				if err := f.db.Transition(ctx, f.task.ID, store.StateWorking, state, source, "test"); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.db.UpdateTaskLanding(ctx, f.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
				t.Fatal(err)
			}
			f.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", f.headSHA, f.headSHA)
			cfg, err := config.Load(f.home, f.project.Name)
			if err != nil {
				t.Fatal(err)
			}
			for tick := 0; tick < 2; tick++ {
				if err := f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, true); err != nil {
					t.Fatal(err)
				}
				if err := f.service.autoTeardownLandedTasks(ctx, f.db, f.project, cfg); err != nil {
					t.Fatal(err)
				}
			}
			task, err := f.db.Task(ctx, f.project.ID, "t1")
			if err != nil {
				t.Fatal(err)
			}
			notices, err := f.db.Notices(ctx, f.project.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			reasons := countNoticeKind(notices, "unsaddle_incomplete") + countNoticeKind(notices, "pr_watch_failing")
			if task.State != store.StateTornDown && (task.State != store.StateLanded || reasons != 1) {
				t.Fatalf("merged PR silently skipped: state=%s reasons=%d notices=%#v", task.State, reasons, notices)
			}
			if countNoticeKind(notices, "pr_merged") != 1 {
				t.Fatalf("merged Notice count = %d", countNoticeKind(notices, "pr_merged"))
			}
		})
	}
}

func TestPRWatchRecordsChecksReviewConflictAndMergeTransitions(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	poll := func() {
		t.Helper()
		if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}

	fixture.setGraphQLState(t, "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
	poll()
	notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "pr_checks_failed") != 1 {
		t.Fatalf("failed checks Notice count=%d notices=%#v err=%v", countNoticeKind(notices, "pr_checks_failed"), notices, err)
	}
	observation, err := fixture.db.LatestPRObservation(ctx, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	checks := decodeChecks(observation.Checks)
	if observation.HeadSHA != fixture.headSHA || checks.State != "FAILURE" || len(checks.Failures) != 1 || checks.Failures[0].Name != "unit" || checks.Failures[0].URL != "https://checks.example/unit" {
		t.Fatalf("failed check snapshot lost its name or URL: %#v %#v", observation, checks)
	}

	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	poll()
	fixture.setGraphQLState(t, "OPEN", "PENDING", "CHANGES_REQUESTED", "CONFLICTING", "", strings.Repeat("2", 40))
	poll()
	notices, err = fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "land_ready") != 1 || countNoticeKind(notices, "pr_changes_requested") != 1 || countNoticeKind(notices, "pr_conflict") != 1 {
		t.Fatalf("review and conflict notices were not recorded once: %#v, %v", notices, err)
	}
	observation, err = fixture.db.LatestPRObservation(ctx, fixture.task.ID)
	if err != nil || observation.HeadSHA != strings.Repeat("2", 40) || decodeChecks(observation.Checks).State != "PENDING" {
		t.Fatalf("new head did not reset check status: %#v, %v", observation, err)
	}

	fixture.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", fixture.headSHA, fixture.headSHA)
	poll()
	task, err := fixture.db.Task(ctx, fixture.project.ID, "t1")
	if err != nil || task.State != store.StateLanded || task.LandedRef != fixture.headSHA {
		t.Fatalf("merged PR did not land at the GitHub merge commit: %#v, %v", task, err)
	}
	notices, err = fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "pr_merged") != 1 {
		t.Fatalf("merged Notice count=%d notices=%#v err=%v", countNoticeKind(notices, "pr_merged"), notices, err)
	}
	var rows int
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pr_observations WHERE task_id=?`, task.ID).Scan(&rows); err != nil || rows != 4 {
		t.Fatalf("observation history has %d rows, want four distinct changes: %v", rows, err)
	}
}

func TestPRWatchLandsDoneTaskWhenItsPRIsMerged(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte("[defaults]\nlanding_mode = \"pr\"\nmerge_method = \"squash\"\nauto_unsaddle = \"finished\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mount := attachPRFixtureMount(t, fixture)
	prURL := "https://github.com/acme/shop/pull/17"
	if err := fixture.db.UpdateTaskLanding(ctx, fixture.task.ID, prURL, ""); err != nil {
		t.Fatal(err)
	}
	mergeCommit := strings.Repeat("9", 40)
	fixture.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", mergeCommit, fixture.headSHA)
	fake, ok := fixture.service.Herdr.(*herdr.Fake)
	if !ok {
		t.Fatal("fixture does not use the fake Herdr adapter")
	}
	fake.SnapshotValue.Agents = []herdr.Agent{{Name: "posse-shop-t1-1", PaneID: "w2:p1"}}
	fixture.service.Herdr = &changingSnapshotAdapter{Fake: fake, snapshot: fake.SnapshotValue}

	if _, err := fixture.service.prepareProject(ctx, fixture.db, fixture.project); err != nil {
		t.Fatalf("reconcile merged PR: %v", err)
	}
	task, err := fixture.db.Task(ctx, fixture.project.ID, "t1")
	if err != nil || task.State != store.StateTornDown || task.LandedRef != mergeCommit {
		t.Fatalf("merged PR did not land and follow auto_unsaddle=finished: %#v, %v", task, err)
	}
	releasedMount, err := fixture.db.MountByTask(ctx, task.ID)
	if err == nil {
		t.Fatalf("auto-teardown left Mount held: %#v", releasedMount)
	}
	mounts, err := fixture.db.Mounts(ctx, fixture.project.ID)
	if err != nil || len(mounts) != 1 || mounts[0].ID != mount.ID || mounts[0].State != "idle" || mounts[0].TaskID != 0 {
		t.Fatalf("safe merged Task Mount was not released: %#v, %v", mounts, err)
	}
	observation, err := fixture.db.LatestPRObservation(ctx, task.ID)
	if err != nil || observation.State != "MERGED" || observation.MergeCommit != mergeCommit {
		t.Fatalf("merge observation did not record the merge commit: %#v, %v", observation, err)
	}
	notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "pr_merged") != 1 {
		t.Fatalf("merged Notice count=%d notices=%#v err=%v", countNoticeKind(notices, "pr_merged"), notices, err)
	}
	transitions, err := fixture.db.TaskTransitions(ctx, task.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	foundLanded := false
	for _, transition := range transitions {
		if transition.From == string(store.StateDone) && transition.To == string(store.StateLanded) {
			foundLanded = true
			break
		}
	}
	if !foundLanded {
		t.Fatalf("done Task did not transition through landed: %#v", transitions)
	}
}

func TestPRWatchPreservesMergedDoneTaskWithUnsafeWorktree(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*testing.T, *prLandingFixture)
	}{
		{
			name: "dirty worktree",
			edit: func(t *testing.T, fixture *prLandingFixture) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(fixture.worktree, "late-change.txt"), []byte("preserve me\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "branch moved past merged head",
			edit: func(t *testing.T, fixture *prLandingFixture) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(fixture.worktree, "follow-up.txt"), []byte("unreviewed\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				gitTest(t, fixture.worktree, "add", "follow-up.txt")
				gitTest(t, fixture.worktree, "commit", "-m", "unreviewed follow-up")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPRLandingFixture(t, "pr", store.StateDone)
			mount := attachPRFixtureMount(t, fixture)
			if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(`[defaults]
landing_mode = "pr"
merge_method = "squash"
auto_unsaddle = "finished"
`), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := fixture.db.UpdateTaskLanding(ctx, fixture.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
				t.Fatal(err)
			}
			if err := fixture.db.SetTaskGatedSHA(ctx, fixture.task.ID, fixture.headSHA); err != nil {
				t.Fatal(err)
			}
			tc.edit(t, fixture)
			fixture.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", fixture.headSHA, fixture.headSHA)
			fake, ok := fixture.service.Herdr.(*herdr.Fake)
			if !ok {
				t.Fatal("fixture does not use the fake Herdr adapter")
			}
			fake.SnapshotValue.Agents = []herdr.Agent{{Name: "posse-shop-t1-1", PaneID: "w2:p1"}}
			adapter := &changingSnapshotAdapter{Fake: fake, snapshot: fake.SnapshotValue}
			fixture.service.Herdr = adapter

			if _, err := fixture.service.prepareProject(ctx, fixture.db, fixture.project); err != nil {
				t.Fatalf("reconcile merged PR: %v", err)
			}
			task, err := fixture.db.Task(ctx, fixture.project.ID, "t1")
			if err != nil || task.State != store.StateTornDown {
				t.Fatalf("merged Task was not torn down: %#v, %v", task, err)
			}
			if containsPane(adapter.currentSnapshot(), "w2:p1") {
				t.Fatal("merged Rider pane remained open")
			}
			if held, err := fixture.db.MountByTask(ctx, task.ID); err == nil {
				t.Fatalf("Mount %d remained held: %#v", mount.ID, held)
			}
			leftover := "refs/heads/posse/t1-leftover"
			if tc.name == "dirty worktree" {
				if content := gitTest(t, fixture.repo, "show", leftover+":late-change.txt"); content != "preserve me\n" {
					t.Fatalf("Leftover omitted untracked work: %q", content)
				}
			} else if commit := gitTest(t, fixture.repo, "log", "-1", "--format=%s", leftover); !strings.Contains(commit, "unreviewed follow-up") {
				t.Fatalf("Leftover omitted committed work: %q", commit)
			}
			decisions, err := fixture.db.Decisions(ctx, fixture.project.ID, true)
			if err != nil || len(decisions) != 1 || decisions[0].Kind != "leftover" {
				t.Fatalf("Leftover Decision: %#v %v", decisions, err)
			}
		})
	}
}

func TestPlainUnsaddleRefusesLandedPRWithLateWork(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateDone)
	if code, out, stderr := f.run("land", "t1"); code != 0 {
		t.Fatalf("land: %d %s %s", code, out, stderr)
	}
	ctx := context.Background()
	landed, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	f.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", f.headSHA, landed.GatedSHA)
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(f.worktree, "uncommitted-follow-up.txt")
	if err := os.WriteFile(work, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := f.run("unsaddle", "t1"); code == 0 {
		t.Fatal("Teardown succeeded without verifying the pane closed")
	}
	if content, err := os.ReadFile(work); err != nil || string(content) != "keep\n" {
		t.Fatalf("work was discarded before pane closure: %q %v", content, err)
	}
}

func TestPRMergePreservesWorkWrittenDuringPaneClosure(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateDone)
	mount := attachPRFixtureMount(t, f)
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("[defaults]\nlanding_mode = \"pr\"\nauto_unsaddle = \"finished\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := f.db.UpdateTaskLanding(ctx, f.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	f.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", f.headSHA, f.headSHA)
	fake := f.service.Herdr.(*herdr.Fake)
	fake.SnapshotValue.Agents = []herdr.Agent{{Name: "posse-shop-t1-1", PaneID: "w2:p1"}}
	work := filepath.Join(f.worktree, "late-rider-work.txt")
	adapter := &changingSnapshotAdapter{Fake: fake, snapshot: fake.SnapshotValue, afterClose: func() {
		if err := os.WriteFile(work, []byte("preserve me\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}}
	f.service.Herdr = adapter
	if _, err := f.service.prepareProject(ctx, f.db, f.project); err != nil {
		t.Fatal(err)
	}
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil || task.State != store.StateTornDown {
		t.Fatalf("Task after late Rider write: %#v %v", task, err)
	}
	if content := gitTest(t, f.repo, "show", "refs/heads/posse/t1-leftover:late-rider-work.txt"); content != "preserve me\n" {
		t.Fatalf("late work missing from snapshot: %q", content)
	}
	if held, err := f.db.MountByTask(ctx, task.ID); err == nil {
		t.Fatalf("Mount %d was not released: %#v", mount.ID, held)
	}
	_ = work
}

func TestPRWatchDoesNotTeardownWorkingTaskWithMergedPR(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	mount := attachPRFixtureMount(t, fixture)
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(`[defaults]
landing_mode = "pr"
merge_method = "squash"
auto_unsaddle = "finished"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := fixture.db.UpdateTaskLanding(ctx, fixture.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.SetTaskGatedSHA(ctx, fixture.task.ID, fixture.headSHA); err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", fixture.headSHA, fixture.headSHA)

	if _, err := fixture.service.prepareProject(ctx, fixture.db, fixture.project); err != nil {
		t.Fatalf("reconcile working Task: %v", err)
	}
	task, err := fixture.db.Task(ctx, fixture.project.ID, "t1")
	if err != nil || task.State != store.StateLanded {
		t.Fatalf("merged PR did not Land working Task: %#v, %v", task, err)
	}
	fake, ok := fixture.service.Herdr.(*herdr.Fake)
	if !ok || !containsPane(fake.SnapshotValue, "w2:p1") {
		t.Fatal("working Task Worker pane was closed")
	}
	preservedMount, err := fixture.db.MountByTask(ctx, task.ID)
	if err != nil || preservedMount.ID != mount.ID || preservedMount.State != "held" {
		t.Fatalf("working Task Mount was not preserved: %#v, %v", preservedMount, err)
	}
}

func TestPRWatchActiveTaskRequiresVerifiedMergedHeadAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, state, source, branch, base, head string
		verified, land                          bool
	}{
		{name: "verified working", state: "MERGED", verified: true, land: true},
		{name: "verified needs decision", state: "MERGED", verified: true, land: true},
		{name: "open PR", state: "OPEN", verified: true},
		{name: "unverified head", state: "MERGED"},
		{name: "different head", state: "MERGED", verified: true, head: strings.Repeat("f", 40)},
		{name: "wrong source", state: "MERGED", verified: true, source: "other/shop"},
		{name: "wrong branch", state: "MERGED", verified: true, branch: "posse/other"},
		{name: "wrong base", state: "MERGED", verified: true, base: "develop"},
		{name: "wrong recorded URL", state: "MERGED", verified: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPRLandingFixture(t, "pr", store.StateWorking)
			ctx := context.Background()
			url := "https://github.com/acme/shop/pull/17"
			if err := fixture.db.UpdateTaskLanding(ctx, fixture.task.ID, url, ""); err != nil {
				t.Fatal(err)
			}
			if tc.name == "verified needs decision" {
				if err := fixture.db.Transition(ctx, fixture.task.ID, store.StateWorking, store.StateNeedsDecision, "worker", "waiting"); err != nil {
					t.Fatal(err)
				}
			}
			if tc.verified {
				verifiedURL := url
				if tc.name == "wrong recorded URL" {
					verifiedURL = "https://github.com/acme/shop/pull/18"
				}
				if err := fixture.db.RecordVerifiedPRHead(ctx, fixture.task.ID, verifiedURL, fixture.headSHA); err != nil {
					t.Fatal(err)
				}
			}
			if tc.source != "" {
				t.Setenv("POSSE_TEST_GH_SOURCE", tc.source)
			}
			if tc.branch != "" {
				t.Setenv("POSSE_TEST_GH_HEAD_BRANCH", tc.branch)
			}
			if tc.base != "" {
				t.Setenv("POSSE_TEST_GH_BASE_BRANCH", tc.base)
			}
			if tc.head != "" {
				fixture.setGHHead(t, tc.head)
			}
			t.Setenv("POSSE_TEST_GH_VIEW_STATE", tc.state)
			fixture.setGraphQLState(t, tc.state, "SUCCESS", "APPROVED", "MERGEABLE", fixture.headSHA, map[bool]string{true: tc.head, false: fixture.headSHA}[tc.head != ""])
			cfg, err := config.Load(fixture.home, fixture.project.Name)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
					t.Fatal(err)
				}
			}
			task, err := fixture.db.Task(ctx, fixture.project.ID, "t1")
			if err != nil {
				t.Fatal(err)
			}
			want := store.StateWorking
			if tc.name == "verified needs decision" {
				want = store.StateNeedsDecision
			}
			if tc.land || tc.name == "unverified head" || tc.name == "wrong recorded URL" {
				// The observed head is on the Mount's Task branch, even if no
				// publish record was retained.
				want = store.StateLanded
			}
			if task.State != want || (want != store.StateLanded && task.LandedRef != "") {
				t.Fatalf("state=%s ref=%s, want=%s", task.State, task.LandedRef, want)
			}
			notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			wantNotices := 0
			if want == store.StateLanded {
				wantNotices = 1
			}
			if got := countNoticeKind(notices, "pr_merged"); got != wantNotices {
				t.Fatalf("merge Notices=%d want=%d", got, wantNotices)
			}
		})
	}
}

func TestPRObservationRejectsStaleTaskIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *prLandingFixture)
	}{
		{"new PR", func(t *testing.T, f *prLandingFixture) {
			t.Helper()
			if err := f.db.UpdateTaskLanding(context.Background(), f.task.ID, "https://github.com/acme/shop/pull/18", ""); err != nil {
				t.Fatal(err)
			}
		}},
		{"new head", func(t *testing.T, f *prLandingFixture) {
			t.Helper()
			if err := f.db.SetTaskGatedSHA(context.Background(), f.task.ID, strings.Repeat("f", 40)); err != nil {
				t.Fatal(err)
			}
		}},
		{"new state", func(t *testing.T, f *prLandingFixture) {
			t.Helper()
			if err := f.db.Transition(context.Background(), f.task.ID, store.StateDone, store.StateWorking, "lead", "follow-up"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPRLandingFixture(t, "pr", store.StateDone)
			ctx := context.Background()
			if err := f.db.UpdateTaskLanding(ctx, f.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
				t.Fatal(err)
			}
			if err := f.db.SetTaskGatedSHA(ctx, f.task.ID, f.headSHA); err != nil {
				t.Fatal(err)
			}
			snapshot, err := f.db.Task(ctx, f.project.ID, "t1")
			if err != nil {
				t.Fatal(err)
			}
			tc.change(t, f)
			observed := store.PRObservation{ProjectID: f.project.ID, TaskID: f.task.ID, PRURL: snapshot.PRURL, HeadSHA: snapshot.GatedSHA, State: "MERGED", Checks: "{}", MergeCommit: f.headSHA}
			effect := prObservationEffect(f.project, snapshot, observed, nil, store.PRObservation{}, false)
			if _, err := f.db.RecordPRObservation(ctx, observed, effect, snapshot); err != nil {
				t.Fatal(err)
			}
			current, err := f.db.Task(ctx, f.project.ID, "t1")
			if err != nil {
				t.Fatal(err)
			}
			if current.State == store.StateLanded {
				t.Fatalf("stale observation landed Task: %#v", current)
			}
		})
	}
}

func attachPRFixtureMount(t *testing.T, fixture *prLandingFixture) store.Mount {
	t.Helper()
	ctx := context.Background()
	if _, err := fixture.db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state) VALUES(?,1,?,'idle')`, fixture.project.ID, fixture.worktree); err != nil {
		t.Fatal(err)
	}
	mount, err := fixture.db.AcquireMount(ctx, fixture.project.ID, fixture.task.ID, filepath.Join(fixture.root, "remuda"))
	if err != nil {
		t.Fatal(err)
	}
	if mount.Path != fixture.worktree || mount.State != "held" || mount.TaskID != fixture.task.ID {
		t.Fatalf("fixture Mount is not attached to the Task worktree: %#v", mount)
	}
	return mount
}

func containsPane(snapshot herdr.Snapshot, paneID string) bool {
	for _, pane := range snapshot.Panes {
		if pane.PaneID == paneID {
			return true
		}
	}
	return false
}

func TestPRWatchRetainsMergeabilityWhenGitHubReportsUnknown(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	poll := func() {
		t.Helper()
		if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	poll()
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "UNKNOWN", "", fixture.headSHA)
	poll()
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	poll()
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "land_ready") != 1 {
		t.Fatalf("UNKNOWN mergeability re-emitted readiness Notice: notices=%#v err=%v", notices, err)
	}
}

func TestPRWatchClosedPRRaisesDecisionOnce(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "CLOSED", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
	if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateLanding {
		t.Fatalf("closed PR moved Task unexpectedly: %#v, %v", task, err)
	}
	before, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(fixture.ghLog)
	if err != nil || strings.Count(string(after), "api graphql") != strings.Count(string(before), "api graphql")+1 {
		t.Fatalf("closed PR was not watched for reopening: %q %q %v", before, after, err)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "pr_closed") != 1 || countNoticeKind(notices, "pr_watch_failing") != 0 {
		t.Fatalf("closed Notice count=%d notices=%#v err=%v", countNoticeKind(notices, "pr_closed"), notices, err)
	}
	decisions, err := fixture.db.Decisions(context.Background(), fixture.project.ID, true)
	if err != nil || len(decisions) != 1 || decisions[0].Kind != "pr_closed" || decisions[0].Origin != "pr_closed:"+task.PRURL {
		t.Fatalf("closed PR Decision: %#v %v", decisions, err)
	}
}

func TestPRMergeAutoTeardownRetriesAfterHerdrReturns(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	configText := "[defaults]\nlanding_mode = \"pr\"\nmerge_method = \"squash\"\nauto_unsaddle = \"landed\"\n"
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output, errOutput := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", fixture.headSHA, fixture.headSHA)
	availableHerdr, ok := fixture.service.Herdr.(*herdr.Fake)
	if !ok {
		t.Fatal("fixture does not use the fake Herdr adapter")
	}
	fixture.service.Herdr = nil
	if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateLanded {
		t.Fatalf("PR merge was not recorded while Herdr was unavailable: %#v, %v", task, err)
	}
	snapshot := availableHerdr.SnapshotValue
	snapshot.Agents = []herdr.Agent{{Name: "posse-shop-t1-1", PaneID: "w2:p1"}}
	adapter := &changingSnapshotAdapter{Fake: availableHerdr, snapshot: snapshot}
	fixture.service.Herdr = adapter
	if err := fixture.service.maintainProjectWatch(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatalf("background watch after Herdr returned: %v; calls=%#v panes=%#v", err, adapter.Calls, adapter.snapshot.Panes)
	}
	task, err = fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateTornDown {
		t.Fatalf("landed Task did not auto-teardown on the next healthy reconcile: %#v, %v", task, err)
	}
}

func TestPRWatchAttributesPartialGraphQLErrorsAndKeepsGlobalFailures(t *testing.T) {
	for _, test := range []struct {
		name, alias string
		global      bool
	}{
		{name: "missing old PR", alias: "pr2"},
		{name: "unattributed error", alias: "unknown", global: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPRLandingFixture(t, "pr", store.StateDone)
			defer fixture.db.Close()
			if code, out, stderr := fixture.run("land", "t1"); code != 0 {
				t.Fatalf("open PR: %d %s %s", code, out, stderr)
			}
			ctx := context.Background()
			badID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 2, Type: "ship", Title: "old PR", State: store.StateSpawning, LandingMode: "pr", Branch: "posse/old", BaseRef: "main"})
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range []struct{ from, to store.State }{
				{store.StateSpawning, store.StateWorking}, {store.StateWorking, store.StateDone}, {store.StateDone, store.StateLanding},
			} {
				source := "cli"
				if step.to == store.StateDone {
					source = "worker"
				}
				if err := fixture.db.Transition(ctx, badID, step.from, step.to, source, "old PR"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET pr_url=? WHERE id=?`, "https://github.com/acme/shop/pull/41", badID); err != nil {
				t.Fatal(err)
			}
			fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
			encoded, err := os.ReadFile(fixture.ghState)
			if err != nil {
				t.Fatal(err)
			}
			var response map[string]any
			if err := json.Unmarshal(encoded, &response); err != nil {
				t.Fatal(err)
			}
			response["data"].(map[string]any)["repo1"].(map[string]any)["pr2"] = nil
			response["errors"] = []any{map[string]any{"message": "PR 41 is inaccessible", "path": []string{"repo1", test.alias}}}
			encoded, err = json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fixture.ghState, encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fixture.ghState+".exit", nil, 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(fixture.home, fixture.project.Name)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fixture.db.LatestPRObservation(ctx, fixture.task.ID); err != nil {
				t.Fatalf("valid PR starved by error: %v", err)
			}
			state, err := fixture.db.ProjectWatchState(ctx, fixture.project.ID)
			if err != nil {
				t.Fatal(err)
			}
			notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			if test.global {
				if state.PRConsecutiveFailures != 3 || countNoticeKind(notices, "pr_watch_failing") == 0 {
					t.Fatalf("unattributed GraphQL failure hidden: state=%#v notices=%#v", state, notices)
				}
			} else {
				if state.PRConsecutiveFailures != 0 {
					t.Fatalf("target-specific error counted as global: %#v", state)
				}
				badFailure := false
				for _, notice := range notices {
					if notice.TaskID == badID && notice.Kind == "pr_watch_failing" && strings.Contains(notice.Summary, "PR 41 is inaccessible") {
						badFailure = true
					}
				}
				if !badFailure {
					t.Fatalf("old PR has no attributed failure: %#v", notices)
				}
			}
		})
	}
}

func TestPRWatchFailureNoticeRequiresThreeConsecutiveFailures(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GH_FAIL", "1")
	for index := 0; index < 4; index++ {
		if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "pr_watch_failing") != 2 {
		t.Fatalf("watch failure Notice count=%d notices=%#v err=%v", countNoticeKind(notices, "pr_watch_failing"), notices, err)
	}
	state, err := fixture.db.ProjectWatchState(context.Background(), fixture.project.ID)
	if err != nil || state.PRConsecutiveFailures != 4 {
		t.Fatalf("failure streak was not tracked: %#v, %v", state, err)
	}
}

func TestPRStateAppearsInShowAndProjectHome(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
	if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	code, show, errOutput := fixture.run("show", "t1")
	if code != 0 || !strings.Contains(show, "pr:") || !strings.Contains(show, "https://github.com/acme/shop/pull/17") || !strings.Contains(show, "pending") {
		t.Fatalf("show omitted PR status: exit=%d output=%s error=%s", code, show, errOutput)
	}
	code, home, errOutput := fixture.run("--full")
	if code != 0 || !strings.Contains(home, "pr_state") || !strings.Contains(home, "OPEN") {
		t.Fatalf("Project home omitted Task PR state: exit=%d output=%s error=%s", code, home, errOutput)
	}
}

func TestPRWatchReconcilesOnlyAfterConfiguredInterval(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	configText := "[defaults]\nlanding_mode = \"pr\"\nmerge_method = \"squash\"\nauto_unsaddle = \"never\"\npr_poll = \"1h\"\n"
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output, errOutput := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	fixture.setGraphQLState(t, "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
	if code, output, errOutput := fixture.run("show", "t1"); code != 0 {
		t.Fatalf("show before the poll interval exit=%d output=%s error=%s", code, output, errOutput)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil || strings.Contains(string(log), "api graphql") {
		t.Fatalf("PR was polled before defaults.pr_poll: %q, %v", log, err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE project_watch_state SET pr_polled_at=? WHERE project_id=?`, time.Now().Add(-2*time.Hour).UnixMilli(), fixture.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.maintainProjectWatch(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatalf("Lookout poll after the configured interval: %v", err)
	}
	log, err = os.ReadFile(fixture.ghLog)
	observation, observationErr := fixture.db.LatestPRObservation(context.Background(), fixture.task.ID)
	if err != nil || strings.Count(string(log), "api graphql") != 1 || observationErr != nil || decodeChecks(observation.Checks).State != "FAILURE" {
		t.Fatalf("stale PR poll was not reconciled: log=%q observation=%#v errors=%v/%v", log, observation, err, observationErr)
	}
}

func TestLookoutPollsPRsAtConfiguredInterval(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateDone)
	configText := "[defaults]\nlanding_mode = \"pr\"\nmerge_method = \"squash\"\nauto_unsaddle = \"never\"\npr_poll = \"20ms\"\n"
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output, errOutput := fixture.run("land", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	if _, err := fixture.db.AckNotices(context.Background(), fixture.project.ID, []string{"all"}); err != nil {
		t.Fatal(err)
	}
	fake, ok := fixture.service.Herdr.(*herdr.Fake)
	if !ok {
		t.Fatal("fixture does not use the isolated fake Herdr adapter")
	}
	fake.SnapshotValue.Panes[0].AgentStatus = "working"
	fixture.setGraphQLState(t, "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
	fixture.test.Chdir(fixture.repo)
	fixture.output.Reset()
	fixture.errOut.Reset()
	herdrCallsBefore := fake.CallCount("session.snapshot")
	cli := fixture.service.CLI()
	cli.Out = &fixture.output
	cli.ErrOut = &fixture.errOut
	done := make(chan int, 1)
	go func() { done <- cli.Run([]string{"lookout", "--timeout", "3000"}) }()

	if !waitForPRCondition(done, func() bool {
		log, err := os.ReadFile(fixture.ghLog)
		return err == nil && strings.Count(string(log), "api graphql") >= 1 && fake.CallCount("session.snapshot") > herdrCallsBefore
	}) {
		t.Fatal("lookout returned before polling the watched PR")
	}
	if !waitForPRCondition(done, func() bool {
		log, err := os.ReadFile(fixture.ghLog)
		return err == nil && strings.Count(string(log), "api graphql") >= 2
	}) {
		t.Fatal("lookout did not poll again at the configured PR interval")
	}
	herdrCallsAfterInitialReconcile := fake.CallCount("session.snapshot")
	if code := <-done; code != 0 || !strings.Contains(fixture.output.String(), "timeout") {
		t.Fatalf("lookout did not finish its bounded wait: exit=%d output=%s error=%s", code, fixture.output.String(), fixture.errOut.String())
	}
	if got := fake.CallCount("session.snapshot"); got != herdrCallsAfterInitialReconcile || herdrCallsAfterInitialReconcile <= herdrCallsBefore {
		t.Fatalf("short PR cadence also repeated Herdr reconciliation: snapshots=%d after initial reconcile=%d before=%d", got, herdrCallsAfterInitialReconcile, herdrCallsBefore)
	}
}

func waitForPRCondition(done <-chan int, condition func() bool) bool {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return true
		}
		select {
		case <-done:
			return condition()
		case <-ticker.C:
		}
	}
}

func countNoticeKind(notices []store.Notice, kind string) int {
	count := 0
	for _, notice := range notices {
		if notice.Kind == kind {
			count++
		}
	}
	return count
}

func updateBareMain(t *testing.T, fixture *prLandingFixture, filename, contents string) string {
	t.Helper()
	checkout := filepath.Join(fixture.root, "upstream-change")
	gitTest(t, fixture.root, "clone", "--branch", "main", fixture.remote, checkout)
	gitTest(t, checkout, "config", "user.name", "Posse test")
	gitTest(t, checkout, "config", "user.email", "posse-test@example.test")
	if err := os.WriteFile(filepath.Join(checkout, filename), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, checkout, "add", filename)
	gitTest(t, checkout, "commit", "-m", "upstream change")
	gitTest(t, checkout, "push", "origin", "main")
	return strings.TrimSpace(gitTest(t, checkout, "rev-parse", "HEAD"))
}

func TestProjectSyncFastForwardsCleanDefaultBranch(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	upstream := updateBareMain(t, fixture, "upstream.txt", "origin update\n")
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	if err != nil || result.Status != "updated" || strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "main")) != upstream {
		t.Fatalf("clean default branch was not fast-forwarded: %#v, %v", result, err)
	}
	if contents, err := os.ReadFile(filepath.Join(fixture.repo, "upstream.txt")); err != nil || string(contents) != "origin update\n" {
		t.Fatalf("upstream file missing after sync: %q, %v", contents, err)
	}
}

func TestProjectSyncUpdatesDefaultBranchWhenItIsNotCheckedOut(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	gitTest(t, fixture.repo, "checkout", "-b", "lead-work")
	dirty := filepath.Join(fixture.repo, "dirty-lead-file.txt")
	if err := os.WriteFile(dirty, []byte("preserve this dirty file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	upstream := updateBareMain(t, fixture, "branch-update.txt", "branch update\n")
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	if err != nil || result.Status != "updated" || strings.TrimSpace(gitTest(t, fixture.repo, "branch", "--show-current")) != "lead-work" || strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/heads/main")) != upstream {
		t.Fatalf("unchecked-out default branch was not safely updated: %#v, %v", result, err)
	}
	if contents, err := os.ReadFile(dirty); err != nil || string(contents) != "preserve this dirty file\n" {
		t.Fatalf("updating an unchecked-out branch changed the root worktree: %q, %v", contents, err)
	}
}

func TestProjectSyncRefusesWhenDefaultBranchIsCheckedOutElsewhere(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	gitTest(t, fixture.repo, "checkout", "-b", "lead-work")
	otherWorktree := filepath.Join(fixture.root, "main-checkout")
	gitTest(t, fixture.repo, "worktree", "add", otherWorktree, "main")
	upstream := updateBareMain(t, fixture, "other-worktree-update.txt", "remote update\n")
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	if err != nil || result.Status != "root_behind" || !strings.Contains(result.Reason, "another worktree") {
		t.Fatalf("sync moved a default branch checked out elsewhere: %#v, %v", result, err)
	}
	if got := strings.TrimSpace(gitTest(t, otherWorktree, "rev-parse", "HEAD")); got == upstream {
		t.Fatalf("the other checked-out default branch unexpectedly advanced to %s", upstream)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "root_behind") != 1 {
		t.Fatalf("blocked sync did not create one root_behind Notice: %#v, %v", notices, err)
	}
}

func TestProjectSyncFastForwardsWithUnrelatedUntrackedFiles(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	localPath := filepath.Join(fixture.repo, "AGENTS.md")
	localContents := []byte("preserve this untracked file\n")
	if err := os.WriteFile(localPath, localContents, 0o600); err != nil {
		t.Fatal(err)
	}
	upstream := updateBareMain(t, fixture, "upstream.txt", "origin update\n")
	code, output, errOutput := fixture.run("sync")
	if code != 0 || !strings.Contains(output, "updated") {
		t.Fatalf("posse sync did not fast-forward with unrelated untracked files: exit=%d output=%s error=%s", code, output, errOutput)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "HEAD")); got != upstream {
		t.Fatalf("posse sync left the branch at %s, want %s", got, upstream)
	}
	if contents, err := os.ReadFile(localPath); err != nil || !bytes.Equal(contents, localContents) {
		t.Fatalf("sync changed unrelated untracked bytes: %q, %v", contents, err)
	}
}

func TestProjectSyncWithUntrackedFilesReportsCurrentWhenAlreadyUpToDate(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	localPath := filepath.Join(fixture.repo, "AGENTS.md")
	localContents := []byte("preserve this untracked file\n")
	if err := os.WriteFile(localPath, localContents, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	if err != nil || result.Status != "current" {
		t.Fatalf("sync with no upstream changes should report current: %#v, %v", result, err)
	}
	if contents, err := os.ReadFile(localPath); err != nil || !bytes.Equal(contents, localContents) {
		t.Fatalf("sync changed unrelated untracked bytes: %q, %v", contents, err)
	}
}

func TestProjectSyncWatcherFastForwardsWithUnrelatedUntrackedFiles(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	localPath := filepath.Join(fixture.repo, "AGENTS.md")
	localContents := []byte("preserve this untracked file\n")
	if err := os.WriteFile(localPath, localContents, 0o600); err != nil {
		t.Fatal(err)
	}
	upstream := updateBareMain(t, fixture, "upstream.txt", "origin update\n")
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, false)
	if err != nil || result.Status != "updated" || strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "HEAD")) != upstream {
		t.Fatalf("watcher did not fast-forward with unrelated untracked files: %#v, %v", result, err)
	}
	if contents, err := os.ReadFile(localPath); err != nil || !bytes.Equal(contents, localContents) {
		t.Fatalf("watcher changed unrelated untracked bytes: %q, %v", contents, err)
	}
}

func TestProjectSyncLeavesDirtyRootBytesUntouchedAndDeduplicatesNotice(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	tracked := filepath.Join(fixture.repo, "README.md")
	if err := os.WriteFile(tracked, []byte("local dirty edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	untracked := filepath.Join(fixture.repo, "untracked-preserve.txt")
	if err := os.WriteFile(untracked, []byte("preserve this untracked file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeTree := snapshotProjectFiles(t, fixture.repo)
	updateBareMain(t, fixture, "dirty-update.txt", "origin update\n")
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		result, syncErr := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
		if syncErr != nil || result.Status != "root_behind" || !strings.Contains(result.Reason, "README.md") {
			t.Fatalf("dirty root should remain behind and name its tracked change: %#v, %v", result, syncErr)
		}
	}
	if afterTree := snapshotProjectFiles(t, fixture.repo); !reflect.DeepEqual(afterTree, beforeTree) {
		t.Fatalf("sync changed Project worktree bytes: before=%#v after=%#v", beforeTree, afterTree)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "root_behind") != 1 || !strings.Contains(notices[0].Summary, "README.md") {
		t.Fatalf("root_behind Notice did not name the tracked blocker: notices=%#v err=%v", notices, err)
	}
	if err := os.WriteFile(tracked, []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(untracked); err != nil {
		t.Fatal(err)
	}
	code, output, errOutput := fixture.run("sync")
	if code != 0 || !strings.Contains(output, "updated") {
		t.Fatalf("explicit sync did not retry after the root was cleaned: exit=%d output=%s error=%s", code, output, errOutput)
	}
	if _, err := os.Stat(filepath.Join(fixture.repo, "dirty-update.txt")); err != nil {
		t.Fatalf("explicit sync did not fast-forward the root after cleanup: %v", err)
	}
}

func TestProjectSyncRefusesToOverwriteUntrackedLocalPath(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	localPath := filepath.Join(fixture.repo, "AGENTS.md")
	localContents := []byte("preserve user content\n")
	if err := os.WriteFile(localPath, localContents, 0o600); err != nil {
		t.Fatal(err)
	}
	before := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "HEAD"))
	updateBareMain(t, fixture, "AGENTS.md", "upstream content\n")
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	if err != nil || result.Status != "root_behind" || !strings.Contains(result.Reason, "AGENTS.md") {
		t.Fatalf("sync did not identify the colliding untracked path: result=%#v err=%v", result, err)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "HEAD")); got != before {
		t.Fatalf("sync moved default branch across local content: before=%s after=%s", before, got)
	}
	if contents, err := os.ReadFile(localPath); err != nil || !bytes.Equal(contents, localContents) {
		t.Fatalf("sync changed local untracked bytes: %q err=%v", contents, err)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "root_behind") != 1 || !strings.Contains(notices[0].Summary, "AGENTS.md") {
		t.Fatalf("root_behind Notice did not name the untracked blocker: notices=%#v err=%v", notices, err)
	}
}

func TestProjectSyncRefusesToOverwriteIgnoredLocalPath(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	exclude := filepath.Join(fixture.repo, ".git", "info", "exclude")
	if err := os.WriteFile(exclude, []byte("/generated.cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(fixture.repo, "generated.cache")
	localContents := []byte("user cache must survive\n")
	if err := os.WriteFile(localPath, localContents, 0o600); err != nil {
		t.Fatal(err)
	}
	before := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/heads/main"))
	updateBareMain(t, fixture, "generated.cache", "tracked upstream content\n")
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	if err != nil || result.Status != "root_behind" || !strings.Contains(result.Reason, "generated.cache") {
		t.Fatalf("sync advanced across an ignored local path: result=%#v err=%v", result, err)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/heads/main")); got != before {
		t.Fatalf("sync moved default branch across ignored local content: before=%s after=%s", before, got)
	}
	if contents, err := os.ReadFile(localPath); err != nil || !bytes.Equal(contents, localContents) {
		t.Fatalf("sync changed ignored local bytes: %q err=%v", contents, err)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "root_behind") != 1 {
		t.Fatalf("sync did not report one root_behind Notice: notices=%#v err=%v", notices, err)
	}
}

func snapshotProjectFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[relative] = contents
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestProjectSyncLeavesLocalCommitsAheadOfOriginUntouched(t *testing.T) {
	fixture := newPRLandingFixture(t, "pr", store.StateWorking)
	if err := os.WriteFile(filepath.Join(fixture.repo, "local-only.txt"), []byte("keep local commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.repo, "add", "local-only.txt")
	gitTest(t, fixture.repo, "commit", "-m", "local only")
	before := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "HEAD"))
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	after := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "HEAD"))
	if err != nil || result.Status != "local_ahead" || after != before {
		t.Fatalf("sync changed a local commit ahead of origin: %#v, before=%s after=%s err=%v", result, before, after, err)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "root_behind") != 0 {
		t.Fatalf("local-ahead checkout emitted a false root_behind Notice: %#v, %v", notices, err)
	}
}
