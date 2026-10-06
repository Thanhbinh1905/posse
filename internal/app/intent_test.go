package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestIntentCrashHelperProcess(t *testing.T) {
	if os.Getenv("POSSE_INTENT_CRASH_HELPER") != "1" {
		return
	}
	ctx := context.Background()
	db, err := store.OpenAt(os.Getenv("POSSE_INTENT_CRASH_DB"))
	if err != nil {
		os.Exit(2)
	}
	intentID, _ := strconv.ParseInt(os.Getenv("POSSE_INTENT_CRASH_ID"), 10, 64)
	oldProcess, _ := strconv.Atoi(os.Getenv("POSSE_INTENT_CRASH_OWNER"))
	commandName := os.Getenv("POSSE_INTENT_CRASH_COMMAND")
	stepName := os.Getenv("POSSE_INTENT_CRASH_STEP")
	claimed, err := db.ClaimIntent(ctx, intentID, oldProcess, os.Getpid(), "child")
	if err != nil || !claimed {
		os.Exit(3)
	}
	intent := store.Intent{ID: intentID, ProcessID: os.Getpid(), Command: commandName}
	service := &Service{}
	err = service.runIntentStep(ctx, db, intent, stepName, func() error {
		return os.WriteFile(os.Getenv("POSSE_INTENT_CRASH_MARKER"), []byte("completed\n"), 0o600)
	})
	_ = db.Close()
	if err != nil {
		os.Exit(5)
	}
	os.Exit(0)
}

func TestIntentProcessAliveChecksBootAndStartTime(t *testing.T) {
	bootID, startTime, err := store.ProcessIdentityForPID(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	intent := store.Intent{ProcessID: os.Getpid(), ProcessBootID: bootID, ProcessStartTime: startTime}
	if !intentProcessAlive(intent) {
		t.Fatal("current process with matching boot id and start time was reported dead")
	}
	intent.ProcessBootID = "different-boot"
	if intentProcessAlive(intent) {
		t.Fatal("PID reuse from another boot was reported alive")
	}
	intent.ProcessBootID = bootID
	intent.ProcessStartTime = "different-start"
	if intentProcessAlive(intent) {
		t.Fatal("PID reuse with a different process start time was reported alive")
	}
}

func TestIntentCrashHookExitsAfterPersistingCompletedStep(t *testing.T) {
	ctx := context.Background()
	checkpoints := map[string][]string{
		"ride":         {"scratch.create", "mount.acquire", "pane.open", "pane.record", "scratch.environment", "repository.prepare", "agent.sequence", "agent.record", "pane.label", "agent.start", "brief.write", "launch.write", "agent.prompt", "pane.metadata", "task.working"},
		"land --merge": {"gate.record", "gate.run", "task.landing", "notice.create", "approval.record", "merge", "landed_ref.record", "task.landed"},
		"unsaddle":     {"approval.record", "discard.capture", "panes.close", "scratch.remove", "mount.release", "branch.remove", "task.torn_down"},
		"relaunch":     {"git.inspect", "pane.open", "agent.stop", "pane.label", "scratch.environment", "agent.sequence", "agent.record", "pane.metadata", "agent.start", "relaunch.write", "agent.prompt", "task.working", "task.progress"},
	}
	dbPath := filepath.Join(t.TempDir(), "posse.db")
	var taskID int64
	var projectID int64
	seq := 1
	for commandName, steps := range checkpoints {
		for _, stepName := range steps {
			db, err := store.OpenAt(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if projectID == 0 {
				project, err := db.CreateProject(ctx, "shop", "/repo", "main")
				if err != nil {
					t.Fatal(err)
				}
				projectID = project.ID
			}
			taskID, err = db.CreateTask(ctx, projectID, store.Task{Seq: seq, Type: "ship", Title: "crash", LandingMode: "local"})
			if err != nil {
				t.Fatal(err)
			}
			owner := os.Getpid()
			if err := db.StartIntent(ctx, projectID, taskID, commandName, "started", `{}`, owner); err != nil {
				t.Fatal(err)
			}
			intent, err := db.IntentByTask(ctx, taskID)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "side-effect")
			command := exec.Command(os.Args[0], "-test.run=^TestIntentCrashHelperProcess$")
			command.Env = append(os.Environ(),
				"POSSE_INTENT_CRASH_HELPER=1",
				"POSSE_INTENT_CRASH_DB="+dbPath,
				"POSSE_INTENT_CRASH_ID="+strconv.FormatInt(intent.ID, 10),
				"POSSE_INTENT_CRASH_OWNER="+strconv.Itoa(owner),
				"POSSE_INTENT_CRASH_MARKER="+marker,
				"POSSE_INTENT_CRASH_COMMAND="+commandName,
				"POSSE_INTENT_CRASH_STEP="+stepName,
				"POSSE_INTENT_CRASH_AT="+commandName+":"+stepName,
			)
			output, err := command.CombinedOutput()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != 86 {
				t.Fatalf("%s at %s crash helper exit = %v, output=%s", commandName, stepName, err, output)
			}
			if contents, err := os.ReadFile(marker); err != nil || string(contents) != "completed\n" {
				t.Fatalf("%s at %s side effect did not complete before process exit: %q, %v", commandName, stepName, contents, err)
			}
			db, err = store.OpenAt(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			intent, err = db.IntentByTask(ctx, taskID)
			if err != nil || intent.Step != "done:"+stepName || intent.ProcessID == owner {
				t.Fatalf("%s at %s checkpoint was not durable after process death: %#v, %v", commandName, stepName, intent, err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			seq++
		}
	}
}
