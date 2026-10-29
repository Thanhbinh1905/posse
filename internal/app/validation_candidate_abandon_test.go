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

// clouddbv1 t32 (#727): a green Member MR at its gated head is merge-ready,
// naming that head, unless a newer candidate is pending; then Merge refuses
// too, naming the pending head.
func TestWorkspaceMemberMergeReadyWithheldWhileCandidatePending(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "no candidate", true: "pending candidate"}[pending], func(t *testing.T) {
			f := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"pr\"\nreview = \"none\"\nauto_teardown = \"never\"\n\n[repositories.worker]\nforge = \"gitlab\"\n", []string{"worker"}, []string{"worker"})
			ctx := context.Background()
			bin := filepath.Join(f.root, "bin")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(f.root, "glab.log")
			const script = `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$POSSE_TEST_GLAB_LOG"
case "$*" in
  *"/approvals"*) printf '{"approved":true,"approvals_left":0}\n' ;;
  *"/merge_requests/17"*) cat "$POSSE_TEST_GLAB_STATE" ;;
  "mr merge "*) printf 'Merged\n' ;;
  *) printf 'unexpected glab call: %s\n' "$*" >&2; exit 90 ;;
esac
`
			if err := os.WriteFile(filepath.Join(bin, "glab"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("POSSE_TEST_GLAB_LOG", log)
			worker := filepath.Join(f.workspace, "worker")
			remote := filepath.Join(f.root, "remotes", "worker.git")
			gitTest(t, worker, "remote", "set-url", "origin", "https://git.example.com/group/worker.git")
			gitTest(t, worker, "config", "remote.origin.pushurl", remote)
			gitTest(t, worker, "config", "url.file://"+remote+".insteadOf", "https://git.example.com/group/worker.git")

			memberWorktree := filepath.Join(f.task.WorktreePath, "worker")
			gated := strings.TrimSpace(gitTest(t, memberWorktree, "rev-parse", "HEAD"))
			repo := f.repos()["worker"]
			repo.PRURL = "https://git.example.com/group/worker/-/merge_requests/17"
			repo.GatedSHA = gated
			repo.State = store.TaskRepoMerge
			if err := f.db.UpdateTaskRepo(ctx, repo); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Transition(ctx, f.task.ID, store.StateDone, store.StateMerging, "cli", "merge t32 shape"); err != nil {
				t.Fatal(err)
			}
			mr, err := json.Marshal(map[string]any{"web_url": repo.PRURL, "state": "opened", "sha": gated, "source_branch": f.task.Branch, "target_branch": "main", "source_project_id": 7, "target_project_id": 7, "detailed_merge_status": "mergeable", "head_pipeline": map[string]any{"status": "success"}})
			if err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(f.root, "glab-state.json")
			if err := os.WriteFile(state, mr, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("POSSE_TEST_GLAB_STATE", state)
			scoped := strings.Repeat("9", 40)
			if pending {
				if _, err := f.db.CreateValidationCandidate(ctx, store.ValidationCandidate{
					TaskID: f.task.ID, Repo: "worker", SubmittedSHA: scoped, CandidateSHA: scoped, TargetSHA: gated,
					RefName: "refs/posse/candidate/t1", WorktreePath: filepath.Join(f.root, "candidate"), Summary: "Scoped candidate",
				}); err != nil {
					t.Fatal(err)
				}
			}

			if err := f.service.pollWorkspacePullRequests(ctx, f.db, f.project, f.config(), true); err != nil {
				t.Fatal(err)
			}
			notices, err := f.db.Notices(ctx, f.project.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			if pending {
				if countNoticeKind(notices, "merge_ready") != 0 {
					t.Fatalf("merge_ready raised while candidate %s is pending: %+v", scoped, notices)
				}
				if code := f.run("merge", "t1", "--execute", "--user-approved", "merge it"); code != 1 || !strings.Contains(f.out.String(), "candidate_pending") || !strings.Contains(f.out.String(), shortSHA(scoped)) {
					t.Fatalf("merge with a pending candidate exit=%d output=%s", code, f.out.String())
				}
				if calls, err := os.ReadFile(log); err != nil || strings.Contains(string(calls), "mr merge") {
					t.Fatalf("Member head was merged while a candidate is pending: %q %v", calls, err)
				}
				return
			}
			found := false
			for _, notice := range notices {
				found = found || notice.Kind == "merge_ready" && strings.Contains(notice.Summary, shortSHA(gated))
			}
			if !found {
				t.Fatalf("merge_ready does not name the gated head %s: %+v", gated, notices)
			}
		})
	}
}
