package heavy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunWaitsForSlotAcrossHomes(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "runtime", "posse", "heavy")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := acquire(context.Background(), lockDir, "cpu", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := tryAcquire(lockDir, "cpu", 1)
	if err != nil {
		t.Fatal(err)
	}
	if second != nil {
		t.Fatal("second home acquired an occupied machine-wide slot")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err = tryAcquire(lockDir, "cpu", 1)
	if err != nil {
		t.Fatal(err)
	}
	if second == nil {
		t.Fatal("slot remained occupied after its holder exited")
	}
	second.Close()
}

func TestHeavySubprocess(t *testing.T) {
	if os.Getenv("POSSE_HEAVY_HELPER") != "1" {
		return
	}
	if err := Run(context.Background(), "cpu", []string{"sh", "-c", os.Getenv("POSSE_HEAVY_SCRIPT")}, os.Stdout, os.Stderr); err != nil {
		t.Fatal(err)
	}
}

func TestCommandsFromSeparateHomesShareMachineSlot(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	logPath := filepath.Join(root, "order")
	start := func(home, script string) *exec.Cmd {
		configHome := filepath.Join(root, home+"-config")
		configDir := filepath.Join(configHome, "posse")
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(configDir, "machine.toml"), []byte("[heavy]\ncpu_slots = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestHeavySubprocess$")
		for _, value := range os.Environ() {
			key, _, _ := strings.Cut(value, "=")
			if key == "POSSE_HEAVY_HELPER" || key == "POSSE_HEAVY_SCRIPT" || key == "POSSE_TEST_LOG" || key == "XDG_RUNTIME_DIR" || key == "XDG_CONFIG_HOME" || key == "POSSE_HOME" || key == "POSSE_TASK_ID" || key == "POSSE_TASK_SEQ" || key == "POSSE_PROJECT" {
				continue
			}
			cmd.Env = append(cmd.Env, value)
		}
		cmd.Env = append(cmd.Env, "POSSE_HEAVY_HELPER=1", "POSSE_HEAVY_SCRIPT="+script, "XDG_RUNTIME_DIR="+runtimeDir, "XDG_CONFIG_HOME="+configHome, "POSSE_HOME="+filepath.Join(root, home), "POSSE_TEST_LOG="+logPath)
		return cmd
	}
	first := start("home-one", `printf 'first-start\n' >> "$POSSE_TEST_LOG"; sleep 0.4; printf 'first-end\n' >> "$POSSE_TEST_LOG"`)
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(75 * time.Millisecond)
	second := start("home-two", `printf 'second-start\n' >> "$POSSE_TEST_LOG"`)
	output, err := second.CombinedOutput()
	if err != nil {
		t.Fatalf("second heavy command: %v\n%s", err, output)
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("first heavy command: %v", err)
	}
	order, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(order)); strings.Join(got, " ") != "first-start first-end second-start" {
		t.Fatalf("command order = %q", got)
	}
	if !strings.Contains(string(output), "waited") {
		t.Fatalf("second command did not report wait time: %s", output)
	}
}

func TestSlotsReadsMachineOverrides(t *testing.T) {
	configHome := t.TempDir()
	configDir := filepath.Join(configHome, "posse")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "machine.toml"), []byte("[heavy]\ncpu_slots = 1\nreviewer_slots = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if got, err := slots("cpu"); err != nil || got != 1 {
		t.Fatalf("CPU slots = %d, %v, want 1", got, err)
	}
	if got, err := slots("reviewer"); err != nil || got != 2 {
		t.Fatalf("reviewer slots = %d, %v, want 2", got, err)
	}
}

func TestSlotCountUsesProcessorDefaultAndOverrides(t *testing.T) {
	if got := defaultSlots("cpu", 12); got != 2 {
		t.Fatalf("default CPU slots = %d, want 2", got)
	}
	if got := defaultSlots("cpu", 4); got != 1 {
		t.Fatalf("small CPU slots = %d, want 1", got)
	}
	if got := defaultSlots("reviewer", 12); got != 1 {
		t.Fatalf("reviewer slots = %d, want 1", got)
	}
}

func TestHeavyCommandParsesClassAndCommand(t *testing.T) {
	class, command, err := HeavyCommand([]string{"--class", "reviewer", "--", "printf", "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if class != "reviewer" || len(command) != 2 || command[0] != "printf" {
		t.Fatalf("parsed command = %q %q", class, command)
	}
	if _, _, err := HeavyCommand([]string{"printf", "ok"}); err == nil {
		t.Fatal("accepted a command without --")
	}
}
