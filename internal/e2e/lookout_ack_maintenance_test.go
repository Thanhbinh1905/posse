//go:build e2e

package e2e

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLookoutKeepsWatchingAfterRetryableForgeProbeTimeout(t *testing.T) {
	f, noticeID, probeLog := newRetryableForgeLookoutFixture(t)
	defer f.db.Close()
	ctx := context.Background()
	glab := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + probeLog + "\"\ncase \"$1 $2\" in\n  'auth status')\n    count=$(grep -c '^auth status' \"" + probeLog + "\")\n    if [ \"$count\" -eq 1 ]; then exec sleep 3; fi\n    sleep 0.7\n    exit 0 ;;\n  'api --hostname')\n    case \"$*\" in\n      *approvals*) printf '%s\\n' '{\"approved\":false,\"approvals_left\":1}' ;;\n      *) printf '%s\\n' '{\"web_url\":\"https://git.paas.vn/acme/shop/-/merge_requests/17\",\"title\":\"Retry forge probe\",\"state\":\"opened\",\"sha\":\"1111111111111111111111111111111111111111\",\"source_branch\":\"posse/t1\",\"target_branch\":\"main\",\"detailed_merge_status\":\"not_approved\",\"has_conflicts\":false,\"head_pipeline\":{\"status\":\"running\"}}' ;;\n    esac ;;\n  *) exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(f.root, "bin", "glab"), []byte(glab), 0o700); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(f.binary, "lookout", "--ack", strconv.FormatInt(noticeID, 10), "--timeout", "7000")
	command.Dir, command.Env = f.repo, f.leadEnv
	outputPath := filepath.Join(f.root, "lookout-output.txt")
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		err := command.Wait()
		_ = output.Close()
		done <- err
	}()
	commandRunning := true
	defer func() {
		if commandRunning {
			_ = command.Process.Kill()
			<-done
		}
	}()
	if !waitForCondition(2*time.Second, func() bool {
		return noticeAckedAt(t, f.db, f.project.ID, noticeID) != 0
	}) {
		t.Fatal("Lookout did not commit the requested acknowledgement")
	}
	if !waitForCondition(2*time.Second, func() bool {
		_, err := os.Stat(probeLog)
		return err == nil
	}) {
		t.Fatal("Lookout did not start the slow fake forge probe")
	}
	if !waitForCondition(4*time.Second, func() bool {
		contents, err := os.ReadFile(probeLog)
		return err == nil && strings.Count(string(contents), "auth status") >= 2
	}) {
		select {
		case err := <-done:
			commandRunning = false
			contents, _ := os.ReadFile(outputPath)
			t.Fatalf("Lookout exited instead of retrying the forge probe: %v\n%s", err, contents)
		default:
			t.Fatal("Lookout did not retry the forge probe on the next PR poll")
		}
	}
	taskID := retryableLookoutTaskID(t, f.db, f.project.ID)
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "pr_watch_failing" {
			t.Fatalf("one transient forge timeout created a pr_watch_failing Notice: %#v", notices)
		}
	}
	select {
	case err := <-done:
		commandRunning = false
		if err != nil {
			contents, _ := os.ReadFile(outputPath)
			t.Fatalf("Lookout failed after a transient forge timeout: %v\n%s", err, contents)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Lookout did not honor its bounded timeout after recovering the forge probe")
	}
	contents, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "acknowledgement committed: 1") || !strings.Contains(string(contents), "acknowledged: 1") {
		t.Fatalf("Lookout did not report the committed acknowledgement: %s", contents)
	}
	if failures := prTaskWatchFailureCount(t, f.db, taskID); failures != 0 {
		t.Fatalf("successful forge observation did not reset the failure streak: %d", failures)
	}
	notices, err = f.db.Notices(ctx, f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "pr_watch_failing" {
			t.Fatalf("recovered forge probe left a spurious pr_watch_failing Notice: %#v", notices)
		}
	}
}

func TestLookoutReportsPersistentForgeProbeFailureAfterThreePolls(t *testing.T) {
	f, noticeID, probeLog := newRetryableForgeLookoutFixture(t)
	defer f.db.Close()
	glab := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + probeLog + "\"\ncase \"$1 $2\" in\n  'auth status') exec sleep 3 ;;\n  *) exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(f.root, "bin", "glab"), []byte(glab), 0o700); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(f.binary, "lookout", "--ack", strconv.FormatInt(noticeID, 10), "--timeout", "15000")
	command.Dir, command.Env = f.repo, f.leadEnv
	outputPath := filepath.Join(f.root, "lookout-output.txt")
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		err := command.Wait()
		_ = output.Close()
		done <- err
	}()
	commandRunning := true
	defer func() {
		if commandRunning {
			_ = command.Process.Kill()
			<-done
		}
	}()
	if !waitForCondition(2*time.Second, func() bool {
		return noticeAckedAt(t, f.db, f.project.ID, noticeID) != 0
	}) {
		t.Fatal("Lookout did not commit the requested acknowledgement")
	}
	taskID := retryableLookoutTaskID(t, f.db, f.project.ID)
	for failure := 1; failure <= 3; failure++ {
		wantCalls := failure
		if !waitForCondition(4*time.Second, func() bool {
			contents, err := os.ReadFile(probeLog)
			return err == nil && strings.Count(string(contents), "auth status") >= wantCalls
		}) {
			select {
			case err := <-done:
				commandRunning = false
				contents, _ := os.ReadFile(outputPath)
				t.Fatalf("Lookout exited before forge failure %d: %v\n%s", failure, err, contents)
			default:
				t.Fatalf("Lookout did not run forge probe %d", failure)
			}
		}
		if !waitForCondition(4*time.Second, func() bool {
			return prTaskWatchFailureCount(t, f.db, taskID) >= failure
		}) {
			t.Fatalf("Task failure streak did not reach %d", failure)
		}
		if failure < 3 {
			notices, err := f.db.Notices(context.Background(), f.project.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, notice := range notices {
				if notice.Kind == "pr_watch_failing" {
					t.Fatalf("failure %d raised a premature pr_watch_failing Notice: %#v", failure, notice)
				}
			}
			select {
			case err := <-done:
				commandRunning = false
				contents, _ := os.ReadFile(outputPath)
				t.Fatalf("Lookout exited after forge failure %d of 3: %v\n%s", failure, err, contents)
			default:
			}
		}
	}
	select {
	case err := <-done:
		commandRunning = false
		if err != nil {
			contents, _ := os.ReadFile(outputPath)
			t.Fatalf("Lookout failed after persistent forge errors: %v\n%s", err, contents)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Lookout did not return the persistent forge failure Notice")
	}
	contents, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "pr_watch_failing") || !strings.Contains(string(contents), "acknowledgement committed: 1") {
		t.Fatalf("persistent failure or acknowledgement outcome missing from Lookout output: %s", contents)
	}
	if failures := prTaskWatchFailureCount(t, f.db, taskID); failures != 3 {
		t.Fatalf("persistent Task failure count = %d; want three", failures)
	}
	state, err := f.db.ProjectWatchState(context.Background(), f.project.ID)
	if err != nil || state.PRConsecutiveFailures != 0 {
		t.Fatalf("Task-scoped forge failures leaked into the Project streak: %#v, %v", state, err)
	}
}

func retryableLookoutTaskID(t *testing.T, db *store.DB, projectID int64) int64 {
	t.Helper()
	var taskID int64
	if err := db.QueryRowContext(context.Background(), `SELECT id FROM tasks WHERE project_id=? AND title='Retry forge probe'`, projectID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	return taskID
}

func prTaskWatchFailureCount(t *testing.T, db *store.DB, taskID int64) int {
	t.Helper()
	var failures int
	if err := db.QueryRowContext(context.Background(), `SELECT COALESCE((SELECT consecutive_failures FROM pr_task_watch_failures WHERE task_id=? AND repo=''), 0)`, taskID).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	return failures
}

func newRetryableForgeLookoutFixture(t *testing.T) (*prLifecycleFixture, int64, string) {
	t.Helper()
	f := newPRLifecycleFixture(t)
	stopBackgroundLookout(t, f)
	return seedRetryableForgeLookoutFixture(t, f)
}

func seedRetryableForgeLookoutFixture(t *testing.T, f *prLifecycleFixture) (*prLifecycleFixture, int64, string) {
	t.Helper()
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("[defaults]\nlanding_mode = \"pr\"\nforge = \"auto\"\npr_poll = \"1ms\"\nauto_unsaddle = \"never\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, f.repo, "remote", "set-url", "origin", "https://git.paas.vn/acme/shop.git")
	gitTest(t, f.env, f.repo, "config", "url.file://"+f.remote+".insteadOf", "https://git.paas.vn/acme/shop.git")
	taskID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{
		Seq: 1, Type: "ship", Title: "Retry forge probe", LandingMode: "pr", Branch: "posse/t1",
		PRURL: "https://git.paas.vn/acme/shop/-/merge_requests/17",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		from, to store.State
		source   string
	}{
		{store.StateSpawning, store.StateWorking, "cli"},
		{store.StateWorking, store.StateFailed, "worker"},
	} {
		if err := f.db.Transition(ctx, taskID, transition.from, transition.to, transition.source, "test"); err != nil {
			t.Fatal(err)
		}
	}
	task, err := f.db.TaskByID(ctx, f.project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.RecordPRObservation(ctx, store.PRObservation{
		ProjectID: f.project.ID, TaskID: taskID, PRURL: task.PRURL, HeadSHA: "1111111111111111111111111111111111111111",
		State: "OPEN", Checks: `{"state":"PENDING","failures":[]}`, Review: "REVIEW_REQUIRED", Mergeable: "UNKNOWN", ObservedAt: time.Now().UnixMilli(),
	}, store.PRObservationEffect{}, task); err != nil {
		t.Fatal(err)
	}
	noticeID, err := f.db.CreateNotice(ctx, store.Notice{
		ProjectID: f.project.ID, Kind: "worker_blocked", Summary: "ack before forge probing", DataJSON: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.MarkNoticesDelivered(ctx, f.project.ID, []int64{noticeID}, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	return f, noticeID, filepath.Join(f.root, "glab-probes.log")
}

func TestLookoutAckCommitsBeforeUnrelatedTeardown(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()

	ctx := context.Background()
	stopBackgroundLookout(t, f)

	maintenanceTaskID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{
		Seq: 90, Type: "ship", Title: "Unrelated landed Task", LandingMode: "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		from, to store.State
		source   string
		note     string
	}{
		{store.StateSpawning, store.StateWorking, "cli", "started"},
		{store.StateWorking, store.StateDone, "worker", "finished"},
		{store.StateDone, store.StateLanding, "cli", "ready to land"},
		{store.StateLanding, store.StateLanded, "cli", "landed"},
	} {
		if err := f.db.Transition(ctx, maintenanceTaskID, transition.from, transition.to, transition.source, transition.note); err != nil {
			t.Fatal(err)
		}
	}
	// Keep the unrelated automatic Teardown active so shared Project preparation
	// reaches its retryable teardown_in_progress failure before acknowledging.
	if err := f.db.StartIntent(ctx, f.project.ID, maintenanceTaskID, "unsaddle", "in_progress:panes.close", `{"discard":false}`, os.Getpid()); err != nil {
		t.Fatalf("start unrelated automatic Teardown: %v", err)
	}
	intent, err := f.db.IntentByTask(ctx, maintenanceTaskID)
	if err != nil {
		t.Fatal(err)
	}

	requestedID, err := f.db.CreateNotice(ctx, store.Notice{
		ProjectID: f.project.ID, Kind: "worker_blocked", Summary: "requested acknowledgement", DataJSON: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	unrelatedID, err := f.db.CreateNotice(ctx, store.Notice{
		ProjectID: f.project.ID, Kind: "needs_decision", Summary: "leave this Notice open", DataJSON: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.MarkNoticesDelivered(ctx, f.project.ID, []int64{requestedID, unrelatedID}, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(f.binary, "lookout", "--ack", strconv.FormatInt(requestedID, 10), "--timeout", "30000")
	command.Dir, command.Env = f.repo, f.leadEnv
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	commandRunning := true
	defer func() {
		if commandRunning {
			_ = command.Process.Kill()
			<-done
		}
	}()

	if !waitForCondition(2*time.Second, func() bool {
		return noticeAckedAt(t, f.db, f.project.ID, requestedID) != 0
	}) {
		select {
		case err := <-done:
			commandRunning = false
			t.Fatalf("Lookout exited before committing the requested acknowledgement: %v", err)
		default:
			t.Fatal("Lookout did not commit the requested acknowledgement before unrelated Teardown maintenance")
		}
	}
	if noticeAckedAt(t, f.db, f.project.ID, unrelatedID) != 0 {
		t.Fatal("Lookout acknowledged a Notice that was not requested")
	}
	select {
	case err := <-done:
		commandRunning = false
		t.Fatalf("Lookout exited instead of remaining armed after its acknowledgement: %v", err)
	default:
	}

	ackedAt := noticeAckedAt(t, f.db, f.project.ID, requestedID)
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done
	commandRunning = false
	if got := noticeAckedAt(t, f.db, f.project.ID, requestedID); got != ackedAt {
		t.Fatalf("acknowledgement changed after interrupt: before=%d after=%d", ackedAt, got)
	}

	retry := exec.Command(f.binary, "lookout", "--ack", strconv.FormatInt(requestedID, 10), "--timeout", "15000")
	retry.Dir, retry.Env = f.repo, f.leadEnv
	output, err := retry.CombinedOutput()
	if err != nil {
		t.Fatalf("retry acknowledged Notice and report unrelated maintenance failure: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "Lookout maintenance failed (retryable)") {
		t.Fatalf("unrelated Teardown failure was not surfaced as a retryable Notice: %s", output)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var maintenanceNotice *store.Notice
	for i := range notices {
		if notices[i].Kind == "pr_watch_failing" && strings.Contains(notices[i].Summary, "Lookout maintenance failed (retryable)") {
			maintenanceNotice = &notices[i]
			break
		}
	}
	if maintenanceNotice == nil || maintenanceNotice.DeliveredAt == 0 || maintenanceNotice.AckedAt != 0 {
		t.Fatalf("maintenance failure Notice state = %#v, want delivered and still open", maintenanceNotice)
	}
	if err := f.db.FinishIntent(ctx, intent.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	finalRetry := exec.Command(f.binary, "lookout", "--ack", strconv.FormatInt(requestedID, 10), "--timeout", "100")
	finalRetry.Dir, finalRetry.Env = f.repo, f.leadEnv
	output, err = finalRetry.CombinedOutput()
	if err != nil {
		t.Fatalf("re-arm Lookout after repairing unrelated Teardown: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "state: timeout") {
		t.Fatalf("re-armed Lookout did not enter its wait loop: %s", output)
	}
	if got := noticeAckedAt(t, f.db, f.project.ID, requestedID); got != ackedAt {
		t.Fatalf("retry changed the successful acknowledgement: before=%d after=%d", ackedAt, got)
	}
	if noticeAckedAt(t, f.db, f.project.ID, unrelatedID) != 0 {
		t.Fatal("retry acknowledged a Notice that was not requested")
	}
	maintenanceTask, err := f.db.TaskByID(ctx, f.project.ID, maintenanceTaskID)
	if err != nil || maintenanceTask.State != store.StateTornDown {
		t.Fatalf("unrelated automatic Teardown was not retried after its owner finished: task=%#v err=%v", maintenanceTask, err)
	}
}

// Stop the fixture's independent poll-only Lookout when a test needs to isolate
// one command from competing message delivery or Teardown maintenance.
func stopBackgroundLookout(t *testing.T, f *prLifecycleFixture) {
	t.Helper()
	stopFixtureBackgroundLookout(t, f.root, f.env, f.db, f.project.ID)
}

func pauseFixtureBackgroundLookout(t *testing.T, root string, env []string, db *store.DB, projectID int64) {
	t.Helper()
	for _, pid := range lookoutPIDs(root) {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("stop background Lookout: %v", err)
		}
	}
	if !waitForCondition(5*time.Second, func() bool { return len(lookoutPIDs(root)) == 0 }) {
		t.Fatalf("background Lookout did not stop: %v", lookoutPIDs(root))
	}
	state, err := db.LookoutRecovery(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	state.RetryAt = time.Now().Add(time.Hour).UnixMilli()
	if err := db.SetLookoutRecovery(context.Background(), projectID, state); err != nil {
		t.Fatal(err)
	}
}

func stopFixtureBackgroundLookout(t *testing.T, root string, env []string, db *store.DB, projectID int64) {
	t.Helper()
	client := herdr.NewWithEnv("herdr", env)
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(root)) == 1 }) {
		t.Fatalf("initial Lookout did not start: %v", lookoutPIDs(root))
	}
	panes := lookoutPanes(t, client)
	if len(panes) != 1 {
		t.Fatalf("initial Lookout tabs = %d, want 1", len(panes))
	}
	for _, pid := range lookoutPIDs(root) {
		// SIGTERM lets active maintenance release its poll leases before exit.
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			t.Fatalf("stop background Lookout: %v", err)
		}
	}
	if !waitForCondition(5*time.Second, func() bool { return len(lookoutPIDs(root)) == 0 }) {
		t.Fatalf("background Lookout did not stop: %v", lookoutPIDs(root))
	}
	state, err := db.LookoutRecovery(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	// Foreground observation must not restart the independent poll-only process.
	state.RetryAt = time.Now().Add(time.Hour).UnixMilli()
	if err := db.SetLookoutRecovery(context.Background(), projectID, state); err != nil {
		t.Fatal(err)
	}
}

func noticeAckedAt(t *testing.T, db *store.DB, projectID, noticeID int64) int64 {
	t.Helper()
	var ackedAt int64
	if err := db.QueryRowContext(context.Background(), `SELECT COALESCE(acked_at, 0) FROM notices WHERE project_id=? AND id=?`, projectID, noticeID).Scan(&ackedAt); err != nil {
		t.Fatal(err)
	}
	return ackedAt
}
