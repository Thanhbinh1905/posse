//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskDiffRepositoryPatchAndStat(t *testing.T) {
	fixture := reviewIdentityFixture(t)
	if code, output := startReviewIdentityTask(t, fixture, "Diff repository task", "author", ""); code != 0 {
		t.Fatalf("start Task: %d %s", code, output)
	}
	task := fixture.mustTask(t, "t1")
	if err := os.WriteFile(filepath.Join(task.WorktreePath, "README.md"), []byte("uncommitted repository edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task.WorktreePath, "diff-proof.txt"), []byte("untracked repository proof\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "diff", "t1")
	if !strings.Contains(patch, "diff-proof.txt") || !strings.Contains(patch, "+untracked repository proof") || !strings.Contains(patch, "+uncommitted repository edit") {
		t.Fatalf("repository Task patch omitted its change: %s", patch)
	}
	stat := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "diff", "t1", "--stat")
	if strings.Contains(stat, "diff --git") || !strings.Contains(stat, "2 files changed") {
		t.Fatalf("repository Task stat is malformed: %s", stat)
	}
}
