package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLeftoverBaseReplaysOnlyUnmergedDiffAfterSquashMerge(t *testing.T) {
	repo := t.TempDir()
	initRepo(t, repo)
	gitTest(t, repo, "checkout", "-b", "posse/first")
	if err := os.WriteFile(filepath.Join(repo, "merged.txt"), []byte("merged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", "merged.txt")
	gitTest(t, repo, "commit", "-m", "first PR")
	gitTest(t, repo, "checkout", "-b", "posse/first-leftover")
	if err := os.WriteFile(filepath.Join(repo, "later.txt"), []byte("only new work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", "later.txt")
	gitTest(t, repo, "commit", "-m", "snapshot")
	gitTest(t, repo, "checkout", "main")
	if err := os.WriteFile(filepath.Join(repo, "merged.txt"), []byte("merged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", "merged.txt")
	gitTest(t, repo, "commit", "-m", "squash first PR")
	merged := strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("current base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", "base.txt")
	gitTest(t, repo, "commit", "-m", "current default")
	base, err := leftoverBase(context.Background(), repo, "refs/heads/posse/first-leftover", merged, "main")
	if err != nil {
		t.Fatal(err)
	}
	if parent := strings.TrimSpace(gitTest(t, repo, "rev-parse", base+"^")); parent != strings.TrimSpace(gitTest(t, repo, "rev-parse", "main")) {
		t.Fatalf("new Task does not start on current default: %s", parent)
	}
	diff := gitTest(t, repo, "diff", "--name-only", "main..."+base)
	if strings.TrimSpace(diff) != "later.txt" {
		t.Fatalf("already merged work returned in new PR: %q", diff)
	}
	if got := gitTest(t, repo, "show", base+":base.txt"); got != "current base\n" {
		t.Fatalf("new default content lost: %q", got)
	}
}
