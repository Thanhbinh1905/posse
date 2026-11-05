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

func newTaskAdoptionFixture(t *testing.T) (*prMergeFixture, string) {
	t.Helper()
	originalPath := os.Getenv("PATH")
	bin := setupFollowupAgentPath(t, "pi")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+originalPath)
	f := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	removeFixtureTaskPane(f)
	f.service.Herdr.(*herdr.Fake).Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	gitTest(t, f.worktree, "push", "origin", "posse/t1")
	repository, err := gitCommonDir(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ClaimBranchAuthor(ctx, repository, f.task.Branch, f.task.ID, 1, f.headSHA, f.headSHA, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET pr_url='https://github.com/acme/shop/pull/17' WHERE id=?`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, f.project.ID, f.worktree, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, f.task.ID, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("[defaults]\nmerge_mode = \"pr\"\n[profiles.maintenance]\nkind = \"pi\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(f.root, "adopt.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Continue PR\ndone_when: verified\nadopt: t1\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return f, brief
}

func TestStartAdoptsStoppedTaskBranchAndPR(t *testing.T) {
	f, brief := newTaskAdoptionFixture(t)
	ctx := context.Background()
	const pr = "https://github.com/acme/shop/pull/17"
	if _, err := f.db.ExecContext(ctx, `INSERT INTO pr_observations(project_id,task_id,pr_url,head_sha,state,observed_at) VALUES(?,?,?,?,'OPEN',1)`, f.project.ID, f.task.ID, pr, f.headSHA); err != nil {
		t.Fatal(err)
	}
	delivery, err := f.db.EnsureDelivery(ctx, f.task.ID, "", pr, "publish")
	if err != nil {
		t.Fatal(err)
	}
	operation, err := f.db.RecordDeliveryOperation(ctx, store.DeliveryOperation{DeliveryID: delivery.ID, Operation: "inspect", Actor: "lead", Outcome: "succeeded"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `INSERT INTO approvals(task_id,action,user_quote,at) VALUES(?,'merge','Approved',1)`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET gated_sha=? WHERE id=?`, f.headSHA, f.task.ID); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := f.run("start", "--brief", brief, "--profile", "maintenance")
	if code != 0 {
		t.Fatalf("adopt exit=%d out=%s stderr=%s", code, out, stderr)
	}
	next, err := f.db.Task(ctx, f.project.ID, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if next.PRURL != "https://github.com/acme/shop/pull/17" || next.Branch != "posse/t1" || next.WorktreePath != f.worktree {
		t.Fatalf("adopting Task=%+v", next)
	}
	old, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil || old.PRURL != "" || old.State != store.StateTornDown || old.GatedSHA != "" {
		t.Fatalf("source=%+v err=%v", old, err)
	}
	observation, err := f.db.LatestPRObservation(ctx, old.ID)
	if err != nil || observation.HeadSHA != f.headSHA {
		t.Fatalf("source observation changed: %+v %v", observation, err)
	}
	if _, err := f.db.LatestPRObservation(ctx, next.ID); !store.IsNotFound(err) {
		t.Fatalf("adopter inherited observations: %v", err)
	}
	operations, err := f.db.DeliveryOperations(ctx, delivery.ID, 10)
	if err != nil || len(operations) != 1 || operations[0].ID != operation.ID {
		t.Fatalf("source operations changed: %+v %v", operations, err)
	}
	var approvals int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE task_id IN (?,?)`, old.ID, next.ID).Scan(&approvals); err != nil || approvals != 0 {
		t.Fatalf("approvals survived: %d %v", approvals, err)
	}
	if code, out, stderr := f.run("relaunch", "t1"); code == 0 {
		t.Fatalf("superseded Task relaunched: %s %s", out, stderr)
	}
	if blocked, err := taskBranchPruneBlocked(ctx, f.db, old); err != nil || !blocked {
		t.Fatalf("source can prune adopted branch: %v %v", blocked, err)
	}
}

func TestStartAdoptionInvalidSourceHasNoEffects(t *testing.T) {
	for _, scenario := range []string{"missing", "research", "without PR", "live executor", "unanswered Decision"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFirstOutcomeFixture(t)
			wantTasks := 0
			if scenario != "missing" {
				taskType := "change"
				if scenario == "research" {
					taskType = "research"
				}
				if _, err := f.db.CreateTask(context.Background(), f.project.ID, store.Task{Seq: 12, Type: taskType, Title: "Prior Task", State: store.StateSpawning}); err != nil {
					t.Fatal(err)
				}
				wantTasks = 1
				if scenario == "unanswered Decision" {
					if _, err := f.db.ExecContext(context.Background(), `UPDATE tasks SET pr_url='https://github.com/acme/shop/pull/17' WHERE seq=12`); err != nil {
						t.Fatal(err)
					}
					if _, err := f.db.ExecContext(context.Background(), `INSERT INTO decisions(project_id,task_id,origin,question,options_json,created_at) SELECT project_id,id,'adoption-test','Keep custody?','[]',1 FROM tasks WHERE seq=12`); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "live executor" {
					if _, err := f.db.ExecContext(context.Background(), `UPDATE tasks SET pr_url='https://github.com/acme/shop/pull/17',pane_id='w1:p2',pane_label='posse:shop:t12' WHERE seq=12`); err != nil {
						t.Fatal(err)
					}
					f.fake.SnapshotValue.Panes = append(f.fake.SnapshotValue.Panes, herdr.Pane{PaneID: "w1:p2", WorkspaceID: "w1", Label: "posse:shop:t12", Agent: "pi", AgentStatus: "idle"})
				}
			}
			if err := os.WriteFile(f.brief, []byte("---\ntype: change\ntitle: Continue prior PR\ndone_when: verified\nadopt: t12\n---\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HERDR_PANE_ID", "w1:p1")
			output := outcomeCLI(t, f.service, 1, "start", "--brief", f.brief)
			if !strings.Contains(output, "adoption_refused") {
				t.Fatal(output)
			}
			var tasks int
			if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM tasks`).Scan(&tasks); err != nil || tasks != wantTasks {
				t.Fatalf("tasks=%d err=%v", tasks, err)
			}
			for _, call := range f.fake.Calls {
				if call.Method == "tab.new" || call.Method == "agent.start" {
					t.Fatalf("refusal allocated a Worker: %#v", call)
				}
			}
		})
	}
}
