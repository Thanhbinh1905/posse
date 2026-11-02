//go:build e2e && linux

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestUserShellPathIsRecordedOnlyByUserShellProcesses(t *testing.T) {
	f := newMultiMemberForgeFixture(t)
	pathFile := filepath.Join(f.home, "user-shell-path")
	run := func(env []string, interactive bool) {
		t.Helper()
		command := exec.Command(f.binary, "--json")
		command.Dir = f.workspace
		command.Env = env
		if interactive {
			session := startPty(t, command, 40, 100)
			if err := session.wait(); err != nil {
				t.Fatalf("interactive posse --json: %v\n%s", err, session.text())
			}
			return
		}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("service-like posse --json: %v\n%s", err, output)
		}
	}
	readPath := func() string {
		t.Helper()
		contents, err := os.ReadFile(pathFile)
		if err != nil {
			t.Fatalf("read recorded User shell PATH: %v", err)
		}
		return string(contents)
	}
	userPath := "/user/shell/bin" + string(os.PathListSeparator) + f.binDir
	env := setEnv(f.env, "POSSE_WATCH_TOKEN", "")
	env = setEnv(env, "HERDR_PLUGIN_ID", "")

	run(setEnv(setEnv(env, "PATH", userPath), "HERDR_PANE_ID", "w1-p1"), true)
	if got := readPath(); got != userPath {
		t.Fatalf("interactive command in a Herdr pane recorded %q, want %q", got, userPath)
	}

	servicePath := "/service/server/bin" + string(os.PathListSeparator) + f.binDir
	for _, paneID := range []string{"", "w1-p1"} {
		run(setEnv(setEnv(env, "PATH", servicePath), "HERDR_PANE_ID", paneID), false)
		if got := readPath(); got != userPath {
			t.Fatalf("service-like process with HERDR_PANE_ID=%q replaced the User shell PATH with %q, want %q", paneID, got, userPath)
		}
	}
}
