//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

// observationWorkload counts what an observation costs, separated from model
// usage. The fixture has no model, so model turns are zero by construction:
// every Notice below is discovered by a Lookout process alone.
type observationWorkload struct {
	ForgeCalls        int           `json:"forge_calls"`
	PollerArms        int           `json:"poller_arms"`
	HandoffArms       int           `json:"handoff_arms"`
	ActionableWakes   int           `json:"actionable_wakes"`
	NoticesCreated    int           `json:"notices_created"`
	MaintenanceFaults int           `json:"maintenance_failure_notices"`
	PollerCPU         time.Duration `json:"poller_cpu_ns"`
	ModelTurns        int           `json:"model_turns"`
}

func forgeCalls(t *testing.T, f *prLifecycleFixture) int {
	t.Helper()
	data, err := os.ReadFile(f.ghLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	calls := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "api graphql") {
			calls++
		}
	}
	return calls
}

// runPollers runs the fixture's real poll-only Lookout tab beside `count`
// foreground producers (ordinary Posse calls reconcile PRs too) for one window.
func (w *observationWorkload) runPollers(t *testing.T, f *prLifecycleFixture, count int, window time.Duration) {
	t.Helper()
	var wait sync.WaitGroup
	var mutex sync.Mutex
	deadline := time.Now().Add(window)
	for i := 0; i < count; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for time.Now().Before(deadline) {
				command := exec.Command(f.binary, "roster")
				command.Dir, command.Env = f.repo, f.leadEnv
				if output, err := command.CombinedOutput(); err != nil {
					t.Errorf("foreground producer: %v\n%s", err, output)
					return
				}
				mutex.Lock()
				w.PollerArms++
				w.PollerCPU += command.ProcessState.UserTime() + command.ProcessState.SystemTime()
				mutex.Unlock()
				time.Sleep(50 * time.Millisecond)
			}
		}()
	}
	wait.Wait()
}

func prepareObservedTaskHeads(t *testing.T, f *prLifecycleFixture, taskID int64) (string, string) {
	t.Helper()
	gitTest(t, f.env, f.repo, "switch", "-c", "posse/t1")
	path := filepath.Join(f.repo, "observed-head.txt")
	if err := os.WriteFile(path, []byte("first observed head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, f.repo, "add", "observed-head.txt")
	gitTest(t, f.env, f.repo, "commit", "-m", "first observed head")
	headOne := strings.TrimSpace(gitTest(t, f.env, f.repo, "rev-parse", "HEAD"))
	if err := os.WriteFile(path, []byte("second observed head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, f.repo, "add", "observed-head.txt")
	gitTest(t, f.env, f.repo, "commit", "-m", "second observed head")
	headTwo := strings.TrimSpace(gitTest(t, f.env, f.repo, "rev-parse", "HEAD"))
	gitTest(t, f.env, f.repo, "switch", "main")
	if _, err := f.db.ExecContext(context.Background(), `UPDATE tasks SET worktree_path=?, target_branch='main' WHERE id=?`, f.repo, taskID); err != nil {
		t.Fatal(err)
	}
	return headOne, headTwo
}

func observationNotices(t *testing.T, f *prLifecycleFixture, kind string) []store.Notice {
	t.Helper()
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var matched []store.Notice
	for _, notice := range notices {
		if notice.Kind == kind {
			matched = append(matched, notice)
		}
	}
	return matched
}

func TestObservationWorkloadDiscoversForgeResultsWithoutModelTurns(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	ctx := context.Background()
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("poll-only Lookout did not start: %v", lookoutPIDs(f.root))
	}

	taskID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{
		Seq: 1, Type: "ship", Title: "Observed change", LandingMode: "pr", Branch: "posse/t1",
		PRURL: "https://github.com/acme/shop/pull/17",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	headOne, headTwo := prepareObservedTaskHeads(t, f, taskID)
	workload := &observationWorkload{}
	promptLog := filepath.Join(f.root, "lead-prompts.log")

	// Two simultaneous producers observe the same failing head for several
	// identical polls. The forge result must become exactly one Notice, found
	// without any Lead turn.
	f.writeGraphQL(t, "pr1", "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", headOne)
	workload.runPollers(t, f, 2, 3*time.Second)
	firstCalls := forgeCalls(t, f)
	if firstCalls < 2 {
		t.Fatalf("two producers made %d forge calls, want repeated polls", firstCalls)
	}
	failures := observationNotices(t, f, "pr_checks_failed")
	if len(failures) != 1 || !strings.Contains(failures[0].DataJSON, headOne) {
		t.Fatalf("identical polls created %d checks Notices, want exactly one for %s: %#v", len(failures), headOne, failures)
	}
	if prompts, _ := os.ReadFile(promptLog); len(prompts) != 0 {
		t.Fatalf("observation typed into the Lead without a delivery path: %q", prompts)
	}

	// A new PR head with the same failure is a new fact.
	f.writeGraphQL(t, "pr1", "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", headTwo)
	workload.runPollers(t, f, 2, 2*time.Second)
	failures = observationNotices(t, f, "pr_checks_failed")
	if len(failures) != 2 || !strings.Contains(failures[1].DataJSON, headTwo) {
		t.Fatalf("new head produced %d checks Notices, want exactly two: %#v", len(failures), failures)
	}

	// With a realistic interval, extra producers add no forge calls inside it.
	configPath := filepath.Join(f.home, "config.toml")
	shortInterval, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(strings.Replace(string(shortInterval), `pr_poll = "1ms"`, `pr_poll = "1h"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // let an in-flight short-interval poll finish
	settled := forgeCalls(t, f)
	workload.runPollers(t, f, 2, time.Second)
	if got := forgeCalls(t, f); got != settled {
		t.Fatalf("producers polled inside a one-hour interval: before=%d after=%d", settled, got)
	}
	if err := os.WriteFile(configPath, shortInterval, 0o600); err != nil {
		t.Fatal(err)
	}

	// A live claim held by another poller suppresses forge calls and keeps its
	// token; an expired claim is reclaimed without losing observations.
	before := forgeCalls(t, f)
	const foreignToken = "foreign-poller-token"
	if _, err := f.db.ExecContext(ctx, `UPDATE project_watch_state SET pr_poll_claim_until=?, pr_poll_claim_token=? WHERE project_id=?`, time.Now().Add(time.Hour).UnixMilli(), foreignToken, f.project.ID); err != nil {
		t.Fatal(err)
	}
	workload.runPollers(t, f, 2, time.Second)
	if after := forgeCalls(t, f); after != before {
		t.Fatalf("poller contacted the forge under another poller's live claim: before=%d after=%d", before, after)
	}
	var token string
	if err := f.db.QueryRowContext(ctx, `SELECT pr_poll_claim_token FROM project_watch_state WHERE project_id=?`, f.project.ID).Scan(&token); err != nil || token != foreignToken {
		t.Fatalf("live foreign claim token = %q err=%v, want it untouched", token, err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE project_watch_state SET pr_poll_claim_until=? WHERE project_id=?`, time.Now().Add(-time.Minute).UnixMilli(), f.project.ID); err != nil {
		t.Fatal(err)
	}
	workload.runPollers(t, f, 1, time.Second)
	if after := forgeCalls(t, f); after == before {
		t.Fatal("poller did not reclaim an expired claim")
	}

	// A forge outage raises one deduplicated maintenance Notice after repeated
	// failures. Recovery resets the streak and does not repeat the old fact.
	if err := os.WriteFile(filepath.Join(f.root, "gh-exit-nonzero"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	workload.runPollers(t, f, 2, 3*time.Second)
	if got := len(observationNotices(t, f, "pr_watch_failing")); got > 1 {
		t.Fatalf("outage raised %d pr_watch_failing Notices, want at most one", got)
	}
	if err := os.Remove(filepath.Join(f.root, "gh-exit-nonzero")); err != nil {
		t.Fatal(err)
	}
	workload.runPollers(t, f, 1, 2*time.Second)
	if streak := prTaskWatchFailureCount(t, f.db, taskID); streak != 0 {
		t.Fatalf("recovered forge left failure streak %d", streak)
	}
	if got := len(observationNotices(t, f, "pr_checks_failed")); got != 2 {
		t.Fatalf("outage recovery repeated observed facts: %d checks Notices, want 2", got)
	}

	// The actionable wake is one receipt-backed batch; accepting it leaves
	// nothing for the next arm to repeat.
	handoff := func(timeout string) noticeHandoffPayload {
		command := exec.Command(f.binary, "lookout", "--json", "--quiet-routine", "--handoff", "--destination", "fake:workload", "--timeout", timeout)
		command.Dir, command.Env = f.repo, f.leadEnv
		output, err := command.Output()
		if err != nil {
			t.Fatalf("handoff Lookout: %v\n%s", err, output)
		}
		workload.HandoffArms++
		var payload noticeHandoffPayload
		if err := json.Unmarshal(output, &payload); err != nil {
			t.Fatalf("decode handoff: %s: %v", output, err)
		}
		return payload
	}
	wake := handoff("5000")
	if wake.Delivery.DeliveryID == "" || len(wake.Delivery.NoticeIDs) < 2 {
		t.Fatalf("handoff did not wake with the pending forge Notices: %#v", wake)
	}
	workload.ActionableWakes++
	resolve := exec.Command(f.binary, "lookout", "--json", "--receipt", wake.Delivery.DeliveryID, "--receipt-outcome", "accepted", "--receipt-token", wake.Delivery.OwnerToken)
	resolve.Dir, resolve.Env = f.repo, f.leadEnv
	if output, err := resolve.CombinedOutput(); err != nil {
		t.Fatalf("accept receipt: %v\n%s", err, output)
	}
	if again := handoff("1000"); again.Delivery.DeliveryID != "" {
		t.Fatalf("accepted batch woke again: %#v", again)
	}

	workload.ForgeCalls = forgeCalls(t, f)
	notices, err := f.db.Notices(ctx, f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	workload.NoticesCreated = len(notices)
	workload.MaintenanceFaults = len(observationNotices(t, f, "pr_watch_failing"))
	if workload.ForgeCalls == 0 || workload.PollerCPU == 0 || workload.ModelTurns != 0 {
		t.Fatalf("workload counters are inconsistent: %#v", workload)
	}
	report, err := json.MarshalIndent(workload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("observation workload (API and CPU cost; no model usage):\n%s", report)
}

// With no poll-only Lookout running and producers run one at a time, every
// counter is exact: each producer makes one forge call, so the counts below
// are the same on every machine.
func TestObservationWorkloadCountersAreDeterministic(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	ctx := context.Background()
	stopBackgroundLookout(t, f)

	taskID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{
		Seq: 1, Type: "ship", Title: "Observed change", LandingMode: "pr", Branch: "posse/t1",
		PRURL: "https://github.com/acme/shop/pull/17",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	headOne, headTwo := prepareObservedTaskHeads(t, f, taskID)
	workload := &observationWorkload{}
	produce := func(times int) {
		for i := 0; i < times; i++ {
			before := forgeCalls(t, f)
			command := exec.Command(f.binary, "roster")
			command.Dir, command.Env = f.repo, f.leadEnv
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("producer: %v\n%s", err, output)
			}
			if delta := forgeCalls(t, f) - before; delta != 1 {
				t.Fatalf("producer made %d forge calls, want exactly one", delta)
			}
			workload.PollerArms++
			workload.PollerCPU += command.ProcessState.UserTime() + command.ProcessState.SystemTime()
		}
	}

	f.writeGraphQL(t, "pr1", "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", headOne)
	produce(3)
	f.writeGraphQL(t, "pr1", "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", headTwo)
	produce(2)

	if calls := forgeCalls(t, f); calls != 5 {
		t.Fatalf("forge calls = %d, want 5", calls)
	}
	if got := len(observationNotices(t, f, "pr_checks_failed")); got != 2 {
		t.Fatalf("checks Notices = %d, want one per head (2)", got)
	}
	handoff := exec.Command(f.binary, "lookout", "--json", "--quiet-routine", "--handoff", "--destination", "fake:deterministic", "--timeout", "5000")
	handoff.Dir, handoff.Env = f.repo, f.leadEnv
	output, err := handoff.Output()
	if err != nil {
		t.Fatalf("handoff: %v\n%s", err, output)
	}
	workload.HandoffArms++
	var wake noticeHandoffPayload
	if err := json.Unmarshal(output, &wake); err != nil {
		t.Fatal(err)
	}
	carried := map[int64]bool{}
	for _, id := range wake.Delivery.NoticeIDs {
		carried[id] = true
	}
	for _, notice := range observationNotices(t, f, "pr_checks_failed") {
		if !carried[notice.ID] {
			t.Fatalf("wake carried %v, missing checks Notice %d", wake.Delivery.NoticeIDs, notice.ID)
		}
	}
	workload.ActionableWakes++
	workload.ForgeCalls = forgeCalls(t, f) // the handoff Lookout's own polls are API cost too
	if workload.PollerArms != 5 || workload.HandoffArms != 1 || workload.ActionableWakes != 1 || workload.ModelTurns != 0 || workload.PollerCPU <= 0 {
		t.Fatalf("counters = %#v", workload)
	}
	if prompts, _ := os.ReadFile(filepath.Join(f.root, "lead-prompts.log")); len(prompts) != 0 {
		t.Fatalf("a model turn was needed: %q", prompts)
	}
}
