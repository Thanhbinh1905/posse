package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func TestPRMergeQueuesThreeGateFixRoundsThenExhaustsBudget(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	installGateValidationPlan(t, fixture, 3)
	writeFailingGate(t, fixture, "printf 'gate output tail'; exit 7")

	for round := 1; round <= 4; round++ {
		code, output, errOutput := fixture.run("merge", "t1")
		if code != 1 || !strings.Contains(output, "gate_failed") || !strings.Contains(output, "exit status 7") {
			t.Fatalf("Gate failure %d was not reported: exit=%d output=%s error=%s", round, code, output, errOutput)
		}
		notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := countNoticeKind(notices, "gate_failed"); got != round {
			t.Fatalf("gate_failed count after failure %d = %d", round, got)
		}
		if round <= 3 {
			if got := countNoticeKind(notices, "gate_fix_queued"); got != round || countNoticeKind(notices, "gate_fix_exhausted") != 0 {
				t.Fatalf("Gate fix Notices after round %d: %#v", round, notices)
			}
			var body string
			if err := fixture.db.QueryRowContext(context.Background(), `SELECT body FROM messages WHERE task_id=? AND status='queued' ORDER BY id DESC LIMIT 1`, fixture.task.ID).Scan(&body); err != nil {
				t.Fatalf("round %d did not queue its fix instruction: %v", round, err)
			}
			gatedHead := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
			if !strings.Contains(body, gatedHead) || !strings.Contains(body, "Exit status: 7") || !strings.Contains(body, "gate output tail") || !strings.Contains(body, "E2E intent") {
				t.Fatalf("round %d instruction omitted failure evidence or Brief intent: %s", round, body)
			}
			var rounds int
			if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='gate'`, fixture.task.ID).Scan(&rounds); err != nil || rounds != round {
				t.Fatalf("Gate rounds after failure %d = %d, err=%v", round, rounds, err)
			}
			if round < 4 {
				completeGateFixRound(t, fixture, round)
			}
			continue
		}

		if countNoticeKind(notices, "gate_fix_queued") != 3 || countNoticeKind(notices, "gate_fix_exhausted") != 1 {
			t.Fatalf("fourth failure did not exhaust the Gate fix budget: %#v", notices)
		}
		var queued, rounds int
		if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE task_id=? AND status='queued'`, fixture.task.ID).Scan(&queued); err != nil {
			t.Fatal(err)
		}
		if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='gate'`, fixture.task.ID).Scan(&rounds); err != nil {
			t.Fatal(err)
		}
		if queued != 0 || rounds != 3 {
			t.Fatalf("exhausted budget queued more work: queued=%d rounds=%d", queued, rounds)
		}
	}
}

func TestLocalMergeQueuesGateFixInstruction(t *testing.T) {
	fixture := newPRMergeFixture(t, "local", store.StateDone)
	installGateValidationPlan(t, fixture, 1)
	writeFailingGate(t, fixture, "printf 'local gate failure'; exit 1")

	code, output, errOutput := fixture.run("merge", "t1")
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var rounds, queued int
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='gate'`, fixture.task.ID).Scan(&rounds); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE task_id=? AND status='queued'`, fixture.task.ID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if code != 1 || !strings.Contains(output, "gate_failed") || errOutput != "" || rounds != 1 || queued != 1 || countNoticeKind(notices, "gate_fix_queued") != 1 {
		t.Fatalf("local Gate failure did not queue its fix round: exit=%d output=%s stderr=%s rounds=%d queued=%d notices=%#v", code, output, errOutput, rounds, queued, notices)
	}
}

func TestPRMergeZeroGateFixRoundsKeepsManualFailurePath(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	installGateValidationPlan(t, fixture, 0)
	writeFailingGate(t, fixture, "printf 'failure'; exit 1")

	code, output, errOutput := fixture.run("merge", "t1")
	task, taskErr := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	notices, noticeErr := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	var queued, rounds int
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE task_id=?`, fixture.task.ID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='gate'`, fixture.task.ID).Scan(&rounds); err != nil {
		t.Fatal(err)
	}
	if code != 1 || !strings.Contains(output, "gate_failed") || !strings.Contains(output, "exit status 1") || errOutput != "" || taskErr != nil || task.State != store.StateDone || queued != 0 || rounds != 0 || noticeErr != nil || countNoticeKind(notices, "gate_failed") != 1 || countNoticeKind(notices, "gate_fix_queued") != 0 || countNoticeKind(notices, "gate_fix_exhausted") != 1 {
		t.Fatalf("zero-round Gate failure did not retain the manual path: exit=%d output=%s stderr=%s task=%#v queued=%d rounds=%d notices=%#v errors=%v/%v", code, output, errOutput, task, queued, rounds, notices, taskErr, noticeErr)
	}
}

func installGateValidationPlan(t *testing.T, fixture *prMergeFixture, maxFixRounds int) {
	t.Helper()
	ctx := context.Background()
	briefPath := filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "brief.md")
	briefData, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	var userChoice *taskplan.UserChoice
	if fixture.task.MergeMode == "local" {
		briefData = []byte(strings.Replace(string(briefData), "done_when: commit exists\n", "done_when: commit exists\nreview: none\n", 1))
		if err := os.WriteFile(briefPath, briefData, 0o600); err != nil {
			t.Fatal(err)
		}
		userChoice = &taskplan.UserChoice{Actor: "user", Source: "start --user-approved", Quote: "User approved no review for this Gate fixture"}
	}
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief:         taskplan.Select("brief", briefPath, briefData),
		Execution:     taskplan.Execution{Mode: "sandbox", Scope: "repo", Base: "main", Target: "main"},
		Launch:        taskplan.Launch{ProfileName: "claude", Profile: config.Profile{Kind: "claude"}},
		Review:        "local",
		Delivery:      fixture.task.MergeMode,
		Autonomy:      config.Autonomy{Review: "ask", Merge: "ask"},
		ConfigSources: []string{"test"},
		UserChoice:    userChoice,
		Paths: taskplan.Paths{
			Scratch: filepath.Join(fixture.home, "scratch", "shop", "{task}"),
			Output:  filepath.Join(fixture.home, "projects", "shop", "tasks", "{task}"),
		},
		ValidationLoops: &taskplan.ValidationLoops{
			Review: config.ReviewLoop{Enabled: true, MaxRounds: 3, BlockAt: "P1", AutoStart: true},
			Gate:   config.GateLoop{MaxFixRounds: maxFixRounds},
			CI:     config.CILoop{MaxFixRounds: 3, RerunCancelled: 1},
		},
		MergeMethod: "squash",
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := plan.Bind("t1").Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET plan_json=? WHERE id=?`, encoded, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `INSERT INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,1,?)`, fixture.task.ID, encoded); err != nil {
		t.Fatal(err)
	}
}

func writeFailingGate(t *testing.T, fixture *prMergeFixture, command string) {
	t.Helper()
	configText := fmt.Sprintf("[defaults]\nmerge_mode = %q\nmerge_method = \"squash\"\nauto_teardown = \"never\"\ngate = [%q]\n\n[gate_loop]\nmax_fix_rounds = 0\n", fixture.task.MergeMode, command)
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
}

func completeGateFixRound(t *testing.T, fixture *prMergeFixture, round int) {
	t.Helper()
	ctx := context.Background()
	adapter := fixture.service.Herdr.(*herdr.Fake)
	if err := fixture.service.deliverQueuedMessages(ctx, fixture.db, fixture.project, adapter.SnapshotValue); err != nil {
		t.Fatalf("deliver Gate fix round %d: %v", round, err)
	}
	current, err := fixture.db.Task(ctx, fixture.project.ID, "t1")
	if err != nil || current.State != store.StateWorking {
		t.Fatalf("Gate fix round %d did not return the Task to working: task=%#v err=%v", round, current, err)
	}
	path := filepath.Join(fixture.worktree, fmt.Sprintf("gate-fix-%d.txt", round))
	if err := os.WriteFile(path, []byte(fmt.Sprintf("fix round %d\n", round)), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.worktree, "add", filepath.Base(path))
	gitTest(t, fixture.worktree, "commit", "-m", fmt.Sprintf("apply Gate fix round %d", round))
	current, err = fixture.db.Task(ctx, fixture.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	turnToken, err := fixture.db.TaskTurnToken(ctx, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"turn_token": turnToken}
	if _, err := fixture.service.recordWorkerSignalWithContinuity(ctx, fixture.db, fixture.project, current, "done", fmt.Sprintf("Completed Gate fix round %d", round), data, "task_done", nil, false); err != nil {
		t.Fatalf("record Gate fix round %d completion: %v", round, err)
	}
}
