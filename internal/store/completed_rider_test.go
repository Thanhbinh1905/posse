package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCompletedRiderObservationIsDurableAtomicAndConcurrent(t *testing.T) {
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
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 2, Type: "ship", Title: "t2", LandingMode: "local", AutonomyReview: "ask", AutonomyLand: "ask"})
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
	for _, state := range []string{"gated", "landing", "landed", "unchanged"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO task_repos(task_id,repo,worktree_path,base_ref,landing_mode,state,gated_sha,updated_at) VALUES(?,?,'/mount','base','local',?,'head',0)`, id, state, state); err != nil {
			t.Fatal(err)
		}
	}
	task, err := db.Task(ctx, project.ID, "t2")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if notice, err := db.ObserveCompletedRider(ctx, task, "w1:p2", "generation", "working", now); err != nil || notice != nil {
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
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_resumed BEFORE INSERT ON notices WHEN NEW.kind='rider_resumed' BEGIN SELECT RAISE(ABORT,'test Notice failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ObserveCompletedRider(ctx, task, "w1:p2", "generation", "idle", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ObserveCompletedRider(ctx, task, "w1:p2", "generation", "working", now.Add(2*time.Second)); err == nil {
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
			_, err := db.ObserveCompletedRider(ctx, task, "w1:p2", "generation", "working", now.Add(3*time.Second))
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
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='rider_resumed'`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("concurrent Notices: %d %v", count, err)
	}
	repos, err := db.TaskRepos(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range repos {
		if repo.Repo == "landed" {
			if repo.State != TaskRepoLanded || repo.GatedSHA != "head" {
				t.Fatalf("merged Member changed: %#v", repo)
			}
		} else if repo.GatedSHA != "" || ((repo.Repo == "gated" || repo.Repo == "unchanged") && repo.State != TaskRepoOpen) {
			t.Fatalf("stale Member gate survived: %#v", repo)
		}
	}
}
