package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store/sqlc"
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
	const largePlanInputSize = 65 << 10
	ctx := context.Background()
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	defer conn.Close()
	db := &DB{DB: conn}
	db.queries = sqlc.New(db)
	for _, statement := range []string{
		`CREATE TABLE tasks (
			id INTEGER PRIMARY KEY, project_id INTEGER NOT NULL, seq INTEGER NOT NULL, type TEXT NOT NULL DEFAULT '',
			reviews_task_id INTEGER, title TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT '', profile TEXT NOT NULL DEFAULT '',
			dispatch_rule TEXT NOT NULL DEFAULT '', landing_mode TEXT NOT NULL DEFAULT '', autonomy_review TEXT NOT NULL DEFAULT '',
			autonomy_land TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '', base_ref TEXT NOT NULL DEFAULT '',
			worktree_path TEXT NOT NULL DEFAULT '', herdr_workspace_id TEXT NOT NULL DEFAULT '', pane_id TEXT NOT NULL DEFAULT '',
			pane_label TEXT NOT NULL DEFAULT '', agent_name TEXT NOT NULL DEFAULT '', agent_session TEXT NOT NULL DEFAULT '',
			pr_url TEXT NOT NULL DEFAULT '', landed_ref TEXT NOT NULL DEFAULT '', last_output_hash TEXT NOT NULL DEFAULT '',
			last_worktree_hash TEXT NOT NULL DEFAULT '', last_progress_at INTEGER NOT NULL DEFAULT 0,
			agent_absent_since INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0,
			mount_id INTEGER, launches INTEGER NOT NULL DEFAULT 0, gated_sha TEXT NOT NULL DEFAULT '',
			agent_server_started_at TEXT NOT NULL DEFAULT '', idle_since INTEGER NOT NULL DEFAULT 0,
			short_name TEXT NOT NULL DEFAULT '', agent_session_kind TEXT NOT NULL DEFAULT '', target_branch TEXT NOT NULL DEFAULT '',
			pr_title TEXT NOT NULL DEFAULT '', plan_json TEXT NOT NULL DEFAULT '', report_kind TEXT NOT NULL DEFAULT '',
			report_digest TEXT NOT NULL DEFAULT '', report_manifest_json TEXT NOT NULL DEFAULT '',
			containment_mode TEXT NOT NULL DEFAULT '', containment_path TEXT NOT NULL DEFAULT '',
			containment_marker TEXT NOT NULL DEFAULT '', pane_shell_identity TEXT NOT NULL DEFAULT '',
			agent_status TEXT NOT NULL DEFAULT '', agent_status_since INTEGER NOT NULL DEFAULT 0,
			plan_revision INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX tasks_project_state_idx ON tasks(project_id,state,seq)`,
		`CREATE TABLE task_plan_revisions (task_id INTEGER NOT NULL)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	inlineInput := strings.Repeat("A", largePlanInputSize)
	legacyPlan := fmt.Sprintf(`{"inputs":[{"role":"review-round-file","bytes":"%s"},{"role":"review-round-file","bytes":"%s"}]}`, inlineInput, inlineInput)
	if _, err := db.ExecContext(ctx, `INSERT INTO tasks(id,project_id,seq,state,plan_json) VALUES(1,7,1,'working',?)`, legacyPlan); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO task_plan_revisions(task_id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	var storedPlanSize int
	if err := db.QueryRowContext(ctx, `SELECT length(plan_json) FROM tasks WHERE id=1`).Scan(&storedPlanSize); err != nil {
		t.Fatal(err)
	}
	if storedPlanSize < 2*largePlanInputSize {
		t.Fatalf("stored plan size=%d, want at least %d", storedPlanSize, 2*largePlanInputSize)
	}
	tasks, err := db.LiveTasks(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d live Tasks, want 1", len(tasks))
	}
	if len(tasks[0].PlanJSON) != 0 {
		t.Fatalf("LiveTasks returned %d bytes of plan data, want none", len(tasks[0].PlanJSON))
	}
}

func TestPlanInputExternalizationExternalizesLargeBrief(t *testing.T) {
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	plan := admittedStorePlan(t, home).Bind("t1")
	plan.Inputs[0] = taskplan.Select("brief", "selected brief", append([]byte("---\ntype: change\ntitle: Large brief\ndone_when: preserved inline\n---\n"), bytes.Repeat([]byte("brief content "), 1<<13)...))
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	stored, err := db.externalizePlanJSON(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var got taskplan.Plan
	if err := json.Unmarshal([]byte(stored), &got); err != nil {
		t.Fatal(err)
	}
	if got.Inputs[0].Path == "" || len(got.Inputs[0].Bytes) != 0 {
		t.Fatalf("large Brief was not externalized: path=%q bytes=%d", got.Inputs[0].Path, len(got.Inputs[0].Bytes))
	}
	externalizedBrief, err := db.readPlanBlob(got.Inputs[0])
	if err != nil || !bytes.Equal(externalizedBrief, plan.Inputs[0].Bytes) {
		t.Fatalf("externalized Brief changed: bytes=%d err=%v", len(externalizedBrief), err)
	}
	got.Inputs[0].Bytes = externalizedBrief
	got.Inputs[0].Path = ""
	storedWithBrief, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := taskplan.Decode(string(storedWithBrief)); err != nil {
		t.Fatalf("decode stored plan with inline Brief: %v", err)
	}
}

func TestPlanInputExternalizationRepairsMixedInlineAndBlobInputs(t *testing.T) {
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	plan := admittedStorePlan(t, home).Bind("t1")
	plan.Inputs = append(plan.Inputs,
		taskplan.Select("review-round-file", "review/one.patch", make([]byte, 1<<20)),
		taskplan.Select("review-round-file", "review/two.patch", bytes.Repeat([]byte{0x5a}, 1<<20)),
	)
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	externalized, err := db.externalizePlanJSON(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var mixed taskplan.Plan
	if err := json.Unmarshal([]byte(externalized), &mixed); err != nil {
		t.Fatal(err)
	}
	second := &mixed.Inputs[len(mixed.Inputs)-1]
	second.Bytes, err = db.readPlanBlob(*second)
	if err != nil {
		t.Fatal(err)
	}
	second.Path = ""
	mixedJSON, err := json.Marshal(mixed)
	if err != nil {
		t.Fatal(err)
	}

	got, err := db.externalizePlanJSON(string(mixedJSON))
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := db.externalizePlanJSON(got)
	if err != nil || repeated != got {
		t.Fatalf("repeat externalization changed output: equal=%v err=%v", repeated == got, err)
	}
	var repaired taskplan.Plan
	if err := json.Unmarshal([]byte(got), &repaired); err != nil {
		t.Fatal(err)
	}
	if len(repaired.Inputs) != len(mixed.Inputs) {
		t.Fatalf("got %d inputs, want %d", len(repaired.Inputs), len(mixed.Inputs))
	}
	for i, input := range repaired.Inputs[2:] {
		if input.Path == "" || len(input.Bytes) != 0 {
			t.Fatalf("input %d was not externalized: path=%q bytes=%d", i+2, input.Path, len(input.Bytes))
		}
	}
	loaded, err := db.loadAdmittedPlan(123, got)
	if err != nil || loaded.SHA256() != plan.SHA256() {
		t.Fatalf("repaired plan changed its digest: got=%v err=%v", loaded, err)
	}
}

func TestPlanInputMigrationExternalizesExistingRowsIdempotently(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
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
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET plan_json=?,plan_revision=0 WHERE id=?`, inline, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=?`, inline, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE task_plan_input_migration SET completed=0,tasks_done=0,revisions_done=0,task_cursor=0,revision_task_cursor=0,revision_cursor=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenAt(filepath.Join(home, "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := db.QueryRowContext(ctx, `SELECT plan_json FROM tasks WHERE id=?`, taskID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != inline {
		t.Fatalf("Open rewrote inline plan input: stored %d bytes, want %d", len(stored), len(inline))
	}
	live, err := db.LiveTasks(ctx, project.ID)
	if err != nil || len(live) != 1 {
		t.Fatalf("load legacy live Task: tasks=%d err=%v", len(live), err)
	}
	livePlan, err := live[0].AdmittedPlan()
	if err != nil || livePlan == nil || len(livePlan.Inputs[len(livePlan.Inputs)-1].Bytes) != 1<<20 {
		t.Fatalf("legacy LiveTasks plan unreadable before migration: plan=%v err=%v", livePlan != nil, err)
	}
	if err := db.migrateTaskPlanRowsBatch(ctx, 0); err != nil {
		t.Fatal(err)
	}
	db.planCacheMu.Lock()
	db.planCache = nil
	db.planCacheMu.Unlock()
	currentTask, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	midMigration, err := currentTask.AdmittedPlan()
	if err != nil || midMigration == nil || len(midMigration.Inputs[len(midMigration.Inputs)-1].Bytes) != 1<<20 {
		t.Fatalf("plan unreadable while revision rows remain inline: plan=%v err=%v", midMigration != nil, err)
	}
	var revisionDuringMigration string
	if err := db.QueryRowContext(ctx, `SELECT plan_json FROM task_plan_revisions WHERE task_id=? AND revision=1`, taskID).Scan(&revisionDuringMigration); err != nil {
		t.Fatal(err)
	}
	if revisionDuringMigration != inline {
		t.Fatal("task revision row changed before its migration batch")
	}
	if err := db.migrateTaskPlanInputs(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.migrateTaskPlanInputs(ctx); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	db.planCacheMu.Lock()
	db.planCache = nil
	db.planCacheMu.Unlock()
	storedTask, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := storedTask.AdmittedPlan()
	if err != nil || loaded == nil || loaded.SHA256() != digest || len(loaded.Inputs[len(loaded.Inputs)-1].Bytes) != 1<<20 {
		gotDigest, gotSize := "", 0
		if loaded != nil {
			gotDigest = loaded.SHA256()
			gotSize = len(loaded.Inputs[len(loaded.Inputs)-1].Bytes)
		}
		t.Fatalf("migrated plan lost its frozen input or digest: got digest=%s size=%d, want digest=%s size=%d, err=%v", gotDigest, gotSize, digest, 1<<20, err)
	}
	var current, revision string
	if err := db.QueryRowContext(ctx, `SELECT plan_json FROM tasks WHERE id=?`, storedTask.ID).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT plan_json FROM task_plan_revisions WHERE task_id=? AND revision=1`, storedTask.ID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if current != revision || strings.Contains(current, `"bytes":"`+strings.Repeat("A", 128)) || !strings.Contains(current, `"path":"blobs/sha256/`) {
		t.Fatal("migration did not externalize both current and revision plan inputs")
	}
}

func TestTaskCreationAcceptsLargeExternalizedPlanInputs(t *testing.T) {
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
	plan.Inputs = append(plan.Inputs, taskplan.Select("review-round-file", "review/large.patch", make([]byte, 1<<20)))
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	digest := plan.Bind("t1").SHA256()
	taskID, _, err := db.CreateTaskWithSequenceAndIntent(ctx, project.ID, project.Name, Task{Type: "change", Title: "Large input", ShortName: "large-input", PlanJSON: encoded}, "start", 12345, func(int) (bool, error) { return false, nil })
	if err != nil {
		t.Fatalf("create Task with large externalized input: %v", err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := task.AdmittedPlan()
	if err != nil || admitted == nil || admitted.SHA256() != digest || len(admitted.Inputs[len(admitted.Inputs)-1].Bytes) != 1<<20 {
		gotDigest, gotSize := "", 0
		if admitted != nil {
			gotDigest = admitted.SHA256()
			gotSize = len(admitted.Inputs[len(admitted.Inputs)-1].Bytes)
		}
		t.Fatalf("created Task lost its externalized input: digest=%s size=%d want digest=%s size=%d err=%v", gotDigest, gotSize, digest, 1<<20, err)
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

func TestCascadeTeardownSkipsTaskWhosePlanChangedAfterClosureSelection(t *testing.T) {
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
	data, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Dependent", State: StateQueued, PlanJSON: data})
	if err != nil {
		t.Fatal(err)
	}
	childPlan := admittedStorePlan(t, home).Bind("t2")
	childPlan.Dependencies = []dispatch.Dependency{{TaskID: "t1", Milestone: "landed"}}
	childData, err := childPlan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	childID, err := db.CreateTask(ctx, project.ID, Task{Seq: 2, Type: "change", Title: "Transitive dependant", State: StateQueued, PlanJSON: childData})
	if err != nil {
		t.Fatal(err)
	}
	childDecision, err := db.RaiseDecision(ctx, DecisionRequest{ProjectID: project.ID, TaskID: childID, Origin: "dependency:test", Kind: "dependency", Question: "Resolve the dependency", Options: []string{"continue"}})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := db.TaskByID(ctx, project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	launch := plan.Launch
	launch.ProfileName = "revised"
	revision, err := plan.Reconfigure(launch)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReviseTaskPlan(ctx, selected, revision); err != nil {
		t.Fatal(err)
	}
	child, err := db.TaskByID(ctx, project.ID, childID)
	if err != nil {
		t.Fatal(err)
	}
	raced, err := db.CascadeTeardownQueued(ctx, []Task{selected, child}, "cascade")
	if err != nil || len(raced) != 1 || raced[0] != id {
		t.Fatalf("cascade result raced=%v err=%v, want Task %d skipped", raced, err, id)
	}
	current, err := db.TaskByID(ctx, project.ID, id)
	if err != nil || current.State != StateQueued || current.PlanRevision != 2 {
		t.Fatalf("changed-plan Task was torn down: task=%+v err=%v", current, err)
	}
	currentChild, err := db.TaskByID(ctx, project.ID, childID)
	if err != nil || currentChild.State != StateQueued {
		t.Fatalf("transitive Task was torn down after its predecessor raced: task=%+v err=%v", currentChild, err)
	}
	decisions, err := db.Decisions(ctx, project.ID, true)
	if err != nil || len(decisions) != 1 || decisions[0].ID != childDecision.ID {
		t.Fatalf("transitive Task Decision was retired after its predecessor raced: decisions=%#v err=%v", decisions, err)
	}
}

func TestReviseTaskPlansRollsBackEveryRevisionWhenOneTaskRaces(t *testing.T) {
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
	basePlan := admittedStorePlan(t, home)
	var tasks []Task
	var revisions []TaskPlanRevision
	for seq := 1; seq <= 2; seq++ {
		plan := basePlan.Bind(fmt.Sprintf("t%d", seq))
		data, err := plan.Encode()
		if err != nil {
			t.Fatal(err)
		}
		taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: seq, Type: "change", Title: fmt.Sprintf("Task %d", seq), State: StateQueued, PlanJSON: data})
		if err != nil {
			t.Fatal(err)
		}
		task, err := db.TaskByID(ctx, project.ID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		launch := plan.Launch
		launch.ProfileName = fmt.Sprintf("profile-%d", seq)
		revision, err := plan.Reconfigure(launch)
		if err != nil {
			t.Fatal(err)
		}
		revisions = append(revisions, TaskPlanRevision{Task: task, Plan: revision})
		tasks = append(tasks, task)
	}
	if err := db.Transition(ctx, tasks[1].ID, StateQueued, StateTornDown, "cli", "raced plan revision"); err != nil {
		t.Fatal(err)
	}
	if err := db.ReviseTaskPlans(ctx, revisions); !errors.Is(err, ErrStateRace) {
		t.Fatalf("batch revision error=%v, want state race", err)
	}
	unchanged, err := db.TaskByID(ctx, project.ID, tasks[0].ID)
	if err != nil || unchanged.PlanRevision != 1 {
		t.Fatalf("earlier plan revision was partially applied: task=%+v err=%v", unchanged, err)
	}
	plans, err := db.TaskPlanRevisions(ctx, tasks[0].ID)
	if err != nil || len(plans) != 1 {
		t.Fatalf("rolled-back plan revision history=%#v err=%v", plans, err)
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
