package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProduceTaskDiffIncludesUncommittedAndUntrackedChanges(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"api", "web"} {
		repo := filepath.Join(root, name)
		initRepo(t, repo)
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("uncommitted edit\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "new-file.txt"), []byte("untracked proof\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sources := []taskDiffSource{{member: "api", root: filepath.Join(root, "api"), base: "HEAD"}, {member: "web", root: filepath.Join(root, "web"), base: "HEAD"}}
	before := gitTest(t, filepath.Join(root, "api"), "status", "--porcelain")
	patch, err := produceTaskDiff(context.Background(), sources, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(patch, "Member: api") || !strings.Contains(patch, "Member: web") || !strings.Contains(patch, "+uncommitted edit") || !strings.Contains(patch, "+untracked proof") {
		t.Fatalf("diff omitted working-tree changes: %s", patch)
	}
	stat, err := produceTaskDiff(context.Background(), sources, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stat, "diff --git") || !strings.Contains(stat, "2 files changed") {
		t.Fatalf("unexpected stat output: %s", stat)
	}
	after := gitTest(t, filepath.Join(root, "api"), "status", "--porcelain")
	if before != after {
		t.Fatalf("diff changed the real Git index or worktree: before %q, after %q", before, after)
	}
}
