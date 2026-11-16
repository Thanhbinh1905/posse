//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWorkspaceTeardownSettlesMergedMemberPRAfterTargetAdvances(t *testing.T) {
	f := newWorkspacePRReadinessFixture(t)
	f.writeView(t, f.head, "APPROVED")
	writeMergedMemberView := func() {
		t.Helper()
		view := map[string]any{
			"url": f.prURL, "state": "MERGED", "headRefOid": f.head, "headRefName": f.branch,
			"baseRefName": "main", "mergeable": "MERGEABLE", "reviewDecision": "APPROVED",
			"headRepository": map[string]any{"nameWithOwner": "acme/member"},
			"mergeCommit":    map[string]any{"oid": f.head}, "statusCheckRollup": []any{},
		}
		encoded, err := json.Marshal(view)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.viewPath, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, f.base.env, f.memberRoot, "merge", "--ff-only", f.head)
	if err := os.WriteFile(filepath.Join(f.memberRoot, "later-change.txt"), []byte("advance target beyond published head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.base.env, f.memberRoot, "add", "later-change.txt")
	gitTest(t, f.base.env, f.memberRoot, "commit", "-m", "advance target beyond published head")
	targetTip := strings.TrimSpace(gitTest(t, f.base.env, f.memberRoot, "rev-parse", "HEAD"))
	if _, err := gitCommand(f.base.env, f.memberRoot, "merge-base", "--is-ancestor", f.head, targetTip); err != nil {
		t.Fatalf("published head is not an ancestor of advanced target: %v", err)
	}
	gitTest(t, f.base.env, f.memberRoot, "push", "origin", "HEAD:refs/heads/main")
	gitTest(t, f.base.env, f.worktree, "fetch", "origin", "main:refs/remotes/origin/main")
	if _, err := f.base.db.ExecContext(context.Background(), `UPDATE task_repos SET state='unchanged',gated_sha='' WHERE task_id=? AND repo='member'`, f.taskID); err != nil {
		t.Fatal(err)
	}
	f.writeView(t, f.head, "APPROVED")
	mergeBeforePoll := exec.Command(f.base.binary, "merge", "t1")
	mergeBeforePoll.Dir, mergeBeforePoll.Env = f.workspace, f.leadEnv
	mergeOutput, mergeErr := mergeBeforePoll.CombinedOutput()
	if mergeErr == nil || !strings.Contains(strings.ToLower(string(mergeOutput)), "no member") {
		t.Fatalf("merge did not reproduce the no-member-change refusal: err=%v\n%s", mergeErr, mergeOutput)
	}
	writeMergedMemberView()

	discard := exec.Command(f.base.binary, "teardown", "t1", "--discard", "--user-approved", "User approved discarding this Task")
	discard.Dir, discard.Env = f.workspace, f.leadEnv
	discardOutput, discardErr := discard.CombinedOutput()
	if discardErr == nil {
		t.Fatalf("discard accepted the externally merged Member PR:\n%s", discardOutput)
	}
	task, err := f.base.db.TaskByID(context.Background(), f.project.ID, f.taskID)
	members, membersErr := f.base.db.TaskRepos(context.Background(), f.taskID)
	if err != nil || task.State != store.StateMerged {
		t.Fatalf("workspace Task after discard reconciliation = %#v, %v; Members=%#v (%v)", task, err, members, membersErr)
	}
	teardown := exec.Command(f.base.binary, "teardown", "t1")
	teardown.Dir, teardown.Env = f.workspace, f.leadEnv
	teardownOutput, teardownErr := teardown.CombinedOutput()
	if teardownErr != nil {
		t.Fatalf("workspace teardown refused the externally merged Member PR: %v\n%s", teardownErr, teardownOutput)
	}
	task, err = f.base.db.TaskByID(context.Background(), f.project.ID, f.taskID)
	if err != nil || task.State != store.StateTornDown {
		t.Fatalf("workspace Task after teardown = %#v, %v", task, err)
	}
	member, err := f.base.db.TaskRepos(context.Background(), f.taskID)
	if err != nil || len(member) != 1 || member[0].State != store.TaskRepoMerged {
		t.Fatalf("workspace Members after teardown = %#v, %v", member, err)
	}
	merge := exec.Command(f.base.binary, "merge", "t1")
	merge.Dir, merge.Env = f.workspace, f.leadEnv
	if output, err := merge.CombinedOutput(); err != nil || !strings.Contains(string(output), "already merged") {
		t.Fatalf("merge disagreed with the settled workspace Task: err=%v\n%s", err, output)
	}
	discard = exec.Command(f.base.binary, "teardown", "t1", "--discard", "--user-approved", "User approved discarding this Task")
	discard.Dir, discard.Env = f.workspace, f.leadEnv
	if output, err := discard.CombinedOutput(); err != nil {
		t.Fatalf("discard disagreed with the settled workspace Task: %v\n%s", err, output)
	}
}
