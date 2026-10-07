package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bytes"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestApplyRecoveryRelaunchesAnsweredTask(t *testing.T) {
	f := newRelaunchFixture(t, store.StateFailed)
	ctx := context.Background()
	t.Chdir(f.project.Root)
	decision, err := f.db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: f.project.ID, TaskID: f.task.ID, Origin: "notice:failed", Kind: "recovery", Question: "Relaunch or discard?", Options: []string{"relaunch", "discard"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.AnswerDecision(ctx, f.project.ID, decision.ID, "relaunch", "User asked to relaunch"); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cli := f.service.CLI()
	cli.Out = &output
	if code := cli.Run([]string{"apply", fmt.Sprint(decision.ID)}); code != 0 {
		t.Fatalf("apply relaunch: %s", output.String())
	}
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil || task.State != store.StateWorking {
		t.Fatalf("relaunch: %#v %v", task, err)
	}
}

func TestApplyRecoveryDiscardWithPrunedMount(t *testing.T) {
	f := newPRLandingFixture(t, "local", store.StateWorking)
	defer f.db.Close()
	ctx := context.Background()
	attachPRFixtureMount(t, f)
	if err := f.db.Transition(ctx, f.task.ID, store.StateWorking, store.StateFailed, "worker", "failed"); err != nil {
		t.Fatal(err)
	}
	decision, err := f.db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: f.project.ID, TaskID: f.task.ID, Origin: "notice:failed", Kind: "recovery", Question: "Relaunch or discard?", Options: []string{"relaunch", "discard"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.AnswerDecision(ctx, f.project.ID, decision.ID, "discard", "User approved discard"); err != nil {
		t.Fatal(err)
	}
	fake := f.service.Herdr.(*herdr.Fake)
	fake.SnapshotValue.Agents = []herdr.Agent{{Name: "posse-shop-t1-1", PaneID: "w2:p1"}}
	f.service.Herdr = &changingSnapshotAdapter{Fake: fake, snapshot: fake.SnapshotValue}
	removeFixtureTaskPane(f)
	// Simulate a pruned worktree link without touching the Project repository.
	if err := os.WriteFile(filepath.Join(f.worktree, ".git"), []byte("gitdir: /missing/pruned/gitdir\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := f.run("apply", fmt.Sprint(decision.ID))
	if code != 0 {
		t.Fatalf("apply discard: %s %s", out, stderr)
	}
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil || task.State != store.StateTornDown {
		t.Fatalf("Task not torn down: %#v %v", task, err)
	}
}

func TestApplyAnsweredReviewChoices(t *testing.T) {
	for _, choice := range []string{"accept", "request-changes", "ignore"} {
		t.Run(choice, func(t *testing.T) {
			f := newPRLandingFixture(t, "local", store.StateWorking)
			defer f.db.Close()
			ctx := context.Background()
			reviewID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{Seq: 2, Type: "review", ReviewsTaskID: f.task.ID, Title: "Review", LandingMode: "local"})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.Transition(ctx, reviewID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Transition(ctx, reviewID, store.StateWorking, store.StateDone, "worker", "done"); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Transition(ctx, reviewID, store.StateDone, store.StateReported, "cli", "reported"); err != nil {
				t.Fatal(err)
			}
			decision, err := f.db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: f.project.ID, TaskID: reviewID, Kind: "review", Origin: "notice:review", Question: "Handle findings?", Options: []string{"accept", "request-changes", "ignore"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.AnswerDecision(ctx, f.project.ID, decision.ID, choice, "User chose "+choice); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				code, out, stderr := f.run("apply", fmt.Sprint(decision.ID))
				if code != 0 {
					t.Fatalf("apply: %s %s", out, stderr)
				}
			}
			var count int
			if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE task_id=?`, f.task.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 1
			if choice == "ignore" {
				want = 0
			}
			if count != want {
				t.Fatalf("messages=%d want %d", count, want)
			}
		})
	}
}

func TestApplyNoMistakesReviewAnswerQueuesRider(t *testing.T) {
	f := newPRLandingFixture(t, "local", store.StateWorking)
	defer f.db.Close()
	ctx := context.Background()
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET landing_mode='no-mistakes' WHERE id=?`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, f.task.ID, store.StateWorking, store.StateNeedsDecision, "worker", "findings"); err != nil {
		t.Fatal(err)
	}
	decision, err := f.db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: f.project.ID, TaskID: f.task.ID, Kind: "review", Origin: "notice:findings", Question: "Handle findings?", Options: []string{"accept", "request-changes", "ignore"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.AnswerDecision(ctx, f.project.ID, decision.ID, "request-changes", "User requests changes"); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := f.run("apply", fmt.Sprint(decision.ID))
	if code != 0 {
		t.Fatalf("apply: %s %s", out, stderr)
	}
	message, err := f.db.OldestQueuedMessage(ctx, f.task.ID)
	if err != nil || !strings.Contains(message.Body, "findings.toon") {
		t.Fatalf("findings not sent to Rider: %#v %v", message, err)
	}
}

func TestReportedTeardownFailureKeepsFilesAndRaisesDecision(t *testing.T) {
	f := newPRLandingFixture(t, "local", store.StateDone)
	defer f.db.Close()
	ctx := context.Background()
	attachPRFixtureMount(t, f)
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET type='scout' WHERE id=?`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, f.task.ID, store.StateDone, store.StateReported, "cli", "report saved"); err != nil {
		t.Fatal(err)
	}
	// The saved report already contains a different attachment at the same path.
	path := filepath.Join(f.home, "projects", "shop", "tasks", "t1", "change.txt")
	if err := os.WriteFile(path, []byte("different"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, "change.txt"), []byte("new edit"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := f.service.Herdr.(*herdr.Fake)
	fake.SnapshotValue.Agents = []herdr.Agent{{Name: "posse-shop-t1-1", PaneID: "w2:p1"}}
	f.service.Herdr = &changingSnapshotAdapter{Fake: fake, snapshot: fake.SnapshotValue}
	removeFixtureTaskPane(f)
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.unsaddleTask(ctx, f.db, f.project, cfg, task, false, "")
	if err == nil {
		t.Fatal("expected attachment conflict")
	}
	pending, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil || len(pending) != 1 || !strings.Contains(pending[0].Origin, "unrecoverable") {
		t.Fatalf("no repair/discard Decision: %#v %v", pending, err)
	}
	content, err := os.ReadFile(filepath.Join(f.worktree, "change.txt"))
	if err != nil || string(content) != "new edit" {
		t.Fatalf("Mount cleaned: %q %v", content, err)
	}
}
