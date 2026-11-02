package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestReviewCarryProofComparesPatchesAgainstTheTarget(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	initRepo(t, repo)
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	commitFile := func(branch, name, contents string) string {
		t.Helper()
		gitTest(t, repo, "switch", branch)
		if err := os.WriteFile(filepath.Join(repo, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, repo, "add", name)
		gitTest(t, repo, "commit", "-m", "update "+name)
		return strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD"))
	}
	mergeInto := func(branch, start, target string) string {
		t.Helper()
		gitTest(t, repo, "switch", "-c", branch, start)
		gitTest(t, repo, "merge", "--no-edit", target)
		return strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD"))
	}
	gitTest(t, repo, "branch", "author")
	author := commitFile("author", "change.txt", "reviewed\n")
	firstTarget := commitFile("main", "target-one.txt", "one\n")
	passed := mergeInto("passed", author, firstTarget)
	target := commitFile("main", "target-two.txt", "two\n")
	refreshed := mergeInto("refreshed", author, target)
	changedAuthor := commitFile("author", "change.txt", "changed after review\n")
	changed := mergeInto("changed", changedAuthor, target)

	for _, check := range []struct {
		name string
		head string
		want bool
	}{
		{name: "unchanged patch on an advanced target", head: refreshed, want: true},
		{name: "changed patch", head: changed, want: false},
	} {
		t.Run(check.name, func(t *testing.T) {
			tree := strings.TrimSpace(gitTest(t, repo, "rev-parse", check.head+"^{tree}"))
			proved, err := reviewCarryProved(ctx, db, repo, 1, passed, target, check.head, tree)
			if err != nil || proved != check.want {
				t.Fatalf("carry proof from %s to %s = %v, %v; want %v", passed, check.head, proved, err, check.want)
			}
		})
	}
}
