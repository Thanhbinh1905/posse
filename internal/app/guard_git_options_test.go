package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerGitGuardRefusesGitAfterUnresolvedCd(t *testing.T) {
	scope, _ := guardFixture(t)
	scope.env = append(scope.env, "HOME="+t.TempDir())
	for _, command := range []string{"cd && git fetch origin", "pushd && git fetch origin", "cd - && git fetch origin", "cd -P /tmp && git fetch origin", "pushd +0 && git fetch origin", "pushd -n /tmp && git fetch origin"} {
		if refused, reason := guardCommand(command, scope); !refused {
			t.Errorf("guard allowed Git after %q changed directory: %#v", command, reason)
		}
	}
}

func TestWorkerGitGuardDoesNotRunGitFromPATHWhileChecking(t *testing.T) {
	scope, _ := guardFixture(t)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "git-ran")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nprintf ran > \""+marker+"\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	scope.env = append(scope.env, "PATH="+bin)
	scope.cwd = repo
	scope.writable = append(scope.writable, repo)
	if _, reason := guardCommand(`cd "$(git rev-parse --show-toplevel)" && git status`, scope); reason.rule != "" {
		t.Fatalf("guard refused the Sandbox Git command: %#v", reason)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("guard executed the PATH-provided Git program: %v", err)
	}
}

func TestWorkerGitGuardAcceptsDocumentedReadOptions(t *testing.T) {
	scope, _ := guardFixture(t)
	for _, command := range []string{"git -P log", "git -p log", "git --no-optional-locks status", "git --no-replace-objects log", "git --literal-pathspecs log", "git --exec-path", "git -C . -P diff", "git -C $(pwd) diff"} {
		if refused, reason := guardCommand(command, scope); refused {
			t.Errorf("read %q refused: %#v", command, reason)
		}
	}
}

func TestWorkerGitGuardBlocksRemoteWritesThroughAliasesAndPlumbing(t *testing.T) {
	scope, _ := guardFixture(t)
	repo := t.TempDir()
	scope.cwd = repo
	scope.writable = append(scope.writable, repo)
	gitTest(t, repo, "init")
	gitTest(t, repo, "config", "alias.p", "push")
	foreign := filepath.Join(t.TempDir(), "foreign-worktree")
	gitTest(t, repo, "config", "alias.foreign", "worktree add "+foreign)
	for _, command := range []string{
		"git subtree push --prefix docs origin main",
		"git subtree --prefix docs push origin main",
		"git http-push origin HEAD:main",
		"git config alias.q push && git q origin HEAD",
		"git p origin HEAD",
		"git -C " + repo + " p origin HEAD",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=alias.r GIT_CONFIG_VALUE_0=push git r origin HEAD",
		"git -C $(pwd) -P push origin HEAD",
	} {
		if refused, reason := guardCommand(command, scope); !refused || !strings.Contains(reason.why, "posse publish") {
			t.Errorf("remote write %q allowed: %v %#v", command, refused, reason)
		}
	}
	for _, command := range []string{"git foreign", "git -c 'alias.foreign=worktree add " + foreign + "' foreign"} {
		if refused, reason := guardCommand(command, scope); !refused || !strings.Contains(reason.why, foreign) {
			t.Errorf("aliased Git write %q allowed: %v %#v", command, refused, reason)
		}
	}
	// A literal read alias and benign git config are not remote writes.
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"git config --get alias.p", "git log", "git subtree split --prefix docs"} {
		if refused, reason := guardCommand(command, scope); refused {
			t.Errorf("read %q refused: %#v", command, reason)
		}
	}
}
