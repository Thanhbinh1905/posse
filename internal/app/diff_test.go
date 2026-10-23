package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProduceTaskDiffSupportsPatchStatAndMembers(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"api", "web"} {
		repo := filepath.Join(root, name)
		initRepo(t, repo)
		if err := os.WriteFile(filepath.Join(repo, "change.txt"), []byte("change\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, repo, "add", "change.txt")
		gitTest(t, repo, "commit", "-m", "change")
	}
	sources := []taskDiffSource{{member: "api", root: filepath.Join(root, "api"), base: "HEAD^"}, {member: "web", root: filepath.Join(root, "web"), base: "HEAD^"}}
	patch, err := produceTaskDiff(context.Background(), sources, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(patch, "Member: api") || !strings.Contains(patch, "Member: web") || !strings.Contains(patch, "+change") {
		t.Fatalf("unexpected Member diffs: %s", patch)
	}
	stat, err := produceTaskDiff(context.Background(), sources, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stat, "diff --git") || !strings.Contains(stat, "1 file changed") {
		t.Fatalf("unexpected stat output: %s", stat)
	}
}
