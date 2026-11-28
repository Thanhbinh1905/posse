package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestConcurrentOpenersReturnRetryableContentionUnderWriterLoad reproduces
// migration-lock contention while other connections continue writing. Openers
// must fail with the store's retryable busy error, not context.DeadlineExceeded.
func TestOpenMigrationLockWaitDoesNotConsumeMigrationBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posse.db")
	lock, err := acquireMigrationLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	started := time.Now()
	db, err := openAt(path, storeOpenMigrationBudget)
	if db != nil {
		_ = db.Close()
	}
	elapsed := time.Since(started)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("contended store open error = %v, want retryable ErrBusy", err)
	}
	if elapsed >= storeOpenMigrationBudget/2 {
		t.Fatalf("migration lock contention waited %s, want the separate %s lock budget rather than the %s migration budget", elapsed, migrationLockStallBudget, storeOpenMigrationBudget)
	}
}

// A migration that outlasts the stall budget must not fail its waiters while
// the holder keeps its heartbeat going.
func TestOpenWaitsForMigrationHolderThatKeepsBeating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posse.db")
	lock, err := acquireMigrationLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	stopBeat := beatMigrationLock(path)
	go func() {
		time.Sleep(migrationLockStallBudget + 2*migrationHeartbeatInterval)
		stopBeat()
		_ = lock.Close()
	}()
	db, err := openAt(path, storeOpenMigrationBudget)
	if err != nil {
		t.Fatalf("open behind a beating migration holder failed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestOpenersOfCurrentStoreDoNotWaitForMigrationLock covers the routine case:
// every CLI command opens a store that already holds this build's schema. Such
// opens must not serialize on the cross-process migration lock.
func TestOpenersOfCurrentStoreDoNotWaitForMigrationLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	lock, err := acquireMigrationLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	for range 4 {
		opened, err := OpenAt(path)
		if err != nil {
			t.Fatalf("open current store with the migration lock held: %v", err)
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConcurrentOpenersReturnRetryableContentionUnderWriterLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A missing checksum receipt sends the open through the locked migration path.
	if _, err := db.ExecContext(context.Background(), `DELETE FROM posse_migration_checksums WHERE version=(SELECT MAX(version) FROM posse_migration_checksums)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE contention_writes (value INTEGER)`); err != nil {
		t.Fatal(err)
	}

	lock, err := acquireMigrationLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	const openers = 12
	const writers = 4
	const writesPerWriter = 25
	start := make(chan struct{})
	errorsByOpener := make(chan error, openers)
	var openerGroup sync.WaitGroup
	for range openers {
		openerGroup.Add(1)
		go func() {
			defer openerGroup.Done()
			<-start
			opened, err := openAt(path, 100*time.Millisecond)
			if err == nil {
				err = opened.Close()
			}
			errorsByOpener <- err
		}()
	}

	var writerGroup sync.WaitGroup
	for writer := range writers {
		writerGroup.Add(1)
		go func(writer int) {
			defer writerGroup.Done()
			<-start
			for i := range writesPerWriter {
				if _, err := db.ExecContext(context.Background(), `INSERT INTO contention_writes(value) VALUES (?)`, writer*writesPerWriter+i); err != nil {
					t.Errorf("write during concurrent opens: %v", err)
					return
				}
			}
		}(writer)
	}

	close(start)
	openerGroup.Wait()
	writerGroup.Wait()
	close(errorsByOpener)
	for err := range errorsByOpener {
		if !errors.Is(err, ErrBusy) {
			t.Errorf("contended OpenAt error = %v, want retryable ErrBusy", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("contended OpenAt error still exposes context.DeadlineExceeded: %v", err)
		}
	}
}

// TestConcurrentWritesNeverReturnBusy reproduces the load posse signal hit under
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
		id, err := db.CreateTask(ctx, project.ID, Task{Seq: i + 1, Type: "change", Title: fmt.Sprintf("Fix %d", i), MergeMode: "local", AutonomyReview: "ask", AutonomyMerge: "ask"})
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
	for i := 0; i < runtime.GOMAXPROCS(0); i++ {
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
					// Exercises the same read-then-write pattern posse signal uses.
					_, err := db.RecordWorkerSignal(ctx, task, "working", "progress", nil, "")
					recordErr(err)
				}
			}()
		}
	}

	// A few goroutines hammering Sandbox acquire/release, the other contended write
	// path shared by multiple posse commands (start, merge, recovery).
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				sandbox, err := db.AcquireSandbox(ctx, project.ID, tasks[0].ID, "/sandboxes")
				if err != nil {
					recordErr(err)
					continue
				}
				if err := db.BeginSandboxRelease(ctx, sandbox.ID, tasks[0].ID); err != nil {
					recordErr(err)
					continue
				}
				if err := db.FinishSandboxRelease(ctx, sandbox.ID, tasks[0].ID); err != nil {
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

func TestTwoProjectsConcurrentSignalsQueueAdmissionsAndStatusReadsConverge(t *testing.T) {
	ctx := context.Background()
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects := make([]Project, 2)
	active := make([][]Task, 2)
	for projectIndex := range projects {
		project, err := db.CreateProject(ctx, fmt.Sprintf("shop-%d", projectIndex), filepath.Join(t.TempDir(), fmt.Sprintf("repo-%d", projectIndex)), "main")
		if err != nil {
			t.Fatal(err)
		}
		projects[projectIndex] = project
		for taskIndex := 0; taskIndex < 4; taskIndex++ {
			taskID, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", Title: fmt.Sprintf("Active %d", taskIndex), ShortName: fmt.Sprintf("active-%d", taskIndex)}, func(int) (bool, error) { return false, nil })
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Transition(ctx, taskID, StateSpawning, StateWorking, "cli", "fixture Worker started"); err != nil {
				t.Fatal(err)
			}
			task, err := db.TaskByID(ctx, project.ID, taskID)
			if err != nil {
				t.Fatal(err)
			}
			active[projectIndex] = append(active[projectIndex], task)
		}
	}

	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(ctx, `UPDATE projects SET last_activity_at=last_activity_at`); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = writer.Rollback()
		close(released)
	}()

	start := make(chan struct{})
	errorsCh := make(chan error, 64)
	var wg sync.WaitGroup
	for projectIndex, project := range projects {
		projectIndex, project := projectIndex, project
		for _, task := range active[projectIndex] {
			task := task
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for signal := 0; signal < 5; signal++ {
					if _, err := db.RecordWorkerSignal(ctx, task, "working", fmt.Sprintf("signal-%d-%d", task.ID, signal), nil, ""); err != nil {
						errorsCh <- err
						return
					}
				}
			}()
		}
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", Title: "Queued", ShortName: "queued", State: StateQueued}, func(int) (bool, error) { return false, nil })
			if err != nil {
				errorsCh <- err
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			for pass := 0; pass < 10; pass++ {
				if _, err := db.Tasks(ctx, project.ID, true); err != nil {
					errorsCh <- err
					return
				}
				if _, err := db.QueuedTaskIDs(ctx, project.ID); err != nil {
					errorsCh <- err
					return
				}
				if _, err := db.ActiveWorkerCount(ctx, project.ID); err != nil {
					errorsCh <- err
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	<-released
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent Project activity: %v", err)
		}
	}

	deadline := time.Now().Add(30 * time.Second)
	for pass := 0; pass < 2; pass++ {
		for projectIndex, project := range projects {
			tasks, err := db.Tasks(ctx, project.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			queuedIDs, err := db.QueuedTaskIDs(ctx, project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 5 || len(queuedIDs) != 1 || time.Now().After(deadline) {
				t.Fatalf("Project %s pass %d has Tasks=%d queued=%d after %s", project.Name, pass+1, len(tasks), len(queuedIDs), 30*time.Second)
			}
			if pass == 0 {
				var decisions sync.WaitGroup
				decisionErrors := make(chan error, 8)
				for range 8 {
					decisions.Add(1)
					go func(taskID int64) {
						defer decisions.Done()
						_, err := db.RaiseDecision(ctx, DecisionRequest{ProjectID: project.ID, TaskID: taskID, Origin: "dependency:stress", Kind: "dependency", Question: "Resolve the queued prerequisite", Options: []string{"cancel", "continue"}})
						if err != nil {
							decisionErrors <- err
						}
					}(queuedIDs[0])
				}
				decisions.Wait()
				close(decisionErrors)
				for err := range decisionErrors {
					if err != nil {
						t.Fatalf("concurrent Decision admission: %v", err)
					}
				}
				pending, err := db.Decisions(ctx, project.ID, false)
				if err != nil || len(pending) != 1 || pending[0].TaskID != queuedIDs[0] {
					t.Fatalf("Project %s pending Decisions=%#v err=%v, want one for queued Task %d", project.Name, pending, err, queuedIDs[0])
				}
			}
			for _, task := range active[projectIndex] {
				current, err := db.TaskByID(ctx, project.ID, task.ID)
				if err != nil || current.State != StateWorking {
					t.Fatalf("active Task %d state=%s err=%v", task.ID, current.State, err)
				}
				signals, err := db.TaskSignals(ctx, task.ID, 100)
				if err != nil || len(signals) != 5 {
					t.Fatalf("active Task %d signals=%d err=%v, want five", task.ID, len(signals), err)
				}
				seen := make(map[string]bool, len(signals))
				for _, signal := range signals {
					if seen[signal.Note] {
						t.Fatalf("active Task %d duplicated Signal %q", task.ID, signal.Note)
					}
					seen[signal.Note] = true
				}
			}
		}
	}
}

// A bounded autocommit write, such as the Watch progress heartbeat, waits on
// the lock in short steps but must keep retrying until its own deadline, as
// bounded transactions do, rather than fail after the first short wait.
func TestBoundedExecRetriesContentionWithinItsDeadline(t *testing.T) {
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
	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(ctx, `UPDATE projects SET name=name WHERE id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(3 * ObservationWriteBusyTimeout)
		_ = writer.Rollback()
	}()
	boundedCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := db.ExecContext(boundedCtx, `UPDATE projects SET last_activity_at=? WHERE id=?`, 1, project.ID); err != nil {
		t.Fatalf("bounded write behind a %s writer failed within its 1s deadline: %v", 3*ObservationWriteBusyTimeout, err)
	}
	<-released

	writer, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, `UPDATE projects SET name=name WHERE id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	shortCtx, cancelShort := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancelShort()
	started := time.Now()
	if _, err := db.ExecContext(shortCtx, `UPDATE projects SET last_activity_at=? WHERE id=?`, 2, project.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("bounded write behind a held writer = %v, want retryable ErrBusy", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded write waited %s past its 400ms deadline", elapsed)
	}
}

// Every reconciliation ensures the lease of every held Sandbox. Confirming an
// existing lease must not queue behind another writer for the write lock.
func TestEnsuringExistingSandboxLeaseDoesNotWaitForWriter(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Lease", MergeMode: "local", AutonomyReview: "ask", AutonomyMerge: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := db.AcquireSandbox(ctx, project.ID, taskID, filepath.Join(t.TempDir(), "pool"))
	if err != nil {
		t.Fatal(err)
	}
	request := ResourceLeaseRequest{ResourceType: ResourcePath, ScopePath: sandbox.Path, OwnerProjectID: project.ID, OwnerTaskID: taskID, Owner: "shop sandbox-1 (t1)", SourceKey: fmt.Sprintf("sandbox:%d", sandbox.ID), SandboxID: sandbox.ID}
	created, err := db.EnsureSandboxResourceLease(ctx, request)
	if err != nil {
		t.Fatal(err)
	}

	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, `UPDATE projects SET name=name WHERE id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	boundedCtx, cancel := context.WithTimeout(ctx, ObservationWriteBusyTimeout)
	defer cancel()
	ensured, err := db.EnsureSandboxResourceLease(boundedCtx, request)
	if err != nil {
		t.Fatalf("ensuring an existing Sandbox lease waited for the write lock: %v", err)
	}
	if ensured.ID != created.ID || ensured.CustodyToken != created.CustodyToken {
		t.Fatalf("ensured lease = %#v, want existing %#v", ensured, created)
	}
}

func TestDeferralEndsOnlyAtSendClaims(t *testing.T) {
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Deferral", MergeMode: "local", AutonomyReview: "ask", AutonomyMerge: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := db.QueueMessage(ctx, taskID, "go", false)
	if err != nil {
		t.Fatal(err)
	}
	deferred := WithDeferrableWrites(ctx)
	if _, err := db.ExecContext(deferred, `UPDATE projects SET last_activity_at=? WHERE id=?`, 1, project.ID); err != nil {
		t.Fatal(err)
	}
	holdWriter := func(hold time.Duration) {
		writer, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.ExecContext(ctx, `UPDATE projects SET name=name WHERE id=?`, project.ID); err != nil {
			t.Fatal(err)
		}
		go func() {
			time.Sleep(hold)
			_ = writer.Rollback()
		}()
	}
	holdWriter(4 * ObservationWriteBusyTimeout)
	if _, err := db.ExecContext(deferred, `UPDATE projects SET last_activity_at=? WHERE id=?`, 2, project.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("observation write after an unrelated write = %v, want ErrBusy after one short wait", err)
	}
	time.Sleep(4 * ObservationWriteBusyTimeout)

	if claimed, err := db.ClaimMessage(deferred, messageID, "token", time.Now().UnixMilli(), 0); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	holdWriter(3 * ObservationWriteBusyTimeout)
	if _, err := db.ExecContext(deferred, `UPDATE projects SET last_activity_at=? WHERE id=?`, 3, project.ID); err != nil {
		t.Fatalf("write after a send claim must wait for the lock: %v", err)
	}
}
