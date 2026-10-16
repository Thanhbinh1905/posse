//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestStoppingBackgroundLookoutReleasesActiveForgePoll(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	probeLog := filepath.Join(f.root, "glab-probes.log")
	glab := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + probeLog + "\"\ncase \"$1 $2\" in\n  'auth status') exec sleep 3 ;;\n  *) exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(f.root, "bin", "glab"), []byte(glab), 0o700); err != nil {
		t.Fatal(err)
	}
	_, noticeID, _ := seedRetryableForgeLookoutFixture(t, f)
	// Stop while the real background process owns its five-minute poll
	// lease, rather than relying on a race with the fixture's first empty poll.
	if !waitForCondition(5*time.Second, func() bool {
		_, err := os.Stat(probeLog)
		return err == nil
	}) {
		t.Fatal("background Lookout did not start the controlled forge probe")
	}
	stopBackgroundLookout(t, f)
	var claimUntil int64
	var claimToken string
	if err := f.db.QueryRowContext(context.Background(), `SELECT pr_poll_claim_until,pr_poll_claim_token FROM project_watch_state WHERE project_id=?`, f.project.ID).Scan(&claimUntil, &claimToken); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(probeLog); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, f.binary, "lookout", "--ack", strconv.FormatInt(noticeID, 10), "--timeout", "500")
	command.Dir, command.Env = f.repo, f.leadEnv
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("replacement Lookout failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(probeLog); err != nil {
		t.Fatalf("replacement Lookout did not run its first forge probe: %v; stopped process left claim_until=%d token_present=%t\n%s", err, claimUntil, claimToken != "", output)
	}
	if claimUntil != 0 || claimToken != "" {
		t.Fatalf("stopped background Lookout retained its poll lease: until=%d token_present=%t", claimUntil, claimToken != "")
	}
}
