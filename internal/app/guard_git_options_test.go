package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRiderGitGuardAcceptsDocumentedReadOptions(t *testing.T) {
	scope, _ := guardFixture(t)
	for _, command := range []string{"git -P log", "git -p log", "git --no-optional-locks status", "git --no-replace-objects log", "git --literal-pathspecs log", "git --exec-path", "git -C . -P diff", "git -C $(pwd) diff"} {
		if refused, reason := guardCommand(command, scope); refused {
			t.Errorf("read %q refused: %#v", command, reason)
		}
	}
}

func TestRiderGitGuardBlocksRemoteWritesThroughAliasesAndPlumbing(t *testing.T) {
	scope, _ := guardFixture(t)
	repo := t.TempDir()
	scope.cwd = repo
	gitTest(t, repo, "init")
	gitTest(t, repo, "config", "alias.p", "push")
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
