package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func admittedStorePlan(t *testing.T, home string) taskplan.Plan {
	t.Helper()
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief:     taskplan.Select("brief", "selected brief", []byte("---\ntype: ship\ntitle: Frozen contract\ndone_when: checked\n---\nKeep the intent.\n")),
		Execution: taskplan.Execution{Scope: "repo", Base: "main", Target: "main"},
		Launch:    taskplan.Launch{ProfileName: "deep", Profile: config.Profile{Kind: "pi"}, Args: []string{}},
		Review:    "none", Delivery: "local", Autonomy: config.Autonomy{Review: "ask", Land: "ask"},
		Paths: taskplan.Paths{Scratch: filepath.Join(home, "scratch", "shop", "{task}"), Output: filepath.Join(home, "projects", "shop", "tasks", "{task}")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestTaskCreationAtomicallyStoresAdmittedPlanAndRevision(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	plan := admittedStorePlan(t, home)
	data, _ := plan.Encode()
	taskID, seq, err := db.CreateTaskWithSequenceAndIntent(ctx, project.ID, project.Name, Task{Type: "ship", Title: "Frozen contract", ShortName: "frozen-contract", Profile: "deep", LandingMode: "local", PlanJSON: data}, "ride", 12345, func(int) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := task.AdmittedPlan()
	if err != nil || admitted == nil || seq != 1 || admitted.Paths.Output != filepath.Join(home, "projects", "shop", "tasks", "t1") {
		t.Fatalf("creation lost admission: %#v %v", admitted, err)
	}
	revisions, err := db.TaskPlanRevisions(ctx, taskID)
	if err != nil || !reflect.DeepEqual(revisions, []string{task.PlanJSON}) {
		t.Fatalf("creation lost revision: %#v %v", revisions, err)
	}
	if intents, err := db.Intents(ctx, project.ID); err != nil || len(intents) != 1 {
		t.Fatalf("creation lost intent: %#v %v", intents, err)
	}
	// The first atomic snapshot must contain the plan even if no launch files or
	// execution resources have been written when the process crashes.
	recoveredDB, err := OpenAt(filepath.Join(home, "rebuilt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer recoveredDB.Close()
	if count, err := recoveredDB.RebuildFromSnapshots(ctx, home); err != nil || count != 1 {
		t.Fatalf("fresh-database rebuild count=%d err=%v", count, err)
	}
	rebuilt, err := recoveredDB.TaskByID(ctx, project.ID, taskID)
	if err != nil || rebuilt.PlanJSON != task.PlanJSON {
		t.Fatalf("rebuild changed admission: %#v %v", rebuilt, err)
	}
}

func TestExplicitPlanRevisionInvalidatesEvidenceAndSurvivesRebuild(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	plan := admittedStorePlan(t, home).Bind("t1")
	data, _ := plan.Encode()
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Profile: "deep", LandingMode: "local", PlanJSON: data})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(ctx, id, "deep", "", false); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTaskRepos(ctx, id, []TaskRepo{{Repo: "api", LandingMode: "local"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET gated_sha='old-gate' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE task_repos SET gated_sha='old-member-gate' WHERE task_id=?`, id); err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	launch := plan.Launch
	launch.ProfileName, launch.Profile.Model, launch.ConfiguredModel, launch.ModelKnown = "next", "new-model", "new-model", true
	revision, err := plan.Reconfigure(launch)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReviseTaskPlan(ctx, task, revision); err != nil {
		t.Fatal(err)
	}
	if err := db.ReviseTaskPlan(ctx, task, revision); !errors.Is(err, ErrStateRace) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(ctx, id, "next", "new-model", true); err != nil {
		t.Fatal(err)
	}
	task, err = db.TaskByID(ctx, project.ID, id)
	if err != nil || task.GatedSHA != "" || task.Profile != "next" {
		t.Fatalf("revision did not invalidate Task evidence: %#v %v", task, err)
	}
	repos, err := db.TaskRepos(ctx, id)
	if err != nil || len(repos) != 1 || repos[0].GatedSHA != "" {
		t.Fatalf("revision did not invalidate Member evidence: %#v %v", repos, err)
	}
	before, err := db.TaskPlanRevisions(ctx, id)
	if err != nil || len(before) != 2 {
		t.Fatalf("revision history=%#v %v", before, err)
	}
	recoveredDB, err := OpenAt(filepath.Join(home, "rebuilt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer recoveredDB.Close()
	if count, err := recoveredDB.RebuildFromSnapshots(ctx, home); err != nil || count != 1 {
		t.Fatalf("fresh-database rebuild count=%d err=%v", count, err)
	}
	after, err := recoveredDB.TaskPlanRevisions(ctx, id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rebuild changed plans: %#v %v", after, err)
	}
	identities, err := recoveredDB.TaskLaunchIdentities(ctx, id)
	if err != nil || len(identities) != 2 || identities[0].ModelKnown || identities[0].ConfiguredModel != "" || identities[0].PlanRevision != 1 || identities[1].PlanRevision != 2 {
		t.Fatalf("rebuild inferred unknown identity or lost revisions: %#v %v", identities, err)
	}
}

func TestConcurrentSnapshotsKeepAdmittedPlanAndHistoryCoherent(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	plan := admittedStorePlan(t, home).Bind("t1")
	data, _ := plan.Encode()
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Profile: "deep", LandingMode: "local", PlanJSON: data})
	if err != nil {
		t.Fatal(err)
	}
	var writers sync.WaitGroup
	errorsFound := make(chan error, 4)
	for writer := 0; writer < 4; writer++ {
		writers.Go(func() {
			for range 12 {
				if err := db.PersistTask(ctx, id); err != nil {
					errorsFound <- err
					return
				}
				contents, err := os.ReadFile(db.TaskSnapshotPath(project.Name, 1))
				if err != nil {
					errorsFound <- err
					return
				}
				var snapshot TaskSnapshot
				if _, err := toml.Decode(string(contents), &snapshot); err != nil {
					errorsFound <- err
					return
				}
				captured, err := snapshot.Task.AdmittedPlan()
				if err != nil {
					errorsFound <- err
					return
				}
				if captured == nil || len(snapshot.PlanRevisions) != captured.Revision || snapshot.PlanRevisions[len(snapshot.PlanRevisions)-1] != snapshot.Task.PlanJSON {
					errorsFound <- fmt.Errorf("incoherent admitted snapshot")
					return
				}
			}
		})
	}
	for range 8 {
		task, err := db.TaskByID(ctx, project.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		current, err := task.AdmittedPlan()
		if err != nil {
			t.Fatal(err)
		}
		next, err := current.Reconfigure(current.Launch)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.ReviseTaskPlan(ctx, task, next); err != nil {
			t.Fatal(err)
		}
	}
	writers.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := task.AdmittedPlan()
	if err != nil || latest == nil || latest.Revision != 9 {
		t.Fatalf("concurrent snapshot overwrote admission: %#v %v", latest, err)
	}
}

func TestHistoricalTaskRemainsLegacyAfterRebuild(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Profile: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := task.AdmittedPlan()
	if err != nil || plan != nil {
		t.Fatalf("legacy Task was silently admitted: %#v %v", plan, err)
	}
}
