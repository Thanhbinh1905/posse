//go:build e2e && perf

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCommandPerformanceBudgets(t *testing.T) {
	harness := newIntentCLIHarness(t)
	fixture := harness.newProject(t, "command-performance")
	if _, err := harness.client.Run(context.Background(), "pane", "report-agent", fixture.project.LeadPaneID, "--source", "posse.fake", "--agent", "claude", "--state", "working"); err != nil {
		t.Fatal(err)
	}

	contextEnv := setEnv(fixture.env, "HERDR_PANE_ID", fixture.project.LeadPaneID)
	contextSamples := measureCommand(t, harness.binary, fixture.repo, contextEnv, []string{"_context"}, "", 20)
	contextCPU, contextWall := summarizeCommand(contextSamples)

	ownedEvent := fmt.Sprintf(`{"event":"pane.agent_status_changed","data":{"pane_id":%q,"agent_status":"working"}}`, fixture.project.LeadPaneID)
	unownedEvent := `{"event":"pane.closed","data":{"pane_id":"w9:p99"}}`
	ownedEnv := setEnv(fixture.env, "HERDR_PLUGIN_EVENT_JSON", ownedEvent)
	unownedEnv := setEnv(fixture.env, "HERDR_PLUGIN_EVENT_JSON", unownedEvent)
	ownedSamples := measureCommand(t, harness.binary, fixture.repo, ownedEnv, []string{"_ingest"}, "", 20)
	unownedSamples := measureCommand(t, harness.binary, fixture.repo, unownedEnv, []string{"_ingest"}, "", 20)
	// The guard runs before every tool call of every Claude Code, Codex and pi
	// session, so deciding that the caller is not a Worker must stay cheap.
	plainTool := `{"tool_name":"Bash","tool_input":{"command":"go test ./..."}}`
	herdrTool := `{"tool_name":"Bash","tool_input":{"command":"herdr workspace list"}}`
	plainGuardCPU, plainGuardWall := summarizeCommand(measureCommand(t, harness.binary, fixture.repo, contextEnv, []string{"_guard"}, plainTool, 20))
	herdrGuardCPU, herdrGuardWall := summarizeCommand(measureCommand(t, harness.binary, fixture.repo, contextEnv, []string{"_guard"}, herdrTool, 20))
	t.Logf("_guard CPU median/p95: plain=%s/%s herdr=%s/%s; wall median/p95: plain=%s/%s herdr=%s/%s",
		plainGuardCPU.median, plainGuardCPU.p95, herdrGuardCPU.median, herdrGuardCPU.p95, plainGuardWall.median, plainGuardWall.p95, herdrGuardWall.median, herdrGuardWall.p95)
	if plainGuardCPU.p95 >= 50*time.Millisecond {
		t.Fatalf("_guard CPU p95 for a command without herdr exceeded 50 ms: %s", plainGuardCPU.p95)
	}
	if herdrGuardCPU.p95 >= 100*time.Millisecond {
		t.Fatalf("_guard CPU p95 for a herdr command exceeded 100 ms: %s", herdrGuardCPU.p95)
	}
	ownedCPU, ownedWall := summarizeCommand(ownedSamples)
	unownedCPU, unownedWall := summarizeCommand(unownedSamples)

	t.Logf("command CPU median/p95: _context=%s/%s owned _ingest=%s/%s unowned _ingest=%s/%s; wall median/p95: _context=%s/%s owned _ingest=%s/%s unowned _ingest=%s/%s",
		contextCPU.median, contextCPU.p95, ownedCPU.median, ownedCPU.p95, unownedCPU.median, unownedCPU.p95,
		contextWall.median, contextWall.p95, ownedWall.median, ownedWall.p95, unownedWall.median, unownedWall.p95)
	if contextCPU.p95 >= 100*time.Millisecond {
		t.Fatalf("_context CPU p95 exceeded 100 ms: %s", contextCPU.p95)
	}
	if unownedCPU.p95 >= 50*time.Millisecond {
		t.Fatalf("unowned _ingest CPU p95 exceeded 50 ms: %s", unownedCPU.p95)
	}
	if ownedCPU.p95 >= 300*time.Millisecond {
		t.Fatalf("owned _ingest CPU p95 exceeded 300 ms: %s", ownedCPU.p95)
	}
	if contextWall.p95 >= 2*time.Second || ownedWall.p95 >= 2*time.Second || unownedWall.p95 >= 2*time.Second {
		t.Fatalf("command wall p95 exceeded the 2 s ceiling: _context=%s owned _ingest=%s unowned _ingest=%s", contextWall.p95, ownedWall.p95, unownedWall.p95)
	}
}

type commandSample struct {
	cpu  time.Duration
	wall time.Duration
}

type commandSummary struct {
	median time.Duration
	p95    time.Duration
}

func measureCommand(t *testing.T, binary, cwd string, env, args []string, input string, count int) []commandSample {
	t.Helper()
	samples := make([]commandSample, 0, count)
	for range count {
		command := exec.Command(binary, args...)
		command.Dir = cwd
		command.Env = env
		command.Stdin = strings.NewReader(input)
		started := time.Now()
		output, err := command.CombinedOutput()
		wall := time.Since(started)
		if err != nil {
			t.Fatalf("posse %v: %v\n%s", args, err, output)
		}
		usage, ok := command.ProcessState.SysUsage().(*syscall.Rusage)
		if !ok || usage == nil {
			t.Fatalf("posse %v child did not report syscall.Rusage CPU usage: %T", args, command.ProcessState.SysUsage())
		}
		samples = append(samples, commandSample{
			cpu:  time.Duration(usage.Utime.Nano() + usage.Stime.Nano()),
			wall: wall,
		})
	}
	return samples
}

func summarizeCommand(samples []commandSample) (commandSummary, commandSummary) {
	cpuValues := make([]time.Duration, len(samples))
	wallValues := make([]time.Duration, len(samples))
	for index, sample := range samples {
		cpuValues[index] = sample.cpu
		wallValues[index] = sample.wall
	}
	summarize := func(values []time.Duration) commandSummary {
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		return commandSummary{median: values[len(values)/2], p95: values[(len(values)*95+99)/100-1]}
	}
	return summarize(cpuValues), summarize(wallValues)
}
