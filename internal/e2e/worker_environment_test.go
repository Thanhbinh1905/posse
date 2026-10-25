//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const hermeticChildEnv = "POSSE_E2E_HERMETIC_CHILD"

// Fixtures that model a plain User shell must give the same results inside a
// Worker pane as in CI. This runs them with the caller's environment and again
// with Worker-like Herdr, Posse and harness environment under a process named
// herdr, which a Worker pane has above the test.
func TestUserShellFixturesIgnoreWorkerEnvironment(t *testing.T) {
	if os.Getenv(hermeticChildEnv) != "" {
		t.Skip("child run of the hermetic check")
	}
	if runtime.GOOS != "linux" {
		t.Skip("the Worker process tree is modeled with /proc on Linux")
	}
	tests := []string{
		"TestNoMistakesConfigNamesFileKeyAndReplacement",
		"TestUpdateRefusesWhileALiveTaskHasANoMistakesPlan",
		"TestStoredNoMistakesReceiptsSurviveSnapshotRebuild",
	}
	pattern := "^(" + strings.Join(tests, "|") + ")$"
	dir := t.TempDir()
	fakeHerdr := filepath.Join(dir, "herdr")
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	shellBytes, err := os.ReadFile(shell)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fakeHerdr, shellBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base := append(os.Environ(), hermeticChildEnv+"=1")
	workerEnv := append(append([]string{}, base...),
		"HERDR_ENV=1", "HERDR_PANE_ID=w0:p0", "HERDR_TAB_ID=w0:t0", "HERDR_WORKSPACE_ID=w0",
		"HERDR_SOCKET_PATH="+filepath.Join(dir, "no-herdr.sock"), "HERDR_BIN_PATH="+fakeHerdr,
		"POSSE_WORKER_HOME="+filepath.Join(dir, "worker-home"), "POSSE_TASK_ID=worker-task", "POSSE_TASK_TEMP="+dir,
		"CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=worker-session", "CLAUDE_CODE_ENTRYPOINT=cli")

	plain := exec.Command(self, "-test.v", "-test.count=1", "-test.run", pattern)
	plain.Env = base
	// The shell stays alive as the ancestor; exec would replace its name.
	worker := exec.Command(fakeHerdr, "-c", `"$0" -test.v -test.count=1 -test.run "$1"; status=$?; exit $status`, self, pattern)
	worker.Env = workerEnv

	want := make([]string, 0, len(tests))
	for _, name := range tests {
		want = append(want, "PASS "+name)
	}
	sort.Strings(want)
	for label, command := range map[string]*exec.Cmd{"plain": plain, "worker-like": worker} {
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		_ = command.Run()
		if got := topLevelResults(output.String()); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s environment: results %v, want %v\n%s", label, got, want, output.String())
		}
	}
}

var topLevelResultLine = regexp.MustCompile(`(?m)^--- (PASS|FAIL): (Test\S*)`)

func topLevelResults(output string) []string {
	var results []string
	for _, match := range topLevelResultLine.FindAllStringSubmatch(output, -1) {
		results = append(results, match[1]+" "+match[2])
	}
	sort.Strings(results)
	return results
}
