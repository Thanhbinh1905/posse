//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestTeardownStopsTaskProcessesAndPruneProtectsHeldBranches(t *testing.T) {
	for _, mode := range []string{"scratch-child", "local-ordering", "local-held-prune", "failed-discard-ordering"} {
		t.Run(mode, func(t *testing.T) { runTeardownOwnershipLifecycle(t, mode) })
	}
}

func runTeardownOwnershipLifecycle(t *testing.T, mode string) {
	root := newFixtureRoot(t, fixturePrefix("t178-teardown-"))
	processNames := []string{}
	switch mode {
	case "scratch-child":
		processNames = []string{"background", "global-child", "other-task-child"}
	case "local-ordering", "failed-discard-ordering":
		processNames = []string{"background"}
	}
	processes := map[string]teardownFixtureProcess{}
	// Register before launch so a refused Signal or failed readiness check
	// cannot leave the Worker's detached fixture processes behind.
	t.Cleanup(func() { cleanupTeardownFixtureProcesses(t, root, processNames, processes) })
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0700)
	binary := filepath.Join(bin, "posse")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	agent := `#!/bin/sh
if [ "$1" = --version ]; then echo claude-test; exit 0; fi
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
case "$PWD/" in
 "$POSSE_HOME/pool/"*)
  IFS= read -r line || exit 0
  herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
  printf '%s\n' "$TMPDIR" > "$POSSE_TEST_ROOT/worker-tmpdir"
  mkdir -p "$TMPDIR/t178-cache"
  printf 'Worker cache\n' > "$TMPDIR/t178-cache/data"
  go test ./... > "$POSSE_TEST_ROOT/worker-test.log" 2>&1 || exit 1
  printf 'Report references evidence.txt\n' > report.md
  printf 'evidence\n' > evidence.txt
  case "$POSSE_T178_MODE" in
   local-*|failed-discard-ordering) git add . && git commit -m 'Worker fixture change' > "$POSSE_TEST_ROOT/commit.log" 2>&1 || exit 1;;
  esac
  case "$POSSE_T178_MODE" in
   scratch-child)
    setsid /bin/sh -c 'cd "$TMPDIR"; dd if=/dev/zero of="$TMPDIR/open-cache" bs=1024 count=64 >/dev/null 2>&1; exec 3<"$TMPDIR/open-cache"; printf ready > "$POSSE_TEST_ROOT/background-ready"; while :; do sleep 0.1; done' </dev/null >/dev/null 2>&1 &
    echo $! > "$POSSE_TEST_ROOT/background-pid"
    TMPDIR="$POSSE_TEST_GLOBAL_TMPDIR" setsid /bin/sh -c 'cd "$TMPDIR"; dd if=/dev/zero of="$TMPDIR/foreign-cache" bs=1024 count=64 >/dev/null 2>&1; exec 4<"$TMPDIR/foreign-cache"; printf ready > "$POSSE_TEST_ROOT/global-child-ready"; while :; do sleep 0.1; done' </dev/null >/dev/null 2>&1 &
    echo $! > "$POSSE_TEST_ROOT/global-child-pid"
    TMPDIR="$POSSE_TEST_OTHER_TASK_SCRATCH" setsid /bin/sh -c 'cd "$TMPDIR"; dd if=/dev/zero of="$TMPDIR/foreign-cache" bs=1024 count=64 >/dev/null 2>&1; exec 4<"$TMPDIR/foreign-cache"; printf ready > "$POSSE_TEST_ROOT/other-task-child-ready"; while :; do sleep 0.1; done' </dev/null >/dev/null 2>&1 &
    echo $! > "$POSSE_TEST_ROOT/other-task-child-pid";;
   local-ordering)
    setsid /bin/sh -c 'trap '\''mkdir -p "$TMPDIR"; printf "cache recreated during shutdown\n" > "$TMPDIR/shutdown-cache"; printf "shutdown handler ran\n" > "$POSSE_TEST_ROOT/shutdown-marker"; exit 0'\'' TERM; printf ready > "$POSSE_TEST_ROOT/background-ready"; while :; do sleep 0.1; done' </dev/null >/dev/null 2>&1 &
    echo $! > "$POSSE_TEST_ROOT/background-pid";;
   failed-discard-ordering)
    setsid /bin/sh -c 'printf ready > "$POSSE_TEST_ROOT/background-ready"; while [ ! -f "$POSSE_TEST_ROOT/allow-background-exit" ]; do sleep 0.1; done; mkdir -p "$TMPDIR"; printf "cache recreated during shutdown\n" > "$TMPDIR/shutdown-cache"; printf "shutdown handler ran\n" > "$POSSE_TEST_ROOT/shutdown-marker"' </dev/null >/dev/null 2>&1 &
    echo $! > "$POSSE_TEST_ROOT/background-pid";;
  esac
  printf ready > "$POSSE_TEST_ROOT/worker-ready"
  while [ ! -f "$POSSE_TEST_ROOT/task-working" ]; do sleep 0.02; done
  case "$POSSE_T178_MODE" in
   failed-discard-ordering)
    posse signal failed 'Fixture failed before delivery' > "$POSSE_TEST_ROOT/signal.log" 2>&1
    while [ ! -f "$POSSE_TEST_ROOT/worker-exit" ]; do sleep 0.02; done
    exit 0;;
   local-*) posse signal done 'Change fixture completed' > "$POSSE_TEST_ROOT/signal.log" 2>&1;;
   *) posse signal done 'Review fixture completed' --report report.md > "$POSSE_TEST_ROOT/signal.log" 2>&1;;
  esac
  printf 'complete\n' > "$POSSE_TEST_ROOT/signal-finished"
  ;;
 *) printf 'lead started\n' > "$POSSE_TEST_ROOT/lead-started";;
esac
while IFS= read -r line; do :; done
`
	os.WriteFile(filepath.Join(bin, "claude"), []byte(agent), 0700)
	env := setEnv(isolatedE2EEnv(t, root), "POSSE_T178_MODE", mode)
	for k, v := range map[string]string{"POSSE_TEST_ROOT": root, "PATH": bin + ":" + os.Getenv("PATH"), "GIT_AUTHOR_NAME": "Review", "GIT_AUTHOR_EMAIL": "review@example.test", "GIT_COMMITTER_NAME": "Review", "GIT_COMMITTER_EMAIL": "review@example.test"} {
		env = setEnv(env, k, v)
	}
	// On main this is the inherited global /tmp analogue, inside our sandbox.
	globalTmp := filepath.Join(root, "global-tmp")
	os.MkdirAll(globalTmp, 0700)
	env = setEnv(env, "TMPDIR", globalTmp)
	env = setEnv(env, "GOTMPDIR", globalTmp)
	env = setEnv(env, "POSSE_TEST_GLOBAL_TMPDIR", globalTmp)
	otherTaskScratch := filepath.Join(root, "posse", "scratch", "shop", "t2")
	if mode == "scratch-child" {
		if err := os.MkdirAll(otherTaskScratch, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env = setEnv(env, "POSSE_TEST_OTHER_TASK_SCRATCH", otherTaskScratch)
	herdr.WriteIsolatedConfig(root)
	for _, dir := range []string{"posse", "home", "claude", "codex", "pi"} {
		os.MkdirAll(filepath.Join(root, dir), 0700)
	}
	config := "[lead]\nkind = \"claude\"\n[defaults]\nmerge_mode = \"local\"\nauto_teardown = \"never\"\n[profiles.test]\nkind = \"claude\"\n[dispatch.default]\nuse = \"test\"\n"
	os.WriteFile(filepath.Join(root, "posse", "config.toml"), []byte(config), 0600)
	repo := filepath.Join(root, "repo")
	os.MkdirAll(repo, 0700)
	gitTest(t, env, repo, "init", "-b", "main")
	os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.test/t178\n\ngo 1.22\n"), 0600)
	os.WriteFile(filepath.Join(repo, "temp_test.go"), []byte("package probe\nimport (\"os\";\"testing\")\nfunc TestTemp(t *testing.T){p,err:=os.MkdirTemp(\"\",\"t178-test-process-\");if err!=nil{t.Fatal(err)};os.WriteFile(p+\"/data\",[]byte(\"test process temp\"),0600)}\n"), 0600)
	gitTest(t, env, repo, "add", ".")
	gitTest(t, env, repo, "commit", "-m", "initial")
	// Validate the server against the fixture parent. The nested global-tmp
	// analogue is for CLI processes, not the isolated server's root check.
	client := herdr.NewWithEnv("herdr", setEnv(env, "TMPDIR", filepath.Dir(root)))
	startServer(t, client)
	if _, err := client.Run(context.Background(), "integration", "install", "claude"); err != nil {
		t.Fatal(err)
	}
	lead, err := createWorkspace(client, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "pane", "run", lead.RootPane.PaneID, "posse up --name shop --yes"); err != nil {
		t.Fatal(err)
	}
	if ok, reason := waitForEvent(t, func() bool {
		_, e := os.Stat(filepath.Join(root, "lead-started"))
		if e != nil {
			return false
		}
		s, e := client.Snapshot(context.Background())
		if e != nil {
			return false
		}
		for _, a := range s.Agents {
			if a.Name == "posse-shop-lead-1" {
				return true
			}
		}
		return false
	}, nil); !ok {
		t.Fatalf("Lead did not start: %s", reason)
	}
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.ProjectByName(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	leadEnv := setEnv(env, "HERDR_ENV", "1")
	leadEnv = setEnv(leadEnv, "HERDR_PANE_ID", project.LeadPaneID)
	leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", project.HerdrWorkspaceID)
	brief := filepath.Join(root, "brief.md")
	taskType, expected := "research", store.StateReported
	if strings.HasPrefix(mode, "local-") {
		taskType, expected = "change", store.StateDone
	}
	if mode == "failed-discard-ordering" {
		taskType, expected = "change", store.StateFailed
	}
	os.WriteFile(brief, []byte("---\ntype: "+taskType+"\ntitle: Scratch release probe\ndone_when: Report retained and scratch removed\n---\nWrite evidence.\n"), 0600)
	startEnv := leadEnv
	pauseMarker := filepath.Join(root, "start-before-working")
	if mode == "failed-discard-ordering" {
		// Force the CI ordering: the Worker finishes its work while Start has
		// not yet transitioned the Task out of spawning.
		startEnv = setEnv(startEnv, "POSSE_INTENT_PAUSE_AT", "start:before:task.working")
		startEnv = setEnv(startEnv, "POSSE_INTENT_PAUSE_FILE", pauseMarker)
	}
	startCtx, cancelStart := context.WithCancel(context.Background())
	defer cancelStart()
	start := exec.CommandContext(startCtx, binary, "start", "--brief", brief, "--name", "scratch-release-probe")
	start.Dir, start.Env, start.WaitDelay = repo, startEnv, time.Second
	var startOutput bytes.Buffer
	start.Stdout, start.Stderr = &startOutput, &startOutput
	if err := start.Start(); err != nil {
		t.Fatal(err)
	}
	startDone := make(chan struct{})
	var startErr error
	go func() {
		startErr = start.Wait()
		close(startDone)
	}()
	t.Cleanup(func() {
		cancelStart()
		<-startDone
	})
	exists := func(path string) func() bool {
		return func() bool { _, err := os.Stat(path); return err == nil }
	}
	startFailed := func() string {
		select {
		case <-startDone:
			if startErr != nil {
				return fmt.Sprintf("Start failed: %v %s", startErr, startOutput.String())
			}
		default:
		}
		return ""
	}
	taskEnded := func() string {
		if reason := startFailed(); reason != "" {
			return reason
		}
		current, err := db.Task(context.Background(), project.ID, "t1")
		if err != nil {
			return ""
		}
		return taskStateUnreachable("t1", current, expected)
	}
	if ok, reason := waitForEvent(t, exists(filepath.Join(root, "worker-ready")), taskEnded); !ok {
		log, _ := os.ReadFile(filepath.Join(root, "worker-test.log"))
		t.Fatalf("Worker did not finish its test process: %s; tests=%s", reason, log)
	}
	for _, name := range processNames {
		if ok, reason := waitForEvent(t, exists(filepath.Join(root, name+"-ready")), taskEnded); !ok {
			t.Fatalf("Worker's %s process did not start: %s", name, reason)
		}
		processes[name] = readTeardownFixtureProcess(t, root, name)
	}
	if mode == "failed-discard-ordering" {
		if ok, reason := waitForEvent(t, exists(pauseMarker), func() string {
			select {
			case <-startDone:
				return fmt.Sprintf("Start exited: %v %s", startErr, startOutput.String())
			default:
				return ""
			}
		}); !ok {
			t.Fatalf("Start did not pause before its working transition: %s", reason)
		}
		spawning, err := db.Task(context.Background(), project.ID, "t1")
		if err != nil || spawning.State != store.StateSpawning {
			t.Fatalf("paused Worker Task = %#v, %v; want spawning", spawning, err)
		}
		if _, err := os.Stat(filepath.Join(root, "signal.log")); !os.IsNotExist(err) {
			t.Fatalf("Worker attempted to signal before working: %v", err)
		}
		if err := os.WriteFile(pauseMarker+".continue", []byte("continue\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if ok, reason := waitForEvent(t, func() bool {
		select {
		case <-startDone:
			return true
		default:
			return false
		}
	}, nil); !ok {
		t.Fatalf("Start did not finish: %s %s", reason, startOutput.String())
	}
	if startErr != nil {
		t.Fatalf("Start failed: %v %s", startErr, startOutput.String())
	}
	if current, err := db.Task(context.Background(), project.ID, "t1"); err != nil || current.State != store.StateWorking {
		t.Fatalf("Worker Task did not reach working before its Signal: %#v %v", current, err)
	}
	if err := os.WriteFile(filepath.Join(root, "task-working"), []byte("working\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var task store.Task
	if ok, reason := waitForEvent(t, func() bool {
		task, err = db.Task(context.Background(), project.ID, "t1")
		return err == nil && task.State == expected
	}, taskEnded); !ok {
		signal, _ := os.ReadFile(filepath.Join(root, "signal.log"))
		log, _ := os.ReadFile(filepath.Join(root, "worker-test.log"))
		t.Fatalf("Report signal: %s %v %s tests=%s task=%#v", reason, err, signal, log, task)
	}
	tmpContents, _ := os.ReadFile(filepath.Join(root, "worker-tmpdir"))
	workerTmp := strings.TrimSpace(string(tmpContents))
	if wanted := filepath.Join(home, "scratch", "shop", "t1"); workerTmp != wanted {
		t.Fatalf("Worker TMPDIR=%q, want Task scratch %q", workerTmp, wanted)
	}
	if mode == "scratch-child" {
		if ok, reason := waitForEvent(t, exists(filepath.Join(root, "signal-finished")), nil); !ok {
			t.Fatalf("Worker done command did not finish before stopping the Project Watch: %s", reason)
		}
		for _, pid := range watchPIDs(root) {
			if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
				t.Fatal(err)
			}
		}
		if ok, reason := waitForEvent(t, func() bool { return len(watchPIDs(root)) == 0 }, nil); !ok {
			t.Fatalf("fixture Watch did not stop before creating the unrelated Task: %s; live PIDs=%v", reason, watchPIDs(root))
		}
		// Prevent the active Lead from restarting its poll-only Watch while
		// the synthetic Task moves through states without a Worker pane.
		recovery, err := db.WatchRecovery(context.Background(), project.ID)
		if err != nil {
			t.Fatal(err)
		}
		recovery.RetryAt = time.Now().Add(time.Hour).UnixMilli()
		if err := db.SetWatchRecovery(context.Background(), project.ID, recovery); err != nil {
			t.Fatal(err)
		}
		otherID, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 2, Type: "research", Title: "Other Task"})
		if err != nil {
			t.Fatal(err)
		}
		for _, transition := range []struct {
			from, to store.State
			source   string
		}{{store.StateSpawning, store.StateWorking, "cli"}, {store.StateWorking, store.StateDone, "worker"}, {store.StateDone, store.StateReported, "cli"}} {
			if err := db.Transition(context.Background(), otherID, transition.from, transition.to, transition.source, "isolated other Task fixture"); err != nil {
				current, readErr := db.TaskByID(context.Background(), project.ID, otherID)
				t.Fatalf("transition unrelated Task %s -> %s failed: %v; current=%#v lookup=%v", transition.from, transition.to, err, current, readErr)
			}
		}
	}
	if mode == "failed-discard-ordering" {
		if err := os.WriteFile(filepath.Join(root, "worker-exit"), []byte("exit\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if ok, reason := waitForEvent(t, func() bool {
			raw, err := client.Call(context.Background(), "pane.process_info", map[string]any{"pane_id": task.PaneID})
			if err != nil {
				return false
			}
			var info struct {
				ProcessInfo struct {
					ShellPID                 int `json:"shell_pid"`
					ForegroundProcessGroupID int `json:"foreground_process_group_id"`
				} `json:"process_info"`
			}
			return json.Unmarshal(raw, &info) == nil && info.ProcessInfo.ShellPID > 0 && (info.ProcessInfo.ForegroundProcessGroupID <= 1 || info.ProcessInfo.ForegroundProcessGroupID == info.ProcessInfo.ShellPID)
		}, nil); !ok {
			t.Fatalf("failed fake Worker did not exit naturally: %s", reason)
		}
	}
	finishTeardownOwnershipLifecycle(t, mode, binary, repo, home, root, env, leadEnv, db, project, task, processes)
}
