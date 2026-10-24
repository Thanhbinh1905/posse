package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func TestWorkspacePRWatchRunsCILoopAgainstMemberPR(t *testing.T) {
	configText := `[defaults]
merge_mode = "local"
review = "none"
auto_teardown = "never"
[repositories.worker]
merge_mode = "pr"
[review_loop]
enabled = true
max_rounds = 3
block_at = "P1"
[ci_loop]
max_fix_rounds = 3
rerun_cancelled = 1
`
	fixture := newWorkspaceMergeFixture(t, configText, []string{"worker"}, []string{"worker"})
	ctx := context.Background()
	workerRoot := filepath.Join(fixture.workspace, "worker")
	remote := filepath.Join(fixture.root, "remotes", "worker.git")
	gitTest(t, workerRoot, "remote", "set-url", "origin", "https://github.com/acme/worker.git")
	gitTest(t, workerRoot, "config", "remote.origin.pushurl", remote)
	gitTest(t, workerRoot, "config", "url.file://"+remote+".insteadOf", "https://github.com/acme/worker.git")

	repo := fixture.repos()["worker"]
	repo.PRURL = "https://github.com/acme/worker/pull/7"
	repo.GatedSHA = strings.TrimSpace(gitTest(t, repo.WorktreePath, "rev-parse", "refs/heads/"+fixture.task.Branch))
	repo.State = store.TaskRepoMerge
	if err := fixture.db.UpdateTaskRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}

	brief := []byte("---\ntype: change\ntitle: Workspace CI loop\ndone_when: checks pass\nrepos: [worker]\n---\nKeep the workspace member changes.\n")
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief: taskplan.Select("brief", "workspace.md", brief),
		Execution: taskplan.Execution{Scope: "workspace", Base: "main", Target: "main", Members: []taskplan.Member{{
			Name: "worker", Path: "worker", Base: "main", Target: "main", Delivery: "pr",
		}}},
		Launch: taskplan.Launch{ProfileName: "author", Profile: config.Profile{Kind: "claude"}},
		Review: "none", Delivery: "local", Autonomy: config.Autonomy{Review: "ask", Merge: "ask"},
		Paths: taskplan.Paths{Scratch: filepath.Join(fixture.home, "scratch", "stack", "t1"), Output: filepath.Join(fixture.home, "projects", "stack", "tasks", "t1")},
		ValidationLoops: &taskplan.ValidationLoops{
			Review: config.ReviewLoop{Enabled: true, MaxRounds: 3, BlockAt: "P1"},
			CI:     config.CILoop{MaxFixRounds: 3, RerunCancelled: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET plan_json=? WHERE id=?`, encoded, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `INSERT INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,1,?)`, fixture.task.ID, encoded); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(fixture.root, "bin")
	logPath := filepath.Join(fixture.root, "gh.log")
	ghScript := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$POSSE_TEST_GH_LOG"
case "$1 $2" in
  "pr view") cat "$POSSE_TEST_GH_VIEW" ;;
  "run rerun") printf 'rerun requested\n' ;;
  "run view") printf 'unit failed log\n' ;;
  *) exit 90 ;;
esac
`
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(ghScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GH_LOG", logPath)
	view, err := json.Marshal(map[string]any{
		"url": repo.PRURL, "state": "OPEN", "headRefOid": repo.GatedSHA, "baseRefName": "main", "mergeable": "MERGEABLE", "reviewDecision": "REVIEW_REQUIRED",
		"statusCheckRollup": []any{map[string]any{
			"__typename": "CheckRun", "name": "unit", "status": "COMPLETED", "conclusion": "CANCELLED",
			"detailsUrl": "https://github.com/acme/worker/actions/runs/123/job/456", "databaseId": 456,
			"checkSuite": map[string]any{"workflowRun": map[string]any{"databaseId": 123}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	viewPath := filepath.Join(fixture.root, "gh-view.json")
	if err := os.WriteFile(viewPath, view, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GH_VIEW", viewPath)

	poll := func() {
		t.Helper()
		if err := fixture.service.pollWorkspacePullRequests(ctx, fixture.db, fixture.project, fixture.config(), true); err != nil {
			t.Fatal(err)
		}
	}
	poll()
	poll()
	var rounds, reruns int
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`, fixture.task.ID).Scan(&rounds); err != nil || rounds != 1 {
		t.Fatalf("workspace CI fix rounds=%d err=%v", rounds, err)
	}
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='ci_rerun_requested'`, fixture.task.ID).Scan(&reruns); err != nil || reruns != 1 {
		t.Fatalf("workspace CI rerun notices=%d err=%v", reruns, err)
	}
	var body string
	if err := fixture.db.QueryRowContext(ctx, `SELECT body FROM messages WHERE task_id=? AND status='queued'`, fixture.task.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "--repo worker") || !strings.Contains(body, "unit failed log") {
		t.Fatalf("workspace CI instruction omitted member evidence: %s", body)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), "run rerun --failed 123 --repo acme/worker") != 1 {
		t.Fatalf("workspace rerun commands = %q", log)
	}
}

// A Member PR reads OPEN, then the target advances before Posse refetches it.
// The new tip may hold the PR's own squash merge, so the poll views the PR
// again instead of asking it to merge its own merge (#482).
func TestWorkspacePRWatchRequeriesMemberPRWhoseTargetAdvancedDuringQuery(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		merged     bool
		wantRounds int
	}{
		{name: "merged during query", merged: true},
		{name: "still open behind", wantRounds: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"local\"\nreview = \"none\"\nauto_teardown = \"never\"\n[repositories.worker]\nmerge_mode = \"pr\"\n", []string{"worker"}, []string{"worker"})
			ctx := context.Background()
			workerRoot := filepath.Join(fixture.workspace, "worker")
			remote := filepath.Join(fixture.root, "remotes", "worker.git")
			gitTest(t, workerRoot, "remote", "set-url", "origin", "https://github.com/acme/worker.git")
			gitTest(t, workerRoot, "config", "remote.origin.pushurl", remote)
			gitTest(t, workerRoot, "config", "url.file://"+remote+".insteadOf", "https://github.com/acme/worker.git")
			repo := fixture.repos()["worker"]
			repo.PRURL = "https://github.com/acme/worker/pull/7"
			repo.GatedSHA = strings.TrimSpace(gitTest(t, repo.WorktreePath, "rev-parse", "refs/heads/"+fixture.task.Branch))
			repo.State = store.TaskRepoMerge
			if err := fixture.db.UpdateTaskRepo(ctx, repo); err != nil {
				t.Fatal(err)
			}
			squash := filepath.Join(fixture.root, "squash-merge")
			gitTest(t, workerRoot, "worktree", "add", "--detach", squash, "refs/heads/main")
			gitTest(t, squash, "merge", "--squash", repo.GatedSHA)
			gitTest(t, squash, "commit", "-m", "squash merge worker PR")
			merge := strings.TrimSpace(gitTest(t, squash, "rev-parse", "HEAD"))

			bin := filepath.Join(fixture.root, "bin")
			ghScript := `#!/bin/sh
set -eu
case "$1 $2" in
  "pr view")
    if [ -e "$POSSE_TEST_GH_ADVANCE" ]; then
      git -C "$POSSE_TEST_GH_ADVANCE_REPO" push --quiet origin "$(cat "$POSSE_TEST_GH_ADVANCE"):refs/heads/main"
      rm "$POSSE_TEST_GH_ADVANCE"
    fi
    cat "$POSSE_TEST_GH_VIEW"
    if [ -e "$POSSE_TEST_GH_VIEW.next" ]; then mv "$POSSE_TEST_GH_VIEW.next" "$POSSE_TEST_GH_VIEW"; fi ;;
  *) exit 90 ;;
esac
`
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(ghScript), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			advance := filepath.Join(fixture.root, "gh-advance")
			if err := os.WriteFile(advance, []byte(merge), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("POSSE_TEST_GH_ADVANCE", advance)
			t.Setenv("POSSE_TEST_GH_ADVANCE_REPO", workerRoot)
			viewPath := filepath.Join(fixture.root, "gh-view.json")
			t.Setenv("POSSE_TEST_GH_VIEW", viewPath)
			writeView := func(path, state, mergeCommit string) {
				t.Helper()
				view, err := json.Marshal(map[string]any{
					"url": repo.PRURL, "state": state, "headRefOid": repo.GatedSHA, "baseRefName": "main", "mergeable": "MERGEABLE", "reviewDecision": "APPROVED",
					"mergeCommit": map[string]any{"oid": mergeCommit}, "statusCheckRollup": []any{},
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, view, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			writeView(viewPath, "OPEN", "")
			if scenario.merged {
				writeView(viewPath+".next", "MERGED", merge)
			}

			if err := fixture.service.pollWorkspacePullRequests(ctx, fixture.db, fixture.project, fixture.config(), true); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(advance); !os.IsNotExist(err) {
				t.Fatalf("fake forge did not advance the target during the PR view: %v", err)
			}
			observation, err := fixture.db.LatestMemberPRObservation(ctx, fixture.task.ID, "worker")
			if err != nil || (scenario.merged && observation.State != "MERGED") || (!scenario.merged && decodeChecks(observation.Checks).State != "STALE") {
				t.Fatalf("observation after the target advanced during the PR view = %#v err=%v", observation, err)
			}
			var entries, rounds int
			if err := fixture.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM merge_queue_entries WHERE task_id=? AND dequeued_at=0),(SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='freshness')`, fixture.task.ID, fixture.task.ID).Scan(&entries, &rounds); err != nil {
				t.Fatal(err)
			}
			if rounds != scenario.wantRounds || entries != scenario.wantRounds {
				t.Fatalf("live merge queue entries=%d merge-target instructions=%d, want %d each", entries, rounds, scenario.wantRounds)
			}
		})
	}
}
