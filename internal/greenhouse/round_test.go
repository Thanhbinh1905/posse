package greenhouse

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettledInspectionKeepsPendingRecord(t *testing.T) {
	worktree := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Test"}} {
		command := exec.Command("git", args...)
		command.Dir = worktree
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(worktree, "file"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "file"}, {"commit", "-q", "-m", "test"}} {
		command := exec.Command("git", args...)
		command.Dir = worktree
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	head, err := worktreeHead(context.Background(), worktree)
	if err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(t.TempDir(), "review.pending")
	if err := os.WriteFile(pending, []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, ok, err := settledInspection(context.Background(), Round{Worktree: worktree}, "shop/t1/shop/r2", response{
		RunID: "run", HostResult: &HostResult{Grant: "shop/t1/shop/r2", Head: head, Current: true, Verdict: "findings"},
	})
	if err != nil || !ok || result.Outcome != Settled {
		t.Fatalf("settled inspection = %+v, %t, %v", result, ok, err)
	}
	if _, err := os.Stat(pending); err != nil {
		t.Fatalf("settled inspection removed pending request before the caller records its Verdict: %v", err)
	}
}

func TestSpentGrantIsRecognizedWhenSettledResultWasNotIngested(t *testing.T) {
	inspected := response{
		Run:        &run{HostGrants: []string{"greenhouse/t143/greenhouse/r1", "greenhouse/t143/greenhouse/r2"}},
		HostResult: &HostResult{Grant: "greenhouse/t143/greenhouse/r2", Head: "old-head", Verdict: "findings", Status: "settled"},
	}
	if !grantWasUsed(nil, inspected, "greenhouse/t143/greenhouse/r2") {
		t.Fatal("spent r2 was not recognized from Greenhouse state")
	}
	if got := nextGrantNumberFromResponse(inspected); got != 3 {
		t.Fatalf("next grant number = %d, want r3 after unrecorded settled r2", got)
	}
	if got := nextGrantNumberFromResponse(response{HostResult: inspected.HostResult}); got != 3 {
		t.Fatalf("next grant number without run metadata = %d, want r3 from settled r2", got)
	}
}

func TestActiveGrantIsNotMistakenForSpent(t *testing.T) {
	inspected := response{
		Run:        &run{HostGrants: []string{"greenhouse/t143/greenhouse/r2"}, Status: "running"},
		HostResult: &HostResult{Grant: "greenhouse/t143/greenhouse/r2", Status: "running", Current: true},
	}
	if grantWasUsed(nil, inspected, "greenhouse/t143/greenhouse/r2") {
		t.Fatal("active grant was mistaken for a spent grant")
	}
	if !grantWasUsed(errors.New("grant already used"), response{}, "greenhouse/t143/greenhouse/r2") {
		t.Fatal("Greenhouse refusal did not identify the spent grant")
	}
}

func TestNextGrantNumberIncludesSettledReviewUsage(t *testing.T) {
	got := nextGrantNumber(&run{HostGrants: []string{"shop/t1/shop/r1"}, ReviewUsed: 2})
	if got != 3 {
		t.Fatalf("next grant number = %d, want 3 after two settled reviews", got)
	}
}

func TestTerminalReviewStatus(t *testing.T) {
	for _, status := range []string{"cancelled", "failed", "inconclusive", "unconfigured", "refused"} {
		if !terminalReviewStatus(status) {
			t.Errorf("terminalReviewStatus(%q) = false, want true", status)
		}
	}
	for _, status := range []string{"running", "cancelling", "paused"} {
		if terminalReviewStatus(status) {
			t.Errorf("terminalReviewStatus(%q) = true, want false", status)
		}
	}
}

func TestReadRetriesStorageCommitRefusal(t *testing.T) {
	home := t.TempDir()
	binary := filepath.Join(t.TempDir(), "greenhouse")
	script := `#!/bin/sh
op=$(grep -o '"op":"[^"]*"' | cut -d '"' -f4)
if [ "$op" = attach ] && [ ! -e "$GREENHOUSE_HOME/first-attach" ]; then
  : > "$GREENHOUSE_HOME/first-attach"
  printf '%s\n' '{"version":2,"outcome":"error","error":"storage commit in progress; retry the read request"}'
  exit 0
fi
if [ "$op" = attach ]; then
  printf '%s\n' '{"version":2,"outcome":"ok","run_id":"run","input_id":"input","gate_id":"gate","token":"token","rules_ready":true,"host_result":{"grant":"project/t1/member/r1","head":"head","status":"complete","verdict":"pass","current":true}}'
  exit 0
fi
printf '%s\n' '{"version":2,"outcome":"ok","run_id":"run","input_id":"input","gate_id":"gate","token":"token","rules_ready":true}'
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Binary: binary, Home: home, Env: os.Environ()}
	response, err := d.attach(context.Background(), Round{Host: Host{Name: "member", ID: "host"}})
	if err != nil {
		t.Fatalf("attach failed after retryable refusal: %v", err)
	}
	if response.HostResult == nil || response.HostResult.Verdict != "pass" {
		t.Fatalf("attach response = %+v, want settled result after retry", response)
	}
}

func TestReadRefusalReturnsAfterRetryBound(t *testing.T) {
	home := t.TempDir()
	binary := filepath.Join(t.TempDir(), "greenhouse")
	script := `#!/bin/sh
n=0
[ -f "$GREENHOUSE_HOME/count" ] && n=$(cat "$GREENHOUSE_HOME/count")
n=$((n + 1))
printf '%s' "$n" > "$GREENHOUSE_HOME/count"
printf '%s\n' '{"version":2,"outcome":"error","error":"storage commit in progress; retry the read request"}'
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Binary: binary, Home: home, Env: os.Environ()}
	_, err := d.axi(context.Background(), home, []byte(`{"version":2,"op":"attach"}`))
	if err == nil || !strings.Contains(err.Error(), readRefusal) {
		t.Fatalf("axi error = %v, want the daemon's refusal reason", err)
	}
	count, err := os.ReadFile(filepath.Join(home, "count"))
	if err != nil {
		t.Fatal(err)
	}
	if string(count) != "4" {
		t.Fatalf("daemon calls = %s, want 4 total attempts", count)
	}
}
