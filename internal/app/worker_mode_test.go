package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type workerFixture struct {
	root, repo, home, sandbox string
	project                   store.Project
}

func newWorkerFixture(t *testing.T) workerFixture {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	sandbox := filepath.Join(home, "pool", "shop", "sandbox-1")
	gitTest(t, repo, "worktree", "add", "--detach", sandbox)
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Work", State: store.StateSpawning, MergeMode: "local", PaneID: "w2:p1", WorktreePath: sandbox})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id,acquired_at,released_at) VALUES(?,?,?,?,?,0,0)`, project.ID, 1, sandbox, "held", taskID); err != nil {
		t.Fatal(err)
	}
	return workerFixture{root: root, repo: repo, home: home, sandbox: sandbox, project: project}
}

func runCLI(t *testing.T, service *Service, args ...string) (int, string) {
	t.Helper()
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	return cli.Run(args), output.String()
}

func TestWorkerCannotRunStateChangingCommandsAgainstItsHome(t *testing.T) {
	fixture := newWorkerFixture(t)
	brief := filepath.Join(fixture.root, "brief.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: T\ndone_when: d\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	forbidden := [][]string{
		{"start", "--brief", brief},
		{"project", "add", "--name", "stray"},
		{"project", "remove", "shop", "--yes"},
		{"up", "--claude"},
		{"setup", "--check"},
		{"config", "set", "defaults.max_workers", "6"},
		{"teardown", "t1"},
		{"accept", "t1"},
		{"list"},
		{},
	}
	for _, caller := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"pane", func(t *testing.T) {
			t.Setenv("HERDR_ENV", "1")
			t.Setenv("HERDR_PANE_ID", "w2:p1")
			t.Chdir(fixture.repo)
		}},
		{"marker", func(t *testing.T) {
			t.Setenv(workerHomeEnv, fixture.home)
			t.Chdir(fixture.root)
		}},
		{"sandbox", func(t *testing.T) {
			t.Chdir(filepath.Join(fixture.sandbox))
		}},
	} {
		t.Run(caller.name, func(t *testing.T) {
			caller.setup(t)
			service := testService(fixture.home, herdr.NewFake())
			for _, args := range forbidden {
				if code, output := runCLI(t, service, args...); code == 0 || !strings.Contains(output, "worker_forbidden") || !strings.Contains(output, "temporary POSSE_HOME") {
					t.Fatalf("Worker (%s) ran `posse %s`: code=%d output=%s", caller.name, strings.Join(args, " "), code, output)
				}
			}
			if code, output := runCLI(t, service, "config", "show"); code != 0 {
				t.Fatalf("Worker (%s) could not read config: code=%d output=%s", caller.name, code, output)
			}
		})
	}
}

func TestNeedsDecisionWorkerCanSignalWorkingAndGetsUsableRefusalHelp(t *testing.T) {
	fixture := newWorkerFixture(t)
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), task.ID, store.StateWorking, store.StateNeedsDecision, "worker", "Waiting on a decision"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workerHomeEnv, fixture.home)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", task.PaneID)
	t.Chdir(fixture.sandbox)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "working"}}}
	service := testService(fixture.home, fake)
	if code, output := runCLI(t, service, "signal", "working", "Resuming after my question"); code != 0 || !strings.Contains(output, "state: working") {
		t.Fatalf("needs-decision Worker could not signal working: code=%d output=%s", code, output)
	}
	db, err = store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	current, err := db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || current.State != store.StateWorking {
		db.Close()
		t.Fatalf("working Signal did not reopen Task: %#v %v", current, err)
	}
	if err := db.Transition(context.Background(), task.ID, store.StateWorking, store.StateDone, "worker", "done"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), task.ID, store.StateDone, store.StateMerged, "cli", "merged"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if code, output := runCLI(t, service, "signal", "working", "Still working"); code == 0 || !strings.Contains(output, "Ask the Lead") || strings.Contains(output, "posse show") || strings.Contains(output, "posse status") {
		t.Fatalf("Worker refusal help suggested an unusable command: code=%d output=%s", code, output)
	}
}

func TestWorkingSignalReopensOwnCompletedTaskWithReason(t *testing.T) {
	for _, initial := range []store.State{store.StateDone, store.StateMerging, store.StateStalled, store.StateNeedsDecision} {
		t.Run(string(initial), func(t *testing.T) {
			fixture := newWorkerFixture(t)
			ctx := context.Background()
			db, err := store.Open(fixture.home)
			if err != nil {
				t.Fatal(err)
			}
			task, err := db.Task(ctx, fixture.project.ID, "t1")
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			switch initial {
			case store.StateDone, store.StateMerging:
				if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateDone, "worker", "done"); err != nil {
					db.Close()
					t.Fatal(err)
				}
				if err := db.SetTaskGatedSHA(ctx, task.ID, "old-gate"); err != nil {
					db.Close()
					t.Fatal(err)
				}
				if initial == store.StateMerging {
					if err := db.Transition(ctx, task.ID, store.StateDone, store.StateMerging, "cli", "gate passed"); err != nil {
						db.Close()
						t.Fatal(err)
					}
					if err := db.StartIntent(ctx, fixture.project.ID, task.ID, "merge --execute", "started", `{}`, os.Getpid()); err != nil {
						db.Close()
						t.Fatal(err)
					}
					if err := db.CreateTaskRepos(ctx, task.ID, []store.TaskRepo{{Repo: "shop", WorktreePath: fixture.sandbox, MergeMode: "pr"}}); err != nil {
						db.Close()
						t.Fatal(err)
					}
					if err := db.UpdateTaskRepo(ctx, store.TaskRepo{TaskID: task.ID, Repo: "shop", WorktreePath: fixture.sandbox, MergeMode: "pr", State: store.TaskRepoMerge, GatedSHA: "old-member-gate", PRURL: "https://github.com/acme/shop/pull/1"}); err != nil {
						db.Close()
						t.Fatal(err)
					}
					if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: fixture.project.ID, TaskID: task.ID, Kind: "merge_ready", Summary: "Task is ready", DataJSON: `{}`}); err != nil {
						db.Close()
						t.Fatal(err)
					}
					if _, err := db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: fixture.project.ID, TaskID: task.ID, Origin: "merge-ready", Kind: "merge_ready", Question: "Merge Task?", Options: []string{"merge", "wait"}}); err != nil {
						db.Close()
						t.Fatal(err)
					}
					if initial == store.StateMerging {
						observed, err := db.TaskByID(ctx, fixture.project.ID, task.ID)
						if err != nil {
							db.Close()
							t.Fatal(err)
						}
						now := time.Now()
						for _, status := range []string{"working", "idle", "working"} {
							if _, err := db.ObserveCompletedWorker(ctx, observed, task.PaneID, "generation", status, now); err != nil {
								db.Close()
								t.Fatalf("watcher observation %s: %v", status, err)
							}
							now = now.Add(time.Second)
						}
					} else if err := db.StartIntent(ctx, fixture.project.ID, task.ID, "merge --open-pr", "pr.lookup", `{}`, os.Getpid()); err != nil {
						db.Close()
						t.Fatal(err)
					}
				}
			case store.StateStalled:
				if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateStalled, "cli", "stalled"); err != nil {
					db.Close()
					t.Fatal(err)
				}
			case store.StateNeedsDecision:
				if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateNeedsDecision, "worker", "needs answer"); err != nil {
					db.Close()
					t.Fatal(err)
				}
			}
			reason := "The User asked me to make one more change."
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			t.Setenv(workerHomeEnv, fixture.home)
			t.Setenv("HERDR_ENV", "1")
			t.Setenv("HERDR_PANE_ID", task.PaneID)
			t.Chdir(fixture.sandbox)
			fake := herdr.NewFake()
			fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "working"}}}
			service := testService(fixture.home, fake)
			if code, output := runCLI(t, service, "signal", "working", reason); code != 0 || !strings.Contains(output, "state: working") {
				t.Fatalf("working Signal did not reopen %s Task: code=%d output=%s", initial, code, output)
			}
			db, err = store.Open(fixture.home)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			current, err := db.Task(ctx, fixture.project.ID, "t1")
			if err != nil || current.State != store.StateWorking || current.GatedSHA != "" {
				t.Fatalf("reopened Task retained its state or gate: %#v %v", current, err)
			}
			transitions, err := db.TaskTransitions(ctx, task.ID, 10)
			if err != nil || len(transitions) == 0 {
				t.Fatalf("read Task transitions: %#v %v", transitions, err)
			}
			last := transitions[0]
			if last.From != string(initial) || last.To != string(store.StateWorking) || last.Source != "worker" || last.Note != reason {
				t.Fatalf("reopen transition = %#v; want Worker transition with quoted reason", last)
			}
			signals, err := db.TaskSignals(ctx, task.ID, 10)
			if err != nil || len(signals) == 0 || signals[0].Verb != "working" || signals[0].Note != reason {
				t.Fatalf("working Signal = %#v %v", signals, err)
			}
			notices, err := db.Notices(ctx, fixture.project.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, notice := range notices {
				if notice.TaskID == task.ID && notice.Kind == "worker_resumed" && strings.Contains(notice.Summary, `"`+reason+`"`) && strings.Contains(notice.DataJSON, reason) {
					found = true
				}
			}
			if !found {
				t.Fatalf("no worker_resumed Notice quoted %q: %#v", reason, notices)
			}
			if _, err := db.IntentByTask(ctx, task.ID); !store.IsNotFound(err) {
				t.Fatalf("pending Merge intent was not canceled: %v", err)
			}
			if initial == store.StateMerging {
				repos, err := db.TaskRepos(ctx, task.ID)
				if err != nil || len(repos) != 1 || repos[0].State != store.TaskRepoOpen || repos[0].GatedSHA != "" {
					t.Fatalf("reopened Task retained member Merge evidence: %#v %v", repos, err)
				}
				decisions, err := db.Decisions(ctx, fixture.project.ID, false)
				if err != nil || len(decisions) != 1 || decisions[0].ObsoleteAt == 0 {
					t.Fatalf("reopened Task retained its merge_ready Decision: %#v %v", decisions, err)
				}
			}
		})
	}
}

func TestWorkingSignalRefusesWhilePullRequestMergeIsInFlight(t *testing.T) {
	fixture := newWorkerFixture(t)
	ctx := context.Background()
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.Task(ctx, fixture.project.ID, "t1")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateDone, "worker", "done"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Transition(ctx, task.ID, store.StateDone, store.StateMerging, "cli", "gate passed"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.StartIntent(ctx, fixture.project.ID, task.ID, "merge --execute", "in_progress:pr.merge:shop", `{}`, os.Getpid()); err != nil {
		db.Close()
		t.Fatal(err)
	}
	observed, err := db.TaskByID(ctx, fixture.project.ID, task.ID)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	now := time.Now()
	for _, status := range []string{"working", "idle", "working"} {
		if notice, err := db.ObserveCompletedWorker(ctx, observed, task.PaneID, "generation", status, now); err != nil || notice != nil {
			db.Close()
			t.Fatalf("watcher reopened Task during in-flight merge at %s: notice=%#v err=%v", status, notice, err)
		}
		now = now.Add(time.Second)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workerHomeEnv, fixture.home)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", task.PaneID)
	t.Chdir(fixture.sandbox)
	service := testService(fixture.home, herdr.NewFake())
	if code, output := runCLI(t, service, "signal", "working", "The User asked me to make one more change."); code == 0 || !strings.Contains(output, "merge") || !strings.Contains(output, "in progress") {
		t.Fatalf("working Signal did not clearly refuse an in-flight merge: code=%d output=%s", code, output)
	}
	db, err = store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	current, err := db.Task(ctx, fixture.project.ID, "t1")
	if err != nil || current.State != store.StateMerging {
		t.Fatalf("in-flight merge refusal changed Task: %#v %v", current, err)
	}
	if _, err := db.IntentByTask(ctx, task.ID); err != nil {
		t.Fatalf("in-flight merge intent was canceled: %v", err)
	}
	signals, err := db.TaskSignals(ctx, task.ID, 10)
	if err != nil || len(signals) != 0 {
		t.Fatalf("in-flight merge refusal recorded a Signal: %#v %v", signals, err)
	}
}

func TestWorkingSignalRejectsTerminalTaskStates(t *testing.T) {
	for _, target := range []store.State{store.StateMerged, store.StateTornDown, store.StateFailed, store.StateLost} {
		t.Run(string(target), func(t *testing.T) {
			fixture := newWorkerFixture(t)
			ctx := context.Background()
			db, err := store.Open(fixture.home)
			if err != nil {
				t.Fatal(err)
			}
			task, err := db.Task(ctx, fixture.project.ID, "t1")
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			switch target {
			case store.StateMerged:
				if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateDone, "worker", "done"); err != nil {
					db.Close()
					t.Fatal(err)
				}
				if err := db.Transition(ctx, task.ID, store.StateDone, store.StateMerged, "cli", "merged"); err != nil {
					db.Close()
					t.Fatal(err)
				}
			case store.StateTornDown:
				if err := db.TransitionWithApproval(ctx, task.ID, store.StateWorking, store.StateTornDown, "user", "discarded", "discard", "User approved discard"); err != nil {
					db.Close()
					t.Fatal(err)
				}
			default:
				source := "worker"
				if target == store.StateLost {
					source = "cli"
				}
				if err := db.Transition(ctx, task.ID, store.StateWorking, target, source, "terminal"); err != nil {
					db.Close()
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			t.Setenv(workerHomeEnv, fixture.home)
			t.Setenv("HERDR_ENV", "1")
			t.Setenv("HERDR_PANE_ID", task.PaneID)
			t.Chdir(fixture.sandbox)
			service := testService(fixture.home, herdr.NewFake())
			if code, output := runCLI(t, service, "signal", "working", "Late follow-up"); code == 0 || !strings.Contains(output, "signal_state_invalid") {
				t.Fatalf("working Signal reopened terminal %s Task: code=%d output=%s", target, code, output)
			}
			db, err = store.Open(fixture.home)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			signals, err := db.TaskSignals(ctx, task.ID, 10)
			if err != nil || len(signals) != 0 {
				t.Fatalf("terminal-state refusal recorded a Signal: %#v %v", signals, err)
			}
		})
	}
}

func TestDoneSignalAcceptsDeliveredTaskInstructions(t *testing.T) {
	fixture := newWorkerFixture(t)
	ctx := context.Background()
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.Task(ctx, fixture.project.ID, "t1")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	gitTest(t, fixture.sandbox, "switch", "-c", "posse/t1")
	if err := os.WriteFile(filepath.Join(fixture.sandbox, "instruction.txt"), []byte("applied\n"), 0o600); err != nil {
		db.Close()
		t.Fatal(err)
	}
	gitTest(t, fixture.sandbox, "add", "instruction.txt")
	gitTest(t, fixture.sandbox, "commit", "-m", "Apply delivered instruction")
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET branch='posse/t1',base_ref='main' WHERE id=?`, task.ID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	messageID, err := db.QueueMessage(ctx, task.ID, "Apply the final instruction", false)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	claimed, err := db.ClaimMessage(ctx, messageID, "delivery", 100, 0)
	if err != nil || !claimed {
		db.Close()
		t.Fatalf("claim queued instruction = %v, %v", claimed, err)
	}
	if err := db.MarkMessageSubmitting(ctx, messageID, "delivery"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.MarkClaimedMessageDelivered(ctx, messageID, "delivery", 101); err != nil {
		db.Close()
		t.Fatal(err)
	}
	pending, err := db.HasPendingTaskInstructions(ctx, task.ID)
	if err != nil || pending {
		db.Close()
		t.Fatalf("delivered instruction remained pending: %v %v", pending, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv(workerHomeEnv, fixture.home)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", task.PaneID)
	t.Chdir(fixture.sandbox)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "working"}}}
	service := testService(fixture.home, fake)
	if code, output := runCLI(t, service, "signal", "done", "Applied the delivered instruction"); code != 0 || !strings.Contains(output, "state: done") {
		t.Fatalf("done Signal after delivered instruction was rejected: code=%d output=%s", code, output)
	}
}

func TestWorkerMayExperimentAgainstATemporaryHome(t *testing.T) {
	fixture := newWorkerFixture(t)
	t.Setenv(workerHomeEnv, fixture.home)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w2:p1")
	scratch := filepath.Join(fixture.root, "scratch")
	initRepo(t, scratch)
	t.Chdir(scratch)
	service := testService(filepath.Join(fixture.root, "experiment-home"), herdr.NewFake())
	if code, output := runCLI(t, service, "project", "add", "--name", "scratch"); code != 0 || !strings.Contains(output, "registered: true") {
		t.Fatalf("Worker could not register a Project in a temporary home: code=%d output=%s", code, output)
	}
}

func TestWorkerProtocolRequiresIsolatedExperiments(t *testing.T) {
	protocol := workerProtocol(store.Project{Name: "shop"}, store.Task{Seq: 3, Type: "change", MergeMode: "local"}, dispatch.Brief{DoneWhen: "done"}, "/tmp/launch.md", "/tmp/posse/scratch/shop/t3", "none", "English")
	for _, want := range []string{"# Worker protocol", "isolated Herdr server", "POSSE_HOME under a temp dir", "never touch panes, tabs or workspaces you did not create", "A `done` Signal is rejected while queued, claimed or interrupting Lead instructions remain undelivered", "first run `posse signal working \"<reason>\"` and wait for success before acting", "Files passed to `--findings` and `--report` may be in this Worker's worktree or this Task's scratch directory (`/tmp/posse/scratch/shop/t3`)"} {
		if !strings.Contains(protocol, want) {
			t.Fatalf("Worker protocol lacks %q:\n%s", want, protocol)
		}
	}
}

func TestProjectRemoveUnregistersOnlyEmptyProjects(t *testing.T) {
	fixture := newWorkerFixture(t)
	stray := filepath.Join(fixture.root, "stray")
	initRepo(t, stray)
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	strayProject, err := db.CreateProject(context.Background(), "stray", stray, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), strayProject.ID, "w9", "w9:p1", "posse:stray:lead"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	strayHome := filepath.Join(fixture.home, "projects", "stray")
	if err := os.MkdirAll(strayHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(strayHome, "lead.md"), []byte("lead\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(fixture.root)
	fake := herdr.NewFake()
	service := testService(fixture.home, fake)

	if code, output := runCLI(t, service, "project", "remove", "shop", "--yes"); code == 0 || !strings.Contains(output, "project_not_empty") {
		t.Fatalf("removed a Project with Tasks: code=%d output=%s", code, output)
	}
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w9:p1", Label: "posse:stray:lead", Agent: "claude"}}}
	if code, output := runCLI(t, service, "project", "remove", "stray", "--yes"); code == 0 || !strings.Contains(output, "lead_running") {
		t.Fatalf("removed a Project with a live Lead: code=%d output=%s", code, output)
	}
	fake.SnapshotValue = herdr.Snapshot{}
	if code, output := runCLI(t, service, "project", "remove", "stray"); code != 0 || !strings.Contains(output, "removed: false") {
		t.Fatalf("dry run failed: code=%d output=%s", code, output)
	}
	if _, err := os.Stat(strayHome); err != nil {
		t.Fatalf("dry run deleted the Project home: %v", err)
	}
	if code, output := runCLI(t, service, "project", "remove", "stray", "--yes"); code != 0 || !strings.Contains(output, "removed: true") {
		t.Fatalf("remove failed: code=%d output=%s", code, output)
	}
	if _, err := os.Stat(strayHome); !os.IsNotExist(err) {
		t.Fatalf("Project home survived removal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stray, "README.md")); err != nil {
		t.Fatalf("removal touched the repository: %v", err)
	}
	observer, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	if _, err := observer.ProjectByName(context.Background(), "stray"); !store.IsNotFound(err) {
		t.Fatalf("stray Project is still registered: %v", err)
	}
	if _, err := observer.ProjectByName(context.Background(), "shop"); err != nil {
		t.Fatalf("removal touched another Project: %v", err)
	}
}

// fakeProc builds a /proc tree where this process descends from a shell
// (pid 50) that descends from an agent (pid 20).
func fakeProc(t *testing.T, agentCWD string, agentEnv []string) string {
	t.Helper()
	root := t.TempDir()
	write := func(pid, parent int, cwd string, env []string) {
		directory := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "stat"), []byte(fmt.Sprintf("%d (proc name) S %d 0 0", pid, parent)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "environ"), []byte(strings.Join(env, "\x00")+"\x00"), 0o600); err != nil {
			t.Fatal(err)
		}
		if cwd != "" {
			if err := os.Symlink(cwd, filepath.Join(directory, "cwd")); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(os.Getpid(), 50, "/", nil)
	write(50, 20, "/", []string{"PATH=/usr/bin"})
	write(20, 1, agentCWD, agentEnv)
	return root
}

func TestWorkerIdentitySurvivesRemovedEnvironmentAndCwd(t *testing.T) {
	fixture := newWorkerFixture(t)
	brief := filepath.Join(fixture.root, "brief.md")
	t.Chdir(fixture.root)
	t.Setenv(workerHomeEnv, "")
	for _, testCase := range []struct {
		name string
		cwd  string
		env  []string
	}{
		{"marker", "/", []string{workerHomeEnv + "=" + fixture.home}},
		{"sandbox", filepath.Join(fixture.sandbox, "internal"), nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if err := os.MkdirAll(filepath.Join(fixture.sandbox, "internal"), 0o700); err != nil {
				t.Fatal(err)
			}
			previous := procRoot
			procRoot = fakeProc(t, testCase.cwd, testCase.env)
			t.Cleanup(func() { procRoot = previous })
			service := testService(fixture.home, herdr.NewFake())
			for _, args := range [][]string{{"config", "set", "defaults.auto_teardown", "never"}, {"start", "--brief", brief}} {
				if code, output := runCLI(t, service, args...); code == 0 || !strings.Contains(output, "worker_forbidden") {
					t.Fatalf("a descendant of a Worker ran `posse %s`: code=%d output=%s", strings.Join(args, " "), code, output)
				}
			}
			if !service.workerCaller(context.Background(), fixture.home) {
				t.Fatal("workerCaller missed the Worker ancestor")
			}
			if service.workerCaller(context.Background(), filepath.Join(fixture.root, "other-home")) {
				t.Fatal("a Worker ancestor made this process a Worker of another home")
			}
		})
	}
	previous := procRoot
	procRoot = fakeProc(t, "/", []string{"PATH=/usr/bin"})
	t.Cleanup(func() { procRoot = previous })
	if testService(fixture.home, herdr.NewFake()).workerCaller(context.Background(), fixture.home) {
		t.Fatal("a process with no Worker ancestor was taken for a Worker")
	}
}

func TestMigrationGateAllowsOnlyTheInstalledBuildOutsideWorkers(t *testing.T) {
	fixture := newWorkerFixture(t)
	path := filepath.Join(fixture.home, "posse.db")
	t.Chdir(fixture.root)
	service := testService(fixture.home, herdr.NewFake())
	if err := service.MigrationGate(path, []int64{13}, false); err != nil {
		t.Fatalf("a home without setup.json refused migration: %v", err)
	}
	if err := writeSetupManifest(filepath.Join(fixture.home, setupManifestName), setupManifest{Version: "1", Binary: "/opt/posse/bin/posse"}); err != nil {
		t.Fatal(err)
	}
	if err := service.MigrationGate(path, []int64{13}, false); err == nil || !strings.Contains(err.Error(), "only the installed posse") {
		t.Fatalf("a foreign build may migrate the home: %v", err)
	}
	if err := service.MigrationGate(path, []int64{13}, true); err != nil {
		t.Fatalf("the User could not force a migration: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSetupManifest(filepath.Join(fixture.home, setupManifestName), setupManifest{Version: "1", Binary: self}); err != nil {
		t.Fatal(err)
	}
	if err := service.MigrationGate(path, []int64{13}, false); err != nil {
		t.Fatalf("the installed build could not migrate: %v", err)
	}
	t.Setenv(workerHomeEnv, fixture.home)
	for _, forced := range []bool{false, true} {
		var schema *store.SchemaError
		if err := service.MigrationGate(path, []int64{13}, forced); !errors.As(err, &schema) || schema.Code != "migration_refused" {
			t.Fatalf("a Worker may migrate its home (forced=%v): %v", forced, err)
		}
	}
}
