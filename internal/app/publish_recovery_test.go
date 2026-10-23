package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestPublishDuringProjectRecoveryReturnsRetryableError(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateWorking)
	ctx := context.Background()
	claimed, _, err := f.db.ClaimProjectRecovery(ctx, f.project.ID, "gen-1", os.Getppid(), time.Now().Unix(), 0)
	if err != nil || !claimed {
		t.Fatalf("claim recovery: %v %v", claimed, err)
	}
	previous := projectRecoveryWait
	projectRecoveryWait = 100 * time.Millisecond
	t.Cleanup(func() { projectRecoveryWait = previous })

	code, output, stderr := f.run("publish", "Worker completion summary")
	if code != 1 || strings.Contains(output, "internal_error") || !strings.Contains(output, "project_recovery_running") || !strings.Contains(output, "\"project_recovery_running\",\"project recovery is still running for shop\",true") {
		t.Fatalf("publish during recovery: %d %s %s", code, output, stderr)
	}
	if got := strings.TrimSpace(gitTest(t, f.remote, "for-each-ref", "--format=%(refname)", "refs/heads/posse/")); got != "" {
		t.Fatalf("publish during recovery pushed %s", got)
	}

	if err := f.db.FinishProjectRecovery(ctx, f.project.ID, "gen-1", os.Getppid(), "", true); err != nil {
		t.Fatal(err)
	}
	f.setGHHead(t, f.headSHA)
	if code, output, stderr := f.run("publish", "Worker completion summary"); code != 0 {
		t.Fatalf("publish after recovery: %d %s %s", code, output, stderr)
	}
}

func TestPushFailureHelpNamesCredentialHelper(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	initRepo(t, repo)
	pushErr := errors.New("git push: fatal: could not read Username for 'https://github.com': terminal prompts disabled")
	if !isCredentialPromptFailure(pushErr) || isCredentialPromptFailure(errors.New("non-fast-forward")) {
		t.Fatal("credential prompt failure misclassified")
	}
	help := strings.Join(pushFailureHelp(ctx, repo, pushErr), "\n")
	if !strings.Contains(help, "no credential helper") {
		t.Fatalf("missing helper hint: %s", help)
	}
	gitTest(t, repo, "config", "credential.https://github.com.helper", "!gh auth git-credential")
	help = strings.Join(pushFailureHelp(ctx, repo, pushErr), "\n")
	if !strings.Contains(help, "!gh auth git-credential") || !strings.Contains(help, "gh auth status") {
		t.Fatalf("help does not name the helper: %s", help)
	}
	if got := pushFailureHelp(ctx, repo, errors.New("non-fast-forward")); len(got) != 1 {
		t.Fatalf("unrelated failure gained credential help: %v", got)
	}
}

func TestPushWithCredentialRetryRecoversFromTransientHelperFailure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	remote := filepath.Join(root, "remote.git")
	initRepo(t, repo)
	gitTest(t, root, "init", "--bare", "-q", remote)
	gitTest(t, repo, "remote", "add", "origin", remote)
	// The hook fails the first push the way git reports a helper that returned nothing.
	counter := filepath.Join(root, "attempts")
	hook := "#!/bin/sh\nn=$(cat '" + counter + "' 2>/dev/null || echo 0)\necho $((n+1)) > '" + counter + "'\n[ \"$n\" -ge 1 ] || { echo \"fatal: could not read Username for 'https://github.com'\" >&2; exit 1; }\n"
	if err := os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	previous := pushCredentialRetryDelay
	pushCredentialRetryDelay = time.Millisecond
	t.Cleanup(func() { pushCredentialRetryDelay = previous })
	if err := pushWithCredentialRetry(ctx, repo, "refs/heads/main"); err != nil {
		t.Fatalf("push did not recover: %v", err)
	}
}
