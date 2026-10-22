//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestConcurrentWatchSignalsAndLeadCommandsNeverReturnStoreBusy(t *testing.T) {
	f := newPRLifecycleFixtureWithLead(t, true)
	defer f.db.Close()

	if !waitForCondition(15*time.Second, func() bool { return len(watchPIDs(f.root)) > 0 }) {
		t.Fatalf("isolated Watch runtime did not start: pids=%v", watchPIDs(f.root))
	}

	ctx := context.Background()
	const workerCount = 6
	const iterations = 8
	workers := createStressWorkers(t, f, workerCount, func(seq int) string { return fmt.Sprintf("posse/concurrent-store-stress-%d", seq) })
	if _, err := f.db.RecordPRPoll(ctx, f.project.ID, 0, ""); err != nil {
		t.Fatalf("reset isolated Watch poll timestamp: %v", err)
	}
	storeBusyText := func(output string) bool {
		lower := strings.ToLower(output)
		return strings.Contains(lower, "store_busy") || strings.Contains(lower, "sqlite_busy") || strings.Contains(lower, "database is locked") || strings.Contains(lower, "database table is locked")
	}
	for iteration := 0; iteration < iterations; iteration++ {
		var contentionReleased chan struct{}
		if iteration == 0 {
			writer, err := f.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin isolated SQLite contention: %v", err)
			}
			if _, err := writer.ExecContext(ctx, `UPDATE projects SET name=name WHERE id=?`, f.project.ID); err != nil {
				_ = writer.Rollback()
				t.Fatalf("hold isolated SQLite writer: %v", err)
			}
			contentionReleased = make(chan struct{})
			go func() {
				time.Sleep(750 * time.Millisecond)
				_ = writer.Rollback()
				close(contentionReleased)
			}()
		}
		results := make(chan stressCommandResult, workerCount*2)
		var group sync.WaitGroup
		for _, worker := range workers {
			note := "concurrent store stress " + time.Now().Format(time.RFC3339Nano)
			args := []string{"signal", "working", note}
			group.Add(1)
			go func(worker stressWorker, args []string) {
				defer group.Done()
				results <- runStressCommand(f.binary, worker.cwd, worker.env, args)
			}(worker, args)
		}
		for command := 0; command < workerCount; command++ {
			leadArgs := []string{"list"}
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
		if contentionReleased != nil {
			<-contentionReleased
		}
		close(results)
		for result := range results {
			if result.err != nil || storeBusyText(result.output) {
				t.Fatalf("iteration %d: posse %s failed under concurrent Watch/Worker/Lead load: err=%v\n%s", iteration+1, strings.Join(result.args, " "), result.err, result.output)
			}
		}
	}

	for _, worker := range workers {
		signals, err := f.db.TaskSignals(ctx, worker.taskID, iterations+1)
		if err != nil {
			t.Fatalf("read stress-run Signals for Task %d: %v", worker.taskID, err)
		}
		if len(signals) != iterations {
			t.Fatalf("Task %d recorded %d Signals under load, want %d", worker.taskID, len(signals), iterations)
		}
		for _, signal := range signals {
			if signal.Verb != "working" {
				t.Fatalf("Task %d recorded unexpected Signal: %#v", worker.taskID, signal)
			}
		}
	}
	if got := len(watchPIDs(f.root)); got == 0 {
		t.Fatal("isolated Watch runtime exited during the stress run")
	}
	var lastPoll int64
	if err := f.db.QueryRowContext(ctx, `SELECT pr_polled_at FROM project_watch_state WHERE project_id=?`, f.project.ID).Scan(&lastPoll); err != nil {
		t.Fatalf("read isolated Watch poll timestamp: %v", err)
	}
	if lastPoll == 0 {
		t.Fatal("isolated Watch did not complete a poll during the stress run")
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil {
		t.Fatalf("read stress-run Notices: %v", err)
	}
	for _, notice := range notices {
		if storeBusyText(notice.Summary) || storeBusyText(notice.DataJSON) {
			t.Fatalf("Watch recorded a visible contention Notice: %#v", notice)
		}
	}
}

type stressWorker struct {
	taskID int64
	seq    int
	cwd    string
	env    []string
}

// createStressWorkers registers working Tasks on branchFor(seq) with live
// isolated Herdr panes and Sandboxes, so Worker commands resolve their Task
// from the environment.
func createStressWorkers(t *testing.T, f *prLifecycleFixture, workerCount int, branchFor func(int) string) []stressWorker {
	t.Helper()
	client := herdr.NewWithEnv("herdr", f.env)
	ctx := context.Background()
	workers := make([]stressWorker, 0, workerCount)
	for seq := 1; seq <= workerCount; seq++ {
		label := fmt.Sprintf("posse:shop:t%d", seq)
		worker, err := createTab(client, f.project.HerdrWorkspaceID, f.repo, label)
		if err != nil {
			t.Fatalf("create isolated Worker %d pane: %v", seq, err)
		}
		if _, err := client.Call(ctx, "pane.rename", map[string]any{"pane_id": worker.RootPane.PaneID, "label": label}); err != nil {
			t.Fatalf("label isolated Worker %d pane: %v", seq, err)
		}
		if _, err := client.Run(ctx, "pane", "report-agent", worker.RootPane.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "working"); err != nil {
			t.Fatalf("mark isolated Worker %d working: %v", seq, err)
		}
		branch := branchFor(seq)
		taskID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{
			Seq: seq, Type: "change", Title: fmt.Sprintf("Concurrent store stress %d", seq), ShortName: fmt.Sprintf("concurrent-store-stress-%d", seq),
			Profile: "deep", MergeMode: "local", Branch: branch, BaseRef: "main",
			HerdrWorkspaceID: f.project.HerdrWorkspaceID, PaneID: worker.RootPane.PaneID,
			PaneLabel: label, AgentName: "claude", AutonomyReview: "ask", AutonomyMerge: "ask",
		})
		if err != nil {
			t.Fatalf("create isolated Worker %d Task: %v", seq, err)
		}
		sandbox, err := f.db.AcquireSandbox(ctx, f.project.ID, taskID, filepath.Join(f.root, "posse", "pool"))
		if err != nil {
			t.Fatalf("acquire isolated Worker %d Sandbox: %v", seq, err)
		}
		gitTest(t, f.env, f.repo, "worktree", "add", "-b", branch, sandbox.Path, "main")
		if err := f.db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "Worker ready"); err != nil {
			t.Fatalf("start isolated Worker %d Task: %v", seq, err)
		}
		workerEnv := setEnv(f.env, "HERDR_ENV", "1")
		workerEnv = setEnv(workerEnv, "HERDR_PANE_ID", worker.RootPane.PaneID)
		workerEnv = setEnv(workerEnv, "HERDR_WORKSPACE_ID", f.project.HerdrWorkspaceID)
		workerEnv = setEnv(workerEnv, "HERDR_TAB_ID", worker.RootPane.TabID)
		workers = append(workers, stressWorker{taskID: taskID, seq: seq, cwd: sandbox.Path, env: workerEnv})
	}
	return workers
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
