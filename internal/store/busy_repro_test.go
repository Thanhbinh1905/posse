package store

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentWritesNeverReturnBusy reproduces the load posse holler hit under
// t1's stress harness: many Workers hammering write paths concurrently while the
// filesystem is under fsync pressure. Before the fix (BEGIN on a deferred
// transaction that later upgrades to a write), this test reliably produced
// hundreds of SQLITE_BUSY errors out of 800 Signal writes. With BEGIN IMMEDIATE
// (see the _txlock=immediate DSN option in OpenAt) plus bounded retry in
// beginTxWithRetry, it must produce none.
func TestConcurrentWritesNeverReturnBusy(t *testing.T) {
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}

	const taskCount = 6
	tasks := make([]Task, taskCount)
	for i := 0; i < taskCount; i++ {
		id, err := db.CreateTask(ctx, project.ID, Task{Seq: i + 1, Type: "ship", Title: fmt.Sprintf("Fix %d", i), LandingMode: "local", AutonomyReview: "ask", AutonomyLand: "ask"})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Transition(ctx, id, StateSpawning, StateWorking, "cli", "agent ready"); err != nil {
			t.Fatal(err)
		}
		task, err := db.TaskByID(ctx, project.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		tasks[i] = task
	}

	// Simulate CPU pressure alongside the fsync pressure that WAL commits already add.
	stopCPU := make(chan struct{})
	var cpuWg sync.WaitGroup
	for i := 0; i < runtime.NumCPU(); i++ {
		cpuWg.Add(1)
		go func() {
			defer cpuWg.Done()
			x := 0
			for {
				select {
				case <-stopCPU:
					return
				default:
					x++
				}
			}
		}()
	}
	defer func() {
		close(stopCPU)
		cpuWg.Wait()
	}()

	const goroutinesPerTask = 8
	const iterations = 20
	var wg sync.WaitGroup
	var busyCount int64
	var otherErrCount int64

	recordErr := func(err error) {
		if err == nil {
			return
		}
		if IsBusy(err) {
			atomic.AddInt64(&busyCount, 1)
			return
		}
		if err != ErrStateRace {
			atomic.AddInt64(&otherErrCount, 1)
		}
	}

	for _, task := range tasks {
		task := task
		for g := 0; g < goroutinesPerTask; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < iterations; i++ {
					// Exercises the same read-then-write pattern posse holler uses.
					_, err := db.RecordWorkerSignal(ctx, task, "working", "progress", nil, "")
					recordErr(err)
				}
			}()
		}
	}

	// A few goroutines hammering Mount acquire/release, the other contended write
	// path shared by multiple posse commands (ride, land, recovery).
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				mount, err := db.AcquireMount(ctx, project.ID, tasks[0].ID, "/mounts")
				if err != nil {
					recordErr(err)
					continue
				}
				if err := db.ReleaseMount(ctx, mount.ID, tasks[0].ID); err != nil {
					recordErr(err)
				}
			}
		}()
	}

	wg.Wait()

	if busyCount > 0 {
		t.Fatalf("got %d SQLITE_BUSY errors across %d writes under load (want 0)", busyCount, taskCount*goroutinesPerTask*iterations)
	}
	if otherErrCount > 0 {
		t.Fatalf("got %d unexpected non-busy errors under load", otherErrCount)
	}
}
