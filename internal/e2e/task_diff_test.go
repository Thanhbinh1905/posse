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
	path := filepath.Join(task.WorktreePath, "diff-proof.txt")
	if err := os.WriteFile(path, []byte("repository diff proof\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.env, task.WorktreePath, "add", "diff-proof.txt")
	gitTest(t, fixture.env, task.WorktreePath, "commit", "-m", "add diff proof")
	patch := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "diff", "t1")
	if !strings.Contains(patch, "diff-proof.txt") || !strings.Contains(patch, "+repository diff proof") {
		t.Fatalf("repository Task patch omitted its change: %s", patch)
	}
	stat := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "diff", "t1", "--stat")
	if strings.Contains(stat, "diff --git") || !strings.Contains(stat, "1 file changed") {
		t.Fatalf("repository Task stat is malformed: %s", stat)
	}
}
