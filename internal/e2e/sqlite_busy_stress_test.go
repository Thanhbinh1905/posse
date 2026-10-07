//go:build e2e

package e2e

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestConcurrentLookoutSignalsAndLeadCommandsNeverReturnStoreBusy(t *testing.T) {
	f := newPRLifecycleFixtureWithLead(t, true)
	defer f.db.Close()

	if !waitForCondition(15*time.Second, func() bool { return len(lookoutPIDs(f.root)) > 0 }) {
		t.Fatalf("isolated Lookout poller did not start: panes=%#v", lookoutPanes(t, herdr.NewWithEnv("herdr", f.env)))
	}

	client := herdr.NewWithEnv("herdr", f.env)
	rider, err := createTab(client, f.project.HerdrWorkspaceID, f.repo, "posse:shop:t1")
	if err != nil {
		t.Fatalf("create isolated Rider pane: %v", err)
	}
	const riderLabel = "posse:shop:t1"
	if _, err := client.Call(context.Background(), "pane.rename", map[string]any{"pane_id": rider.RootPane.PaneID, "label": riderLabel}); err != nil {
		t.Fatalf("label isolated Rider pane: %v", err)
	}
	if _, err := client.Run(context.Background(), "pane", "report-agent", rider.RootPane.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "working"); err != nil {
		t.Fatalf("mark isolated Rider working: %v", err)
	}

	ctx := context.Background()
	taskID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{
		Seq: 1, Type: "ship", Title: "Concurrent store stress", ShortName: "concurrent-store-stress",
		Profile: "deep", LandingMode: "local", Branch: "posse/concurrent-store-stress", BaseRef: "main",
		HerdrWorkspaceID: f.project.HerdrWorkspaceID, PaneID: rider.RootPane.PaneID,
		PaneLabel: riderLabel, AgentName: "claude", AutonomyReview: "ask", AutonomyLand: "ask",
	})
	if err != nil {
		t.Fatalf("create isolated Rider Task: %v", err)
	}
	mount, err := f.db.AcquireMount(ctx, f.project.ID, taskID, filepath.Join(f.root, "posse", "remuda"))
	if err != nil {
		t.Fatalf("acquire isolated Rider Mount: %v", err)
	}
	gitTest(t, f.env, f.repo, "worktree", "add", "-b", "posse/concurrent-store-stress", mount.Path, "main")
	if err := f.db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "Rider ready"); err != nil {
		t.Fatalf("start isolated Rider Task: %v", err)
	}

	riderEnv := setEnv(f.env, "HERDR_ENV", "1")
	riderEnv = setEnv(riderEnv, "HERDR_PANE_ID", rider.RootPane.PaneID)
	riderEnv = setEnv(riderEnv, "HERDR_WORKSPACE_ID", f.project.HerdrWorkspaceID)
	riderEnv = setEnv(riderEnv, "HERDR_TAB_ID", rider.RootPane.TabID)

	const iterations = 12
	const commandsPerRole = 8
	if _, err := f.db.RecordPRPoll(ctx, f.project.ID, 0, ""); err != nil {
		t.Fatalf("reset isolated Lookout poll timestamp: %v", err)
	}
	busyText := func(output string) bool {
		lower := strings.ToLower(output)
		return strings.Contains(lower, "store_busy") || strings.Contains(lower, "sqlite_busy") || strings.Contains(lower, "database is locked") || strings.Contains(lower, "database table is locked") || strings.Contains(lower, "database contention")
	}

	for iteration := 0; iteration < iterations; iteration++ {
		results := make(chan stressCommandResult, commandsPerRole*2)
		var group sync.WaitGroup
		for command := 0; command < commandsPerRole; command++ {
			note := "concurrent store stress " + time.Now().Format(time.RFC3339Nano)
			args := []string{"holler", "working", note}
			group.Add(1)
			go func(args []string) {
				defer group.Done()
				results <- runStressCommand(f.binary, mount.Path, riderEnv, args)
			}(args)

			leadArgs := []string{"roster"}
			if command%2 == 1 {
				leadArgs = nil
			}
			group.Add(1)
			go func(args []string) {
				defer group.Done()
				results <- runStressCommand(f.binary, f.repo, f.leadEnv, args)
			}(leadArgs)
		}
		group.Wait()
		close(results)
		for result := range results {
			if result.err != nil || busyText(result.output) {
				t.Fatalf("iteration %d: posse %s failed under concurrent Lookout/Rider/Lead load: err=%v\n%s", iteration+1, strings.Join(result.args, " "), result.err, result.output)
			}
		}
	}

	signals, err := f.db.TaskSignals(ctx, taskID, iterations*commandsPerRole+1)
	if err != nil {
		t.Fatalf("read stress-run Signals: %v", err)
	}
	if len(signals) != iterations*commandsPerRole {
		t.Fatalf("recorded %d Signals under load, want %d", len(signals), iterations*commandsPerRole)
	}
	for _, signal := range signals {
		if signal.Verb != "working" {
			t.Fatalf("stress-run recorded unexpected Signal: %#v", signal)
		}
	}
	if got := len(lookoutPIDs(f.root)); got == 0 {
		t.Fatal("isolated Lookout poller exited during the stress run")
	}
	var lastPoll int64
	if err := f.db.QueryRowContext(ctx, `SELECT pr_polled_at FROM project_watch_state WHERE project_id=?`, f.project.ID).Scan(&lastPoll); err != nil {
		t.Fatalf("read isolated Lookout poll timestamp: %v", err)
	}
	if lastPoll == 0 {
		t.Fatal("isolated Lookout did not complete a poll during the stress run")
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil {
		t.Fatalf("read stress-run Notices: %v", err)
	}
	for _, notice := range notices {
		if busyText(notice.Summary) || busyText(notice.DataJSON) {
			t.Fatalf("Lookout recorded a visible contention Notice: %#v", notice)
		}
	}
}

func runStressCommand(binary, cwd string, env []string, args []string) stressCommandResult {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir, command.Env = cwd, env
	output, err := command.CombinedOutput()
	return stressCommandResult{args: args, output: string(output), err: err}
}

type stressCommandResult struct {
	args   []string
	output string
	err    error
}
