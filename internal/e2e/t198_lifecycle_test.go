//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// cannot leave the Rider's detached fixture processes behind.
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
 "$POSSE_HOME/remuda/"*)
  IFS= read -r line || exit 0
  herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
  printf '%s\n' "$TMPDIR" > "$POSSE_TEST_ROOT/rider-tmpdir"
  mkdir -p "$TMPDIR/t178-cache"
  printf 'Rider cache\n' > "$TMPDIR/t178-cache/data"
  go test ./... > "$POSSE_TEST_ROOT/rider-test.log" 2>&1 || exit 1
  printf 'Report references evidence.txt\n' > report.md
  printf 'evidence\n' > evidence.txt
  case "$POSSE_T178_MODE" in
   local-*|failed-discard-ordering) git add . && git commit -m 'Rider fixture change' > "$POSSE_TEST_ROOT/commit.log" 2>&1 || exit 1;;
  esac
  case "$POSSE_T178_MODE" in
   scratch-child)
    setsid /bin/sh -c 'cd "$TMPDIR"; dd if=/dev/zero of="$TMPDIR/open-cache" bs=1024 count=64 >/dev/null 2>&1; exec 3<"$TMPDIR/open-cache"; printf ready > "$POSSE_TEST_ROOT/background-ready"; while :; do sleep 0.1; done' </dev/null >/dev/null 2>&1 &
    echo $! > "$POSSE_TEST_ROOT/background-pid"
    TMPDIR="$POSSE_TEST_GLOBAL_TMPDIR" setsid /bin/sh -c 'cd "$TMPDIR"; dd if=/dev/zero of="$TMPDIR/foreign-cache" bs=1024 count=64 >/dev/null 2>&1; exec 4<"$TMPDIR/foreign-cache"; printf ready > "$POSSE_TEST_ROOT/global-child-ready"; while :; do sleep 0.1; done' </dev/null >/dev/null 2>&1 &
    echo $! > "$POSSE_TEST_ROOT/global-child-pid"
    TMPDIR="$POSSE_TEST_OTHER_TASK_SCRATCH" setsid /bin/sh -c 'cd "$TMPDIR"; dd if=/dev/zero of="$TMPDIR/foreign-cache" bs=1024 count=64 >/dev/null 2>&1; exec 4<"$TMPDIR/foreign-cache"; printf ready > "$POSSE_TEST_ROOT/other-task-child-ready"; while :; do sleep 0.1; done' </dev/null >/dev/null 2>&1 &
    echo $! > "$POSSE_TEST_ROOT/other-task-child-pid";;
   local-ordering|failed-discard-ordering)
    setsid /bin/sh -c 'trap '\''mkdir -p "$TMPDIR"; printf "cache recreated during shutdown\n" > "$TMPDIR/shutdown-cache"; printf "shutdown handler ran\n" > "$POSSE_TEST_ROOT/shutdown-marker"; exit 0'\'' TERM; printf ready > "$POSSE_TEST_ROOT/background-ready"; while :; do sleep 0.1; done' </dev/null >/dev/null 2>&1 &
    echo $! > "$POSSE_TEST_ROOT/background-pid";;
  esac
  printf ready > "$POSSE_TEST_ROOT/rider-ready"
  while [ ! -f "$POSSE_TEST_ROOT/task-working" ]; do sleep 0.02; done
  case "$POSSE_T178_MODE" in
   failed-discard-ordering) posse holler failed 'Fixture failed before delivery' > "$POSSE_TEST_ROOT/signal.log" 2>&1;;
   local-*) posse holler done 'Ship fixture completed' > "$POSSE_TEST_ROOT/signal.log" 2>&1;;
   *) posse holler done 'Review fixture completed' --report report.md > "$POSSE_TEST_ROOT/signal.log" 2>&1;;
  esac
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
	config := "[lead]\nkind = \"claude\"\n[defaults]\nlanding_mode = \"local\"\nauto_unsaddle = \"never\"\n[profiles.test]\nkind = \"claude\"\n[dispatch.default]\nuse = \"test\"\n"
	if mode == "failed-discard-ordering" {
		config = strings.Replace(config, "auto_unsaddle = \"never\"\n", "auto_unsaddle = \"never\"\nauto_recover = false\n", 1)
	}
	os.WriteFile(filepath.Join(root, "posse", "config.toml"), []byte(config), 0600)
	repo := filepath.Join(root, "repo")
	os.MkdirAll(repo, 0700)
	gitTest(t, env, repo, "init", "-b", "main")
	os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.test/t178\n\ngo 1.22\n"), 0600)
	os.WriteFile(filepath.Join(repo, "temp_test.go"), []byte("package probe\nimport (\"os\";\"testing\")\nfunc TestTemp(t *testing.T){p,err:=os.MkdirTemp(\"\",\"t178-test-process-\");if err!=nil{t.Fatal(err)};os.WriteFile(p+\"/data\",[]byte(\"test process temp\"),0600)}\n"), 0600)
	gitTest(t, env, repo, "add", ".")
	gitTest(t, env, repo, "commit", "-m", "initial")
	client := herdr.NewWithEnv("herdr", env)
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
	if !waitForCondition(30*time.Second, func() bool {
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
	}) {
		t.Fatal("Lead did not start")
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
	taskType, expected := "scout", store.StateReported
	if strings.HasPrefix(mode, "local-") {
		taskType, expected = "ship", store.StateDone
	}
	if mode == "failed-discard-ordering" {
		taskType, expected = "ship", store.StateFailed
	}
	os.WriteFile(brief, []byte("---\ntype: "+taskType+"\ntitle: Scratch release probe\ndone_when: Report retained and scratch removed\n---\nWrite evidence.\n"), 0600)
	rideEnv := leadEnv
	pauseMarker := filepath.Join(root, "ride-before-working")
	if mode == "failed-discard-ordering" {
		// Force the CI ordering: the Rider finishes its work while Ride has
		// not yet transitioned the Task out of spawning.
		rideEnv = setEnv(rideEnv, "POSSE_INTENT_PAUSE_AT", "ride:before:task.working")
		rideEnv = setEnv(rideEnv, "POSSE_INTENT_PAUSE_FILE", pauseMarker)
	}
	rideCtx, cancelRide := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancelRide()
	ride := exec.CommandContext(rideCtx, binary, "ride", "--brief", brief, "--name", "scratch-release-probe")
	ride.Dir, ride.Env, ride.WaitDelay = repo, rideEnv, time.Second
	var rideOutput bytes.Buffer
	ride.Stdout, ride.Stderr = &rideOutput, &rideOutput
	if err := ride.Start(); err != nil {
		t.Fatal(err)
	}
	rideDone := make(chan struct{})
	var rideErr error
	go func() {
		rideErr = ride.Wait()
		close(rideDone)
	}()
	t.Cleanup(func() {
		cancelRide()
		<-rideDone
	})
	if !waitForCondition(30*time.Second, func() bool { _, err := os.Stat(filepath.Join(root, "rider-ready")); return err == nil }) {
		t.Fatal("Rider did not finish its test process")
	}
	for _, name := range processNames {
		if !waitForCondition(5*time.Second, func() bool { _, err := os.Stat(filepath.Join(root, name+"-ready")); return err == nil }) {
			t.Fatalf("Rider's %s process did not start", name)
		}
		processes[name] = readTeardownFixtureProcess(t, root, name)
	}
	if mode == "failed-discard-ordering" {
		if !waitForCondition(10*time.Second, func() bool { _, err := os.Stat(pauseMarker); return err == nil }) {
			t.Fatal("Ride did not pause before its working transition")
		}
		spawning, err := db.Task(context.Background(), project.ID, "t1")
		if err != nil || spawning.State != store.StateSpawning {
			t.Fatalf("paused Rider Task = %#v, %v; want spawning", spawning, err)
		}
		if _, err := os.Stat(filepath.Join(root, "signal.log")); !os.IsNotExist(err) {
			t.Fatalf("Rider attempted to signal before working: %v", err)
		}
		if err := os.WriteFile(pauseMarker+".continue", []byte("continue\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	<-rideDone
	if rideErr != nil {
		t.Fatalf("Ride failed: %v %s", rideErr, rideOutput.String())
	}
	if !waitForCondition(10*time.Second, func() bool {
		current, err := db.Task(context.Background(), project.ID, "t1")
		return err == nil && current.State == store.StateWorking
	}) {
		t.Fatal("Rider Task did not reach working before its Signal")
	}
	if err := os.WriteFile(filepath.Join(root, "task-working"), []byte("working\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var task store.Task
	if !waitForCondition(30*time.Second, func() bool {
		task, err = db.Task(context.Background(), project.ID, "t1")
		return err == nil && task.State == expected
	}) {
		signal, _ := os.ReadFile(filepath.Join(root, "signal.log"))
		log, _ := os.ReadFile(filepath.Join(root, "rider-test.log"))
		t.Fatalf("Report signal: %v %s tests=%s task=%#v", err, signal, log, task)
	}
	tmpContents, _ := os.ReadFile(filepath.Join(root, "rider-tmpdir"))
	riderTmp := strings.TrimSpace(string(tmpContents))
	if wanted := filepath.Join(home, "scratch", "shop", "t1"); riderTmp != wanted {
		t.Fatalf("Rider TMPDIR=%q, want Task scratch %q", riderTmp, wanted)
	}
	if mode == "scratch-child" {
		otherID, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 2, Type: "scout", Title: "Other Task"})
		if err != nil {
			t.Fatal(err)
		}
		for _, transition := range []struct {
			from, to store.State
			source   string
		}{{store.StateSpawning, store.StateWorking, "cli"}, {store.StateWorking, store.StateDone, "worker"}, {store.StateDone, store.StateReported, "cli"}} {
			if err := db.Transition(context.Background(), otherID, transition.from, transition.to, transition.source, "isolated other Task fixture"); err != nil {
				t.Fatal(err)
			}
		}
	}
	finishTeardownOwnershipLifecycle(t, mode, binary, repo, home, root, env, leadEnv, db, project, task, processes)
}
