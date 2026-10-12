package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestDiscardCaptureRemovesTemporaryGitRefOnBundleFailure(t *testing.T) {
	fixture := newPRLandingFixture(t, "local", store.StateWorking)
	ctx := context.Background()
	attachPRFixtureMount(t, fixture)
	commit := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/heads/posse/t1"))
	base := strings.TrimSpace(gitTest(t, fixture.repo, "merge-base", commit, "main"))
	artifactDir, err := ensureDiscardArtifactDirectory(fixture.home, fixture.project, fixture.task)
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(fixture.root, "fake-bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(binDir, "git")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "-C" ]; then repo=$2; shift 2; else repo=.; fi
if [ "$1" = "bundle" ] && [ "$2" = "create" ]; then echo injected bundle failure >&2; exit 23; fi
exec %q -C "$repo" "$@"
`, realGit)
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	err = captureDiscardTip(ctx, discardTip{repository: fixture.project.Name, root: fixture.repo, sha: commit, baseCommit: base}, artifactDir, fixture.task)
	if err == nil || !strings.Contains(err.Error(), "injected bundle failure") {
		t.Fatalf("capture failure = %v, want injected bundle creation error", err)
	}
	refsOutput, err := exec.Command(realGit, "-C", fixture.repo, "for-each-ref", "--format=%(refname)", "refs/posse/discard-captures/t1").CombinedOutput()
	refs := strings.TrimSpace(string(refsOutput))
	if err != nil || refs != "" {
		t.Fatalf("failed capture left a temporary Git ref: %q %v", refs, err)
	}
	temporary, err := filepath.Glob(filepath.Join(artifactDir, ".discard-*.bundle"))
	if err != nil || len(temporary) != 0 {
		t.Fatalf("failed capture left temporary bundle files: %#v %v", temporary, err)
	}
}

func TestCaptureApprovedDiscardTipAsRetainedGitBundle(t *testing.T) {
	fixture := newPRLandingFixture(t, "local", store.StateWorking)
	ctx := context.Background()
	attachPRFixtureMount(t, fixture)
	commit := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/heads/posse/t1"))
	if err := fixture.db.RecordApprovalWithBranch(ctx, fixture.task.ID, "discard", "approved discard", commit); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.captureDiscardTips(ctx, fixture.db, fixture.home, fixture.project, fixture.task); err != nil {
		t.Fatalf("capture approved tip: %v", err)
	}
	artifactDir, err := ensureDiscardArtifactDirectory(fixture.home, fixture.project, fixture.task)
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(artifactDir, discardTipName(fixture.project.Name, commit))
	if _, err := gitOutput(ctx, fixture.repo, "bundle", "verify", bundle); err != nil {
		t.Fatalf("verify discard bundle: %v", err)
	}
	heads, err := gitOutput(ctx, fixture.repo, "bundle", "list-heads", bundle)
	if err != nil || !bundleHasCommit(heads, commit) {
		t.Fatalf("discard bundle heads = %q, want approved commit %s: %v", heads, commit, err)
	}
	clone := filepath.Join(fixture.root, "discard-restore.git")
	if output, err := exec.Command("git", "init", "--bare", clone).CombinedOutput(); err != nil {
		t.Fatalf("initialize discard restore repository: %v %s", err, output)
	}
	gitTest(t, clone, "fetch", fixture.repo, "refs/heads/main:refs/heads/main")
	fields := strings.Fields(heads)
	if len(fields) < 2 {
		t.Fatalf("discard bundle head is malformed: %q", heads)
	}
	gitTest(t, clone, "fetch", bundle, fields[1]+":refs/heads/discarded")
	if got := strings.TrimSpace(gitTest(t, clone, "show", "refs/heads/discarded:change.txt")); got != "worker change" {
		t.Fatalf("restored discard content = %q", got)
	}
	refs, err := gitOutput(ctx, fixture.repo, "for-each-ref", "--format=%(refname)", "refs/posse/discard-captures/t1")
	if err != nil || refs != "" {
		t.Fatalf("temporary discard refs remain after capture: %q %v", refs, err)
	}
	if err := fixture.service.captureDiscardTips(ctx, fixture.db, fixture.home, fixture.project, fixture.task); err != nil {
		t.Fatalf("repeat discard capture: %v", err)
	}
}
