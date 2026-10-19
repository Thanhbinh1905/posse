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

func TestLookoutPollerDiscoversAndTearsDownMergedPR(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	brief := filepath.Join(f.root, "lookout-follow-up.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR follow-up\ndone_when: committed change exists\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.rideAndComplete(t, brief, "t1")
	runPosse(t, f.binary, f.repo, f.leadEnv, "land", "t1")
	landing := f.mustTask(t, "t1")
	merge := f.mergeOnLocalOrigin(t, landing, "t1")
	f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, landing.GatedSHA)
	// No Lead command drives this reconcile. The hidden Project poller observes the merge.
	f.waitTaskState(t, "t1", store.StateTornDown)
	assertNoValidationFixRound(t, f, "t1", "gate")
	assertNoValidationFixRound(t, f, "t1", "freshness")
	f.requireNotice(t, "t1", "pr_merged")
}

func TestMergedPRHasExactlyOneVisibleTeardownReason(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	brief := filepath.Join(f.root, "unsafe-follow-up.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR follow-up\ndone_when: committed change exists\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.rideAndComplete(t, brief, "t1")
	runPosse(t, f.binary, f.repo, f.leadEnv, "land", "t1")
	landing := f.mustTask(t, "t1")
	if err := os.WriteFile(filepath.Join(landing.WorktreePath, "untracked.txt"), []byte("leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	merge := f.mergeOnLocalOrigin(t, landing, "t1")
	f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, landing.GatedSHA)
	for i := 0; i < 2; i++ {
		showAfterActiveTeardown(t, f, "t1")
	}
	task := f.mustTask(t, "t1")
	if task.State != store.StateTornDown || task.LandedRef != merge {
		t.Fatalf("merged PR not torn down: %#v", task)
	}
	assertNoValidationFixRound(t, f, "t1", "gate")
	assertNoValidationFixRound(t, f, "t1", "freshness")
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	reasons := 0
	for _, n := range notices {
		if n.TaskID == task.ID && n.Kind == "unsaddle_incomplete" {
			reasons++
		}
	}
	if reasons != 0 {
		t.Fatalf("torn-down PR still has %d teardown failure reasons", reasons)
	}
	decisions, err := f.db.Decisions(context.Background(), f.project.ID, true)
	if err != nil || len(decisions) != 1 || decisions[0].Kind != "leftover" {
		t.Fatalf("Leftover Decision: %#v %v", decisions, err)
	}
}

func assertNoValidationFixRound(t *testing.T, f *prLifecycleFixture, taskID, kind string) {
	t.Helper()
	task := f.mustTask(t, taskID)
	var count int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind=?`, task.ID, kind).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("merged PR queued %s fix rounds after merge: %d", kind, count)
	}
}

// Assert against Git's worktree registry, not only the checkout filesystem.
func assertMountUnlocked(t *testing.T, env []string, repo, path string) {
	t.Helper()
	listing := gitTest(t, env, repo, "worktree", "list", "--porcelain")
	for _, block := range strings.Split(listing, "\n\n") {
		if strings.HasPrefix(block, "worktree "+path+"\n") && strings.Contains("\n"+block+"\n", "\nlocked ") {
			t.Fatalf("released Mount %s is still locked: %s", path, block)
		}
	}
}

func TestMergeInterruptsFollowUpAndReleasesRider(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("hidden Project poller missing: %v", lookoutPIDs(f.root))
	}
	if panes := lookoutPanes(t, client); len(panes) != 0 {
		t.Fatalf("Project poller opened visible Lookout panes: %#v", panes)
	}
	brief := filepath.Join(f.root, "follow-up.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR follow-up\ndone_when: committed change exists\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.rideAndComplete(t, brief, "t1")
	runPosse(t, f.binary, f.repo, f.leadEnv, "land", "t1")
	landing := f.mustTask(t, "t1")
	f.writeGraphQL(t, "pr1", "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", landing.GatedSHA)
	if err := os.WriteFile(filepath.Join(f.root, "pause-fix-commit"), []byte("wait"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "send", "t1", "Handle the follow-up.")
	if got := f.mustTask(t, "t1"); got.State != store.StateWorking {
		t.Fatalf("follow-up state: %s", got.State)
	}
	merge := f.mergeOnLocalOrigin(t, landing, "t1")
	f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, landing.GatedSHA)
	command := exec.Command(f.binary, "send", "t1", "Do not deliver after the merge")
	command.Dir, command.Env = f.repo, f.leadEnv
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "pr_merged") {
		t.Fatalf("send did not refresh the merged PR: %s %v", output, err)
	}
	f.waitTaskState(t, "t1", store.StateTornDown)
	task := f.mustTask(t, "t1")
	if task.State != store.StateTornDown || task.LandedRef != merge {
		t.Fatalf("merge left Rider open: state=%s ref=%s", task.State, task.LandedRef)
	}
	mounts, err := f.db.Mounts(context.Background(), f.project.ID)
	if err != nil || len(mounts) != 1 || mounts[0].TaskID != 0 {
		t.Fatalf("Mount retained: %#v %v", mounts, err)
	}
	assertMountUnlocked(t, f.env, f.repo, mounts[0].Path)
	f.requireNotice(t, "t1", "pr_merged")
	if log, err := os.ReadFile(f.ghLog); err != nil || strings.Contains(string(log), "pr merge") {
		t.Fatalf("watcher merged PR: %s %v", log, err)
	}
}
