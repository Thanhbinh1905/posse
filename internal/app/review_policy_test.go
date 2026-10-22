package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func freezeReviewPolicy(t *testing.T, f *reviewVerdictFixture, policy string, version int) {
	t.Helper()
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief:     taskplan.Select("brief", "change.md", []byte("---\ntype: change\ntitle: Candidate\ndone_when: checked\n---\n")),
		Execution: taskplan.Execution{Scope: "repo", Base: "main", Target: "main"},
		Launch:    taskplan.Launch{ProfileName: "author", Profile: config.Profile{Kind: "claude"}},
		Review:    "none", Delivery: "local", Autonomy: config.Autonomy{Review: "lead", Merge: "auto"},
		Paths: taskplan.Paths{Scratch: filepath.Join(f.home, "scratch", "shop", "t1"), Output: filepath.Join(f.home, "projects", "shop", "tasks", "t1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan.Review, plan.Version, plan.Preferences[1].Value = policy, version, policy
	data, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `UPDATE tasks SET plan_json=? WHERE id=?`, data, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `INSERT INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,1,?)`, f.task.ID, data); err != nil {
		t.Fatal(err)
	}
	f.task.PlanJSON = data
}

func TestSelectedReviewPolicyFrozenAndLegacyEvidenceVisible(t *testing.T) {
	for _, tc := range []struct {
		policy           string
		version          int
		blocked          bool
		blockedAfterPass bool
	}{
		{"none", taskplan.Version, false, false}, {"quick", taskplan.Version, true, false}, {"local", 3, true, false},
		{"full-pipeline", taskplan.Version, true, true}, {"no-mistakes", taskplan.Version, true, true},
		{"always", 1, true, false}, {"on_risk", 1, false, false}, {"never", 1, false, false},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			f := newReviewVerdictFixture(t)
			freezeReviewPolicy(t, f, tc.policy, tc.version)
			stubNoMistakesReceipt(t, func(string) (string, error) { return "", errors.New("exit status 1: unknown flag: --head") })
			cfg := config.Config{Defaults: config.Defaults{Review: "none"}}
			err := requireSelectedReview(context.Background(), f.db, f.project, cfg, f.task)
			if (err != nil) != tc.blocked {
				t.Fatalf("policy %s: %v", tc.policy, err)
			}
			var refusal *axi.Error
			if tc.policy == "no-mistakes" && (!errors.As(err, &refusal) || refusal.Code != "review_receipt_unsupported") {
				t.Fatal(err)
			}
			if (reviewEvidenceStatus(f.task) == "legacy-evidence") != (tc.version == 1) {
				t.Fatal("wrong evidence status")
			}
			if code, out := f.signal(t, f.review.PaneID, "done", "Checked", "--report", "report.md", "--verdict", "pass"); code != 0 {
				t.Fatalf("verdict: %d %s", code, out)
			}
			err = requireSelectedReview(context.Background(), f.db, f.project, cfg, f.task)
			if (err != nil) != tc.blockedAfterPass {
				t.Fatalf("policy with review pass: %v", err)
			}
			if tc.policy == "full-pipeline" {
				var refusal *axi.Error
				if !errors.As(err, &refusal) || refusal.Code != "candidate_missing" {
					t.Fatalf("full-pipeline did not require a private candidate: %v", err)
				}
			}
		})
	}
}

func TestWorkspaceSelectedReviewOverridesStayFrozen(t *testing.T) {
	f := newWorkspaceMergeFixture(t, localWorkspaceConfig+"review='quick'\n[repositories.backend]\nreview='none'\n[repositories.worker]\nreview='no-mistakes'\n", []string{"backend", "worker"}, []string{"backend"})
	ctx := context.Background()
	task, err := f.db.TaskByID(ctx, f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.config()
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief: taskplan.Select("brief", "workspace.md", []byte("---\ntype: change\ntitle: Member policy\ndone_when: checked\nrepos: [backend, worker]\n---\n")),
		Execution: taskplan.Execution{Scope: "workspace", Members: []taskplan.Member{
			{Name: "backend", Path: "backend", Delivery: "local", Review: cfg.MemberReview("backend"), ReviewSource: "repositories.backend.review"},
			{Name: "worker", Path: "worker", Delivery: "local", Review: cfg.MemberReview("worker"), ReviewSource: "repositories.worker.review"},
		}},
		Launch: taskplan.Launch{ProfileName: "author", Profile: config.Profile{Kind: "claude"}},
		Review: cfg.Defaults.Review, Delivery: "local", Autonomy: config.Autonomy{Review: "ask", Merge: "ask"},
		Paths: taskplan.Paths{Scratch: filepath.Join(f.home, "scratch", "stack", "t1"), Output: filepath.Join(f.home, "projects", "stack", "tasks", "t1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	task.PlanJSON, err = plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, "UPDATE tasks SET plan_json=? WHERE id=?", task.PlanJSON, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `INSERT INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,1,?)`, task.ID, task.PlanJSON); err != nil {
		t.Fatal(err)
	}
	// Live config cannot replace the selected Member policy. Unchanged worker's
	// no-mistakes selection must not block backend's none policy.
	if _, err := config.SetFile(config.ConfigPath(f.home, ""), "repositories.backend.review", "no-mistakes", false); err != nil {
		t.Fatal(err)
	}
	if code := f.run("merge", "t1", "--execute", "--user-approved", "Merge the selected local member"); code != 0 {
		t.Fatalf("frozen member policy refused: %s", f.out.String())
	}
}

func TestLocalReviewRecheckedBeforeMerge(t *testing.T) {
	f := newReviewVerdictFixture(t)
	configText := "[defaults]\nmerge_mode='local'\nreview='quick'\nauto_teardown='never'\n"
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(configText), 0600); err != nil {
		t.Fatal(err)
	}
	if code, out := f.signal(t, f.review.PaneID, "done", "Checked", "--report", "report.md", "--verdict", "pass"); code != 0 {
		t.Fatalf("verdict: %d %s", code, out)
	}
	if code, out, _ := f.run("merge", "t1"); code != 0 {
		t.Fatalf("merge: %d %s", code, out)
	}
	before := gitTest(t, f.repo, "rev-parse", "main")
	// Evidence becomes ineligible even though the branch still equals the recorded merge head.
	if _, err := f.db.NextTaskLaunchWithIdentity(context.Background(), f.task.ID, "reviewer", "model-b", true); err != nil {
		t.Fatal(err)
	}
	code, out, _ := f.run("merge", "t1", "--execute", "--user-approved", "Merge this candidate")
	if code != 1 || !strings.Contains(out, "review_required") {
		t.Fatalf("merge boundary: %d %s", code, out)
	}
	if got := gitTest(t, f.repo, "rev-parse", "main"); got != before {
		t.Fatal("refusal changed target")
	}
	task, err := f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil || task.State == store.StateMerged {
		t.Fatalf("refusal merged: %#v %v", task, err)
	}
}
