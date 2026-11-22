//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestTeardownStopsTaskGreenhouseReviewOwner(t *testing.T) {
	f := newWorkerTabsFixture(t)
	projectConfig := filepath.Join(f.home, "projects", "shop", "config.toml")
	if err := os.WriteFile(projectConfig, []byte("review_provider = \"greenhouse\"\n[defaults]\nreview = \"full-pipeline\"\nmerge_mode = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newGreenhouseContract(t, f)
	c.change = f.start(t, "t1", "Greenhouse owner teardown", "greenhouse-owner-down")
	head := c.authorCommit(t)
	roundDir := filepath.Join(f.home, "projects", "shop", "tasks", "t1", "greenhouse")
	if err := os.MkdirAll(roundDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(roundDir, "shop.parallel.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	ready := filepath.Join(roundDir, "ready")
	owner := exec.Command("python3", "-c", "import os,time; open(os.environ['READY'], 'w').close(); time.sleep(30)", "_greenhouse", "review", "t1")
	owner.Env = append(c.env, "READY="+ready)
	owner.ExtraFiles = []*os.File{lock}
	if err := owner.Start(); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	_ = lock.Close()
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = owner.Process.Kill()
			_ = owner.Wait()
		}
	})
	if !waitForCondition(5*time.Second, func() bool { _, err := os.Stat(ready); return err == nil }) {
		t.Fatal("Greenhouse owner fixture did not start")
	}
	ownerRecord, err := json.Marshal(struct {
		PID         int    `json:"pid"`
		Head        string `json:"head"`
		RequestedAt int64  `json:"requested_at"`
	}{owner.Process.Pid, head, 1})
	if err != nil {
		t.Fatal(err)
	}
	ownerPath := filepath.Join(roundDir, "shop.parallel-owner.json")
	if err := os.WriteFile(ownerPath, ownerRecord, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := c.posse(c.env, f.repo, "teardown", "t1", "--discard", "--user-approved", "User approved this Task discard")
	if err != nil {
		t.Fatalf("approved teardown: %v\n%s", err, output)
	}
	if err := owner.Wait(); err == nil {
		t.Fatal("Greenhouse owner exited successfully after teardown")
	}
	waited = true
	if _, err := os.Stat(ownerPath); !os.IsNotExist(err) {
		t.Fatalf("Greenhouse owner record remains after teardown: %v", err)
	}
	if task := f.task(t, "t1"); task.State != store.StateTornDown {
		t.Fatalf("Task state after teardown = %q, want torn-down", task.State)
	}
	if output, err := c.posse(c.env, f.repo, "watch", "--poll-only", "--timeout", "1000"); err != nil {
		t.Fatalf("Watch after teardown: %v\n%s", err, output)
	}
	if _, err := os.Stat(ownerPath); !os.IsNotExist(err) {
		t.Fatalf("Watch recreated the review owner for a torn-down Task: %v", err)
	}
}

func TestGreenhouseNewHeadAdvancesPastEveryLedgerGrant(t *testing.T) {
	f := newWorkerTabsFixture(t)
	projectConfig := filepath.Join(f.home, "projects", "shop", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfig, []byte("review_provider = \"greenhouse\"\nreview_parallel = \"greenhouse\"\n[defaults]\nreview = \"full-pipeline\"\nmerge_mode = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newGreenhouseContract(t, f)
	c.change = f.start(t, "t1", "Greenhouse ledger grant recovery", "greenhouse-ledger-retry")
	headA := c.authorCommit(t)
	task := f.task(t, "t1")
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	failed := store.GreenhouseReviewRequest{ProjectID: task.ProjectID, TaskID: task.ID, Repository: "shop", RequestedHead: headA, Provider: store.GreenhouseReviewProvider, RequestedAt: 100, SettledAt: 200, Outcome: "failed", Grant: "shop/t1/shop/r2"}
	if err := db.RecordGreenhouseReviewRequest(context.Background(), failed); err != nil {
		t.Fatal(err)
	}
	pass := store.Verdict{TaskID: task.ID, Repository: "shop", Provider: store.GreenhouseReviewProvider, Protocol: "2", Grant: "shop/t1/shop/r1", Head: headA, Target: headA, ReviewBase: headA, Outcome: "clean", Mode: "parallel"}
	if recorded, err := db.RecordGreenhouseVerdict(context.Background(), pass, func(context.Context) (string, error) { return headA, nil }); err != nil || !recorded {
		t.Fatalf("record earlier passing Verdict = %v, %v", recorded, err)
	}
	if err := db.RecordGreenhouseReviewRequest(context.Background(), store.GreenhouseReviewRequest{ProjectID: task.ProjectID, TaskID: task.ID, Repository: "shop", RequestedHead: headA, Provider: store.GreenhouseReviewProvider, RequestedAt: 90, SettledAt: 150, Outcome: "verdict", Grant: pass.Grant}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	headB := c.authorCommit(t)
	result := c.review(t)
	if result["outcome"] != "settled" || result["head"] != headB || result["verdict"] != "clean" || result["recorded"] != true {
		t.Fatalf("new-head review result = %v; want a recorded clean Verdict for %s", result, headB)
	}
	if result["grant"] != "shop/t1/shop/r3" {
		t.Fatalf("new-head review grant = %v; want r3 after ledger grants r1 and r2", result["grant"])
	}
	requestsDB, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := requestsDB.GreenhouseReviewRequests(context.Background(), task.ProjectID)
	_ = requestsDB.Close()
	if err != nil || len(requests) != 3 || requests[0].Grant != "shop/t1/shop/r1" || requests[1].Grant != "shop/t1/shop/r2" || requests[2].Grant != "shop/t1/shop/r3" || requests[2].Outcome != "verdict" {
		t.Fatalf("Greenhouse request ledger = %+v, %v; want failed r2, passing r1 and new-head Verdict r3", requests, err)
	}
}

func TestGreenhouseParallelInconclusiveRunEndsOwnerAndAdvancesGrant(t *testing.T) {
	f := newWorkerTabsFixture(t)
	projectConfig := filepath.Join(f.home, "projects", "shop", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfig, []byte("review_provider = \"greenhouse\"\nreview_parallel = \"greenhouse\"\n[defaults]\nreview = \"full-pipeline\"\nmerge_mode = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newGreenhouseContract(t, f)
	c.change = f.start(t, "t1", "Greenhouse inconclusive request", "greenhouse-inconclus")
	c.authorCommit(t)
	c.setMode(t, "invalid-output")
	args := []string{"_greenhouse", "review", "t1", "--reviewer-backend", "pi", "--reviewer-model", "fixture/model", "--timeout", "5000", "--json"}
	output, err := c.posse(c.env, f.repo, args...)
	if err == nil || !strings.Contains(output, "greenhouse_failed") {
		t.Fatalf("inconclusive review = err %v, output %s; want terminal failure", err, output)
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := db.GreenhouseReviewRequests(context.Background(), f.projectID)
	_ = db.Close()
	if err != nil || len(requests) != 1 || requests[0].Outcome != "failed" || requests[0].Grant != "shop/t1/shop/r1" {
		t.Fatalf("terminal review requests = %+v, %v; want failed r1", requests, err)
	}
	if retryAt := requests[0].SettledAt + 30_000; retryAt <= time.Now().UnixMilli() {
		t.Fatalf("fresh review test did not exercise active retry backoff: retry at %d", retryAt)
	}
	if got := c.notices(t, "greenhouse_review_failed"); got != 1 {
		t.Fatalf("greenhouse_review_failed Notices = %d, want 1", got)
	}
	pending := filepath.Join(f.home, "projects", "shop", "tasks", "t1", "greenhouse", "shop.pending.json")
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatalf("terminal pending record still exists: %v", err)
	}
	c.setMode(t, "clean")
	output, err = c.posse(c.env, f.repo, "review", "start", "t1")
	if err != nil || !strings.Contains(output, "Greenhouse Verdict requested") {
		t.Fatalf("fresh review start: %v\n%s", err, output)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		db, err := store.OpenReadOnly(f.home)
		if err != nil {
			t.Fatal(err)
		}
		requests, err = db.GreenhouseReviewRequests(context.Background(), f.projectID)
		_ = db.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(requests) == 2 && requests[1].Outcome == "verdict" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(requests) != 2 || requests[1].Outcome != "verdict" || requests[1].Grant != "shop/t1/shop/r2" {
		t.Fatalf("fresh review requests = %+v; want a settled r2 request", requests)
	}
}

func TestGreenhouseParallelTimeoutAndRecoveryCountOneRequest(t *testing.T) {
	f := newWorkerTabsFixture(t)
	projectConfig := filepath.Join(f.home, "projects", "shop", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfig, []byte("review_parallel = \"greenhouse\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newGreenhouseContract(t, f)
	c.change = f.start(t, "t1", "Greenhouse parallel request", "greenhouse-paralle-retry")

	c.authorCommit(t)
	c.hold(t)
	pendingOutput, err := c.posse(c.env, f.repo, "_greenhouse", "review", "t1", "--reviewer-backend", "pi", "--reviewer-model", "fixture/model", "--timeout", "1", "--json")
	if err != nil {
		t.Fatalf("timed-out parallel review: %v\n%s", err, pendingOutput)
	}
	pending := decodeGreenhouseOutput(t, pendingOutput)
	if pending["outcome"] != "running" {
		t.Fatalf("timed-out review = %v; want running", pending)
	}
	c.waitInvocations(t, 1)
	assertGreenhouseRequestStats(t, f, c.env, 1, 0)

	c.release(t)
	var recovered map[string]any
	if !waitForCondition(30*time.Second, func() bool {
		output, err := c.posse(c.env, f.repo, "_greenhouse", "review", "t1", "--reviewer-backend", "pi", "--reviewer-model", "fixture/model", "--timeout", "60000", "--json")
		if err != nil {
			return false
		}
		recovered = decodeGreenhouseOutput(t, output)
		return recovered["outcome"] == "settled"
	}) {
		t.Fatalf("timed-out review did not recover: %v", recovered)
	}
	assertGreenhouseRequestStats(t, f, c.env, 1, 1)

	c.authorCommit(t)
	c.hold(t)
	before := c.invocations(t)
	done := c.reviewAsync(c.env)
	c.waitInvocations(t, before+1)
	if err := syscall.Kill(c.daemonPID(t), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	parked := c.finish(t, done)
	if parked["outcome"] != "recovery-required" {
		t.Fatalf("interrupted review = %v; want recovery-required", parked)
	}
	assertGreenhouseRequestStats(t, f, c.env, 2, 1)
	c.release(t)
	var final map[string]any
	if !waitForCondition(30*time.Second, func() bool {
		final = c.review(t)
		return final["outcome"] == "settled"
	}) {
		t.Fatalf("parked review did not recover: %v", final)
	}
	assertGreenhouseRequestStats(t, f, c.env, 2, 2)
}

func assertGreenhouseRequestStats(t *testing.T, f *workerTabsFixture, env []string, requests, verdicts int) {
	t.Helper()
	output := runPosse(t, f.binary, f.repo, env, "stats", "--json")
	var stats struct {
		Comparison struct {
			Total struct {
				Requests int `json:"requests"`
				Verdicts int `json:"verdicts"`
			} `json:"total"`
		} `json:"review_comparison"`
	}
	if err := json.Unmarshal([]byte(output), &stats); err != nil {
		t.Fatalf("decode request stats: %v\n%s", err, output)
	}
	if stats.Comparison.Total.Requests != requests || stats.Comparison.Total.Verdicts != verdicts {
		t.Fatalf("Greenhouse request stats = %d requests, %d verdicts; want %d, %d\n%s", stats.Comparison.Total.Requests, stats.Comparison.Total.Verdicts, requests, verdicts, strings.TrimSpace(output))
	}
}
