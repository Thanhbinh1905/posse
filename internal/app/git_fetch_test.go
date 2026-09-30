package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestGitFetchRetriesTransientRefLock(t *testing.T) {
	root := newGitFetchTestRepo(t)
	bin, countPath := installFakeGitFetch(t)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GIT_FETCH_FAILS", "1")
	t.Setenv("POSSE_TEST_GIT_FETCH_ERROR", "fatal: cannot lock ref 'refs/remotes/origin/main': is at old but expected new")

	output, err := gitFetch(context.Background(), root, "origin")
	if err != nil || output != "fetched" {
		t.Fatalf("gitFetch() = %q, %v; want successful fetch after retry", output, err)
	}
	if got := fakeGitFetchCalls(t, countPath); got != 2 {
		t.Fatalf("fetch attempts = %d, want 2", got)
	}
}

func TestGitFetchDoesNotRetryUnrelatedOrPersistentErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		failures   string
		message    string
		wantCalls  int
		wantErrSub string
	}{
		{
			name:       "unrelated error",
			failures:   "1",
			message:    "fatal: unable to access remote",
			wantCalls:  1,
			wantErrSub: "unable to access remote",
		},
		{
			name:       "persistent ref lock",
			failures:   "10",
			message:    "fatal: Unable to create '.git/refs/remotes/origin/main.lock': File exists",
			wantCalls:  gitFetchAttempts,
			wantErrSub: "main.lock",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := newGitFetchTestRepo(t)
			bin, countPath := installFakeGitFetch(t)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("POSSE_TEST_GIT_FETCH_FAILS", test.failures)
			t.Setenv("POSSE_TEST_GIT_FETCH_ERROR", test.message)

			_, err := gitFetch(context.Background(), root, "origin")
			if err == nil || !strings.Contains(err.Error(), test.wantErrSub) {
				t.Fatalf("gitFetch() error = %v, want containing %q", err, test.wantErrSub)
			}
			if got := fakeGitFetchCalls(t, countPath); got != test.wantCalls {
				t.Fatalf("fetch attempts = %d, want %d", got, test.wantCalls)
			}
		})
	}
}

func TestGitFetchSerializesConcurrentFetchesAcrossWorktrees(t *testing.T) {
	root, worktree := newGitFetchTestWorktree(t)
	bin, countPath := installFakeGitFetch(t)
	active := filepath.Join(t.TempDir(), "active-fetch")
	overlap := filepath.Join(t.TempDir(), "overlap")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GIT_FETCH_ACTIVE", active)
	t.Setenv("POSSE_TEST_GIT_FETCH_OVERLAP", overlap)
	t.Setenv("POSSE_TEST_GIT_FETCH_FAILS", "0")
	t.Setenv("POSSE_TEST_GIT_FETCH_ERROR", "")

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, checkout := range []string{root, worktree} {
		wg.Add(1)
		go func(checkout string) {
			defer wg.Done()
			<-start
			_, err := gitFetch(context.Background(), checkout, "origin")
			errs <- err
		}(checkout)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent fetch: %v", err)
		}
	}
	if _, err := os.Stat(overlap); err == nil {
		t.Fatal("git fetches overlapped across worktrees of the same repository")
	}
	if got := fakeGitFetchCalls(t, countPath); got != 2 {
		t.Fatalf("fetch calls = %d, want 2", got)
	}
}

func newGitFetchTestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{{"init", "--quiet", root}, {"-C", root, "config", "user.name", "Test"}, {"-C", root, "config", "user.email", "test@example.test"}, {"-C", root, "commit", "--allow-empty", "--quiet", "-m", "initial"}} {
		command := exec.Command("git", args...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
		}
	}
	return root
}

func newGitFetchTestWorktree(t *testing.T) (string, string) {
	t.Helper()
	root := newGitFetchTestRepo(t)
	worktree := filepath.Join(t.TempDir(), "worktree")
	command := exec.Command("git", "-C", root, "worktree", "add", "--quiet", "-b", "rider", worktree, "HEAD")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, output)
	}
	return root, worktree
}

func installFakeGitFetch(t *testing.T) (string, string) {
	t.Helper()
	bin := t.TempDir()
	countPath := filepath.Join(bin, "calls")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_REAL_GIT", realGit)
	script := `#!/bin/sh
set -eu
case " $* " in
  *" fetch "*) ;;
  *) exec "$POSSE_TEST_REAL_GIT" "$@" ;;
esac
if [ -n "${POSSE_TEST_GIT_FETCH_ACTIVE:-}" ]; then
  if ! mkdir "$POSSE_TEST_GIT_FETCH_ACTIVE" 2>/dev/null; then
    printf 'overlap\n' > "$POSSE_TEST_GIT_FETCH_OVERLAP"
  fi
  sleep 0.1
  rmdir "$POSSE_TEST_GIT_FETCH_ACTIVE" 2>/dev/null || true
fi
count=0
if [ -f "$POSSE_TEST_GIT_FETCH_COUNT" ]; then count=$(cat "$POSSE_TEST_GIT_FETCH_COUNT"); fi
count=$((count + 1))
printf '%s\n' "$count" > "$POSSE_TEST_GIT_FETCH_COUNT"
if [ "$count" -le "$POSSE_TEST_GIT_FETCH_FAILS" ]; then
  printf '%s\n' "$POSSE_TEST_GIT_FETCH_ERROR" >&2
  exit 1
fi
printf 'fetched\n'
`
	gitPath := filepath.Join(bin, "git")
	if err := os.WriteFile(gitPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GIT_FETCH_COUNT", countPath)
	return bin, countPath
}

func fakeGitFetchCalls(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return count
}
