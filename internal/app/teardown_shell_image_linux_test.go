//go:build linux

package app

import (
	"os/exec"
	"strings"
	"testing"
)

func startTestProcess(t *testing.T, name string, args ...string) int {
	t.Helper()
	command := exec.Command(name, args...)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	return command.Process.Pid
}

func TestTaskPaneShellImageAcceptsLegacyAndUpgradedLoginShells(t *testing.T) {
	shell := startTestProcess(t, "sh", "-c", "read -r line")
	identity, err := sandboxProcessExecutableIdentity(shell)
	if err != nil {
		t.Fatal(err)
	}
	for name, recorded := range map[string]string{"recorded": identity, "legacy": "", "upgraded": "0:1"} {
		if err := verifyTaskPaneShellImageIdentity(1, shell, recorded); err != nil {
			t.Errorf("%s shell image was refused: %v", name, err)
		}
	}
}

func TestTaskPaneShellImageRefusesCommandThatReplacedTheShell(t *testing.T) {
	command := startTestProcess(t, "sleep", "60")
	if err := verifyTaskPaneShellImageIdentity(1, command, "0:1"); err == nil || !strings.Contains(err.Error(), "differs from the verified Task pane shell") {
		t.Fatalf("replacement command image error = %v", err)
	}
	if err := verifyTaskPaneShellImageIdentity(1, command, ""); err == nil || !strings.Contains(err.Error(), "does not run a login shell") {
		t.Fatalf("legacy replacement command image error = %v", err)
	}
}
