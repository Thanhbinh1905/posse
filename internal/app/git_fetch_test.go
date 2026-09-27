package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestGitFetchRetriesTransientRefLock(t *testing.T) {
	bin, countPath := installFakeGitFetch(t)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GIT_FETCH_FAILS", "1")
	t.Setenv("POSSE_TEST_GIT_FETCH_ERROR", "fatal: cannot lock ref 'refs/remotes/origin/main': is at old but expected new")

	output, err := gitFetch(context.Background(), t.TempDir(), "origin")
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
			bin, countPath := installFakeGitFetch(t)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("POSSE_TEST_GIT_FETCH_FAILS", test.failures)
			t.Setenv("POSSE_TEST_GIT_FETCH_ERROR", test.message)

			_, err := gitFetch(context.Background(), t.TempDir(), "origin")
			if err == nil || !strings.Contains(err.Error(), test.wantErrSub) {
				t.Fatalf("gitFetch() error = %v, want containing %q", err, test.wantErrSub)
			}
			if got := fakeGitFetchCalls(t, countPath); got != test.wantCalls {
				t.Fatalf("fetch attempts = %d, want %d", got, test.wantCalls)
			}
		})
	}
}

func installFakeGitFetch(t *testing.T) (string, string) {
	t.Helper()
	bin := t.TempDir()
	countPath := filepath.Join(bin, "calls")
	script := `#!/bin/sh
set -eu
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
