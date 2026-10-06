//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestFailedLandIntentDoesNotPoisonNextCLICommand(t *testing.T) {
	harness := newIntentCLIHarness(t)
	fixture := harness.newProject(t, "land-failure")
	taskID, err := fixture.addShipTask(t, store.StateDone)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.repo, "user-edit.txt"), []byte("uncommitted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output := fixture.run(t, "", "land", taskID, "--merge", "--user-approved", "User approved this merge")
	if code == 0 || !strings.Contains(output, "land_target_busy") {
		t.Fatalf("land with an uncommitted Project edit = %d, %s", code, output)
	}
	code, output = fixture.run(t, "", "roster")
	if code != 0 {
		t.Fatalf("roster failed after the Land command returned: code=%d output=%s", code, output)
	}
	if err := fixture.openDB(t, func(db *store.DB) error {
		if _, err := db.IntentByTask(context.Background(), fixture.taskDBID); !store.IsNotFound(err) {
			return fmt.Errorf("failed Land left an intent: %v", err)
		}
		task, err := db.TaskByID(context.Background(), fixture.project.ID, fixture.taskDBID)
		if err != nil {
			return err
		}
		if task.State != store.StateDone {
			return fmt.Errorf("failed Land left Task in %s, want done", task.State)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLocalLandReportsConcurrentTeardownOutcomeExactlyOnce(t *testing.T) {
	harness := newIntentCLIHarness(t)
	for _, scenario := range []struct {
		name         string
		failTeardown bool
	}{
		{name: "competing teardown fails", failTeardown: true},
		{name: "competing teardown finishes before claim"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := harness.newProject(t, strings.ReplaceAll(scenario.name, " ", "-"))
			taskID, err := fixture.addShipTask(t, store.StateDone)
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.attachTaskPane(t, taskID, false); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(fixture.home, "config.toml")
			configText, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			configText = []byte(strings.Replace(string(configText), `auto_unsaddle = "never"`, `auto_unsaddle = "finished"`, 1))
			if err := os.WriteFile(configPath, configText, 0o600); err != nil {
				t.Fatal(err)
			}

			startCLI := func(env []string, args ...string) (*bytes.Buffer, <-chan error) {
				t.Helper()
				command := exec.Command(fixture.harness.binary, args...)
				command.Dir, command.Env = fixture.repo, env
				output := &bytes.Buffer{}
				command.Stdout, command.Stderr = output, output
				if err := command.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = command.Process.Kill() })
				done := make(chan error, 1)
				go func() { done <- command.Wait() }()
				return output, done
			}
			waitCLI := func(name string, done <-chan error, output *bytes.Buffer) error {
				t.Helper()
				select {
				case err := <-done:
					return err
				case <-time.After(30 * time.Second):
					t.Fatalf("%s timed out: %s", name, output.String())
					return nil
				}
			}
			waitMarker := func(marker string) {
				t.Helper()
				if !waitForCondition(20*time.Second, func() bool { _, err := os.Stat(marker); return err == nil }) {
					t.Fatalf("timed out waiting for pause marker %s", marker)
				}
			}
			continueAt := func(marker string) {
				t.Helper()
				if err := os.WriteFile(marker+".continue", []byte("continue\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			landMarker := filepath.Join(fixture.root, "land-teardown-ready")
			landEnv := setEnv(fixture.env, "POSSE_INTENT_PAUSE_AT", "land:after:teardown.ready")
			landEnv = setEnv(landEnv, "POSSE_INTENT_PAUSE_FILE", landMarker)
			landOutput, landDone := startCLI(landEnv, "land", taskID, "--merge", "--user-approved", "User approved this local merge")
			waitMarker(landMarker)

			var landErr error
			if scenario.failTeardown {
				otherMarker := filepath.Join(fixture.root, "automatic-teardown-ready")
				otherEnv := setEnv(fixture.env, "POSSE_INTENT_PAUSE_AT", "unsaddle:after:panes.close")
				otherEnv = setEnv(otherEnv, "POSSE_INTENT_PAUSE_FILE", otherMarker)
				otherOutput, otherDone := startCLI(otherEnv, "roster")
				waitMarker(otherMarker)
				continueAt(landMarker)
				time.Sleep(time.Second)
				var task store.Task
				if err := fixture.openDB(t, func(db *store.DB) error {
					var readErr error
					task, readErr = db.Task(context.Background(), fixture.project.ID, taskID)
					return readErr
				}); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(task.WorktreePath, ".git"), []byte("invalid git worktree metadata\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				continueAt(otherMarker)
				if err := waitCLI("automatic Teardown", otherDone, otherOutput); err != nil {
					t.Fatalf("automatic Teardown reconcile failed: %v output=%s", err, otherOutput.String())
				}
				landErr = waitCLI("local Land", landDone, landOutput)
			} else {
				if code, output := fixture.run(t, "", "roster"); code != 0 {
					t.Fatalf("automatic Teardown failed: code=%d output=%s", code, output)
				}
				continueAt(landMarker)
				landErr = waitCLI("local Land", landDone, landOutput)
			}
			if (landErr != nil) != scenario.failTeardown {
				t.Fatalf("local Land error=%v, want error=%v; output=%s", landErr, scenario.failTeardown, landOutput.String())
			}
			teardownSuccessCount := strings.Count(landOutput.String(), "teardown: torn-down")
			wantTeardownSuccessCount := 1
			if scenario.failTeardown {
				wantTeardownSuccessCount = 0
			}
			if teardownSuccessCount != wantTeardownSuccessCount {
				t.Fatalf("Land teardown success count=%d, want %d: %s", teardownSuccessCount, wantTeardownSuccessCount, landOutput.String())
			}

			var task store.Task
			var mount store.Mount
			var landedCount, tornDownCount, approvalCount, intentCount, incompleteCount int
			if err := fixture.openDB(t, func(db *store.DB) error {
				var err error
				task, err = db.Task(context.Background(), fixture.project.ID, taskID)
				if err != nil {
					return err
				}
				mounts, err := db.Mounts(context.Background(), fixture.project.ID)
				if err != nil {
					return err
				}
				for _, candidate := range mounts {
					if candidate.Path == task.WorktreePath {
						mount = candidate
						break
					}
				}
				query := `SELECT
				 (SELECT COUNT(*) FROM transitions WHERE task_id=? AND to_state='landed'),
				 (SELECT COUNT(*) FROM transitions WHERE task_id=? AND to_state='torn-down'),
				 (SELECT COUNT(*) FROM approvals WHERE task_id=? AND action='merge'),
				 (SELECT COUNT(*) FROM intents WHERE task_id=?),
				 (SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='unsaddle_incomplete')`
				return db.QueryRowContext(context.Background(), query, task.ID, task.ID, task.ID, task.ID, task.ID).Scan(&landedCount, &tornDownCount, &approvalCount, &intentCount, &incompleteCount)
			}); err != nil {
				t.Fatal(err)
			}
			wantState, wantMount, wantTornDown, wantIncomplete := store.StateTornDown, "idle", 1, 0
			if scenario.failTeardown {
				wantState, wantMount, wantTornDown, wantIncomplete = store.StateLanded, "broken", 0, 1
			}
			if task.State != wantState || mount.State != wantMount || landedCount != 1 || tornDownCount != wantTornDown || approvalCount != 1 || intentCount != 0 || incompleteCount != wantIncomplete {
				t.Fatalf("unexpected exactly-once outcome: state=%s mount=%s landed=%d torn-down=%d approvals=%d intents=%d incomplete=%d", task.State, mount.State, landedCount, tornDownCount, approvalCount, intentCount, incompleteCount)
			}
		})
	}
}

func TestRideCrashImmediatelyAfterTaskCreationIsRecovered(t *testing.T) {
	harness := newIntentCLIHarness(t)
	fixture := harness.newProject(t, "ride-crash")
	brief := filepath.Join(fixture.root, "brief.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: Crash recovery\ndone_when: the recovery reaches a stable state\n---\nCreate a small change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output := fixture.run(t, "ride:after:task.create", "ride", "--brief", brief, "--name", "crash-recovery")
	if code != 86 {
		t.Fatalf("ride did not exit at the task-create crash point: code=%d output=%s", code, output)
	}
	code, output = fixture.run(t, "", "roster")
	if code != 0 {
		t.Fatalf("roster failed while recovering the interrupted ride: code=%d output=%s", code, output)
	}
	if err := fixture.openDB(t, func(db *store.DB) error {
		intents, err := db.Intents(context.Background(), fixture.project.ID)
		if err != nil {
			return err
		}
		if len(intents) != 0 {
			return fmt.Errorf("ride recovery left %d intent(s)", len(intents))
		}
		task, err := db.Task(context.Background(), fixture.project.ID, "t1")
		if err != nil {
			return err
		}
		if task.State != store.StateFailed {
			return fmt.Errorf("ride recovery left Task in %s, want failed", task.State)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStartupAndExplicitRecoveryDoNotDoubleRelaunch(t *testing.T) {
	harness := newIntentCLIHarness(t)
	fixture := harness.newProject(t, "concurrent-recovery")
	_, err := fixture.addShipTask(t, store.StateWorking)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.attachTaskPane(t, "t1", true); err != nil {
		t.Fatal(err)
	}
	if code, output := fixture.run(t, "", "roster"); code != 0 {
		t.Fatalf("initial reconcile failed: code=%d output=%s", code, output)
	}
	var before store.Task
	if err := fixture.openDB(t, func(db *store.DB) error {
		var taskErr error
		before, taskErr = db.Task(context.Background(), fixture.project.ID, "t1")
		return taskErr
	}); err != nil {
		t.Fatal(err)
	}
	if before.AgentServerStartedAt != harness.session.serverAt || before.AgentName == "" {
		t.Fatalf("initial Worker generation was not recorded: %#v server=%q", before, harness.session.serverAt)
	}
	oldGeneration := harness.session.serverAt
	newGeneration := oldGeneration + "-after-restart"
	gate := make(chan struct{})
	harness.session.mu.Lock()
	harness.session.serverAt = newGeneration
	harness.session.agentStartGate = gate
	startsBefore := len(harness.session.agentStartNames)
	harness.session.mu.Unlock()
	for {
		select {
		case <-harness.session.requests:
		default:
			goto requestsDrained
		}
	}

requestsDrained:
	startRecover := func() (*exec.Cmd, *bytes.Buffer, <-chan error) {
		command := exec.Command(harness.binary, "recover", "--all")
		command.Dir = fixture.repo
		command.Env = fixture.env
		output := &bytes.Buffer{}
		command.Stdout, command.Stderr = output, output
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		return command, output, done
	}
	_, startupOutput, startupDone := startRecover()
	waitForRecoveryRequest(t, harness.session.requests, "agent.start")
	_, explicitOutput, explicitDone := startRecover()
	waitForRecoveryRequest(t, harness.session.requests, "session.snapshot")
	close(gate)
	results := []struct {
		label string
		done  <-chan error
		out   *bytes.Buffer
	}{
		{label: "startup recovery", done: startupDone, out: startupOutput},
		{label: "explicit recover --all", done: explicitDone, out: explicitOutput},
	}
	for _, result := range results {
		select {
		case err := <-result.done:
			if err != nil {
				t.Fatalf("%s failed: %v output=%s", result.label, err, result.out.String())
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s did not finish after the recovery barrier opened: %s", result.label, result.out.String())
		}
	}
	if code, output := fixture.run(t, "", "roster"); code != 0 {
		t.Fatalf("post-recovery reconcile failed: code=%d output=%s", code, output)
	}
	var after store.Task
	var recovery store.ProjectRecovery
	var recoveryNotices int
	if err := fixture.openDB(t, func(db *store.DB) error {
		var taskErr error
		after, taskErr = db.Task(context.Background(), fixture.project.ID, "t1")
		if taskErr != nil {
			return taskErr
		}
		var stateErr error
		recovery, stateErr = db.ProjectRecovery(context.Background(), fixture.project.ID)
		if stateErr != nil {
			return stateErr
		}
		return db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE project_id=? AND kind='recovery'`, fixture.project.ID).Scan(&recoveryNotices)
	}); err != nil {
		t.Fatal(err)
	}
	harness.session.mu.Lock()
	workerStarts := 0
	for _, name := range harness.session.agentStartNames[startsBefore:] {
		if strings.HasPrefix(name, "posse-"+fixture.project.Name+"-t1-") {
			workerStarts++
		}
	}
	harness.session.mu.Unlock()
	if after.Launches != before.Launches+1 || after.AgentServerStartedAt != newGeneration || recovery.OwnerPID != 0 || recovery.Generation != newGeneration || recoveryNotices != 1 || workerStarts != 1 {
		t.Fatalf("overlapping recovery duplicated or lost relaunch: before=%#v after=%#v recovery=%#v notices=%d workerStarts=%d startup=%s explicit=%s", before, after, recovery, recoveryNotices, workerStarts, startupOutput.String(), explicitOutput.String())
	}
}

func waitForRecoveryRequest(t *testing.T, requests <-chan string, method string) {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case got := <-requests:
			if got == method {
				return
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for isolated Herdr request %s", method)
		}
	}
}

func TestRealCLIIntentCrashMatrix(t *testing.T) {
	harness := newIntentCLIHarness(t)
	steps := []struct {
		command string
		step    string
		phase   string
	}{
		{command: "ride", step: "task.create", phase: "before"},
		{command: "ride", step: "task.create", phase: "after"},
	}
	for _, step := range []string{"scratch.create", "mount.acquire", "pane.open", "pane.record", "scratch.environment", "repository.prepare", "agent.sequence", "agent.record", "pane.label", "agent.start", "brief.write", "launch.write", "agent.prompt", "pane.metadata", "task.working"} {
		steps = append(steps, struct {
			command string
			step    string
			phase   string
		}{"ride", step, "before"}, struct {
			command string
			step    string
			phase   string
		}{"ride", step, "after"})
	}
	for _, step := range []string{"gate.record", "gate.run", "task.landing", "notice.create", "approval.record", "merge", "landed_ref.record", "task.landed"} {
		steps = append(steps, struct {
			command string
			step    string
			phase   string
		}{"land --merge", step, "before"}, struct {
			command string
			step    string
			phase   string
		}{"land --merge", step, "after"})
	}
	for _, step := range []string{"approval.record", "discard.capture", "panes.close", "mount.release", "branch.remove", "scratch.remove", "task.torn_down"} {
		steps = append(steps, struct {
			command string
			step    string
			phase   string
		}{"unsaddle", step, "before"}, struct {
			command string
			step    string
			phase   string
		}{"unsaddle", step, "after"})
	}
	for _, step := range []string{"git.inspect", "pane.open", "agent.stop", "pane.label", "scratch.environment", "agent.sequence", "agent.record", "pane.metadata", "agent.start", "relaunch.write", "agent.prompt", "task.working", "task.progress"} {
		steps = append(steps, struct {
			command string
			step    string
			phase   string
		}{"relaunch", step, "before"}, struct {
			command string
			step    string
			phase   string
		}{"relaunch", step, "after"})
	}
	for _, item := range steps {
		item := item
		t.Run(item.command+"/"+item.phase+"/"+item.step, func(t *testing.T) {
			fixture := harness.newProject(t, strings.ReplaceAll(item.command+"-"+item.step+"-"+item.phase, " ", "-"))
			harness.session.mu.Lock()
			workspaceCreates := harness.session.workspaceCreates
			harness.session.mu.Unlock()
			var taskID string
			var branchSHA, defaultSHA string
			var err error
			switch item.command {
			case "ride":
				brief := filepath.Join(fixture.root, "brief.md")
				if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: Crash matrix\ndone_when: the recovery settles the intent\n---\nCreate a small change.\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				taskID = "t1"
				args := []string{"ride", "--brief", brief, "--name", "crash-matrix"}
				crashAt := item.command + ":" + item.phase + ":" + item.step
				code, output := fixture.run(t, crashAt, args...)
				assertCrashExit(t, code, output, crashAt)
			case "land --merge":
				taskID, err = fixture.addShipTask(t, store.StateDone)
				if err != nil {
					t.Fatal(err)
				}
				branchSHA = strings.TrimSpace(gitTest(t, fixture.harness.baseEnv, fixture.repo, "rev-parse", "refs/heads/posse/t1"))
				defaultSHA = strings.TrimSpace(gitTest(t, fixture.harness.baseEnv, fixture.repo, "rev-parse", "refs/heads/main"))
				crashAt := item.command + ":" + item.phase + ":" + item.step
				code, output := fixture.run(t, crashAt, "land", taskID, "--merge", "--user-approved", "User approved the test merge")
				assertCrashExit(t, code, output, crashAt)
			case "unsaddle":
				state := store.StateFailed
				if item.step == "scratch.remove" {
					state = store.StateLost
				}
				taskID, err = fixture.addShipTask(t, state)
				if err != nil {
					t.Fatal(err)
				}
				scratch := filepath.Join(fixture.home, "scratch", fixture.project.Name, "t1")
				if err := os.MkdirAll(scratch, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(scratch, "teardown.tmp"), []byte("remove after teardown\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				branchSHA = strings.TrimSpace(gitTest(t, fixture.harness.baseEnv, fixture.repo, "rev-parse", "refs/heads/posse/t1"))
				if err := fixture.attachTaskPane(t, "t1", true); err != nil {
					t.Fatal(err)
				}
				crashAt := item.command + ":" + item.phase + ":" + item.step
				code, output := fixture.run(t, crashAt, "unsaddle", taskID, "--discard", "--user-approved", "User approved discard")
				assertCrashExit(t, code, output, crashAt)
				if item.step == "mount.release" && item.phase == "after" {
					if err := fixture.openDB(t, func(db *store.DB) error {
						var savedSHA string
						if err := db.QueryRowContext(context.Background(), `SELECT branch_sha FROM approvals WHERE task_id=? AND action='discard' ORDER BY at DESC,id DESC LIMIT 1`, fixture.taskDBID).Scan(&savedSHA); err != nil {
							return err
						}
						if savedSHA != branchSHA {
							return fmt.Errorf("discard approval saved branch tip %q, want %q before Mount release", savedSHA, branchSHA)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
			case "relaunch":
				initialState := store.StateLost
				if item.step == "task.progress" {
					initialState = store.StateWorking
				}
				taskID, err = fixture.addShipTask(t, initialState)
				if err != nil {
					t.Fatal(err)
				}
				if item.step != "pane.open" {
					if err := fixture.attachTaskPane(t, "t1", true); err != nil {
						t.Fatal(err)
					}
				}
				crashAt := item.command + ":" + item.phase + ":" + item.step
				code, output := fixture.run(t, crashAt, "relaunch", taskID)
				assertCrashExit(t, code, output, crashAt)
			}
			code, output := fixture.run(t, "", "roster")
			if code != 0 {
				t.Fatalf("roster failed after recovery: code=%d output=%s", code, output)
			}
			harness.session.mu.Lock()
			created := harness.session.workspaceCreates - workspaceCreates
			var leftovers []herdr.Pane
			for _, pane := range harness.session.panes {
				if pane.PaneID != fixture.project.LeadPaneID && strings.HasPrefix(pane.CWD, filepath.Join(fixture.home, "remuda")+string(os.PathSeparator)) {
					leftovers = append(leftovers, pane)
				}
			}
			harness.session.mu.Unlock()
			if created != 0 {
				t.Fatalf("%s created %d top-level Herdr workspaces; Riders open as linked worktrees", item.command, created)
			}
			if item.command == "ride" {
				if err := fixture.openDB(t, func(db *store.DB) error {
					task, err := db.Task(context.Background(), fixture.project.ID, taskID)
					if store.IsNotFound(err) {
						task.State = store.StateFailed
					} else if err != nil {
						return err
					}
					// A failed ride leaves no Rider pane in the Lead or a linked child.
					if task.State == store.StateFailed && len(leftovers) != 0 {
						return fmt.Errorf("failed ride left Rider panes: %#v", leftovers)
					}
					if task.State == store.StateWorking && (len(leftovers) != 1 || leftovers[0].Label != task.PaneLabel) {
						return fmt.Errorf("working ride has panes %#v, want its one labeled Rider pane", leftovers)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := fixture.openDB(t, func(db *store.DB) error {
				intents, err := db.Intents(context.Background(), fixture.project.ID)
				if err != nil {
					return err
				}
				if len(intents) != 0 {
					return fmt.Errorf("crash recovery left %d intent(s): %#v", len(intents), intents)
				}
				task, err := db.Task(context.Background(), fixture.project.ID, taskID)
				if item.command == "ride" && item.step == "task.create" && item.phase == "before" {
					if !store.IsNotFound(err) {
						return fmt.Errorf("pre-create crash unexpectedly left a Task: %#v, %v", task, err)
					}
					return nil
				}
				if err != nil {
					return err
				}
				switch item.command {
				case "ride":
					if task.State != store.StateFailed && task.State != store.StateWorking {
						return fmt.Errorf("ride recovery left Task in %s", task.State)
					}
				case "land --merge":
					mergeCompleted := item.step == "merge" && item.phase == "after" || item.step == "landed_ref.record" || item.step == "task.landed"
					wantState := store.StateDone
					if mergeCompleted {
						wantState = store.StateLanded
					}
					if task.State != wantState {
						return fmt.Errorf("Land crash at %s/%s recovered Task to %s, want %s", item.phase, item.step, task.State, wantState)
					}
					if mergeCompleted && task.LandedRef != branchSHA || !mergeCompleted && task.LandedRef != "" {
						return fmt.Errorf("Land crash at %s/%s recovered landed ref %q, merge completed=%v branch=%q", item.phase, item.step, task.LandedRef, mergeCompleted, branchSHA)
					}
				case "unsaddle":
					wantState := store.StateFailed
					if item.step == "scratch.remove" {
						wantState = store.StateLost
					}
					if item.step == "task.torn_down" && item.phase == "after" {
						wantState = store.StateTornDown
					}
					if task.State != wantState {
						return fmt.Errorf("discard crash at %s/%s recovered Task to %s, want %s", item.phase, item.step, task.State, wantState)
					}
				case "relaunch":
					if task.State != store.StateLost && task.State != store.StateWorking {
						return fmt.Errorf("relaunch recovery left Task in %s", task.State)
					}
					if item.step == "task.working" && item.phase == "after" && task.Launches != 1 {
						return fmt.Errorf("completed relaunch started %d launches, want one", task.Launches)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if item.command == "unsaddle" && item.step == "approval.record" && item.phase == "before" {
				if got := strings.TrimSpace(gitTest(t, fixture.harness.baseEnv, fixture.repo, "rev-parse", "refs/heads/posse/t1")); got != branchSHA {
					t.Fatalf("discard crash changed branch tip to %s, want %s", got, branchSHA)
				}
				if err := fixture.openDB(t, func(db *store.DB) error {
					var approvals, notices int
					if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM approvals WHERE task_id=? AND action='discard'`, fixture.taskDBID).Scan(&approvals); err != nil {
						return err
					}
					if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE project_id=? AND task_id=? AND kind='intent_stuck'`, fixture.project.ID, fixture.taskDBID).Scan(&notices); err != nil {
						return err
					}
					if approvals != 0 || notices != 1 {
						return fmt.Errorf("discard crash left %d approvals and %d intent_stuck Notices, want 0 and 1", approvals, notices)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if item.command == "land --merge" {
				mergeCompleted := item.step == "merge" && item.phase == "after" || item.step == "landed_ref.record" || item.step == "task.landed"
				wantDefaultSHA := defaultSHA
				if mergeCompleted {
					wantDefaultSHA = branchSHA
				}
				if got := strings.TrimSpace(gitTest(t, fixture.harness.baseEnv, fixture.repo, "rev-parse", "refs/heads/main")); got != wantDefaultSHA {
					t.Fatalf("Land crash at %s/%s left default branch at %s, want %s", item.phase, item.step, got, wantDefaultSHA)
				}
			}
			if item.command == "unsaddle" {
				scratchPath := filepath.Join(fixture.home, "scratch", fixture.project.Name, "t1")
				if item.step == "scratch.remove" && item.phase == "before" {
					if _, err := os.Stat(scratchPath); err != nil {
						t.Fatalf("pre-removal crash unexpectedly changed Task scratch: %v", err)
					}
					_, output := fixture.run(t, "", "unsaddle", taskID, "--discard", "--user-approved", "User approved discard retry")
					if !strings.Contains(output, "torn-down") {
						t.Fatalf("retry did not complete interrupted discard: %s", output)
					}
				}
				if item.step == "scratch.remove" || item.step == "task.torn_down" {
					if _, err := os.Stat(scratchPath); !os.IsNotExist(err) {
						t.Fatalf("discard recovery kept Task scratch after %s/%s: %v", item.phase, item.step, err)
					}
				}
				branchRemoved := item.step == "task.torn_down" || item.step == "scratch.remove" || item.step == "branch.remove" && item.phase == "after"
				branchTip, branchErr := gitCommand(fixture.harness.baseEnv, fixture.repo, "rev-parse", "--verify", "refs/heads/posse/t1")
				if branchRemoved && branchErr == nil {
					t.Fatalf("discard crash at %s/%s left branch at %s", item.phase, item.step, strings.TrimSpace(branchTip))
				}
				if !branchRemoved && (branchErr != nil || strings.TrimSpace(branchTip) != branchSHA) {
					t.Fatalf("discard crash at %s/%s changed branch tip to %s, want %s (err %v)", item.phase, item.step, strings.TrimSpace(branchTip), branchSHA, branchErr)
				}
				if err := fixture.openDB(t, func(db *store.DB) error {
					var approvals, notices int
					if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM approvals WHERE task_id=? AND action='discard'`, fixture.taskDBID).Scan(&approvals); err != nil {
						return err
					}
					if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE project_id=? AND task_id=? AND kind='intent_stuck'`, fixture.project.ID, fixture.taskDBID).Scan(&notices); err != nil {
						return err
					}
					wantApprovals, wantNotices := 1, 1
					if item.step == "approval.record" && item.phase == "before" {
						wantApprovals = 0
					}
					if item.step == "scratch.remove" && item.phase == "before" {
						wantApprovals = 2
					}
					if item.step == "task.torn_down" && item.phase == "after" {
						wantNotices = 0
					}
					if approvals != wantApprovals || notices != wantNotices {
						return fmt.Errorf("discard crash at %s/%s left %d approvals and %d intent_stuck Notices, want %d and %d", item.phase, item.step, approvals, notices, wantApprovals, wantNotices)
					}
					var mountState string
					if err := db.QueryRowContext(context.Background(), `SELECT state FROM mounts WHERE path=?`, filepath.Join(fixture.home, "remuda", fixture.project.Name, "mount-1")).Scan(&mountState); err != nil {
						return err
					}
					mountReleased := item.step == "mount.release" && item.phase == "after" || item.step == "branch.remove" || item.step == "scratch.remove" || item.step == "task.torn_down"
					wantMountState := "held"
					if mountReleased {
						wantMountState = "idle"
					}
					if mountState != wantMountState {
						return fmt.Errorf("discard crash at %s/%s left Mount %s, want %s", item.phase, item.step, mountState, wantMountState)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func assertCrashExit(t *testing.T, code int, output, crashAt string) {
	t.Helper()
	if code != 86 {
		t.Fatalf("CLI did not exit at crash point %s: code=%d output=%s", crashAt, code, output)
	}
}

type intentCLIHarness struct {
	root      string
	baseEnv   []string
	binary    string
	session   *fakeHerdrSession
	client    *herdr.Client
	caseCount int
}

type intentProjectFixture struct {
	harness  *intentCLIHarness
	root     string
	repo     string
	home     string
	env      []string
	project  store.Project
	dbPath   string
	taskDBID int64
}

func newIntentCLIHarness(t *testing.T) *intentCLIHarness {
	t.Helper()
	root := newFixtureRoot(t, fixturePrefix("intent-"))
	baseEnv := isolatedE2EEnv(t, root)
	baseEnv = setEnv(baseEnv, "POSSE_TEST_ROOT", root)
	baseEnv = setEnv(baseEnv, "GIT_CONFIG_GLOBAL", "/dev/null")
	baseEnv = setEnv(baseEnv, "GIT_CONFIG_NOSYSTEM", "1")
	baseEnv = setEnv(baseEnv, "GIT_AUTHOR_NAME", "Posse E2E")
	baseEnv = setEnv(baseEnv, "GIT_AUTHOR_EMAIL", "posse-e2e@example.test")
	baseEnv = setEnv(baseEnv, "GIT_COMMITTER_NAME", "Posse E2E")
	baseEnv = setEnv(baseEnv, "GIT_COMMITTER_EMAIL", "posse-e2e@example.test")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	herdrPath := filepath.Join(binDir, "herdr")
	status := "#!/bin/sh\nprintf '%s\\n' '{\"running\":true,\"version\":\"0.9.1\",\"protocol\":22,\"compatible\":true}'\n"
	if err := os.WriteFile(herdrPath, []byte(status), 0o700); err != nil {
		t.Fatal(err)
	}
	baseEnv = setEnv(baseEnv, "PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	socketPath := filepath.Join(root, "xdg", "herdr", "herdr.sock")
	session := newFakeHerdrSession(t, socketPath)
	client := herdr.NewWithEnv(herdrPath, baseEnv)
	posseBinary := filepath.Join(binDir, "posse")
	build := exec.Command("go", "build", "-o", posseBinary, "./cmd/posse")
	build.Dir = moduleRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse: %v\n%s", err, output)
	}
	return &intentCLIHarness{root: root, baseEnv: baseEnv, binary: posseBinary, session: session, client: client}
}

func (h *intentCLIHarness) newProject(t *testing.T, name string) *intentProjectFixture {
	t.Helper()
	h.caseCount++
	root := filepath.Join(h.root, "cases", fmt.Sprintf("%02d-%s", h.caseCount, name))
	repo := filepath.Join(root, "repo")
	remote := filepath.Join(root, "remote.git")
	home := filepath.Join(root, "posse-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	initRepository(t, repo, remote, h.baseEnv)
	configText := "[defaults]\nlanding_mode = \"local\"\nauto_unsaddle = \"never\"\nmax_workers = 100\nstall_after = \"1h\"\nidle_after = \"1h\"\n\n[lead]\nkind = \"claude\"\n\n[kinds.claude]\nmodel_args = [\"--model\", \"{model}\"]\neffort_args = [\"--effort\", \"{effort}\"]\n\n[profiles.deep]\nkind = \"claude\"\nmodel = \"sonnet\"\neffort = \"high\"\n\n[dispatch.default]\nuse = \"deep\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := h.session.createWorkspace(repo, "Lead:"+name)
	if err != nil {
		t.Fatal(err)
	}
	// As `posse up` does, the Lead pane carries the Lead label.
	if _, apiErr := h.session.call("pane.rename", map[string]any{"pane_id": workspace.RootPane.PaneID, "label": "posse:" + name + ":lead"}); apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), name, repo, "main")
	if err == nil {
		err = db.SetProjectWorkspace(context.Background(), project.ID, workspace.Workspace.WorkspaceID)
	}
	if err == nil {
		err = db.SetProjectLead(context.Background(), project.ID, workspace.Workspace.WorkspaceID, workspace.RootPane.PaneID, "posse:"+name+":lead")
	}
	closeErr := db.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	project.HerdrWorkspaceID = workspace.Workspace.WorkspaceID
	project.LeadPaneID = workspace.RootPane.PaneID
	project.LeadLabel = "posse:" + name + ":lead"
	env := append([]string(nil), h.baseEnv...)
	env = setEnv(env, "HOME", filepath.Join(root, "user-home"))
	env = setEnv(env, "POSSE_HOME", home)
	env = setEnv(env, "HERDR_ENV", "1")
	env = setEnv(env, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	env = setEnv(env, "HERDR_WORKSPACE_ID", workspace.Workspace.WorkspaceID)
	return &intentProjectFixture{harness: h, root: root, repo: repo, home: home, env: env, project: project, dbPath: filepath.Join(home, "posse.db")}
}

func (f *intentProjectFixture) addShipTask(t *testing.T, state store.State) (string, error) {
	t.Helper()
	ctx := context.Background()
	mountPath := filepath.Join(f.home, "remuda", f.project.Name, "mount-1")
	if err := os.MkdirAll(filepath.Dir(mountPath), 0o700); err != nil {
		return "", err
	}
	gitTest(t, f.harness.baseEnv, f.repo, "worktree", "add", "-b", "posse/t1", mountPath, "main")
	if err := os.WriteFile(filepath.Join(mountPath, "change.txt"), []byte("worker change\n"), 0o600); err != nil {
		return "", err
	}
	gitTest(t, f.harness.baseEnv, mountPath, "add", "change.txt")
	gitTest(t, f.harness.baseEnv, mountPath, "commit", "-m", "worker change")
	db, err := store.Open(f.home)
	if err != nil {
		return "", err
	}
	taskID, err := db.CreateTask(ctx, f.project.ID, store.Task{Seq: 1, Type: "ship", Title: "Crash test", Profile: "deep", LandingMode: "local", Branch: "posse/t1", BaseRef: "main", WorktreePath: mountPath, PaneLabel: "posse:" + f.project.Name + ":t1", AutonomyLand: "ask"})
	if err == nil {
		_, err = db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state) VALUES(?,1,?,'idle')`, f.project.ID, mountPath)
	}
	if err == nil {
		_, err = db.AcquireMount(ctx, f.project.ID, taskID, filepath.Dir(mountPath))
	}
	if err == nil {
		err = db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started")
	}
	if err == nil && state == store.StateDone {
		err = db.Transition(ctx, taskID, store.StateWorking, store.StateDone, "worker", "Worker completed")
	}
	if err == nil && state == store.StateFailed {
		err = db.Transition(ctx, taskID, store.StateWorking, store.StateFailed, "worker", "Worker failed")
	}
	if err == nil && state == store.StateLost {
		err = db.Transition(ctx, taskID, store.StateWorking, store.StateLost, "cli", "Worker disappeared")
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	f.taskDBID = taskID
	return "t1", err
}

func (f *intentProjectFixture) run(t *testing.T, crashAt string, args ...string) (int, string) {
	t.Helper()
	env := append([]string(nil), f.env...)
	if crashAt != "" {
		env = setEnv(env, "POSSE_INTENT_CRASH_AT", crashAt)
	}
	command := exec.Command(f.harness.binary, args...)
	command.Dir = f.repo
	command.Env = env
	output, err := command.CombinedOutput()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			code = 1
		}
	}
	return code, string(output)
}

func (f *intentProjectFixture) openDB(t *testing.T, check func(*store.DB) error) error {
	t.Helper()
	db, err := store.Open(f.home)
	if err != nil {
		return err
	}
	defer db.Close()
	return check(db)
}

func (f *intentProjectFixture) attachTaskPane(t *testing.T, taskID string, withAgent bool) error {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(f.home)
	if err != nil {
		return err
	}
	task, err := db.Task(ctx, f.project.ID, taskID)
	if err != nil {
		_ = db.Close()
		return err
	}
	agentName := ""
	if withAgent {
		agentName = posseAgentName(f.project.Name, task.Seq, 1)
	}
	workspaceID, paneID, err := f.addIntentPane(t, task.WorktreePath, task.PaneLabel, agentName)
	if err == nil {
		err = db.UpdateTaskLaunch(ctx, task.ID, task.WorktreePath, workspaceID, paneID, task.PaneLabel, agentName)
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	return err
}

type fakeHerdrSession struct {
	mu              sync.Mutex
	listener        net.Listener
	serverAt        string
	sequence        int
	requests        chan string
	prompts         []string
	promptTargets   []string
	agentStartGate  chan struct{}
	agentStartNames []string
	promptError     *herdr.APIError
	promptGate      chan struct{}
	// workspaceCreates counts top-level workspace.create requests, not linked worktree opens.
	workspaceCreates int
	workspaces       map[string]herdr.Workspace
	panes            map[string]herdr.Pane
	agents           map[string]herdr.Agent
	// processes holds one real process group per started agent, so posse's
	// stop path signals something real.
	processes map[string]*fakeAgentProcess
}

type fakeAgentProcess struct {
	pid    int
	exited chan struct{}
}

const fakeShellPID = 999999999

func startFakeAgentProcess() (*fakeAgentProcess, error) {
	command := exec.Command("sleep", "600")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	process := &fakeAgentProcess{pid: command.Process.Pid, exited: make(chan struct{})}
	go func() { _ = command.Wait(); close(process.exited) }()
	return process, nil
}

func (p *fakeAgentProcess) alive() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

func (p *fakeAgentProcess) kill() {
	_ = syscall.Kill(-p.pid, syscall.SIGKILL)
	<-p.exited
}

// forgetAgent drops the pane's agent and ends its process.
func (s *fakeHerdrSession) forgetAgent(paneID string) {
	delete(s.agents, paneID)
	if process := s.processes[paneID]; process != nil {
		process.kill()
		delete(s.processes, paneID)
	}
}

func newFakeHerdrSession(t *testing.T, socketPath string) *fakeHerdrSession {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	session := &fakeHerdrSession{listener: listener, serverAt: "e2e-server-generation", requests: make(chan string, 512), workspaces: map[string]herdr.Workspace{}, panes: map[string]herdr.Pane{}, agents: map[string]herdr.Agent{}, processes: map[string]*fakeAgentProcess{}}
	t.Cleanup(func() {
		_ = listener.Close()
		session.mu.Lock()
		defer session.mu.Unlock()
		for paneID := range session.processes {
			session.forgetAgent(paneID)
		}
	})
	go session.serve()
	return session
}

func (s *fakeHerdrSession) createWorkspace(path, label string) (workspaceResult, error) {
	result, apiErr := s.call("workspace.create", map[string]any{"cwd": path, "label": label})
	if apiErr != nil {
		return workspaceResult{}, fmt.Errorf("create fake workspace: %s", apiErr.Message)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return workspaceResult{}, err
	}
	var response workspaceResult
	err = json.Unmarshal(encoded, &response)
	return response, err
}

func (s *fakeHerdrSession) serve() {
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(connection)
	}
}

func (s *fakeHerdrSession) handle(connection net.Conn) {
	defer connection.Close()
	var request herdr.APIRequest
	if err := json.NewDecoder(bufio.NewReader(connection)).Decode(&request); err != nil {
		return
	}
	select {
	case s.requests <- request.Method:
	default:
	}
	result, apiErr := s.call(request.Method, request.Params)
	response := herdr.APIResponse{ID: request.ID}
	if apiErr != nil {
		response.Error = apiErr
	} else {
		response.Result, _ = json.Marshal(result)
	}
	_ = json.NewEncoder(connection).Encode(response)
}

func (s *fakeHerdrSession) call(method string, params map[string]any) (any, *herdr.APIError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stringParam := func(key string) string {
		value, _ := params[key].(string)
		return value
	}
	newID := func(prefix string) string {
		s.sequence++
		return fmt.Sprintf("%s-%d", prefix, s.sequence)
	}
	if !herdr.APIMethods[method] {
		return nil, &herdr.APIError{Code: "invalid_request", Message: "unknown Herdr method " + method}
	}
	switch method {
	case "session.snapshot":
		panes := make([]herdr.Pane, 0, len(s.panes))
		for _, pane := range s.panes {
			panes = append(panes, pane)
		}
		return herdr.Snapshot{Type: "session_snapshot", Version: "1", ServerStartedAt: s.serverAt, Protocol: herdr.MinimumProtocol, Workspaces: mapValues(s.workspaces), Panes: panes, Agents: mapAgentValues(s.agents)}, nil
	case "workspace.create":
		s.workspaceCreates++
		workspaceID, paneID, tabID := newID("workspace"), newID("pane"), newID("tab")
		workspace := herdr.Workspace{WorkspaceID: workspaceID, Label: stringParam("label"), Root: stringParam("cwd")}
		s.workspaces[workspaceID] = workspace
		s.panes[paneID] = herdr.Pane{PaneID: paneID, WorkspaceID: workspaceID, TabID: tabID, Label: stringParam("label"), CWD: stringParam("cwd")}
		return map[string]any{"workspace": map[string]any{"workspace_id": workspaceID}, "root_pane": map[string]any{"pane_id": paneID, "tab_id": tabID}}, nil
	case "worktree.open":
		workspaceID, paneID, tabID := newID("workspace"), newID("pane"), newID("tab")
		workspace := herdr.Workspace{WorkspaceID: workspaceID, Label: stringParam("label"), Root: stringParam("path")}
		workspace.Worktree.CheckoutPath = stringParam("path")
		s.workspaces[workspaceID] = workspace
		s.panes[paneID] = herdr.Pane{PaneID: paneID, WorkspaceID: workspaceID, TabID: tabID, Label: stringParam("label"), CWD: stringParam("path")}
		return map[string]any{"workspace": map[string]any{"workspace_id": workspaceID}, "root_pane": map[string]any{"pane_id": paneID, "tab_id": tabID}}, nil
	case "tab.create":
		// Like Herdr, the label names the tab; the root pane starts unlabeled.
		workspaceID, paneID, tabID := stringParam("workspace_id"), newID("pane"), newID("tab")
		if _, ok := s.workspaces[workspaceID]; !ok {
			return nil, &herdr.APIError{Code: "workspace_not_found", Message: "workspace " + workspaceID + " not found"}
		}
		s.panes[paneID] = herdr.Pane{PaneID: paneID, WorkspaceID: workspaceID, TabID: tabID, CWD: stringParam("cwd")}
		return map[string]any{"tab": map[string]any{"tab_id": tabID, "workspace_id": workspaceID}, "root_pane": map[string]any{"pane_id": paneID, "tab_id": tabID, "workspace_id": workspaceID}}, nil
	case "tab.close":
		tabID, closed := stringParam("tab_id"), false
		for paneID, pane := range s.panes {
			if pane.TabID == tabID {
				delete(s.panes, paneID)
				s.forgetAgent(paneID)
				closed = true
			}
		}
		if !closed {
			return nil, &herdr.APIError{Code: "tab_not_found", Message: "tab " + tabID + " not found"}
		}
		return map[string]any{}, nil
	case "pane.rename":
		paneID := stringParam("pane_id")
		pane := s.panes[paneID]
		pane.Label = stringParam("label")
		s.panes[paneID] = pane
		return map[string]any{}, nil
	case "workspace.rename":
		workspaceID := stringParam("workspace_id")
		workspace := s.workspaces[workspaceID]
		workspace.Label = stringParam("label")
		s.workspaces[workspaceID] = workspace
		return map[string]any{}, nil
	case "tab.rename":
		return map[string]any{}, nil
	case "agent.start":
		s.agentStartNames = append(s.agentStartNames, stringParam("name"))
		if s.agentStartGate != nil {
			<-s.agentStartGate
		}
		paneID := stringParam("pane_id")
		pane := s.panes[paneID]
		pane.Agent, pane.AgentStatus = stringParam("kind"), "idle"
		s.panes[paneID] = pane
		s.agents[paneID] = herdr.Agent{Name: stringParam("name"), PaneID: paneID, Kind: stringParam("kind"), Agent: stringParam("kind"), Status: "idle", AgentStatus: "idle"}
		process, err := startFakeAgentProcess()
		if err != nil {
			return nil, &herdr.APIError{Code: "agent_start_failed", Message: err.Error()}
		}
		s.processes[paneID] = process
		return map[string]any{}, nil
	case "agent.get":
		agent, ok := s.agents[stringParam("target")]
		if !ok {
			return nil, &herdr.APIError{Code: "agent_not_found", Message: "agent not found"}
		}
		return map[string]any{"agent": map[string]any{"agent_status": agent.AgentStatus, "interactive_ready": true, "launch_pending": false}}, nil
	case "pane.process_info":
		paneID := stringParam("pane_id")
		if process := s.processes[paneID]; process != nil && process.alive() {
			return map[string]any{"process_info": map[string]any{"pane_id": paneID, "foreground_process_group_id": process.pid, "shell_pid": fakeShellPID}}, nil
		}
		if process := s.processes[paneID]; process != nil {
			s.forgetAgent(paneID)
			pane := s.panes[paneID]
			pane.Agent, pane.AgentStatus = "", ""
			s.panes[paneID] = pane
		}
		return map[string]any{"process_info": map[string]any{"pane_id": paneID, "foreground_process_group_id": fakeShellPID, "shell_pid": fakeShellPID}}, nil
	case "pane.clear_agent_authority":
		paneID := stringParam("pane_id")
		delete(s.agents, paneID)
		pane := s.panes[paneID]
		pane.Agent, pane.AgentStatus = "", ""
		s.panes[paneID] = pane
		return map[string]any{}, nil
	case "pane.close":
		paneID := stringParam("pane_id")
		delete(s.panes, paneID)
		s.forgetAgent(paneID)
		return map[string]any{}, nil
	case "workspace.close":
		workspaceID := stringParam("workspace_id")
		delete(s.workspaces, workspaceID)
		for paneID, pane := range s.panes {
			if pane.WorkspaceID == workspaceID {
				delete(s.panes, paneID)
				s.forgetAgent(paneID)
			}
		}
		return map[string]any{}, nil
	case "pane.read":
		return map[string]any{"text": ""}, nil
	case "agent.prompt":
		if stringParam("text") == "/exit" || stringParam("text") == "/quit" {
			paneID := stringParam("target")
			s.forgetAgent(paneID)
			pane := s.panes[paneID]
			pane.Agent, pane.AgentStatus = "", ""
			s.panes[paneID] = pane
			return map[string]any{}, nil
		}
		if s.promptGate != nil && strings.HasPrefix(stringParam("text"), "Read ") {
			gate := s.promptGate
			s.mu.Unlock()
			<-gate
			s.mu.Lock()
		}
		if s.promptError != nil && strings.HasPrefix(stringParam("text"), "Read ") {
			return nil, s.promptError
		}
		s.prompts = append(s.prompts, stringParam("text"))
		s.promptTargets = append(s.promptTargets, stringParam("target"))
		return map[string]any{}, nil
	case "agent.send_keys", "pane.send_input", "pane.report_metadata", "workspace.report_metadata", "notification.show":
		return map[string]any{}, nil
	default:
		return nil, &herdr.APIError{Code: "unsupported_test_method", Message: method}
	}
}

func (f *intentProjectFixture) addIntentPane(t *testing.T, path, label string, agentName string) (string, string, error) {
	t.Helper()
	opened, err := f.harness.session.call("worktree.open", map[string]any{"workspace_id": f.project.HerdrWorkspaceID, "path": path, "label": label})
	if err != nil {
		return "", "", fmt.Errorf("open fake Task pane: %s", err.Message)
	}
	encoded, _ := json.Marshal(opened)
	var response struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	if err := json.Unmarshal(encoded, &response); err != nil {
		return "", "", err
	}
	if agentName != "" {
		_, apiErr := f.harness.session.call("agent.start", map[string]any{"name": agentName, "kind": "claude", "pane_id": response.RootPane.PaneID})
		if apiErr != nil {
			return "", "", fmt.Errorf("seed fake agent: %s", apiErr.Message)
		}
	}
	return response.Workspace.WorkspaceID, response.RootPane.PaneID, nil
}

func mapValues(values map[string]herdr.Workspace) []herdr.Workspace {
	result := make([]herdr.Workspace, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

func mapAgentValues(values map[string]herdr.Agent) []herdr.Agent {
	result := make([]herdr.Agent, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

// posseAgentName builds a Rider's agent name as posse does: sanitized and cut
// to Herdr's 32-character agent name limit before the launch suffix.
func posseAgentName(project string, sequence, launch int) string {
	base := "posse-" + strings.Trim(regexp.MustCompile(`[^a-z0-9_-]+`).ReplaceAllString(strings.ToLower(project), "-"), "-_ ")
	suffix := fmt.Sprintf("-t%d-%d", sequence, launch)
	if len(base)+len(suffix) > 32 {
		base = strings.TrimRight(base[:32-len(suffix)], "-_")
	}
	return base + suffix
}
