//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestMergedPRSettlesAfterTargetAdvancesPastPublishedHead(t *testing.T) {
	f := newPRLifecycleFixtureWithManualWatchOwner(t)
	defer f.db.Close()
	brief := filepath.Join(f.root, "base-advance.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Base advance\ndone_when: merged PR settles\n---\nPublish a change for an external merge.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.startAndComplete(t, brief, "t1")
	if output := runPosse(t, f.binary, f.repo, f.leadEnv, "merge", "t1"); !strings.Contains(output, "merging") {
		t.Fatalf("PR did not enter merging: %s", output)
	}
	task := f.mustTask(t, "t1")
	checkout := filepath.Join(f.root, "external-merge-t1")
	target := task.TargetBranch
	if target == "" {
		target = f.project.DefaultBranch
	}
	gitTest(t, f.env, f.root, "clone", "--branch", target, f.remote, checkout)
	gitTest(t, f.env, checkout, "config", "user.name", "Posse E2E")
	gitTest(t, f.env, checkout, "config", "user.email", "posse-e2e@example.test")
	gitTest(t, f.env, checkout, "fetch", "origin", "refs/heads/"+task.Branch+":refs/remotes/origin/"+task.Branch)
	gitTest(t, f.env, checkout, "merge", "--ff-only", "refs/remotes/origin/"+task.Branch)
	mergedHead := strings.TrimSpace(gitTest(t, f.env, checkout, "rev-parse", "HEAD"))
	if mergedHead != task.GatedSHA {
		t.Fatalf("external merge head = %s, want published head %s", mergedHead, task.GatedSHA)
	}
	if err := os.WriteFile(filepath.Join(checkout, "later-change.txt"), []byte("advance target beyond the published head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, checkout, "add", "later-change.txt")
	gitTest(t, f.env, checkout, "commit", "-m", "advance target beyond published head")
	targetTip := strings.TrimSpace(gitTest(t, f.env, checkout, "rev-parse", "HEAD"))
	gitTest(t, f.env, checkout, "push", "origin", "HEAD:refs/heads/"+target)
	if _, err := gitCommand(f.env, checkout, "merge-base", "--is-ancestor", task.GatedSHA, targetTip); err != nil {
		t.Fatalf("published head %s is not an ancestor of advanced target %s: %v", task.GatedSHA, targetTip, err)
	}
	gitTest(t, f.env, task.WorktreePath, "fetch", "origin", target+":refs/remotes/origin/"+target)
	f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", mergedHead, task.GatedSHA)
	if err := f.db.Transition(context.Background(), task.ID, store.StateMerging, store.StateDone, "cli", "external merge observation delayed"); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(f.binary, "teardown", "t1")
	command.Dir, command.Env = f.repo, f.leadEnv
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("teardown could not settle the externally merged Task: %v\n%s", err, output)
	}
	mergeOutput := runPosse(t, f.binary, f.repo, f.leadEnv, "merge", "t1")
	if !strings.Contains(mergeOutput, "already merged") {
		t.Fatalf("merge disagreed with the settled PR: %s", mergeOutput)
	}
	discard := exec.Command(f.binary, "teardown", "t1", "--discard", "--user-approved", "User approved discarding this Task")
	discard.Dir, discard.Env = f.repo, f.leadEnv
	if output, err := discard.CombinedOutput(); err != nil {
		t.Fatalf("discard disagreed with the settled Task state: %v\n%s", err, output)
	}
	settled := f.mustTask(t, "t1")
	if settled.State != store.StateMerged && settled.State != store.StateTornDown {
		t.Fatalf("externally merged PR did not settle on the next poll: state=%s", settled.State)
	}
	if settled.MergedRef != mergedHead {
		t.Fatalf("merged ref = %s, want published merge head %s", settled.MergedRef, mergedHead)
	}
	f.requireNotice(t, "t1", "pr_merged")
}
