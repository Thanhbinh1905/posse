//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestStoppedRiderPaneCanBeRelaunched(t *testing.T) {
	attempts := 1
	if os.Getenv("POSSE_E2E_RELAUNCH_STRESS") == "1" {
		attempts = 30
	}
	for _, kind := range []string{"codex", "opencode"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newPRLifecycleFixture(t)
			defer fixture.db.Close()

			runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "config", "set", "profiles.deep.kind", kind)
			agent := "#!/bin/sh\n" +
				"herdr pane report-agent \"$HERDR_PANE_ID\" --source posse.fake --agent " + kind + " --state idle >/dev/null 2>&1\n" +
				"while IFS= read -r line; do herdr pane report-agent \"$HERDR_PANE_ID\" --source posse.fake --agent " + kind + " --state working >/dev/null 2>&1; done\n"
			if err := os.WriteFile(filepath.Join(fixture.root, "bin", kind), []byte(agent), 0o700); err != nil {
				t.Fatal(err)
			}

			brief := filepath.Join(fixture.root, "relaunch.md")
			content := "---\ntype: ship\ntitle: Repeated stopped Rider relaunch\ndone_when: relaunch succeeds repeatedly\n---\nKeep the Rider alive for the relaunch regression.\n"
			if err := os.WriteFile(brief, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "ride", "--brief", brief, "--name", "repeated-stopped-rider")
			initial := fixture.mustTask(t, "t1")

			for attempt := 1; attempt <= attempts; attempt++ {
				started := time.Now()
				command := exec.Command(fixture.binary, "relaunch", "t1")
				command.Dir, command.Env = fixture.repo, fixture.leadEnv
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("relaunch %d/%d failed: %v\n%s", attempt, attempts, err, output)
				}
				current := fixture.mustTask(t, "t1")
				if current.Launches != initial.Launches+attempt || current.State != store.StateWorking || current.PaneID != initial.PaneID || current.WorktreePath != initial.WorktreePath {
					t.Fatalf("relaunch %d/%d changed Rider identity or Mount: initial=%#v current=%#v output=%s", attempt, attempts, initial, current, output)
				}
				t.Logf("relaunch %d/%d completed in %s", attempt, attempts, time.Since(started))
			}
		})
	}
}
