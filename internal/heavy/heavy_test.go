package heavy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
	_ = Run(context.Background(), "cpu", []string{"sh", "-c", os.Getenv("POSSE_HEAVY_SCRIPT")}, os.Stdout, os.Stderr)
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

func TestKilledJobGroupFreesSlot(t *testing.T) {
	lockDir := configureHeavyProcessTest(t)
	wrapper := startHeavyProcess(t, "sleep 60")
	holder := waitForHolder(t, lockDir, wrapper.Process.Pid)
	if holder.PID <= 1 {
		t.Fatalf("unsafe job pid %d", holder.PID)
	}
	if err := syscall.Kill(-holder.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = wrapper.Wait()
	file := waitForSlot(t, lockDir)
	file.Close()
}

func TestWrapperKilledLeavesChildSlotHeldUntilChildExits(t *testing.T) {
	lockDir := configureHeavyProcessTest(t)
	wrapper := startHeavyProcess(t, "sleep 60")
	holder := waitForHolder(t, lockDir, wrapper.Process.Pid)
	if holder.PID <= 1 || wrapper.Process.Pid <= 1 {
		t.Fatalf("unsafe wrapper or child pid: wrapper=%d child=%d", wrapper.Process.Pid, holder.PID)
	}
	cleanupChild := true
	defer func() {
		if cleanupChild {
			_ = syscall.Kill(-holder.PID, syscall.SIGKILL)
		}
	}()
	if err := syscall.Kill(wrapper.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = wrapper.Wait()
	if file, err := tryAcquire(lockDir, "cpu", 1); err != nil || file != nil {
		if file != nil {
			file.Close()
		}
		t.Fatalf("slot freed while child pid %d was still running, file=%v err=%v", holder.PID, file, err)
	}
	if summary := strings.Join(HolderSummary(), "\n"); !strings.Contains(summary, fmt.Sprintf("pid %d", holder.PID)) {
		t.Fatalf("holder status omitted surviving child pid %d: %s", holder.PID, summary)
	}
	if err := syscall.Kill(-holder.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	cleanupChild = false
	file := waitForSlot(t, lockDir)
	file.Close()
}

func configureHeavyProcessTest(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	configHome := filepath.Join(root, "config")
	configDir := filepath.Join(configHome, "posse")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "machine.toml"), []byte("[heavy]\ncpu_slots = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("POSSE_HOME", filepath.Join(root, "home"))
	return filepath.Join(runtimeDir, "posse", "heavy")
}

func startHeavyProcess(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHeavySubprocess$")
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if key == "POSSE_HEAVY_HELPER" || key == "POSSE_HEAVY_SCRIPT" || key == "XDG_RUNTIME_DIR" || key == "XDG_CONFIG_HOME" || key == "POSSE_HOME" {
			continue
		}
		cmd.Env = append(cmd.Env, value)
	}
	cmd.Env = append(cmd.Env, "POSSE_HEAVY_HELPER=1", "POSSE_HEAVY_SCRIPT="+script, "XDG_RUNTIME_DIR="+os.Getenv("XDG_RUNTIME_DIR"), "XDG_CONFIG_HOME="+os.Getenv("XDG_CONFIG_HOME"), "POSSE_HOME="+os.Getenv("POSSE_HOME"))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func waitForHolder(t *testing.T, lockDir string, wrapperPID int) record {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(lockDir, "cpu-0.json"))
		if err == nil {
			var holder record
			if json.Unmarshal(data, &holder) == nil && holder.PID > 1 && holder.PID != wrapperPID {
				return holder
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("heavy command did not publish its holder record")
	return record{}
}

func waitForSlot(t *testing.T, lockDir string) *os.File {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		file, err := tryAcquire(lockDir, "cpu", 1)
		if err != nil {
			t.Fatal(err)
		}
		if file != nil {
			return file
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("slot remained held after the job exited")
	return nil
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
	statusClass, statusCommand, err := HeavyCommand([]string{"--status"})
	if err != nil || statusClass != "status" || len(statusCommand) != 0 {
		t.Fatalf("status command = %q %q %v", statusClass, statusCommand, err)
	}
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
