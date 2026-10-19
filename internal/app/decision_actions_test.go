package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestDiscardDecisionEligibilityIsScopedByKind(t *testing.T) {
	for _, test := range []struct {
		kind  string
		state store.State
		want  bool
	}{
		{kind: "recovery", state: store.StateFailed, want: true},
		{kind: "recovery", state: store.StateReported},
		{kind: "recovery", state: store.StateLanded},
		{kind: "worker_exited", state: store.StateDone, want: true},
		{kind: "worker_exited", state: store.StateReported},
		{kind: "land_ready", state: store.StateLanding, want: true},
		{kind: "land_ready", state: store.StateLanded},
		{kind: "pr_closed", state: store.StateFailed, want: true},
		{kind: "pr_closed", state: store.StateReported},
		{kind: "pr_closed", state: store.StateLanded},
		{kind: "leftover", state: store.StateReported, want: true},
		{kind: "leftover", state: store.StateLanded, want: true},
	} {
		t.Run(test.kind+"/"+string(test.state), func(t *testing.T) {
			if got := canDiscardFromDecision(test.kind, test.state); got != test.want {
				t.Fatalf("canDiscardFromDecision(%q, %q) = %t, want %t", test.kind, test.state, got, test.want)
			}
		})
	}
}

func TestTeardownStopsMountWriterBeforeSnapshot(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateDone)
	defer f.db.Close()
	ctx := context.Background()
	attachPRFixtureMount(t, f)
	if err := f.db.Transition(ctx, f.task.ID, store.StateDone, store.StateLanding, "cli", "PR ready"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, f.task.ID, store.StateLanding, store.StateLanded, "cli", "merged"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateTaskLanding(ctx, f.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.RecordPRObservation(ctx, store.PRObservation{TaskID: task.ID, ProjectID: f.project.ID, PRURL: task.PRURL, State: "MERGED", HeadSHA: f.headSHA, MergeCommit: f.headSHA}, store.PRObservationEffect{}, task); err != nil {
		t.Fatal(err)
	}
	writer := exec.Command("bash", "-c", "trap 'printf late\\n > late.txt; exit 0' TERM; while true; do read -t 0.1 || :; done")
	writer.Dir = f.worktree
	writer.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := writer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if writer.Process != nil {
			_ = writer.Process.Kill()
			_ = writer.Wait()
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		pids, err := mountProcessIDs(f.worktree)
		if err != nil {
			t.Fatal(err)
		}
		if len(pids) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writer did not start in Mount")
		}
		time.Sleep(10 * time.Millisecond)
	}
	fake := f.service.Herdr.(*herdr.Fake)
	if err := f.db.UpdateTaskLaunch(ctx, task.ID, task.WorktreePath, task.HerdrWorkspaceID, task.PaneID, task.PaneLabel, "posse-shop-t1-1"); err != nil {
		t.Fatal(err)
	}
	task.AgentName = "posse-shop-t1-1"
	fake.SnapshotValue.Panes[0].Agent = ""
	fake.SnapshotValue.Agents = []herdr.Agent{{Name: task.AgentName, PaneID: "w2:p1"}}
	fake.Results["pane.process_info"] = json.RawMessage(fmt.Sprintf(`{"process_info":{"pane_id":"w2:p1","shell_pid":2147483647,"foreground_process_group_id":%d,"foreground_processes":[{"pid":%d,"name":"claude","cwd":%q}]}}`, writer.Process.Pid, writer.Process.Pid, f.worktree))
	adapter := &changingSnapshotAdapter{Fake: fake, snapshot: fake.SnapshotValue, autoCloseWhenFile: filepath.Join(f.worktree, "late.txt"), autoClosePaneID: task.PaneID}
	f.service.Herdr = adapter
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.unsaddleTask(ctx, f.db, f.project, cfg, task, false, ""); err != nil {
		t.Fatal(err)
	}
	if got := gitTest(t, f.repo, "show", "refs/heads/posse/t1-leftover:late.txt"); !strings.Contains(got, "late") {
		t.Fatalf("Mount writer's final edit was lost: %q", got)
	}
}

func TestUnrecoverableLeftoverOffersApprovedDiscard(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateDone)
	defer f.db.Close()
	ctx := context.Background()
	attachPRFixtureMount(t, f)
	if err := f.db.Transition(ctx, f.task.ID, store.StateDone, store.StateLanding, "cli", "PR ready"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, f.task.ID, store.StateLanding, store.StateLanded, "cli", "merged"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateTaskLanding(ctx, f.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.RecordPRObservation(ctx, store.PRObservation{TaskID: task.ID, ProjectID: f.project.ID, PRURL: task.PRURL, State: "MERGED", HeadSHA: f.headSHA, MergeCommit: f.headSHA}, store.PRObservationEffect{}, task); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, "unsaved.txt"), []byte("unfinished\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.repo, "branch", "posse/t1-leftover")
	saved := filepath.Join(f.root, "saved-leftover")
	gitTest(t, f.repo, "worktree", "add", saved, "posse/t1-leftover")
	if err := os.WriteFile(filepath.Join(saved, "independent.txt"), []byte("independent saved work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, saved, "add", "independent.txt")
	gitTest(t, saved, "commit", "-m", "save independent work")
	gitTest(t, f.repo, "worktree", "remove", saved)
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	fake := f.service.Herdr.(*herdr.Fake)
	markPRFixtureRiderExited(fake)
	f.service.Herdr = &changingSnapshotAdapter{Fake: fake, snapshot: fake.SnapshotValue}
	removeFixtureTaskPane(f)
	if _, err := f.service.unsaddleTask(ctx, f.db, f.project, cfg, task, false, ""); err == nil {
		t.Fatal("conflicting Leftover snapshot should stop Teardown")
	} else {
		t.Logf("Teardown failure: %v", err)
	}
	decisions, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	var unrecoverable store.Decision
	for _, decision := range decisions {
		if strings.HasPrefix(decision.Origin, "leftover:unrecoverable:") {
			unrecoverable = decision
		}
	}
	if unrecoverable.ID == 0 {
		t.Fatalf("missing repair/discard Decision: %#v", decisions)
	}
	if _, err := f.db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: f.project.ID, TaskID: task.ID, Kind: "leftover", Origin: "leftover:posse/t1-leftover", Question: "Keep the independently saved Leftover?", Options: []string{"open-task", "discard"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.AnswerDecision(ctx, f.project.ID, unrecoverable.ID, "discard", "User approved losing the unsaved work"); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := f.run("apply", fmt.Sprint(unrecoverable.ID))
	if code != 0 {
		t.Fatalf("apply: %s %s", out, stderr)
	}
	task, err = f.db.Task(ctx, f.project.ID, "t1")
	if err != nil || task.State != store.StateTornDown {
		t.Fatalf("discard did not tear down: %#v %v", task, err)
	}
	pending, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil || len(pending) != 1 || pending[0].Origin != "leftover:posse/t1-leftover" {
		t.Fatalf("discard of unrecoverable work retired another Member's saved Leftover: %#v %v", pending, err)
	}
	applied, err := f.db.Decision(ctx, f.project.ID, unrecoverable.ID)
	if err != nil || applied.ObsoleteAt == 0 || applied.ObsoleteReason != "Unrecoverable Leftover resolved during Teardown" {
		t.Fatalf("applied unrecoverable Leftover Decision was not obsoleted: %#v %v", applied, err)
	}
}

func TestAnsweredClosedPRDecisionDiscardsTask(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateDone)
	defer f.db.Close()
	ctx := context.Background()
	attachPRFixtureMount(t, f)
	if code, out, stderr := f.run("land", "t1"); code != 0 {
		t.Fatalf("land: %s %s", out, stderr)
	}
	f.setGraphQLState(t, "CLOSED", "SUCCESS", "APPROVED", "MERGEABLE", "", f.headSHA)
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	decisions, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	var closed store.Decision
	for _, decision := range decisions {
		if decision.Kind == "pr_closed" {
			closed = decision
		}
	}
	if closed.ID == 0 {
		t.Fatalf("closed PR Decision absent: %#v", decisions)
	}
	if _, err := f.db.AnswerDecision(ctx, f.project.ID, closed.ID, "discard", "User approved discarding the closed PR"); err != nil {
		t.Fatal(err)
	}
	fake := f.service.Herdr.(*herdr.Fake)
	markPRFixtureRiderExited(fake)
	fake.SnapshotValue.Agents = []herdr.Agent{{Name: agentName(f.project.Name, f.task.Seq, 1), PaneID: f.task.PaneID}}
	f.service.Herdr = &changingSnapshotAdapter{Fake: fake, snapshot: fake.SnapshotValue}
	removeFixtureTaskPane(f)
	if err := os.WriteFile(filepath.Join(f.worktree, ".git"), []byte("gitdir: /missing/pruned/gitdir\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := f.run("apply", fmt.Sprint(closed.ID))
	if code != 0 {
		t.Fatalf("apply discard: %s %s", out, stderr)
	}
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil || task.State != store.StateTornDown {
		t.Fatalf("discard did not release Task: %#v %v", task, err)
	}
	transitions, err := f.db.TaskTransitions(ctx, task.ID, 10)
	if err != nil || len(transitions) == 0 || transitions[0].From != string(store.StateLanding) || transitions[0].To != string(store.StateTornDown) || transitions[0].Source != "user" {
		t.Fatalf("closed-PR discard did not transition directly from landing: %#v %v", transitions, err)
	}
}

func markPRFixtureRiderExited(fake *herdr.Fake) {
	fake.SnapshotValue.Agents = nil
	for index := range fake.SnapshotValue.Panes {
		if fake.SnapshotValue.Panes[index].PaneID == "w2:p1" {
			fake.SnapshotValue.Panes[index].Agent = ""
			fake.SnapshotValue.Panes[index].AgentStatus = ""
		}
	}
	fake.Results["pane.process_info"] = []byte(`{"process_info":{"pane_id":"w2:p1","shell_pid":4242,"foreground_process_group_id":4242,"foreground_processes":[]}}`)
}

func TestAnsweredLeftoverDecisionAppliesDiscardOrOffersNewTask(t *testing.T) {
	for _, option := range []string{"open-task", "discard"} {
		t.Run(option, func(t *testing.T) {
			f := newPRLandingFixture(t, "pr", store.StateDone)
			defer f.db.Close()
			ctx := context.Background()
			if err := f.db.UpdateTaskLanding(ctx, f.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.worktree, "untracked.txt"), []byte("save\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			f.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", f.headSHA, f.headSHA)
			cfg, err := config.Load(f.home, f.project.Name)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, true); err != nil {
				t.Fatal(err)
			}
			task, err := f.db.Task(ctx, f.project.ID, "t1")
			if err != nil {
				t.Fatal(err)
			}
			if err := snapshotPRLeftover(ctx, f.db, f.project, task); err != nil {
				t.Fatal(err)
			}
			decisions, err := f.db.Decisions(ctx, f.project.ID, true)
			if err != nil || len(decisions) != 1 {
				t.Fatalf("decisions=%#v err=%v", decisions, err)
			}
			decision, err := f.db.AnswerDecision(ctx, f.project.ID, decisions[0].ID, option, "User selected "+option)
			if err != nil {
				t.Fatal(err)
			}
			code, out, stderr := f.run("apply", fmt.Sprint(decision.ID))
			if code != 0 {
				t.Fatalf("apply: %s %s", out, stderr)
			}
			if option == "open-task" {
				if !strings.Contains(out, "--from-leftover") {
					t.Fatalf("new Task command missing: %s", out)
				}
				if got := gitTest(t, f.repo, "show", "refs/heads/posse/t1-leftover:untracked.txt"); got != "save\n" {
					t.Fatalf("Leftover lost: %q", got)
				}
			} else {
				if _, err := gitOutput(ctx, f.repo, "rev-parse", "--verify", "refs/heads/posse/t1-leftover"); err == nil {
					t.Fatal("discard left branch behind")
				}
			}
		})
	}
}

func TestApprovedDiscardRequiresQuoteBeforeDestructiveWork(t *testing.T) {
	f := newPRLandingFixture(t, "local", store.StateFailed)
	defer f.db.Close()
	ctx := context.Background()
	mount := attachPRFixtureMount(t, f)
	if err := f.db.Transition(ctx, f.task.ID, store.StateWorking, store.StateFailed, "worker", "Worker failed"); err != nil {
		t.Fatal(err)
	}
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	f.task = task
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.unsaddleTask(ctx, f.db, f.project, cfg, f.task, true, "")
	commandError, ok := err.(*axi.Error)
	if !ok || commandError.Code != "teardown_refused" || commandError.Retryable {
		t.Fatalf("missing-quote discard refusal = %#v, want non-retryable teardown_refused", err)
	}
	current, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil || current.State != store.StateFailed {
		t.Fatalf("Task after refusal = %#v, %v", current, err)
	}
	currentMount, err := f.db.MountByTask(ctx, f.task.ID)
	if err != nil || currentMount.ID != mount.ID || currentMount.State != "held" {
		t.Fatalf("Mount after refusal = %#v, %v", currentMount, err)
	}
	if _, err := os.Stat(f.worktree); err != nil {
		t.Fatalf("worktree after refusal: %v", err)
	}
	if got := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "refs/heads/posse/t1")); got != f.headSHA {
		t.Fatalf("branch after refusal = %s, want %s", got, f.headSHA)
	}
	hasApproval, err := f.db.HasApproval(ctx, f.task.ID, "discard")
	if err != nil || hasApproval {
		t.Fatalf("illegal discard recorded approval: has=%v err=%v", hasApproval, err)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if countNoticeKind(notices, "unsaddle_incomplete") != 0 {
		t.Fatalf("deterministic refusal raised an incomplete Teardown Notice: %#v", notices)
	}
}

func TestUnsaddleIncompleteDeterministicRefusalIsNotRetryable(t *testing.T) {
	f := newPRLandingFixture(t, "local", store.StateWorking)
	defer f.db.Close()
	err := f.service.unsaddleIncomplete(context.Background(), f.db, f.project, f.task, "Task state update", store.ErrInvalidApprovedDiscardTransition)
	commandError, ok := err.(*axi.Error)
	if !ok || commandError.Code != "unsaddle_incomplete" || commandError.Retryable {
		t.Fatalf("deterministic incomplete refusal = %#v, want non-retryable unsaddle_incomplete", err)
	}
}
