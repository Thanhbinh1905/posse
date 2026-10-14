//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// Reproduce #255: t2 has already signalled done, but the User gives its
// Rider more work directly, bypassing posse send and all Rider Signals.
func TestT283DirectPaneWorkReopensCompletedShip(t *testing.T) {
	for _, state := range []store.State{store.StateDone, store.StateLanding} {
		t.Run(string(state), func(t *testing.T) {
			fixture := newPRLifecycleFixture(t)
			defer fixture.db.Close()
			brief := filepath.Join(fixture.root, "first.md")
			if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nMake a change.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			fixture.rideAndComplete(t, brief, "t1")
			brief = filepath.Join(fixture.root, "second.md")
			if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: External merge change\ndone_when: committed change exists\n---\nMake a second change.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			completed := fixture.rideAndComplete(t, brief, "t2")
			fixture.writeGraphQL(t, "pr2", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", strings.TrimSpace(gitTest(t, fixture.env, fixture.remote, "rev-parse", completed.Branch)))
			if state == store.StateLanding {
				runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "land", "t2")
			}
			before := fixture.mustTask(t, "t2")
			client := herdr.NewWithEnv("herdr", fixture.env)
			waitForRelaunchHelpAgentStatus(t, fixture, before.PaneID, "idle")
			// Keep the worktree clean: sustained Herdr busy status alone must suffice.
			if err := os.WriteFile(filepath.Join(fixture.root, "pause-fix-commit"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Run(context.Background(), "pane", "run", before.PaneID, "Continue t2 directly from User input."); err != nil {
				t.Fatal(err)
			}
			waitForRelaunchHelpAgentStatus(t, fixture, before.PaneID, "working")
			if !waitForCondition(12*time.Second, func() bool {
				runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t2")
				return fixture.mustTask(t, "t2").State == store.StateWorking
			}) {
				t.Fatalf("t2 Rider is working from direct input, but Task remains %s", fixture.mustTask(t, "t2").State)
			}
			working := fixture.mustTask(t, "t2")
			if working.GatedSHA != "" || working.PRURL != before.PRURL || working.Branch != before.Branch {
				t.Fatalf("reopen lost PR/branch or retained gate: %#v", working)
			}
			var transitions, notices int
			if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM transitions WHERE task_id=? AND from_state=? AND to_state='working' AND source='watcher'`, before.ID, state).Scan(&transitions); err != nil {
				t.Fatal(err)
			}
			if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='rider_resumed'`, before.ID).Scan(&notices); err != nil {
				t.Fatal(err)
			}
			if transitions != 1 || notices != 1 {
				t.Fatalf("transitions=%d Notices=%d, want one of each", transitions, notices)
			}
			command := exec.Command(fixture.binary, "land", "t2")
			command.Dir, command.Env = fixture.repo, fixture.leadEnv
			if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "done or landing") {
				t.Fatalf("Land accepted busy Task: err=%v output=%s", err, output)
			}
			// Settling the pane cannot complete the Task. Only a fresh done Signal can.
			if _, err := client.Run(context.Background(), "pane", "report-agent", before.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
				t.Fatal(err)
			}
			runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t2")
			if fixture.mustTask(t, "t2").State != store.StateWorking {
				t.Fatal("Herdr idle completed t2 without a Signal")
			}
			if err := os.Remove(filepath.Join(fixture.root, "pause-fix-commit")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(fixture.fixGate, "t2"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			fixture.waitTaskState(t, "t2", store.StateDone)
		})
	}
}
