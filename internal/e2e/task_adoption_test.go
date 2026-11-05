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
)

func TestMaintenanceAdoptsReleasedPRBranch(t *testing.T) {
	testMaintenanceAdoptsPRBranch(t, true)
}

func TestMaintenanceAdoptsLockedPRBranch(t *testing.T) {
	testMaintenanceAdoptsPRBranch(t, false)
}

func testMaintenanceAdoptsPRBranch(t *testing.T, released bool) {
	t.Helper()
	f := newPRLifecycleFixture(t)
	brief := filepath.Join(f.root, "source.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Source PR\ndone_when: committed change\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	source := f.startAndComplete(t, brief, "t1")
	stopBackgroundWatch(t, f)
	head := strings.TrimSpace(gitTest(t, f.env, source.WorktreePath, "rev-parse", "HEAD"))
	f.writeGraphQLPulls(t, fakePull{alias: "pr1", state: "OPEN", checks: "PENDING", review: "REVIEW_REQUIRED", mergeable: "MERGEABLE", head: head})
	if released {
		runPosse(t, f.binary, f.repo, f.leadEnv, "pr", "release", "t1", "--policy", "release")
	} else {
		locks := gitTest(t, f.env, f.repo, "worktree", "list", "--porcelain")
		if !strings.Contains(locks, "locked posse: held by t1") {
			t.Fatalf("source is not normally locked: %s", locks)
		}
		f.exitWorker(t, "t1")
		client := herdr.NewWithEnv("herdr", f.env)
		if _, err := client.Call(context.Background(), "pane.close", map[string]any{"pane_id": source.PaneID}); err != nil {
			t.Fatal(err)
		}
	}
	adoption := filepath.Join(f.root, "adopt.md")
	if err := os.WriteFile(adoption, []byte("---\ntype: change\ntitle: Maintain existing PR\ndone_when: maintenance committed\nadopt: t1\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := f.runStartWithStoreBusyRetry(t, adoption, "maintenance-adoption")
	if !strings.Contains(output, "posse/t1") {
		t.Fatalf("admission did not retain branch: %s", output)
	}
	next := f.mustTask(t, "t2")
	if !released {
		if next.SandboxID != source.SandboxID || next.WorktreePath != source.WorktreePath {
			t.Fatalf("held checkout was not transferred: %+v", next)
		}
		locks := gitTest(t, f.env, f.repo, "worktree", "list", "--porcelain")
		if !strings.Contains(locks, "locked posse: held by t2") {
			t.Fatalf("lock was not transferred: %s", locks)
		}
		runPosse(t, f.binary, f.repo, f.leadEnv, "list")
	}
	if next.Branch != source.Branch || next.PRURL != source.PRURL {
		t.Fatalf("adopting Task lost custody: %+v", next)
	}
	if !waitForCondition(15*time.Second, func() bool { _, err := os.Stat(filepath.Join(f.root, "worker-ready-t2")); return err == nil }) {
		t.Fatal("maintenance Worker did not commit its change")
	}
	if _, err := os.Stat(filepath.Join(next.WorktreePath, "e2e-worker-t1.txt")); err != nil {
		t.Fatal("maintenance checkout lost source work: ", err)
	}
	if _, err := os.Stat(filepath.Join(next.WorktreePath, "e2e-worker-t2.txt")); err != nil {
		t.Fatal("maintenance checkout has no new work: ", err)
	}
	gitTest(t, f.env, next.WorktreePath, "merge-base", "--is-ancestor", head, "HEAD")
	lineage, err := f.db.TaskAdoptions(context.Background(), next.ID)
	if err != nil || len(lineage) != 1 || lineage[0].SourceTaskID != source.ID || lineage[0].HeadSHA != head {
		t.Fatalf("lineage=%+v err=%v", lineage, err)
	}
	if output := runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t1"); !strings.Contains(output, "adopted_by") {
		t.Fatalf("source does not show transfer: %s", output)
	}
}
