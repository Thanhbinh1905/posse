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

	"github.com/thanhbinh1905/posse/internal/store"
)

// Reproduce #436: Claude Code collapses a multi-line Posse instruction into a
// "[Pasted text #N]" placeholder and can swallow the Enter that follows the
// paste. Posse must confirm a new turn started and press Enter again, for
// both `posse send` and a Posse-generated CI fix round.
func TestClaudePastePlaceholderInstructionIsSubmitted(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for the fake Claude Code TUI")
	}
	source, err := os.ReadFile(filepath.Join(moduleRoot(t), "internal", "e2e", "testdata", "claude_paste_placeholder.py"))
	if err != nil {
		t.Fatal(err)
	}
	// A direct interpreter shebang keeps the process named claude, which Herdr
	// requires of the pane's foreground agent.
	if err := os.WriteFile(filepath.Join(fixture.root, "bin", "claude"), append([]byte("#!"+python+"\n"), source...), 0o700); err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "ci_loop.max_fix_rounds", "2")
	brief := filepath.Join(fixture.root, "change.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Paste placeholder change\ndone_when: committed change exists\n---\nCreate a committed change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := fixture.startAndComplete(t, brief, "t1")
	inputLog := filepath.Join(fixture.root, "worker-input.log")
	swallowedLog := filepath.Join(fixture.root, "swallowed-enter.log")
	waitForInput := func(want string) {
		t.Helper()
		if !waitForCondition(30*time.Second, func() bool {
			input, err := os.ReadFile(inputLog)
			return err == nil && strings.Contains(string(input), want)
		}) {
			input, _ := os.ReadFile(inputLog)
			swallowed, _ := os.ReadFile(swallowedLog)
			screen := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "peek", "t1")
			t.Fatalf("Worker never submitted %q\ninput:\n%s\nswallowed Enter:\n%s\npane:\n%s", want, input, swallowed, screen)
		}
	}
	// The Worker can start its turn before Posse records the confirmation.
	waitDelivered := func(body string) {
		t.Helper()
		var status string
		if !waitForCondition(15*time.Second, func() bool {
			err := fixture.db.QueryRowContext(context.Background(), `SELECT status FROM messages WHERE task_id=? AND body LIKE ? ORDER BY id DESC LIMIT 1`, task.ID, "%"+body+"%").Scan(&status)
			return err == nil && status == "delivered"
		}) {
			t.Fatalf("instruction %q status=%s, want delivered", body, status)
		}
	}

	instruction := "Rebase onto main and report back."
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", instruction); !strings.Contains(output, "delivered") {
		t.Fatalf("posse send did not deliver the instruction: %s", output)
	}
	waitForInput(instruction)
	waitDelivered(instruction)
	fixture.waitTaskState(t, "t1", store.StateDone)

	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", "t1"); !strings.Contains(output, "merging") {
		t.Fatalf("PR did not open: %s", output)
	}
	gated := fixture.mustTask(t, "t1").GatedSHA
	if gated == "" {
		t.Fatal("merge did not bind a gated PR head")
	}
	fixture.writeGraphQL(t, "pr1", "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", gated)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	if !waitForCondition(20*time.Second, func() bool {
		notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
		for _, notice := range notices {
			if err == nil && notice.TaskID == task.ID && notice.Kind == "ci_fix_queued" {
				return true
			}
		}
		return false
	}) {
		t.Fatal("failed CI observation did not queue a fix round")
	}
	waitForInput("Required CI checks failed")
	waitDelivered("Required CI checks failed")
	swallowed, err := os.ReadFile(swallowedLog)
	if err != nil || strings.Count(string(swallowed), "[Pasted text #") < 2 {
		t.Fatalf("fake Claude did not swallow the first Enter of both instructions: %q err=%v", swallowed, err)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "message_delivery_uncertain" {
			t.Fatalf("confirmed submission raised %s: %#v", notice.Kind, notice)
		}
	}
}
