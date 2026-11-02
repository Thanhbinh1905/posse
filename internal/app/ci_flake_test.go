package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

const flakyShardLog = "e2e shard 3\tRun E2E shard\t2026-10-30T02:52:11.9Z --- FAIL: TestFlaky (15.06s)\n" +
	"e2e shard 3\tRun E2E shard\t2026-10-30T02:52:11.9Z     store_load_test.go:254: contention lines = 3\n" +
	"e2e shard 3\tRun E2E shard\t2026-10-30T02:54:02.9Z FAIL\n" +
	"e2e shard 3\tRun E2E shard\t2026-10-30T02:54:03.0Z FAIL\tgithub.com/acme/shop/internal/e2e\t214.606s"

type flakeFixture struct {
	*prMergeFixture
	cfg config.Config
}

func newFlakeFixture(t *testing.T) *flakeFixture {
	t.Helper()
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GH_FAILURE_JOB", "456")
	t.Setenv("POSSE_TEST_GH_RUN_LOG", flakyShardLog)
	t.Setenv("POSSE_TEST_GH_FAILURE_NAME", "e2e shard 3")
	return &flakeFixture{prMergeFixture: fixture, cfg: config.Config{}}
}

func (f *flakeFixture) freeze(t *testing.T, window string) {
	t.Helper()
	freezeCILoopConfig(t, f.prMergeFixture, config.CILoop{MaxFixRounds: 3, RerunCancelled: 1, FlakeWindow: window}, "none")
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg
}

func (f *flakeFixture) poll(t *testing.T) {
	t.Helper()
	if err := f.service.pollProjectPullRequests(context.Background(), f.db, f.project, f.cfg, true); err != nil {
		t.Fatal(err)
	}
}

func (f *flakeFixture) recordElsewhere(t *testing.T, test string, age time.Duration) {
	t.Helper()
	err := f.db.RecordCITestFailures(context.Background(), []store.CITestFailure{{
		ProjectID: f.project.ID, TaskID: f.task.ID, PRURL: "https://github.com/acme/shop/pull/9", HeadSHA: strings.Repeat("a", 40),
		CheckName: "e2e shard 1", JobID: "11", Package: "github.com/acme/shop/internal/e2e", Test: test, ObservedAt: time.Now().Add(-age).UnixMilli(),
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *flakeFixture) count(t *testing.T, query string) int {
	t.Helper()
	var count int
	if err := f.db.QueryRowContext(context.Background(), query, f.task.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func (f *flakeFixture) ghLogText(t *testing.T) string {
	t.Helper()
	log, err := os.ReadFile(f.ghLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(log)
}

func TestPRWatchRerunsUntouchedFlakyTestOnceInsteadOfQueueingTheWorker(t *testing.T) {
	f := newFlakeFixture(t)
	f.freeze(t, "168h")
	f.recordElsewhere(t, "TestFlaky", 2*time.Hour)
	f.setGraphQLState(t, "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", f.headSHA)

	f.poll(t)
	if fixes := f.count(t, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`); fixes != 0 {
		t.Fatalf("a flaky failure the PR does not touch queued %d CI fix rounds, want a rerun instead", fixes)
	}
	if queued := f.count(t, `SELECT COUNT(*) FROM messages WHERE task_id=? AND status='queued'`); queued != 0 {
		t.Fatalf("a flaky failure queued %d Worker instructions", queued)
	}
	if log := f.ghLogText(t); strings.Count(log, "run rerun --job 456 --repo acme/shop") != 1 {
		t.Fatalf("flaky check rerun commands = %q, want one job rerun", log)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var flake *store.Notice
	for index := range notices {
		if notices[index].Kind == "ci_flake_rerun" {
			flake = &notices[index]
		}
	}
	if flake == nil || !strings.Contains(flake.Summary, "TestFlaky") || !strings.Contains(flake.Summary, "e2e shard 3") {
		t.Fatalf("flake Notice = %#v, want one naming the check and TestFlaky", flake)
	}
	reruns, err := f.db.CIReruns(context.Background(), f.task.ID, "", "https://github.com/acme/shop/pull/17", 10)
	if err != nil || len(reruns) != 1 || reruns[0].Reason != "flake" || reruns[0].Outcome != "pending" || strings.Join(reruns[0].Tests, ",") != "TestFlaky" || reruns[0].CheckName != "e2e shard 3" {
		t.Fatalf("recorded reruns = %#v err=%v, want one pending flake rerun of TestFlaky", reruns, err)
	}

	// The rerun failed again: the Worker gets today's CI fix round.
	f.poll(t)
	if fixes := f.count(t, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`); fixes != 1 {
		t.Fatalf("a check that failed again after its rerun queued %d CI fix rounds, want one", fixes)
	}
	if log := f.ghLogText(t); strings.Count(log, "run rerun") != 1 {
		t.Fatalf("a flaky check was rerun more than once: %q", log)
	}
	reruns, err = f.db.CIReruns(context.Background(), f.task.ID, "", "https://github.com/acme/shop/pull/17", 10)
	if err != nil || len(reruns) != 1 || reruns[0].Outcome != "failed" {
		t.Fatalf("rerun after a second failure = %#v err=%v, want outcome failed", reruns, err)
	}

	if code, output, errOutput := f.run("pr", "inspect", "t1"); code != 0 || !strings.Contains(output, "reruns") || !strings.Contains(output, "TestFlaky") || !strings.Contains(output, "flake") || !strings.Contains(output, "failed") {
		t.Fatalf("pr inspect exit=%d output=%s error=%s, want the flake rerun with its test and outcome", code, output, errOutput)
	}
}

func TestPRWatchRecordsFlakeRerunThatPasses(t *testing.T) {
	f := newFlakeFixture(t)
	f.freeze(t, "168h")
	f.recordElsewhere(t, "TestFlaky", time.Hour)
	f.setGraphQLState(t, "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", f.headSHA)
	f.poll(t)
	f.setGraphQLState(t, "OPEN", "SUCCESS", "REVIEW_REQUIRED", "MERGEABLE", "", f.headSHA)
	f.poll(t)
	reruns, err := f.db.CIReruns(context.Background(), f.task.ID, "", "https://github.com/acme/shop/pull/17", 10)
	if err != nil || len(reruns) != 1 || reruns[0].Outcome != "passed" || reruns[0].SettledAt == 0 {
		t.Fatalf("rerun after a green head = %#v err=%v, want outcome passed", reruns, err)
	}
	if fixes := f.count(t, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`); fixes != 0 {
		t.Fatalf("a passing flake rerun queued %d CI fix rounds", fixes)
	}
}

func TestPRWatchQueuesCIFixRoundWhenFailureIsNotAKnownUntouchedFlake(t *testing.T) {
	for _, scenario := range []string{"no-history", "same-pr-history", "old-history", "touched-package", "window-off"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFlakeFixture(t)
			window := "168h"
			switch scenario {
			case "no-history":
			case "same-pr-history":
				if err := f.db.RecordCITestFailures(context.Background(), []store.CITestFailure{{
					ProjectID: f.project.ID, TaskID: f.task.ID, PRURL: "https://github.com/acme/shop/pull/17", HeadSHA: strings.Repeat("b", 40),
					CheckName: "e2e shard 3", JobID: "12", Package: "github.com/acme/shop/internal/e2e", Test: "TestFlaky", ObservedAt: time.Now().UnixMilli(),
				}}); err != nil {
					t.Fatal(err)
				}
			case "old-history":
				f.recordElsewhere(t, "TestFlaky", 200*time.Hour)
			case "touched-package":
				f.recordElsewhere(t, "TestFlaky", time.Hour)
				if err := os.MkdirAll(filepath.Join(f.worktree, "internal", "e2e"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.worktree, "internal", "e2e", "flaky_test.go"), []byte("package e2e\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				gitTest(t, f.worktree, "add", ".")
				gitTest(t, f.worktree, "commit", "-m", "touch the e2e package")
				f.headSHA = strings.TrimSpace(gitTest(t, f.worktree, "rev-parse", "HEAD"))
				f.setGHHead(t, f.headSHA)
			case "window-off":
				f.recordElsewhere(t, "TestFlaky", time.Hour)
				window = ""
			}
			f.freeze(t, window)
			f.setGraphQLState(t, "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", f.headSHA)
			f.poll(t)
			if fixes := f.count(t, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`); fixes != 1 {
				t.Fatalf("CI fix rounds = %d, want today's single round", fixes)
			}
			if log := f.ghLogText(t); strings.Contains(log, "run rerun") {
				t.Fatalf("a failure that is not a known untouched flake was rerun: %q", log)
			}
			if notices := f.count(t, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='ci_flake_rerun'`); notices != 0 {
				t.Fatalf("flake Notices = %d, want none", notices)
			}
			if scenario != "window-off" {
				if recorded := f.count(t, `SELECT COUNT(*) FROM ci_test_failures WHERE task_id=? AND pr_url='https://github.com/acme/shop/pull/17' AND test='TestFlaky' AND head_sha<>'`+strings.Repeat("b", 40)+`'`); recorded != 1 {
					t.Fatalf("recorded TestFlaky failures for this head = %d, want one", recorded)
				}
			}
		})
	}
}

func TestParseGoTestFailures(t *testing.T) {
	failures := parseGoTestFailures(flakyShardLog + "\n" +
		"unit\tTest\t2026-10-30T02:52:11Z --- FAIL: TestA (0.10s)\n" +
		"unit\tTest\t2026-10-30T02:52:11Z     --- FAIL: TestA/sub (0.00s)\n" +
		"unit\tTest\t2026-10-30T02:52:11Z FAIL\texample.com/m/pkg/a\t0.2s\n" +
		"unit\tTest\t2026-10-30T02:52:11Z --- FAIL: TestOrphan (0.10s)\n")
	got := []string{}
	for _, failure := range failures.Tests {
		got = append(got, failure.Package+"."+failure.Test)
	}
	want := "github.com/acme/shop/internal/e2e.TestFlaky,example.com/m/pkg/a.TestA,.TestOrphan"
	if strings.Join(got, ",") != want || failures.BuildFailed {
		t.Fatalf("parsed failures = %v build=%t, want %s", got, failures.BuildFailed, want)
	}
	if !parseGoTestFailures("FAIL\texample.com/m/pkg [build failed]").BuildFailed {
		t.Fatal("a build failure was not detected")
	}
}

func TestStewardRerunIsRecordedWithTestsAndOutcome(t *testing.T) {
	f := newFlakeFixture(t)
	f.freeze(t, "168h")
	f.setGraphQLState(t, "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", f.headSHA)
	f.poll(t)
	if code, output, errOutput := f.run("pr", "rerun", "t1", "--run", "123"); code != 0 {
		t.Fatalf("pr rerun exit=%d output=%s error=%s", code, output, errOutput)
	}
	f.setGraphQLState(t, "OPEN", "SUCCESS", "REVIEW_REQUIRED", "MERGEABLE", "", f.headSHA)
	f.poll(t)
	reruns, err := f.db.CIReruns(context.Background(), f.task.ID, "", "https://github.com/acme/shop/pull/17", 10)
	if err != nil || len(reruns) != 1 || reruns[0].Reason != "steward" || reruns[0].Outcome != "passed" || strings.Join(reruns[0].Tests, ",") != "TestFlaky" {
		t.Fatalf("recorded steward reruns = %#v err=%v, want one passed rerun of TestFlaky", reruns, err)
	}
	if code, output, errOutput := f.run("pr", "inspect", "t1"); code != 0 || !strings.Contains(output, "steward") || !strings.Contains(output, "TestFlaky") || !strings.Contains(output, "passed") {
		t.Fatalf("pr inspect exit=%d output=%s error=%s, want the steward rerun with its test and outcome", code, output, errOutput)
	}
}

func TestPRWatchFindsFlakyTestAboveTheInstructionTail(t *testing.T) {
	f := newFlakeFixture(t)
	lines := strings.SplitN(flakyShardLog, "\n", 3)
	passing := strings.Repeat("e2e shard 3\tRun E2E shard\t2026-10-30T02:53:00.0Z --- PASS: TestOther (0.10s)\n", 250)
	t.Setenv("POSSE_TEST_GH_RUN_LOG", lines[0]+"\n"+lines[1]+"\n"+passing+lines[2])
	f.freeze(t, "168h")
	f.recordElsewhere(t, "TestFlaky", time.Hour)
	f.setGraphQLState(t, "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", f.headSHA)
	f.poll(t)
	if log := f.ghLogText(t); strings.Count(log, "run rerun --job 456 --repo acme/shop") != 1 {
		t.Fatalf("a flaky test named more than 200 lines above the log end was not rerun: %q", log)
	}
	if fixes := f.count(t, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`); fixes != 0 {
		t.Fatalf("CI fix rounds = %d, want none", fixes)
	}
}
