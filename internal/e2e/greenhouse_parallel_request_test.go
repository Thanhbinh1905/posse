//go:build e2e && linux

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

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
