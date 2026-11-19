package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func admittedStorePlan(t *testing.T, home string) taskplan.Plan {
	t.Helper()
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief:     taskplan.Select("brief", "selected brief", []byte("---\ntype: change\ntitle: Frozen contract\ndone_when: checked\n---\nKeep the intent.\n")),
		Execution: taskplan.Execution{Scope: "repo", Base: "main", Target: "main"},
		Launch:    taskplan.Launch{ProfileName: "deep", Profile: config.Profile{Kind: "pi"}, Args: []string{}},
		Review:    "none", Delivery: "local", Autonomy: config.Autonomy{Review: "ask", Merge: "ask"},
		Paths: taskplan.Paths{Scratch: filepath.Join(home, "scratch", "shop", "{task}"), Output: filepath.Join(home, "projects", "shop", "tasks", "{task}")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestLiveTasksDoesNotLoadLargePlanInputs(t *testing.T) {
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
	plan.Inputs = append(plan.Inputs, taskplan.Select("review-round-file", "review/cumulative.patch", make([]byte, 30<<20)))
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	admittedDigest := plan.SHA256()
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Profile: "deep", MergeMode: "local", State: StateSpawning, PlanJSON: encoded})
	if err != nil {
		t.Fatal(err)
	}

	tasks, err := db.LiveTasks(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d live Tasks, want 1", len(tasks))
	}
	if len(tasks[0].PlanJSON) != 0 {
		t.Fatalf("LiveTasks returned %d bytes of plan data, want none", len(tasks[0].PlanJSON))
	}
	stored, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var currentJSON, revisionJSON string
	if err := db.QueryRowContext(ctx, `SELECT plan_json FROM tasks WHERE id=?`, taskID).Scan(&currentJSON); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT plan_json FROM task_plan_revisions WHERE task_id=? AND revision=1`, taskID).Scan(&revisionJSON); err != nil {
		t.Fatal(err)
	}
	if len(currentJSON) > 1<<20 || !strings.Contains(currentJSON, `"admitted_sha256"`) || !strings.Contains(currentJSON, `"path":"blobs/sha256/`) || strings.Contains(currentJSON, `"bytes":"`+strings.Repeat("A", 128)) {
		t.Fatalf("large input was not stored as a content-addressed reference: %d bytes", len(currentJSON))
	}
	if currentJSON != revisionJSON {
		t.Fatal("current plan and initial plan revision differ")
	}
	loaded, err := stored.AdmittedPlan()
	if err != nil || loaded == nil {
		t.Fatalf("load externalized plan: %v", err)
	}
	if got := loaded.SHA256(); got != admittedDigest {
		t.Fatalf("admitted plan digest changed: got %s, want %s", got, admittedDigest)
	}
	if got := len(loaded.Inputs[len(loaded.Inputs)-1].Bytes); got != 30<<20 {
		t.Fatalf("loaded input size=%d, want %d", got, 30<<20)
	}
	cached, err := stored.AdmittedPlan()
	if err != nil || cached == loaded || &cached.Inputs[len(cached.Inputs)-1].Bytes[0] == &loaded.Inputs[len(loaded.Inputs)-1].Bytes[0] {
		t.Fatalf("plan revision was not served from the DB cache: same=%v err=%v", cached == loaded, err)
	}
	rebuiltDB, err := OpenAt(filepath.Join(home, "rebuilt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rebuiltDB.Close()
	if count, err := rebuiltDB.RebuildFromSnapshots(ctx, home); err != nil || count != 1 {
		t.Fatalf("rebuild from content-addressed snapshots: count=%d err=%v", count, err)
	}
	rebuilt, err := rebuiltDB.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	rebuiltPlan, err := rebuilt.AdmittedPlan()
	if err != nil || rebuiltPlan.SHA256() != admittedDigest || len(rebuiltPlan.Inputs[len(rebuiltPlan.Inputs)-1].Bytes) != 30<<20 {
		t.Fatalf("snapshot rebuild changed the externalized plan: plan=%v err=%v", rebuiltPlan != nil, err)
	}
}

func TestPlanInputMigrationExternalizesExistingRowsIdempotently(t *testing.T) {
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
	initial, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Profile: "deep", MergeMode: "local", State: StateSpawning, PlanJSON: initial})
	if err != nil {
		t.Fatal(err)
	}
	plan.Inputs = append(plan.Inputs, taskplan.Select("review-round-file", "review/delta.patch", make([]byte, 1<<20)))
	inline, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	digest := plan.SHA256()
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET plan_json=? WHERE project_id=?`, inline, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=(SELECT id FROM tasks WHERE project_id=?)`, inline, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE task_plan_input_migration SET completed=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	db.planCacheMu.Lock()
	db.planCache = nil
	db.planCacheMu.Unlock()
	if err := db.migrateTaskPlanInputs(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.migrateTaskPlanInputs(ctx); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	stored, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := stored.AdmittedPlan()
	if err != nil || loaded == nil || loaded.SHA256() != digest || len(loaded.Inputs[len(loaded.Inputs)-1].Bytes) != 1<<20 {
		gotDigest, gotSize := "", 0
		if loaded != nil {
			gotDigest = loaded.SHA256()
			gotSize = len(loaded.Inputs[len(loaded.Inputs)-1].Bytes)
		}
		t.Fatalf("migrated plan lost its frozen input or digest: got digest=%s size=%d, want digest=%s size=%d, err=%v", gotDigest, gotSize, digest, 1<<20, err)
	}
	var current, revision string
	if err := db.QueryRowContext(ctx, `SELECT plan_json FROM tasks WHERE id=?`, stored.ID).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT plan_json FROM task_plan_revisions WHERE task_id=? AND revision=1`, stored.ID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if current != revision || strings.Contains(current, `"bytes":"`+strings.Repeat("A", 128)) || !strings.Contains(current, `"path":"blobs/sha256/`) {
		t.Fatal("migration did not externalize both current and revision plan inputs")
	}
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
	taskID, seq, err := db.CreateTaskWithSequenceAndIntent(ctx, project.ID, project.Name, Task{Type: "change", Title: "Frozen contract", ShortName: "frozen-contract", Profile: "deep", MergeMode: "local", PlanJSON: data}, "start", 12345, func(int) (bool, error) { return false, nil })
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
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Profile: "deep", MergeMode: "local", PlanJSON: data})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(ctx, id, "deep", "", false); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTaskRepos(ctx, id, []TaskRepo{{Repo: "api", MergeMode: "local"}}); err != nil {
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
	transitions, err := db.TaskTransitions(ctx, id, 10)
	if err != nil || len(transitions) == 0 || !strings.Contains(transitions[0].Note, "changed fields: launch") {
		t.Fatalf("plan revision did not report changed fields to the Lead: %#v, %v", transitions, err)
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

func TestPlanRevisionPreservesMergeGateForMergingTask(t *testing.T) {
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
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Profile: "deep", MergeMode: "local", PlanJSON: data})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateWorking, StateDone, "worker", "ready"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateDone, StateMerging, "cli", "merge started"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTaskRepos(ctx, id, []TaskRepo{{Repo: "api", MergeMode: "local"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET gated_sha='gated-head' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE task_repos SET gated_sha='member-gate' WHERE task_id=?`, id); err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	launch := plan.Launch
	launch.ProfileName = "next"
	revision, err := plan.Reconfigure(launch)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReviseTaskPlan(ctx, task, revision); err != nil {
		t.Fatal(err)
	}
	updated, err := db.TaskByID(ctx, project.ID, id)
	if err != nil || updated.State != StateMerging || updated.GatedSHA != "gated-head" {
		t.Fatalf("revised merging Task = state %s gated_sha %q, err=%v", updated.State, updated.GatedSHA, err)
	}
	repos, err := db.TaskRepos(ctx, id)
	if err != nil || len(repos) != 1 || repos[0].GatedSHA != "member-gate" {
		t.Fatalf("revised merging Task Member gates = %#v, err=%v", repos, err)
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
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Profile: "deep", MergeMode: "local", PlanJSON: data})
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
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Profile: "old"})
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
