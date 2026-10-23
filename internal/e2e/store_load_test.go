//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStoreLoadAtWorkerLimitKeepsForegroundCommandsFree drives one Project
// store with a running Watch and the traffic of several Workers and their Lead
// at once. Foreground commands must neither fail on contention nor report a
// deferred Project reconciliation. POSSE_E2E_STORE_LOAD_ROUNDS raises the load.
func TestStoreLoadAtWorkerLimitKeepsForegroundCommandsFree(t *testing.T) {
	runStoreLoadAtWorkerLimit(t, false)
}

// TestStoreLoadWithProjectHistoryKeepsWatchAndLeadFree repeats the Worker
// limit load on a store with weeks of Project history and a backlog of open
// Notices (#428). Every Task write used to rebuild the Project snapshot from
// that backlog, which held the Sandbox state lock and spent the reconcile
// budget, so foreground commands reported contention and Watch fell behind.
func TestStoreLoadWithProjectHistoryKeepsWatchAndLeadFree(t *testing.T) {
	runStoreLoadAtWorkerLimit(t, true)
}

func runStoreLoadAtWorkerLimit(t *testing.T, withHistory bool) {
	f := newPRLifecycleFixtureWithLead(t, true)
	defer f.db.Close()
	if !waitForCondition(15*time.Second, func() bool { return len(watchPIDs(f.root)) > 0 }) {
		t.Fatalf("isolated Watch runtime did not start: pids=%v", watchPIDs(f.root))
	}
	const workerCount = 6
	if withHistory {
		configPath := filepath.Join(f.home, "config.toml")
		configText, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		limited := strings.Replace(string(configText), "[defaults]\n", fmt.Sprintf("[defaults]\nmax_workers = %d\n", workerCount), 1)
		if err := os.WriteFile(configPath, []byte(limited), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rounds := 6
	if value, err := strconv.Atoi(os.Getenv("POSSE_E2E_STORE_LOAD_ROUNDS")); err == nil && value > 0 {
		rounds = value
	}
	// The shared fixture polls every millisecond; real Projects poll PRs and
	// sync the root checkout on a multi-second interval.
	workers := createStressWorkers(t, f, workerCount, func(seq int) string {
		// The fake forge serves PRs for posse/t1, so that Worker also publishes.
		if seq == 1 {
			return "posse/t1"
		}
		return fmt.Sprintf("posse/store-load-%d", seq)
	})
	for _, worker := range workers {
		taskDir := filepath.Join(f.home, "projects", f.project.Name, "tasks", "t"+strconv.Itoa(worker.seq))
		if err := os.MkdirAll(taskDir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, contents := range map[string]string{
			"launch.md": "# Worker protocol\n\nLoad test launch Brief.\n",
			"brief.md":  "---\ntype: change\ntitle: Store load\ndone_when: load measured\n---\nLoad test Brief.\n",
		} {
			if err := os.WriteFile(filepath.Join(taskDir, name), []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if withHistory {
		seedProductionHistory(t, f)
	}
	publisher := workers[0]
	if _, err := f.db.ExecContext(context.Background(), `UPDATE tasks SET landing_mode='pr' WHERE id=?`, publisher.taskID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(publisher.cwd, "store-load.txt"), []byte("load\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, publisher.cwd, "add", "store-load.txt")
	gitTest(t, f.env, publisher.cwd, "commit", "-m", "store load change")
	if _, err := f.db.RecordPRPoll(context.Background(), f.project.ID, 0, ""); err != nil {
		t.Fatalf("reset isolated Watch poll timestamp: %v", err)
	}

	type sample struct {
		command  string
		elapsed  time.Duration
		failed   bool
		busy     bool
		deferred bool
		output   string
	}
	samples := make(chan sample, workerCount*rounds*8)
	run := func(cwd string, env []string, args ...string) {
		started := time.Now()
		result := runStressCommand(f.binary, cwd, env, args)
		lower := strings.ToLower(result.output)
		samples <- sample{
			command:  strings.TrimSpace("posse " + strings.Join(args[:min(len(args), 1)], " ")),
			elapsed:  time.Since(started),
			failed:   result.err != nil,
			busy:     strings.Contains(lower, "store_busy") || strings.Contains(lower, "sqlite_busy") || strings.Contains(lower, "database is locked"),
			deferred: strings.Contains(result.output, "reconciliation was deferred") || strings.Contains(result.output, "maintenance deferred"),
			output:   result.output,
		}
	}
	// Workers run test suites that open their own stores and sync files to
	// the same disk. Mirror that pressure, since it stretches every fsync.
	pressureDone := make(chan struct{})
	var pressure sync.WaitGroup
	for writer := range workerCount {
		pressure.Add(1)
		go func(writer int) {
			defer pressure.Done()
			path := filepath.Join(f.root, fmt.Sprintf("fsync-pressure-%d", writer))
			chunk := make([]byte, 256<<10)
			for {
				select {
				case <-pressureDone:
					_ = os.Remove(path)
					return
				default:
				}
				file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
				if err != nil {
					return
				}
				_, _ = file.Write(chunk)
				_ = file.Sync()
				_ = file.Close()
			}
		}(writer)
	}
	// Sample the Watch heartbeat throughout the load. A stalled Watch loop or
	// a failed heartbeat write shows up as a gap between observed advances.
	loadStarted := time.Now()
	heartbeatDone := make(chan struct{})
	heartbeatGap := make(chan time.Duration, 1)
	go func() {
		var last int64
		lastAdvance, maxGap := time.Now(), time.Duration(0)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatDone:
				heartbeatGap <- max(maxGap, time.Since(lastAdvance))
				return
			case <-ticker.C:
			}
			var heartbeat int64
			if err := f.db.QueryRowContext(context.Background(), `SELECT COALESCE(MAX(heartbeat_at),0) FROM project_lookout_pollers WHERE project_id=?`, f.project.ID).Scan(&heartbeat); err != nil {
				continue
			}
			if heartbeat != last {
				maxGap = max(maxGap, time.Since(lastAdvance))
				last, lastAdvance = heartbeat, time.Now()
			}
		}
	}()
	var group sync.WaitGroup
	for _, worker := range workers {
		group.Add(2)
		go func(worker stressWorker) {
			defer group.Done()
			for round := range rounds {
				run(worker.cwd, worker.env, "signal", "working", fmt.Sprintf("load round %d", round))
				run(worker.cwd, worker.env, "brief")
				if worker.seq == publisher.seq {
					run(worker.cwd, worker.env, "publish", fmt.Sprintf("Store load round %d", round))
				}
			}
		}(worker)
		go func(worker stressWorker) {
			defer group.Done()
			task := "t" + strconv.Itoa(worker.seq)
			for round := range rounds {
				run(f.repo, f.leadEnv, "send", task, fmt.Sprintf("load instruction %d", round), "--queue")
				run(f.repo, f.leadEnv, "show", task)
				if worker.seq == 1 {
					run(f.repo, f.leadEnv, "ack", "all")
					run(f.repo, f.leadEnv)
				}
			}
		}(worker)
	}
	group.Wait()
	close(heartbeatDone)
	maxHeartbeatGap := <-heartbeatGap
	close(pressureDone)
	pressure.Wait()
	close(samples)

	byCommand := map[string][]sample{}
	var failures []string
	for sample := range samples {
		byCommand[sample.command] = append(byCommand[sample.command], sample)
		if sample.failed || sample.busy || sample.deferred {
			failures = append(failures, fmt.Sprintf("%s (%s): %s", sample.command, sample.elapsed.Round(time.Millisecond), strings.TrimSpace(sample.output)))
		}
	}
	commands := make([]string, 0, len(byCommand))
	for command := range byCommand {
		commands = append(commands, command)
	}
	sort.Strings(commands)
	var report strings.Builder
	for _, command := range commands {
		list := byCommand[command]
		sort.Slice(list, func(i, j int) bool { return list[i].elapsed < list[j].elapsed })
		var failed, busy, deferred int
		for _, sample := range list {
			if sample.failed {
				failed++
			}
			if sample.busy {
				busy++
			}
			if sample.deferred {
				deferred++
			}
		}
		percentile := func(p int) time.Duration { return list[(len(list)-1)*p/100].elapsed.Round(time.Millisecond) }
		fmt.Fprintf(&report, "%-14s n=%-3d failed=%-3d busy=%-3d deferred=%-3d p50=%-7s p95=%-7s max=%s\n", command, len(list), failed, busy, deferred, percentile(50), percentile(95), percentile(100))
	}
	watchLog, _ := os.ReadFile(filepath.Join(f.home, "projects", f.project.Name, "watch.log"))
	lowerWatchLog := strings.ToLower(string(watchLog))
	watchBusy := strings.Count(lowerWatchLog, "sqlite_busy") + strings.Count(lowerWatchLog, "database is locked") + strings.Count(lowerWatchLog, "store contention")
	var lastPoll int64
	if err := f.db.QueryRowContext(context.Background(), `SELECT COALESCE((SELECT pr_polled_at FROM project_watch_state WHERE project_id=?),0)`, f.project.ID).Scan(&lastPoll); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&report, "watch.log contention lines=%d max heartbeat gap=%s\n", watchBusy, maxHeartbeatGap.Round(time.Millisecond))
	t.Logf("store load with %d Workers, %d rounds:\n%s", workerCount, rounds, report.String())
	if len(failures) > 0 {
		t.Fatalf("%d foreground commands failed or deferred under load:\n%s\n%s", len(failures), report.String(), strings.Join(failures[:min(len(failures), 10)], "\n---\n"))
	}
	if watchBusy > 0 {
		t.Fatalf("Watch logged %d contention failures under load:\n%s", watchBusy, watchLog)
	}
	// Watch passes run every two seconds; a third of the stale threshold
	// leaves room for one slow pass but not for a stalled loop.
	if maxHeartbeatGap > 10*time.Second {
		t.Fatalf("Watch heartbeat did not advance for %s under load:\n%s", maxHeartbeatGap, report.String())
	}
	if lastPoll < loadStarted.UnixMilli() {
		t.Fatalf("pull requests were not polled during the load: last poll %d, load started %d", lastPoll, loadStarted.UnixMilli())
	}
}
