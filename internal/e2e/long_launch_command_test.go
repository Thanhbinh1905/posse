//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A /bin/sh pane reads input in canonical mode, where the tty discards a line
// longer than 4095 bytes. A Worker launch longer than that must still run.
func TestWorkerLaunchLongerThanTTYLineRuns(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()

	argument := strings.Repeat("Posse's long launch argument. ", 200)
	configPath := filepath.Join(fixture.home, "config.toml")
	configText, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configText = []byte(strings.Replace(string(configText), "[profiles.deep]\nkind = \"claude\"\n", "[profiles.deep]\nkind = \"codex\"\nargs = [\"--long\", "+strconv.Quote(argument)+"]\n", 1))
	if err := os.WriteFile(configPath, configText, 0o600); err != nil {
		t.Fatal(err)
	}
	argsLog := filepath.Join(fixture.root, "codex-args.log")
	agent := "#!/bin/sh\n" +
		"for argument in \"$@\"; do printf '%s\\n' \"$argument\"; done >> " + shellQuote(argsLog) + "\n" +
		"herdr pane report-agent \"$HERDR_PANE_ID\" --source posse.fake --agent codex --state idle >/dev/null 2>&1\n" +
		"while IFS= read -r line; do herdr pane report-agent \"$HERDR_PANE_ID\" --source posse.fake --agent codex --state working >/dev/null 2>&1; done\n"
	if err := os.WriteFile(filepath.Join(fixture.root, "bin", "codex"), []byte(agent), 0o700); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(fixture.root, "long-launch.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Long launch\ndone_when: the Worker runs\n---\nStart with a launch line over the tty limit.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", brief, "--name", "long-launch")
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "relaunch", "t1")

	logged, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(logged), "--long\n"+argument+"\n"); got != 2 {
		t.Fatalf("Worker start and relaunch passed the long argument %d times, want 2:\n%s", got, logged)
	}
	launchPrompt := "Read " + filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "launch.md") + " and follow it.\n"
	if !strings.Contains(string(logged), launchPrompt) {
		t.Fatalf("Worker start omitted its opening prompt %q:\n%s", launchPrompt, logged)
	}
}
