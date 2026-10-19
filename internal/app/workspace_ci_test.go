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

func TestWorkspacePRWatchRunsCILoopAgainstMemberGate(t *testing.T) {
	configText := `[defaults]
landing_mode = "local"
review = "none"
auto_unsaddle = "never"
[repositories.worker]
landing_mode = "pr"
[review_loop]
enabled = true
max_rounds = 3
block_at = "P1"
[gate_loop]
max_fix_rounds = 3
[ci_loop]
max_fix_rounds = 3
rerun_cancelled = 1
`
	fixture := newWorkspaceLandFixture(t, configText, []string{"worker"}, []string{"worker"})
	ctx := context.Background()
	workerRoot := filepath.Join(fixture.workspace, "worker")
	remote := filepath.Join(fixture.root, "remotes", "worker.git")
	gitTest(t, workerRoot, "remote", "set-url", "origin", "https://github.com/acme/worker.git")
	gitTest(t, workerRoot, "config", "remote.origin.pushurl", remote)
	gitTest(t, workerRoot, "config", "url.file://"+remote+".insteadOf", "https://github.com/acme/worker.git")

	repo := fixture.repos()["worker"]
	repo.PRURL = "https://github.com/acme/worker/pull/7"
	repo.GatedSHA = strings.TrimSpace(gitTest(t, repo.WorktreePath, "rev-parse", "refs/heads/"+fixture.task.Branch))
	repo.State = store.TaskRepoLanding
	if err := fixture.db.UpdateTaskRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}

	brief := []byte("---\ntype: ship\ntitle: Workspace CI loop\ndone_when: checks pass\nrepos: [worker]\n---\nKeep the workspace member changes.\n")
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief: taskplan.Select("brief", "workspace.md", brief),
		Execution: taskplan.Execution{Scope: "workspace", Base: "main", Target: "main", Members: []taskplan.Member{{
			Name: "worker", Path: "worker", Base: "main", Target: "main", Delivery: "pr",
		}}},
		Launch: taskplan.Launch{ProfileName: "author", Profile: config.Profile{Kind: "claude"}},
		Review: "none", Delivery: "local", Autonomy: config.Autonomy{Review: "ask", Land: "ask"},
		Paths: taskplan.Paths{Scratch: filepath.Join(fixture.home, "scratch", "stack", "t1"), Output: filepath.Join(fixture.home, "projects", "stack", "tasks", "t1")},
		ValidationLoops: &taskplan.ValidationLoops{
			Review: config.ReviewLoop{Enabled: true, MaxRounds: 3, BlockAt: "P1"},
			Gate:   config.GateLoop{MaxFixRounds: 3},
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
