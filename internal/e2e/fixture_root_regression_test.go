//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestFixtureRootRemovesReadOnlyGoModuleDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "posse-e2e-readonly")
	module := filepath.Join(root, "home", "go", "pkg", "mod", "example.test", "module@v1")
	if err := os.MkdirAll(module, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module example.test/module\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(module, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err == nil {
		t.Fatal("original cleanup unexpectedly removed a read-only Go module directory")
	}
	if err := removeFixtureRoot(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("fixture remains after cleanup: %v", err)
	}
}

func TestFixtureRootPreservesLiveIsolatedProcesses(t *testing.T) {
	t.Setenv("POSSE_E2E_TMP_PREFIX", "posse-e2e-")
	if runtime.GOOS != "linux" {
		t.Skip("process environment scanning requires Linux procfs")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "posse-e2e-live-server")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, fixtureLockName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	process := exec.Command("sleep", "10")
	process.Env = []string{"XDG_CONFIG_HOME=" + filepath.Join(root, "xdg")}
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
	// Start returns before the child necessarily execs. Until then /proc may
	// still expose the parent's environment, not process.Env. Wait for the
	// fixture environment before testing whether reclamation preserves it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ready, err := fixtureProcessAlive(root)
		if err != nil {
			t.Fatal(err)
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated process did not advertise its fixture environment")
		}
		time.Sleep(time.Millisecond)
	}
	if err := reclaimAbandonedFixtures(parent); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("removed fixture of a live isolated process: %v", err)
	}
	if err := process.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err == nil {
		t.Fatal("expected terminated process")
	}
	if err := reclaimAbandonedFixtures(parent); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("dead process fixture remains: %v", err)
	}
}

func TestFixtureRootReclaimsOnlyAbandonedOwnedRoots(t *testing.T) {
	t.Setenv("POSSE_E2E_TMP_PREFIX", "posse-e2e-")
	parent := t.TempDir()
	active := filepath.Join(parent, "posse-e2e-active")
	abandoned := filepath.Join(parent, "posse-e2e-abandoned")
	unmarked := filepath.Join(parent, "posse-e2e-unmarked")
	for _, root := range []string{active, abandoned, unmarked} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, root := range []string{active, abandoned} {
		if err := os.WriteFile(filepath.Join(root, fixtureLockName), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := os.OpenFile(filepath.Join(active, fixtureLockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if err := reclaimAbandonedFixtures(parent); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{active, unmarked} {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("removed active/unmarked fixture %s: %v", root, err)
		}
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("abandoned fixture remains: %v", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := reclaimAbandonedFixtures(parent); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(active); !os.IsNotExist(err) {
		t.Fatalf("unlocked fixture remains: %v", err)
	}
}

func TestFixtureRootReclaimsOnlyConfiguredPrefix(t *testing.T) {
	t.Setenv("POSSE_E2E_TMP_PREFIX", "posse-e2e-owned-")
	parent := t.TempDir()
	owned := filepath.Join(parent, "posse-e2e-owned-abandoned")
	unrelated := filepath.Join(parent, "posse-e2e-other-abandoned")
	for _, root := range []string{owned, unrelated} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, fixtureLockName), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := reclaimAbandonedFixtures(parent); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Fatalf("configured abandoned fixture remains: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("removed fixture outside configured prefix: %v", err)
	}
}
