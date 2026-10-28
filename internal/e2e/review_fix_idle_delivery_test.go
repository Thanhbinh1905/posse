//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

// A Review fix queued while the done Change Worker already sits idle sees no
// agent status event, so only the Watch poll can deliver it (#615).
func TestReviewFixReachesIdleDoneWorkerWithinOneWatchPoll(t *testing.T) {
	f := reviewIdentityFixture(t)
	_, _, changeSeq := startPrivateReviewChange(t, f, "3")
	recordBlockingReviewFinding(t, f, changeSeq)
	change := f.mustTask(t, changeSeq)
	if _, err := herdr.NewWithEnv("herdr", f.env).Run(context.Background(), "pane", "report-agent", change.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	code, output := runReviewIdentityCLI(t, f, "review", "fix", changeSeq)
	if code != 0 || !strings.Contains(output, "queued") {
		t.Fatalf("queue whole-pattern fix: %d %s", code, output)
	}
	var messageID int64
	if err := f.db.QueryRowContext(context.Background(), `SELECT message_id FROM validation_fix_rounds WHERE ship_task_id=? AND kind='review'`, change.ID).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, f.repo, setEnv(f.leadEnv, "POSSE_WATCH_TOKEN", ""), "watch", "--poll-only", "--timeout", "2500"); code != 0 {
		t.Fatalf("posse watch --poll-only: %d %s", code, output)
	}
	var status string
	if err := f.db.QueryRowContext(context.Background(), `SELECT status FROM messages WHERE id=?`, messageID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" {
		t.Fatalf("Review fix message %d is %q after one Watch poll, want delivered", messageID, status)
	}
}
