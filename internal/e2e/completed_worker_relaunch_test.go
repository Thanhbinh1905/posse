//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// Reproduce #595: a Herdr restart left done and merging Tasks without Worker
// panes. posse send queued follow-ups that nothing delivered, and posse
// relaunch refused both states.
func TestCompletedTaskWithoutPaneRelaunchesAndReceivesQueuedMessage(t *testing.T) {
	for _, state := range []store.State{store.StateDone, store.StateMerging} {
		t.Run(string(state), func(t *testing.T) {
			fixture := newPRLifecycleFixture(t)
			defer fixture.db.Close()
			brief := filepath.Join(fixture.root, "change.md")
			if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nMake a change.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			completed := fixture.startAndComplete(t, brief, "t1")
			fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", strings.TrimSpace(gitTest(t, fixture.env, fixture.remote, "rev-parse", completed.Branch)))
			if state == store.StateMerging {
				runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t1")
			}
			before := fixture.mustTask(t, "t1")
			if before.State != state {
				t.Fatalf("t1 state = %s, want %s", before.State, state)
			}

			// The relaunched Worker reads its prompt without changing the
			// repository. Each Lead instruction gets a fresh done Signal with
			// the envelope's turn token.
			relaunched := `#!/bin/sh
set -eu
report() { herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state "$1" >/dev/null 2>&1; }
log="$POSSE_TEST_ROOT/relaunched-worker.log"
report idle
IFS= read -r prompt || exit 0
report working
printf '%s\n' "$prompt" >> "$log"
report idle
: > "$POSSE_TEST_ROOT/relaunch-turn-ended-$(git rev-parse HEAD)"
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$log"
  turn_token=$(printf '%s\n' "$line" | sed -n 's/.*| turn \([^]]*\)].*/\1/p')
  [ -n "$turn_token" ] || continue
  report working
  IFS= read -r body || body=
  printf '%s\n' "$body" >> "$log"
  "$POSSE_E2E_POSSE_BIN" publish "Follow-up applied" >> "$log" 2>&1
  "$POSSE_E2E_POSSE_BIN" signal done "Follow-up applied" --pr https://github.com/acme/shop/pull/17 --turn "$turn_token" >> "$log" 2>&1
  report idle
done
`
			if err := os.WriteFile(filepath.Join(fixture.root, "bin", "claude"), []byte(relaunched), 0o700); err != nil {
				t.Fatal(err)
			}
			client := herdr.NewWithEnv("herdr", fixture.env)
			killPane := func(paneID string) {
				t.Helper()
				if _, err := client.Call(context.Background(), "pane.close", map[string]any{"pane_id": paneID}); err != nil {
					t.Fatal(err)
				}
				if !waitForCondition(10*time.Second, func() bool {
					snapshot, err := client.Snapshot(context.Background())
					if err != nil {
						return false
					}
					for _, pane := range snapshot.Panes {
						if pane.PaneID == paneID {
							return false
						}
					}
					return true
				}) {
					t.Fatalf("Worker pane %s did not close", paneID)
				}
			}
			relaunch := func() string {
				t.Helper()
				output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "relaunch", "t1")
				if !strings.Contains(output, "state: "+string(state)) {
					t.Fatalf("relaunch did not report the kept state %s: %s", state, output)
				}
				return output
			}
			workerLog := func() string {
				log, _ := os.ReadFile(filepath.Join(fixture.root, "relaunched-worker.log"))
				return string(log)
			}

			// A relaunch alone keeps the Task's state, gate and Sandbox.
			killPane(before.PaneID)
			relaunch()
			head := strings.TrimSpace(gitTest(t, fixture.env, before.WorktreePath, "rev-parse", "HEAD"))
			if !waitForCondition(15*time.Second, func() bool {
				_, err := os.Stat(filepath.Join(fixture.root, "relaunch-turn-ended-"+head))
				return err == nil
			}) {
				t.Fatalf("relaunched Worker did not finish its prompt turn: %s", workerLog())
			}
			time.Sleep(3 * time.Second)
			runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
			first := fixture.mustTask(t, "t1")
			if first.State != state || first.WorktreePath != before.WorktreePath || first.Branch != before.Branch || first.GatedSHA != before.GatedSHA || first.PRURL != before.PRURL || first.Launches != before.Launches+1 || first.PaneID == before.PaneID {
				t.Fatalf("relaunch changed the completed Task: before=%#v after=%#v", before, first)
			}
			if !strings.Contains(workerLog(), "relaunch.md") {
				t.Fatalf("relaunched Worker did not receive the relaunch prompt: %s", workerLog())
			}

			// A Herdr restart again, plus an out-of-band amend and force push.
			killPane(first.PaneID)
			gitTest(t, fixture.env, before.WorktreePath, "commit", "--amend", "-m", "worker change t1 (reworded)")
			amended := strings.TrimSpace(gitTest(t, fixture.env, before.WorktreePath, "rev-parse", "HEAD"))
			gitTest(t, fixture.env, before.WorktreePath, "push", "--force", "origin", "HEAD:"+before.Branch)

			const followUp = "T595 follow-up queued while the Worker pane was gone"
			sent := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", followUp)
			for _, expected := range []string{"state: queued", "reason: worker_pane_missing", "posse relaunch t1"} {
				if !strings.Contains(sent, expected) {
					t.Fatalf("send to %s Task without a pane omitted %q: %s", state, expected, sent)
				}
			}
			relaunch()
			if !waitForCondition(20*time.Second, func() bool {
				runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
				return strings.Contains(workerLog(), followUp)
			}) {
				show := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "peek", "t1")
				t.Fatalf("queued follow-up never reached the relaunched Worker: %s\nshow: %s", workerLog(), show)
			}
			var status string
			if err := fixture.db.QueryRowContext(context.Background(), `SELECT status FROM messages WHERE task_id=? AND body=?`, before.ID, followUp).Scan(&status); err != nil || status != "delivered" {
				t.Fatalf("follow-up status = %q, %v; want delivered", status, err)
			}
			// The delivered follow-up reopens the Task as it does for a live
			// pane, and the Worker's next done Signal records the amended head.
			fixture.waitTaskState(t, "t1", store.StateDone)
			var discontinuities int
			if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='head_discontinuity'`, before.ID).Scan(&discontinuities); err != nil || discontinuities != 0 {
				t.Fatalf("head_discontinuity Notices = %d, %v; log: %s", discontinuities, err, workerLog())
			}
			if state == store.StateMerging {
				fixture.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", amended)
				runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t1")
				if current := fixture.mustTask(t, "t1"); current.State != store.StateMerging || current.GatedSHA != amended {
					t.Fatalf("Merge after relaunch gated %q in state %s; want %s merging", current.GatedSHA, current.State, amended)
				}
			}
		})
	}
}
