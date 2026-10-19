package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWorkspaceGitLabMemberMergesOnlyAfterEveryMember(t *testing.T) {
	t.Setenv("POSSE_TEST_GLAB_BASE_BRANCH", "staging")
	f := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"pr\"\nauto_teardown = \"never\"\n\n[repositories.worker]\nforge = \"gitlab\"\n", []string{"worker", "e2e-tool"}, []string{"worker", "e2e-tool"})
	fake, ok := f.service.Herdr.(*herdr.Fake)
	if !ok {
		t.Fatal("fixture does not use the fake Herdr adapter")
	}
	f.service.Herdr = nil
	bin := filepath.Join(f.root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(f.root, "glab.log")
	state := filepath.Join(f.root, "glab-state.json")
	const script = `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$POSSE_TEST_GLAB_LOG"
case "$*" in
  *"merge_requests?state=opened"*) if [ -n "${POSSE_TEST_GLAB_LIST:-}" ]; then cat "$POSSE_TEST_GLAB_LIST"; else printf '[]\n'; fi ;;
  *"/approvals"*) printf '{"approved":true,"approvals_left":0}\n' ;;
  *"/merge_requests/17"*) cat "$POSSE_TEST_GLAB_STATE" ;;
  "mr create "*) printf 'https://git.example.com/group/worker/-/merge_requests/17\n' ;;
  "mr merge "*) printf 'Merged\n' ;;
  *) printf 'unexpected glab call: %s\n' "$*" >&2; exit 90 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "glab"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GLAB_LOG", log)
	t.Setenv("POSSE_TEST_GLAB_STATE", state)
	worker := filepath.Join(f.workspace, "worker")
	gitTest(t, worker, "branch", "staging", "main")
	gitTest(t, worker, "push", "origin", "staging")
	gitTest(t, worker, "remote", "set-url", "origin", "https://git.example.com/group/worker.git")
	gitTest(t, worker, "config", "remote.origin.pushurl", filepath.Join(f.root, "remotes", "worker.git"))
	gitTest(t, worker, "config", "url.file://"+filepath.Join(f.root, "remotes", "worker.git")+".insteadOf", "https://git.example.com/group/worker.git")
	if _, err := f.db.ExecContext(context.Background(), `UPDATE task_repos SET target_branch='staging' WHERE task_id=? AND repo='worker'`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	memberWorktree := filepath.Join(f.task.WorktreePath, "worker")
	head := strings.TrimSpace(gitTest(t, memberWorktree, "rev-parse", "HEAD"))
	branch := strings.TrimSpace(gitTest(t, memberWorktree, "branch", "--show-current"))
	writeMR := func(mrState, pipeline string) {
		mr := map[string]any{"web_url": "https://git.example.com/group/worker/-/merge_requests/17", "state": mrState, "sha": head, "source_branch": branch, "target_branch": "staging", "source_project_id": 7, "target_project_id": 7, "detailed_merge_status": "mergeable", "head_pipeline": map[string]any{"status": pipeline}}
		if mrState == "merged" {
			mr["merge_commit_sha"] = head
		}
		encoded, err := json.Marshal(mr)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(state, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMR("opened", "running")
	if err := f.db.Transition(context.Background(), f.task.ID, store.StateDone, store.StateWorking, "lead", "workspace publish test"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_WORKER_HOME", f.home)
	t.Chdir(f.task.WorktreePath)
	if code := f.run("publish", "--repo", "worker", "Worker summary"); code != 0 || !strings.Contains(f.out.String(), "https://git.example.com/group/worker/-/merge_requests/17") {
		t.Fatalf("publish MR: %d %s repos=%#v", code, f.out.String(), f.repos())
	}
	listPath := filepath.Join(f.root, "open-mrs.json")
	if err := os.WriteFile(listPath, []byte(`[{"web_url":"https://git.example.com/group/worker/-/merge_requests/17","sha":"`+head+`","source_branch":"`+branch+`","target_branch":"staging","source_project_id":7,"target_project_id":7}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GLAB_LIST", listPath)
	if code := f.run("publish", "--repo", "worker", "Worker summary"); code != 0 || !strings.Contains(f.out.String(), "https://git.example.com/group/worker/-/merge_requests/17") {
		t.Fatalf("republish MR: %d %s", code, f.out.String())
	}
	calls, err := os.ReadFile(log)
	if err != nil || strings.Count(string(calls), "mr create") != 1 || !strings.Contains(string(calls), "--target-branch staging") {
		t.Fatalf("workspace MR target or duplicate-free republish failed: %s (%v)", calls, err)
	}
	if code := f.run("signal", "done", "Worker summary"); code != 0 {
		t.Fatalf("done: %d %s", code, f.out.String())
	}
	t.Setenv("POSSE_WORKER_HOME", "")
	t.Chdir(f.workspace)
	f.service.Herdr = fake
	if code := f.run("merge", "t1"); code != 0 {
		t.Fatalf("open MR: %d %s", code, f.out.String())
	}
	if f.repos()["worker"].State != store.TaskRepoMerge {
		t.Fatalf("worker MR not open: %#v", f.repos())
	}
	if code := f.run("merge", "t1", "--execute", "--user-approved", "merge both"); code != 0 {
		t.Fatalf("merge members: %d %s", code, f.out.String())
	}
	if f.state() != store.StateMerging || f.repos()["e2e-tool"].State != store.TaskRepoMerged {
		t.Fatalf("premature Merge: %s %#v", f.state(), f.repos())
	}
	if err := f.service.pollWorkspacePullRequests(context.Background(), f.db, f.project, f.config(), true); err != nil {
		t.Fatal(err)
	}
	if f.state() != store.StateMerging {
		t.Fatalf("running pipeline Merged: %s", f.state())
	}
	gitTest(t, memberWorktree, "push", "origin", "HEAD:refs/heads/staging")
	writeMR("merged", "success")
	if err := f.service.pollWorkspacePullRequests(context.Background(), f.db, f.project, f.config(), true); err != nil {
		t.Fatal(err)
	}
	if f.state() != store.StateMerged || f.repos()["worker"].MergedRef != head || f.repos()["worker"].TargetBranch != "staging" {
		t.Fatalf("merged MR not Merged against staging: %s %#v", f.state(), f.repos())
	}
	fake.SnapshotValue.Agents = []herdr.Agent{{Name: "posse-stack-t1-1", PaneID: "w2:p1"}}
	f.service.Herdr = &changingSnapshotAdapter{Fake: fake, snapshot: fake.SnapshotValue}
	if err := f.service.autoTeardownMergedTasks(context.Background(), f.db, f.project, f.config()); err != nil {
		t.Fatalf("teardown merged workspace Task: %v", err)
	}
	if f.state() != store.StateTornDown {
		notices, _ := f.db.Notices(context.Background(), f.project.ID, false)
		t.Fatalf("merged workspace Task was not torn down: state=%s notices=%+v", f.state(), notices)
	}
	invocations, err := os.ReadFile(log)
	if err != nil || !strings.Contains(string(invocations), "--sha "+head) || !strings.Contains(string(invocations), "--auto-merge=false") {
		t.Fatalf("MR merge not pinned: %q %v", invocations, err)
	}
}
