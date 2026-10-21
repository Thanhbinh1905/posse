//go:build e2e && linux

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/thanhbinh1905/posse/internal/app"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type teardownCloseGuardAdapter struct {
	*herdr.Client
	closed     string
	afterInfo  func()
	afterClose func()
}

func (adapter *teardownCloseGuardAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method == "workspace.close" || method == "tab.close" || method == "pane.close" {
		adapter.closed = method
	}
	raw, err := adapter.Client.Call(ctx, method, params)
	if err == nil && method == "pane.process_info" && adapter.afterInfo != nil {
		adapter.afterInfo()
	}
	if err == nil && method == "pane.close" && adapter.afterClose != nil {
		adapter.afterClose()
	}
	return raw, err
}

func markTaskReported(t *testing.T, f *workerTabsFixture, task store.Task) {
	t.Helper()
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateDone, "worker", "E2E marks the completed Worker done"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, task.ID, store.StateDone, store.StateReported, "cli", "E2E records the completed Worker report"); err != nil {
		t.Fatal(err)
	}
}

// TestTeardownNeverClosesAReplacementAtThePaneCloseBoundary verifies that a
// replacement launched between Worker shutdown and the close guard survives.
func TestTeardownNeverClosesAReplacementAtThePaneCloseBoundary(t *testing.T) {
	f := newWorkerTabsFixture(t)
	ctx := context.Background()
	task := f.start(t, "t1", "Close race", "close-race")
	f.fail(t, "t1")
	work := filepath.Join(task.WorktreePath, "foreign-work.txt")
	if err := os.WriteFile(work, []byte("preserve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := &teardownCloseGuardAdapter{Client: f.client}
	replaced := false
	replacementPID := 0
	closeReads := 0
	adapter.afterInfo = func() {
		db, err := store.OpenReadOnly(f.home)
		if err != nil {
			t.Fatal(err)
		}
		intent, err := db.IntentByTask(ctx, task.ID)
		_ = db.Close()
		if replaced || err != nil || intent.Step != "in_progress:panes.close" {
			return
		}
		closeReads++
		if closeReads != 3 {
			return
		}
		replaced = true
		if _, err := f.client.Call(ctx, "pane.clear_agent_authority", map[string]any{"pane_id": task.PaneID}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.client.Run(ctx, "agent", "start", "close-replacement", "--kind", "claude", "--pane", task.PaneID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.client.Run(ctx, "agent", "rename", task.PaneID, task.AgentName); err != nil {
			t.Fatal(err)
		}
		replacementPID = foregroundPID(t, f, task.PaneID)
	}
	for _, entry := range f.leadEnv {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			t.Setenv(key, value)
		}
	}
	t.Chdir(f.repo)
	cli := app.New(f.home, adapter).CLI()
	var output bytes.Buffer
	cli.Out, cli.ErrOut = &output, &output
	code := cli.Run([]string{"teardown", "t1", "--discard", "--user-approved", "User approved this discard"})
	if !replaced {
		t.Fatalf("replacement did not reach the pane-close boundary: code=%d output=%s", code, output.String())
	}
	if adapter.closed != "" {
		t.Fatalf("Teardown issued an unconditional %s RPC: %s", adapter.closed, output.String())
	}

	paneAlive := false
	for _, pane := range f.snapshot(t).Panes {
		if pane.PaneID == task.PaneID {
			paneAlive = true
		}
	}
	if _, _, err := store.ProcessIdentityForPID(replacementPID); err != nil || foregroundPID(t, f, task.PaneID) != replacementPID {
		t.Fatalf("same-named close-boundary replacement was stopped: pid=%d err=%v", replacementPID, err)
	}
	contents, fileErr := os.ReadFile(work)
	branchOut, branchErr := exec.Command("git", "-C", f.repo, "show-ref", "--verify", "refs/heads/"+task.Branch).CombinedOutput()
	taskAfter := f.task(t, "t1")
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	sandboxes, sandboxErr := db.Sandboxes(ctx, f.projectID)
	_ = db.Close()
	if code == 0 || !strings.Contains(output.String(), "teardown_incomplete") || !strings.Contains(output.String(), "Replacement foreground process") || !paneAlive || fileErr != nil || string(contents) != "preserve\n" || branchErr != nil || taskAfter.State != store.StateFailed || sandboxErr != nil || len(sandboxes) != 1 || sandboxes[0].State != "held" {
		t.Fatalf("discard did not preserve the open pane and Sandbox: pane=%v file=%q fileErr=%v branch=%s Task=%s Sandboxes=%#v output=%s", paneAlive, contents, fileErr, branchOut, taskAfter.State, sandboxes, output.String())
	}
}

func TestDiscardTeardownIntentWinsAgainstLostDetector(t *testing.T) {
	f := newWorkerTabsFixture(t)
	task := f.start(t, "t1", "Discard race", "discard-race")
	// Reproduce a legacy Task pane in the Lead workspace, where absence is
	// immediately actionable rather than protected by grouped-Worker grace.
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.ProjectByID(context.Background(), f.projectID)
	if err == nil {
		_, err = db.ExecContext(context.Background(), `UPDATE tasks SET herdr_workspace_id=? WHERE id=?`, project.HerdrWorkspaceID, task.ID)
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	closeEntered := make(chan struct{})
	releaseClose := make(chan struct{})
	var closeOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseClose) }) }
	adapter := &teardownCloseGuardAdapter{Client: f.client}
	adapter.afterClose = func() {
		closeOnce.Do(func() { close(closeEntered) })
		<-releaseClose
	}
	for _, entry := range f.leadEnv {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			t.Setenv(key, value)
		}
	}
	t.Chdir(f.repo)
	cli := app.New(f.home, adapter).CLI()
	var output bytes.Buffer
	cli.Out, cli.ErrOut = &output, &output
	type teardownResult struct {
		code   int
		output string
	}
	teardownDone := make(chan teardownResult, 1)
	go func() {
		code := cli.Run([]string{"teardown", "t1", "--discard", "--user-approved", "User approved this discard"})
		teardownDone <- teardownResult{code: code, output: output.String()}
	}()
	defer release()
	select {
	case <-closeEntered:
	case result := <-teardownDone:
		t.Fatalf("discard returned before the pane-close boundary: code=%d output=%s", result.code, result.output)
	case <-time.After(10 * time.Second):
		t.Fatal("discard did not reach the pane-close boundary")
	}

	// Reconcile from a second real CLI while the admitted Teardown has closed
	// the pane but has not yet transitioned the Task to torn-down.
	closedSnapshot, err := f.client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, pane := range closedSnapshot.Panes {
		if pane.PaneID == task.PaneID {
			t.Fatalf("pane %s still exists at the Teardown boundary", task.PaneID)
		}
	}
	command := exec.Command(f.binary, "list")
	command.Dir, command.Env = f.repo, f.leadEnv
	listOutput, listErr := command.CombinedOutput()
	if listErr != nil {
		t.Fatalf("concurrent posse list: %v\n%s", listErr, listOutput)
	}
	if current := f.task(t, "t1"); current.State != store.StateWorking {
		t.Fatalf("lost detector changed Task state during admitted discard: %s", current.State)
	}
	db, err = store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	notices, err := db.Notices(context.Background(), f.projectID, false)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	decisions, err := db.Decisions(context.Background(), f.projectID, true)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.TaskID == task.ID && notice.Kind == "task_lost" {
			t.Fatalf("lost detector raised task_lost during admitted discard: %#v", notice)
		}
	}
	for _, decision := range decisions {
		if decision.TaskID == task.ID && decision.Kind == "recovery" {
			t.Fatalf("lost detector raised a recovery Decision during admitted discard: %#v", decision)
		}
	}

	release()
	select {
	case result := <-teardownDone:
		if result.code != 0 || !strings.Contains(result.output, "torn-down") {
			t.Fatalf("first discard did not complete after the close race: code=%d output=%s", result.code, result.output)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("discard did not finish after releasing the pane-close boundary")
	}
	if current := f.task(t, "t1"); current.State != store.StateTornDown {
		t.Fatalf("Task state after discard = %s", current.State)
	}
}

func TestTeardownPreservesForegroundCommandInTaskPane(t *testing.T) {
	f := newWorkerTabsFixture(t)
	task := f.start(t, "t1", "Preserve pane foreground", "preserve-pane-foreground")
	markTaskReported(t, f, task)
	f.exitWorker(t, "t1")

	pidPath := filepath.Join(f.root, "foreground-command.pid")
	scriptPath := filepath.Join(task.WorktreePath, "foreground-command.py")
	script := "import os, signal, time\nsignal.signal(signal.SIGTERM, signal.SIG_IGN)\nopen(" + fmt.Sprintf("%q", pidPath) + ", 'w').write(str(os.getpid()))\ntime.sleep(60)\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	paneRunDone := make(chan struct{}, 1)
	go func() {
		_, _ = f.client.Run(context.Background(), "pane", "run", task.PaneID, "python3 "+shellQuote(scriptPath))
		paneRunDone <- struct{}{}
	}()
	var commandPID int
	if !waitForCondition(5*time.Second, func() bool {
		contents, err := os.ReadFile(pidPath)
		if err != nil {
			return false
		}
		commandPID, err = strconv.Atoi(strings.TrimSpace(string(contents)))
		return err == nil && commandPID > 1
	}) {
		t.Fatal("foreground test command did not start in the Task pane")
	}
	t.Cleanup(func() {
		if commandPID > 1 {
			_ = syscall.Kill(commandPID, syscall.SIGKILL)
		}
	})
	environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", commandPID))
	if err != nil || !strings.Contains(string(environ), "POSSE_TASK_ID="+task.ContainmentMarker) {
		t.Fatalf("foreground command did not inherit the Task marker: %q %v", environ, err)
	}
	if observed := foregroundPID(t, f, task.PaneID); observed != commandPID {
		t.Fatalf("foreground command PID %d is not Herdr's foreground process group: %d", commandPID, observed)
	}
	command := exec.Command(f.binary, "teardown", "t1")
	command.Dir, command.Env = f.repo, f.leadEnv
	output, teardownErr := command.CombinedOutput()
	if teardownErr == nil || !strings.Contains(string(output), "teardown_incomplete") {
		t.Fatalf("Teardown should preserve the user foreground command: err=%v output=%s", teardownErr, output)
	}
	if _, _, err := store.ProcessIdentityForPID(commandPID); err != nil {
		t.Fatalf("Teardown stopped the user foreground command: %v", err)
	}
	if current := f.task(t, "t1"); current.State != store.StateReported {
		t.Fatalf("refused Teardown changed Task state to %s", current.State)
	}
	f.assertAlive(t, f.snapshot(t), task.PaneID)
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	sandboxes, err := db.Sandboxes(context.Background(), f.projectID)
	_ = db.Close()
	if err != nil || len(sandboxes) != 1 || sandboxes[0].State != "held" || sandboxes[0].TaskID != task.ID {
		t.Fatalf("refused Teardown released the user's Sandbox: %#v %v", sandboxes, err)
	}
	_ = syscall.Kill(commandPID, syscall.SIGKILL)
	select {
	case <-paneRunDone:
	case <-time.After(5 * time.Second):
		t.Fatal("foreground pane command did not exit after teardown refusal")
	}
}

func TestTeardownBackfillsLegacyTaskPaneShellIdentity(t *testing.T) {
	f := newWorkerTabsFixture(t)
	task := f.start(t, "t1", "Backfill legacy pane identity", "legacy-pane-identity")
	if task.PaneShellIdentity == "" {
		t.Fatal("fixture Task pane executable identity was not recorded at launch")
	}
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE tasks SET pane_shell_identity='' WHERE id=?`, task.ID); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.PersistTask(context.Background(), task.ID); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	markTaskReported(t, f, task)

	teardown := exec.Command(f.binary, "teardown", "t1")
	teardown.Dir, teardown.Env = f.repo, f.leadEnv
	outputBytes, teardownErr := teardown.CombinedOutput()
	output := string(outputBytes)
	if teardownErr == nil {
		if !strings.Contains(output, "torn-down") {
			t.Fatalf("Teardown returned success without finishing: %s", output)
		}
	} else if !strings.Contains(output, "teardown_incomplete") || !strings.Contains(output, "not retained during Task ownership proof") {
		t.Fatalf("Teardown failed for a reason other than a newly observed Worker process: %s", output)
	}
	if current := f.task(t, "t1"); current.PaneShellIdentity == "" {
		t.Fatal("Teardown did not persist the verified legacy Task pane shell identity")
	}
	if teardownErr != nil {
		f.exitWorker(t, "t1")
		if output := runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t1"); !strings.Contains(output, "torn-down") {
			t.Fatalf("Teardown did not retry after the Worker exited: %s", output)
		}
	}
}

func TestTeardownPreservesForeignCommandInTaskMarkedSandboxTab(t *testing.T) {
	f := newWorkerTabsFixture(t)
	task := f.start(t, "t1", "Preserve marked-tab command", "marked-tab-command")
	markTaskReported(t, f, task)
	f.exitWorker(t, "t1")

	raw, err := f.client.Call(context.Background(), "tab.create", map[string]any{
		"workspace_id": task.HerdrWorkspaceID,
		"cwd":          f.root,
		"label":        "e2e-task-marked-user-tab",
		"focus":        false,
		"env":          map[string]string{"POSSE_TASK_ID": task.ContainmentMarker},
	})
	if err != nil {
		t.Fatalf("create Task-marked Sandbox tab: %v", err)
	}
	var markedTab workspaceResult
	if err := json.Unmarshal(raw, &markedTab); err != nil || markedTab.RootPane.PaneID == "" || markedTab.RootPane.TabID == "" {
		t.Fatalf("Task-marked tab response = %s: %v", raw, err)
	}
	pidPath := filepath.Join(f.root, "marked-tab-command.pid")
	scriptPath := filepath.Join(f.root, "marked-tab-command.py")
	script := "import os, signal, time\nsignal.signal(signal.SIGTERM, signal.SIG_IGN)\nopen(" + fmt.Sprintf("%q", pidPath) + ", 'w').write(str(os.getpid()))\ntime.sleep(60)\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	paneRunDone := make(chan struct{}, 1)
	go func() {
		_, _ = f.client.Run(context.Background(), "pane", "run", markedTab.RootPane.PaneID, "python3 "+shellQuote(scriptPath))
		paneRunDone <- struct{}{}
	}()
	var commandPID int
	if !waitForCondition(5*time.Second, func() bool {
		contents, err := os.ReadFile(pidPath)
		if err != nil {
			return false
		}
		commandPID, err = strconv.Atoi(strings.TrimSpace(string(contents)))
		return err == nil && commandPID > 1
	}) {
		t.Fatal("Task-marked tab command did not start")
	}
	commandCWD, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", commandPID))
	if err != nil || filepath.Clean(commandCWD) == filepath.Clean(task.WorktreePath) {
		t.Fatalf("test command should be outside the Sandbox worktree: cwd=%q err=%v", commandCWD, err)
	}
	environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", commandPID))
	if err != nil || !strings.Contains(string(environ), "POSSE_TASK_ID="+task.ContainmentMarker) {
		t.Fatalf("foreground command did not inherit the Task marker: %q %v", environ, err)
	}

	teardown := exec.Command(f.binary, "teardown", "t1")
	teardown.Dir, teardown.Env = f.repo, f.leadEnv
	output, teardownErr := teardown.CombinedOutput()
	if teardownErr == nil || !strings.Contains(string(output), "teardown_incomplete") {
		t.Fatalf("Teardown should preserve an unverified command in a Task-marked tab: err=%v output=%s", teardownErr, output)
	}
	if _, _, err := store.ProcessIdentityForPID(commandPID); err != nil {
		t.Fatalf("Teardown signalled the foreign Task-marked tab command: %v", err)
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	notices, err := db.Notices(context.Background(), f.projectID, false)
	sandboxes, sandboxErr := db.Sandboxes(context.Background(), f.projectID)
	_ = db.Close()
	if err != nil || sandboxErr != nil {
		t.Fatalf("inspect refused Teardown state: notices=%v sandboxes=%v", err, sandboxErr)
	}
	if len(sandboxes) != 1 || sandboxes[0].State != "held" || sandboxes[0].TaskID != task.ID {
		t.Fatalf("refused Teardown released the Sandbox: %#v", sandboxes)
	}
	blockedNotice := false
	for _, notice := range notices {
		blockedNotice = blockedNotice || notice.TaskID == task.ID && notice.Kind == "teardown_blocked" && strings.Contains(notice.Summary, strconv.Itoa(commandPID))
	}
	if !blockedNotice {
		t.Fatalf("Teardown did not issue a PID-specific Notice for the unverified foreground: %#v", notices)
	}

	if err := syscall.Kill(commandPID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		t.Fatalf("stop test-owned marked-tab command: %v", err)
	}
	select {
	case <-paneRunDone:
	case <-time.After(5 * time.Second):
		t.Fatal("marked tab command did not exit after cleanup")
	}
	if _, err := f.client.Call(context.Background(), "tab.close", map[string]any{"tab_id": markedTab.RootPane.TabID}); err != nil {
		t.Fatalf("close Task-marked test tab: %v", err)
	}
	if output := runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t1"); !strings.Contains(output, "torn-down") {
		t.Fatalf("Teardown did not finish after closing the Task-marked tab: %s", output)
	}
}

func TestTeardownPreservesCommandThatReplacedTaskPaneShell(t *testing.T) {
	f := newWorkerTabsFixture(t)
	task := f.start(t, "t1", "Preserve replaced pane shell", "replaced-shell")
	if task.PaneShellIdentity == "" {
		t.Fatal("Task pane executable identity was not recorded at launch")
	}
	markTaskReported(t, f, task)
	f.exitWorker(t, "t1")

	pidPath := filepath.Join(f.root, "replacement-command.pid")
	scriptPath := filepath.Join(task.WorktreePath, "replacement-command.py")
	script := "import os, signal, time\nsignal.signal(signal.SIGTERM, signal.SIG_IGN)\nopen(" + fmt.Sprintf("%q", pidPath) + ", 'w').write(str(os.getpid()))\ntime.sleep(60)\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	paneRunDone := make(chan struct{}, 1)
	go func() {
		_, _ = f.client.Run(context.Background(), "pane", "run", task.PaneID, "exec python3 "+shellQuote(scriptPath))
		paneRunDone <- struct{}{}
	}()
	var commandPID int
	if !waitForCondition(5*time.Second, func() bool {
		contents, err := os.ReadFile(pidPath)
		if err != nil {
			return false
		}
		commandPID, err = strconv.Atoi(strings.TrimSpace(string(contents)))
		return err == nil && commandPID > 1
	}) {
		t.Fatal("replacement command did not start in the Task pane")
	}
	t.Cleanup(func() {
		if commandPID > 1 {
			_ = syscall.Kill(commandPID, syscall.SIGKILL)
		}
	})
	if observed := foregroundPID(t, f, task.PaneID); observed != commandPID {
		t.Fatalf("replacement command PID %d is not Herdr's foreground process group: %d", commandPID, observed)
	}
	raw, err := f.client.Call(context.Background(), "pane.process_info", map[string]any{"pane_id": task.PaneID})
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		ProcessInfo struct {
			ShellPID int `json:"shell_pid"`
		} `json:"process_info"`
	}
	if err := json.Unmarshal(raw, &info); err != nil || info.ProcessInfo.ShellPID != commandPID {
		t.Fatalf("test command did not replace the retained pane shell: %s %v", raw, err)
	}

	command := exec.Command(f.binary, "teardown", "t1")
	command.Dir, command.Env = f.repo, f.leadEnv
	output, teardownErr := command.CombinedOutput()
	if teardownErr == nil || !strings.Contains(string(output), "teardown_incomplete") {
		t.Fatalf("Teardown should preserve the command that replaced its shell: err=%v output=%s", teardownErr, output)
	}
	if _, _, err := store.ProcessIdentityForPID(commandPID); err != nil {
		t.Fatalf("Teardown stopped the command that replaced the pane shell: %v", err)
	}
	if current := f.task(t, "t1"); current.State != store.StateReported {
		t.Fatalf("refused Teardown changed Task state to %s", current.State)
	}
	_ = syscall.Kill(commandPID, syscall.SIGKILL)
	select {
	case <-paneRunDone:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement pane command did not exit after teardown refusal")
	}
}

func TestTeardownKeepsSandboxHeldUntilDetachedSSHControlPersistExits(t *testing.T) {
	f := newWorkerTabsFixture(t)
	task := f.start(t, "t1", "Preserve SSH ControlPersist", "ssh-controlpersist")
	markTaskReported(t, f, task)
	f.exitWorker(t, "t1")

	command := exec.Command("bash", "-c", `exec -a ssh bash -c "(cd /; exec sleep 120) & wait" -o ControlPersist=yes host`)
	command.Dir = task.WorktreePath
	command.Env = setEnv(f.env, "POSSE_TASK_ID", task.ContainmentMarker)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	masterPID := command.Process.Pid
	defer func() {
		if command.Process != nil {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			_ = command.Wait()
		}
	}()
	var cmdline []byte
	var cmdlineErr error
	if !waitForCondition(5*time.Second, func() bool {
		cmdline, cmdlineErr = os.ReadFile(filepath.Join("/proc", strconv.Itoa(masterPID), "cmdline"))
		return cmdlineErr == nil && strings.Contains(string(cmdline), "ControlPersist=yes")
	}) {
		t.Fatalf("fixture does not look like a ControlPersist daemon: %q %v", cmdline, cmdlineErr)
	}
	if _, _, err := store.ProcessIdentityForPID(masterPID); err != nil {
		t.Fatalf("read detached SSH ControlPersist process identity: %v", err)
	}

	teardown := exec.Command(f.binary, "teardown", "t1")
	teardown.Dir, teardown.Env = f.repo, f.leadEnv
	outputBytes, teardownErr := teardown.CombinedOutput()
	output := string(outputBytes)
	if teardownErr == nil || !strings.Contains(output, "teardown_incomplete") || !strings.Contains(output, strconv.Itoa(masterPID)) || !strings.Contains(strings.ToLower(output), "controlpersist") {
		t.Fatalf("Teardown should preserve the detached SSH daemon and hold its Sandbox with a PID-specific reason: err=%v output=%s", teardownErr, output)
	}
	if _, _, err := store.ProcessIdentityForPID(masterPID); err != nil {
		t.Fatalf("Teardown stopped the shared SSH daemon: %v", err)
	}
	if taskAfter := f.task(t, "t1"); taskAfter.State != store.StateReported {
		t.Fatalf("blocked Teardown changed Task state to %s", taskAfter.State)
	}
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	sandboxes, err := db.Sandboxes(context.Background(), f.projectID)
	notices, noticeErr := db.Notices(context.Background(), f.projectID, false)
	_ = db.Close()
	if err != nil || len(sandboxes) != 1 || sandboxes[0].State != "held" || sandboxes[0].TaskID != task.ID {
		t.Fatalf("Sandbox released while its SSH daemon still used it: %#v %v", sandboxes, err)
	}
	blocked := false
	for _, notice := range notices {
		lowerSummary := strings.ToLower(notice.Summary)
		blocked = blocked || (notice.TaskID == task.ID && notice.Kind == "teardown_blocked" &&
			strings.Contains(notice.Summary, strconv.Itoa(masterPID)) &&
			strings.Contains(lowerSummary, "controlpersist") &&
			strings.Contains(lowerSummary, "stop the listed processes") &&
			strings.Contains(lowerSummary, "retry teardown"))
	}
	if noticeErr != nil || !blocked {
		t.Fatalf("missing actionable Notice for the preserved SSH daemon: %#v %v", notices, noticeErr)
	}

	if err := syscall.Kill(-masterPID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		t.Fatalf("stop test-owned SSH ControlPersist fixture: %v", err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("SSH ControlPersist fixture exited without a signal")
	}
	command.Process = nil
	if !waitForCondition(5*time.Second, func() bool {
		_, _, err := store.ProcessIdentityForPID(masterPID)
		return err != nil
	}) {
		t.Fatal("detached SSH ControlPersist fixture survived cleanup")
	}
	if output := runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t1"); !strings.Contains(output, "torn-down") {
		t.Fatalf("Teardown retry did not release the Sandbox after the daemon exited: %s", output)
	}

	reused := f.start(t, "t2", "Reuse released Sandbox", "reuse-released-sandbox")
	if filepath.Clean(reused.WorktreePath) != filepath.Clean(task.WorktreePath) {
		t.Fatalf("next Task got Sandbox %s, want released Sandbox %s", reused.WorktreePath, task.WorktreePath)
	}
	markTaskReported(t, f, reused)
	f.exitWorker(t, "t2")
	if output := runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t2"); !strings.Contains(output, "torn-down") {
		t.Fatalf("next Task could not tear down the reused Sandbox: %s", output)
	}
}

func TestTeardownStopsRetainedShellByVerifiedIdentity(t *testing.T) {
	f := newWorkerTabsFixture(t)
	task := f.start(t, "t1", "Retained Worker shell", "retained-worker-shell")
	markTaskReported(t, f, task)
	f.exitWorker(t, "t1")
	shellPID := foregroundPID(t, f, task.PaneID)
	if shellPID <= 1 {
		t.Fatalf("invalid retained shell PID %d", shellPID)
	}
	output := runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t1")
	if !strings.Contains(output, "torn-down") {
		t.Fatalf("normal Teardown did not stop the verified Worker shell: %s", output)
	}
	if !waitForCondition(5*time.Second, func() bool { return syscall.Kill(shellPID, 0) != nil }) {
		t.Fatalf("retained Worker shell PID %d survived Teardown", shellPID)
	}
	if taskAfter := f.task(t, "t1"); taskAfter.State != store.StateTornDown {
		t.Fatalf("Task state after Teardown = %s", taskAfter.State)
	}
	paneAlive := false
	for _, pane := range f.snapshot(t).Panes {
		paneAlive = paneAlive || pane.PaneID == task.PaneID
	}
	if paneAlive {
		t.Fatalf("Worker pane %s survived Teardown", task.PaneID)
	}
	if output, err := exec.Command("git", "-C", f.repo, "show-ref", "--verify", "refs/heads/"+task.Branch).CombinedOutput(); err == nil {
		t.Fatalf("finished branch survived Teardown: %s", output)
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	sandboxes, err := db.Sandboxes(context.Background(), f.projectID)
	_ = db.Close()
	if err != nil || len(sandboxes) != 1 || sandboxes[0].State != "idle" || sandboxes[0].TaskID != 0 {
		t.Fatalf("Sandbox was not released after verified shell cleanup: %#v %v", sandboxes, err)
	}
}

func TestTeardownPidfdLauncherHelper(t *testing.T) {
	if os.Getenv("POSSE_T193_BLOCK_PIDFD") != "1" {
		return
	}
	filters := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: uint32(unix.SYS_PIDFD_OPEN)},
		{Code: unix.BPF_RET | unix.BPF_K, K: uint32(unix.SECCOMP_RET_ERRNO) | uint32(unix.ENOSYS)},
		{Code: unix.BPF_RET | unix.BPF_K, K: uint32(unix.SECCOMP_RET_ALLOW)},
	}
	program := unix.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		t.Fatalf("set no-new-privileges: %v", err)
	}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0); err != nil {
		t.Fatalf("install pidfd seccomp filter: %v", err)
	}
	runtime.KeepAlive(filters)
	runtime.KeepAlive(program)
	binary := os.Getenv("POSSE_T193_PIDFD_EXEC")
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("POSSE_T193_PIDFD_ARGS")), &args); err != nil {
		t.Fatalf("decode CLI args: %v", err)
	}
	if err := unix.Exec(binary, append([]string{binary}, args...), os.Environ()); err != nil {
		t.Fatalf("exec Posse with pidfd disabled: %v", err)
	}
}

func TestTeardownWithoutPidfdPreservesResourcesAndReportsCapability(t *testing.T) {
	f := newWorkerTabsFixture(t)
	task := f.start(t, "t1", "No pidfd", "no-pidfd")
	markTaskReported(t, f, task)
	ownedPID := foregroundPID(t, f, task.PaneID)
	work := filepath.Join(task.WorktreePath, "preserved-work.txt")
	if err := os.WriteFile(work, []byte("preserve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal([]string{"teardown", "t1"})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestTeardownPidfdLauncherHelper$")
	command.Dir, command.Env = f.repo, append(append([]string(nil), f.leadEnv...), "POSSE_T193_BLOCK_PIDFD=1", "POSSE_T193_PIDFD_EXEC="+f.binary, "POSSE_T193_PIDFD_ARGS="+string(args))
	output, runErr := command.CombinedOutput()
	t.Logf("no-pidfd CLI: owned=%d error=%v output=%s", ownedPID, runErr, output)
	if runErr == nil || !strings.Contains(string(output), "teardown_incomplete") || !strings.Contains(string(output), "pidfd is unavailable") {
		t.Fatal("real CLI did not retain the unsupported-pidfd refusal")
	}
	if err := syscall.Kill(ownedPID, 0); err != nil {
		t.Fatalf("owned agent was signaled after capability refusal: %v", err)
	}
	if contents, err := os.ReadFile(work); err != nil || string(contents) != "preserve\n" {
		t.Fatalf("work damaged: %q %v", contents, err)
	}
	if after := f.task(t, "t1"); after.State != store.StateReported {
		t.Fatalf("Task changed: %#v", after)
	}
	if output, err := exec.Command("git", "-C", f.repo, "show-ref", "--verify", "refs/heads/"+task.Branch).CombinedOutput(); err != nil {
		t.Fatalf("branch changed: %v %s", err, output)
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sandboxes, err := db.Sandboxes(context.Background(), f.projectID)
	if err != nil || len(sandboxes) != 1 || sandboxes[0].State != "held" || sandboxes[0].TaskID != task.ID {
		t.Fatalf("Sandbox changed: %#v %v", sandboxes, err)
	}
	notices, err := db.Notices(context.Background(), f.projectID, false)
	if err != nil {
		t.Fatal(err)
	}
	foundCapabilityNotice := false
	for _, notice := range notices {
		foundCapabilityNotice = foundCapabilityNotice || notice.Kind == "teardown_incomplete" && strings.Contains(notice.Summary, "pidfd is unavailable")
	}
	if !foundCapabilityNotice {
		t.Fatalf("teardown_incomplete Notice omitted the pidfd capability reason: %#v", notices)
	}
}
