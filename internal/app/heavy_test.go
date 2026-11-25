package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestHeavyCommandRequiresSeparator(t *testing.T) {
	code, output := runCLI(t, testService(t.TempDir(), nil), "heavy", "printf", "missing-separator")
	if code != 2 || !strings.Contains(output, "usage: posse heavy") {
		t.Fatalf("heavy without separator exit=%d output=%s", code, output)
	}
}

func TestHeavyStatusCommandNamesHolderPID(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	lockDir := filepath.Join(runtimeDir, "posse", "heavy")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(lockDir, "cpu-0.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	owner := fmt.Sprintf(`{"pid":%d,"command":"go test"}`, os.Getpid())
	if err := os.WriteFile(filepath.Join(lockDir, "cpu-0.json"), []byte(owner), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output := runCLI(t, testService(t.TempDir(), nil), "heavy", "--status")
	if code != 0 || !strings.Contains(output, fmt.Sprintf("pid %d", os.Getpid())) {
		t.Fatalf("heavy status exit=%d output=%s", code, output)
	}
}
