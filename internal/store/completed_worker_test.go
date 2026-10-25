package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCompletedWorkerObservationIsDurableAtomicAndConcurrent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 2, Type: "change", Title: "t2", MergeMode: "local", AutonomyReview: "ask", AutonomyMerge: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateSpawning, StateWorking, "cli", "start"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateWorking, StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTaskGatedSHA(ctx, id, "head"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"gated", "merging", "merged", "unchanged"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO task_repos(task_id,repo,worktree_path,base_ref,landing_mode,state,gated_sha,updated_at) VALUES(?,?,'/sandbox','base','local',?,'head',0)`, id, state, state); err != nil {
			t.Fatal(err)
		}
	}
	task, err := db.Task(ctx, project.ID, "t2")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if notice, err := db.ObserveCompletedWorker(ctx, task, "w1:p2", "generation", "working", now); err != nil || notice != nil {
		t.Fatalf("first observation: %#v %v", notice, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Another CLI process must reuse the persisted first observation.
	db, err = OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Inject a failed Notice insert: the state, gate and evidence must roll back.
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_resumed BEFORE INSERT ON notices WHEN NEW.kind='worker_resumed' BEGIN SELECT RAISE(ABORT,'test Notice failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ObserveCompletedWorker(ctx, task, "w1:p2", "generation", "idle", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ObserveCompletedWorker(ctx, task, "w1:p2", "generation", "working", now.Add(2*time.Second)); err == nil {
		t.Fatal("failed Notice insert was ignored")
	}
	current, _ := db.Task(ctx, project.ID, "t2")
	if current.State != StateDone || current.GatedSHA != "head" {
		t.Fatalf("failed Notice left partial reopen: %#v", current)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER reject_resumed`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.ObserveCompletedWorker(ctx, task, "w1:p2", "generation", "working", now.Add(3*time.Second))
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, ErrStateRace) {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='worker_resumed'`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("concurrent Notices: %d %v", count, err)
	}
	repos, err := db.TaskRepos(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range repos {
		if repo.Repo == "merged" {
			if repo.State != TaskRepoMerged || repo.GatedSHA != "head" {
				t.Fatalf("merged Member changed: %#v", repo)
			}
		} else if repo.GatedSHA != "" || ((repo.Repo == "gated" || repo.Repo == "merging" || repo.Repo == "unchanged") && repo.State != TaskRepoOpen) {
			t.Fatalf("stale Member gate survived: %#v", repo)
		}
	}
}

func TestCompletedWorkerUnchangedNewTurnKeepsDone(t *testing.T) {
	ctx := context.Background()
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "t1", MergeMode: "local", AutonomyReview: "ask", AutonomyMerge: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		from, to State
		source   string
	}{{StateSpawning, StateWorking, "cli"}, {StateWorking, StateDone, "worker"}} {
		if err := db.Transition(ctx, id, step.from, step.to, step.source, "step"); err != nil {
			t.Fatal(err)
		}
	}
	task, err := db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := "head-1"
	read := func() (string, error) { return fingerprint, nil }
	now := time.Now()
	observe := func(status string) *Notice {
		t.Helper()
		now = now.Add(time.Second)
		notice, err := db.ObserveCompletedWorkerState(ctx, task, "w1:p1", "generation", status, RepositoryFingerprints{TurnEnd: read, Current: read}, now)
		if err != nil {
			t.Fatal(err)
		}
		return notice
	}
	observe("working")
	observe("idle")
	if observe("working") != nil {
		t.Fatal("unchanged new turn reopened the Task")
	}
	observe("idle")
	if current, _ := db.Task(ctx, project.ID, "t1"); current.State != StateDone {
		t.Fatalf("state = %s; want done", current.State)
	}
	fingerprint = "head-2"
	notice := observe("working")
	if notice == nil || notice.Kind != "worker_resumed" {
		t.Fatalf("changed new turn Notice = %#v", notice)
	}
	if queued, err := db.OldestQueuedMessage(ctx, id); err != nil || queued.Body == "" {
		t.Fatalf("no signal-again instruction queued: %#v %v", queued, err)
	}
}

func TestCompletedWorkerUnreadableTurnEndBaselineReopensOnNewTurn(t *testing.T) {
	ctx := context.Background()
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "t1", MergeMode: "local", AutonomyReview: "ask", AutonomyMerge: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateSpawning, StateWorking, "cli", "start"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateWorking, StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
	task, err := db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	unreadable := func() (string, error) { return "", errors.New("git unavailable") }
	readable := func() (string, error) { return "head-1", nil }
	now := time.Now()
	for _, step := range []struct {
		status string
		read   func() (string, error)
	}{{"working", readable}, {"idle", unreadable}} {
		now = now.Add(time.Second)
		if _, err := db.ObserveCompletedWorkerState(ctx, task, "w1:p1", "generation", step.status, RepositoryFingerprints{TurnEnd: step.read, Current: step.read}, now); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Second)
	notice, err := db.ObserveCompletedWorkerState(ctx, task, "w1:p1", "generation", "working", RepositoryFingerprints{Current: readable}, now)
	if err != nil || notice == nil {
		t.Fatalf("new turn after an unreadable turn end did not reopen: %#v %v", notice, err)
	}
}

// Reproduce #509: a new turn's only working observation can precede its
// commit, so the turn's end must compare the repository again.
func TestCompletedWorkerTurnChangeAfterWorkingObservationReopensAtTurnEnd(t *testing.T) {
	for _, test := range []struct {
		name string
		// staleTurnEnd reports the pre-snapshot read taken before the commit.
		staleTurnEnd bool
	}{{"turn end read after commit", false}, {"turn end read before commit", true}} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			project, err := db.CreateProject(ctx, "shop", "/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "t1", MergeMode: "local", AutonomyReview: "ask", AutonomyMerge: "ask"})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Transition(ctx, id, StateSpawning, StateWorking, "cli", "start"); err != nil {
				t.Fatal(err)
			}
			if err := db.Transition(ctx, id, StateWorking, StateDone, "worker", "done"); err != nil {
				t.Fatal(err)
			}
			task, err := db.Task(ctx, project.ID, "t1")
			if err != nil {
				t.Fatal(err)
			}
			head := "head-1"
			read := func() (string, error) { return head, nil }
			stale := func() (string, error) { return "head-1", nil }
			now := time.Now()
			observe := func(status string, fingerprints RepositoryFingerprints) *Notice {
				t.Helper()
				now = now.Add(time.Second)
				notice, err := db.ObserveCompletedWorkerState(ctx, task, "w1:p1", "generation", status, fingerprints, now)
				if err != nil {
					t.Fatal(err)
				}
				return notice
			}
			same := RepositoryFingerprints{TurnEnd: read, Current: read}
			observe("working", same)
			observe("idle", same)
			if observe("working", same) != nil {
				t.Fatal("new turn reopened the Task before it changed the repository")
			}
			head = "head-2"
			turnEnd := same
			if test.staleTurnEnd {
				turnEnd.TurnEnd = stale
			}
			notice := observe("idle", turnEnd)
			if notice == nil || notice.Kind != "worker_resumed" {
				t.Fatalf("turn that committed after its working observation did not reopen: %#v", notice)
			}
			if current, _ := db.Task(ctx, project.ID, "t1"); current.State != StateWorking {
				t.Fatalf("state = %s; want working", current.State)
			}
			if queued, err := db.OldestQueuedMessage(ctx, id); err != nil || queued.Body == "" {
				t.Fatalf("no signal-again instruction queued: %#v %v", queued, err)
			}
		})
	}
}

func TestCompletedWorkerUnchangedResumedTurnEndKeepsDone(t *testing.T) {
	ctx := context.Background()
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "t1", MergeMode: "local", AutonomyReview: "ask", AutonomyMerge: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateSpawning, StateWorking, "cli", "start"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateWorking, StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
	task, err := db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	read := func() (string, error) { return "head-1", nil }
	same := RepositoryFingerprints{TurnEnd: read, Current: read}
	now := time.Now()
	for _, status := range []string{"working", "idle", "working", "idle", "idle"} {
		now = now.Add(time.Second)
		if notice, err := db.ObserveCompletedWorkerState(ctx, task, "w1:p1", "generation", status, same, now); err != nil || notice != nil {
			t.Fatalf("%s observation = %#v, %v; want no reopen", status, notice, err)
		}
	}
	var turnEnded, resumed int
	if err := db.QueryRowContext(ctx, `SELECT turn_ended,resumed FROM completed_rider_busy WHERE task_id=?`, id).Scan(&turnEnded, &resumed); err != nil || turnEnded != 1 || resumed != 0 {
		t.Fatalf("turn boundary = ended %d resumed %d, %v; want ended 1 resumed 0", turnEnded, resumed, err)
	}
	if current, _ := db.Task(ctx, project.ID, "t1"); current.State != StateDone {
		t.Fatalf("state = %s; want done", current.State)
	}
}
