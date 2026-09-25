package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWorkspaceGitLabMemberLandsOnlyAfterEveryMember(t *testing.T) {
	f := newWorkspaceLandFixture(t, "[defaults]\nlanding_mode = \"pr\"\nauto_unsaddle = \"never\"\n\n[repositories.worker]\nforge = \"gitlab\"\n", []string{"worker", "e2e-tool"}, []string{"worker", "e2e-tool"})
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
  *"merge_requests?state=opened"*) printf '[]\n' ;;
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
	gitTest(t, worker, "remote", "set-url", "origin", "https://git.example.com/group/worker.git")
	gitTest(t, worker, "config", "remote.origin.pushurl", filepath.Join(f.root, "remotes", "worker.git"))
	head := strings.TrimSpace(gitTest(t, filepath.Join(f.task.WorktreePath, "worker"), "rev-parse", "HEAD"))
	writeMR := func(mrState, pipeline string) {
		mr := map[string]any{"web_url": "https://git.example.com/group/worker/-/merge_requests/17", "state": mrState, "sha": head, "source_branch": "posse/t1", "source_project_id": 7, "target_project_id": 7, "detailed_merge_status": "mergeable", "head_pipeline": map[string]any{"status": pipeline}}
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
	if code := f.run("land", "t1"); code != 0 {
		t.Fatalf("open MR: %d %s", code, f.out.String())
	}
	if f.repos()["worker"].State != store.TaskRepoLanding {
		t.Fatalf("worker MR not open: %#v", f.repos())
	}
	if code := f.run("land", "t1", "--merge", "--user-approved", "merge both"); code != 0 {
		t.Fatalf("merge members: %d %s", code, f.out.String())
	}
	if f.state() != store.StateLanding || f.repos()["e2e-tool"].State != store.TaskRepoLanded {
		t.Fatalf("premature Land: %s %#v", f.state(), f.repos())
	}
	if err := f.service.pollWorkspacePullRequests(context.Background(), f.db, f.project, f.config(), true); err != nil {
		t.Fatal(err)
	}
	if f.state() != store.StateLanding {
		t.Fatalf("running pipeline Landed: %s", f.state())
	}
	writeMR("merged", "success")
	if err := f.service.pollWorkspacePullRequests(context.Background(), f.db, f.project, f.config(), true); err != nil {
		t.Fatal(err)
	}
	if f.state() != store.StateLanded || f.repos()["worker"].LandedRef != head {
		t.Fatalf("merged MR not Landed: %s %#v", f.state(), f.repos())
	}
	invocations, err := os.ReadFile(log)
	if err != nil || !strings.Contains(string(invocations), "--sha "+head) || !strings.Contains(string(invocations), "--auto-merge=false") {
		t.Fatalf("MR merge not pinned: %q %v", invocations, err)
	}
}
