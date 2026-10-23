package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWorkerAgentArgsWithoutConfiguredArgumentsMarshalAsEmptyArray(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[profiles.pi]\nkind = \"pi\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	args := workerAgentArgs(cfg.Profiles["pi"], cfg.Kinds["pi"], "")
	if args == nil {
		t.Fatal("Worker args are nil")
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("Worker args JSON = %s, want []", encoded)
	}
}

func TestOpenCodeWorkerArgsAndResume(t *testing.T) {
	cfg, err := config.Load(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	profile := config.Profile{Kind: "opencode", Model: "provider/model", Args: []string{"--mini"}}
	if got := workerAgentArgs(profile, cfg.Kinds["opencode"], ""); !equalStrings(got, []string{"--auto", "--model", "provider/model", "--mini"}) {
		t.Fatalf("OpenCode Worker args = %#v", got)
	}
	if got := workerAgentArgs(profile, cfg.Kinds["opencode"], "ses_123"); !equalStrings(got, []string{"--auto", "--model", "provider/model", "--mini", "--session", "ses_123"}) {
		t.Fatalf("OpenCode resume args = %#v", got)
	}
}

func TestWorkerDisplayUsesSidebarFieldsWithoutChangingCanonicalIdentity(t *testing.T) {
	task := store.Task{Seq: 12, Title: "Stable output for workers", ShortName: "stable-output", Branch: "posse/t12", WorktreePath: "/tmp/pool/shop/sandbox-2", PaneID: "w2:p1", PaneLabel: "posse:shop:t12"}
	if got := workerTabLabel(task); got != "stable-output" {
		t.Fatalf("Worker tab label = %q", got)
	}
	metadata := workerDisplayMetadata(task, "claude", "")
	if metadata["title"] != "Stable output for workers · stable-output · /tmp/pool/shop/sandbox-2" || metadata["clear_display_agent"] != nil || metadata["display_agent"] != "claude" || metadata["pane_id"] != task.PaneID {
		t.Fatalf("Worker display metadata = %#v", metadata)
	}
	if got := taskPaneLabel.FindString(task.PaneLabel); got != task.PaneLabel {
		t.Fatalf("canonical pane label changed = %q", got)
	}
	tokens := metadata["tokens"].(map[string]string)
	if tokens["posse_name"] != task.ShortName || tokens["posse_branch"] != task.Branch {
		t.Fatalf("sidebar name/branch tokens = %#v, want %q and %q", tokens, task.ShortName, task.Branch)
	}
}

func TestWorkerDisplaySandboxContextDoesNotTruncateMidPath(t *testing.T) {
	sandbox := filepath.Join("/tmp", strings.Repeat("ci-run-", 18), "posse", "pool", "shop", "sandbox-3")
	task := store.Task{Seq: 3, Title: "Show Worker worktrees", ShortName: "show-worktrees", Branch: "posse/show-worktrees", WorktreePath: sandbox, PaneID: "w2:p1"}
	metadata := workerDisplayMetadata(task, "codex", "")
	if metadata["clear_display_agent"] != nil || metadata["display_agent"] != "codex" {
		t.Fatalf("Worker harness subtitle does not match the detected agent: %#v", metadata)
	}
	tokens := metadata["tokens"].(map[string]string)
	if got := tokens["posse_sandbox"]; got != "sandbox-3" {
		t.Fatalf("Sandbox token = %q, want stable basename rather than Herdr's truncated path", got)
	}
	if got := metadata["title"].(string); !strings.Contains(got, sandbox) {
		t.Fatalf("full Sandbox path was not submitted as pane title: %q", got)
	}
}

func TestTaskTitlesGenerateDisplayNames(t *testing.T) {
	for title, want := range map[string]string{
		"Hide tool calls in the Pi Lead while lowkey is on": "hide-tool-calls",
		"Span backend and worker":                           "span-backend-worker",
		"Fix the flaky login test":                          "fix-flaky-login",
		"feat(app): enable outcome-first first runs":        "outcome-first-runs",
		"Outcome-first first runs":                          "outcome-first-runs",
		"first-first runs":                                  "first-runs",
		"fix(auth)!: repair the flaky login test":           "repair-flaky-login",
	} {
		if got := taskTitleSlug(title); got != want {
			t.Errorf("generated Task Name for %q = %q, want %q", title, got, want)
		}
	}
}

func TestStartRejectsInvalidOptionalDisplayName(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "home")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProject(context.Background(), "shop", repo, "main"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(root, "brief.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Fix unstable tests\ndone_when: Tests pass\n---\nRepair.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	for _, name := range []string{"Worker-tree", "worker_tree", "worker--tree", "-worker", "worker-", "t36", "worker-name-that-is-too-long-for-sidebar"} {
		t.Run(name, func(t *testing.T) {
			output := &bytes.Buffer{}
			cli := testService(home, nil).CLI()
			cli.Out, cli.ErrOut = output, output
			if code := cli.Run([]string{"start", "--brief", brief, "--name", name}); code != 1 || !strings.Contains(output.String(), "name_invalid") || !strings.Contains(output.String(), "lowercase kebab-case") {
				t.Fatalf("invalid Worker name exit=%d output=%s", code, output.String())
			}
		})
	}
	if got := defaultTaskName("Fix unstable tests"); got != "fix-unstable-tests" {
		t.Fatalf("default Task name = %q", got)
	}
	if got := defaultTaskName("東京"); got != "task" {
		t.Fatalf("unsafe derived Task name = %q, want safe fallback", got)
	}
	output := &bytes.Buffer{}
	cli := testService(home, nil).CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"start", "--brief", brief, "--name", "research-generalizing-opus"}); code != 1 || !strings.Contains(output.String(), "--name research-generaliz-retry") {
		t.Fatalf("invalid name did not suggest an accepted retry: code=%d output=%s", code, output)
	}
	retry := workerNameRetrySuggestion("research-generalizing-opus")
	if retry != "research-generaliz-retry" || validateWorkerName(retry) != nil {
		t.Fatalf("suggested retry is not an accepted Task Name: %q", retry)
	}
}

func TestStartQueueAdmitsWithoutAllocatingResourcesAtCapacity(t *testing.T) {
	fixture := newStartFixture(t)
	preferences := filepath.Join(fixture.home, "projects", "shop", "preferences")
	if err := os.MkdirAll(preferences, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(preferences, "worker.md"), []byte("Admission-time Worker guidance."), 0o600); err != nil {
		t.Fatal(err)
	}
	configText := `[defaults]
max_workers = 1
merge_mode = "local"

[profiles.claude]
kind = "claude"

[dispatch.default]
use = "claude"
`
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(context.Background(), fixture.project.ID, store.Task{Seq: 1, Type: "change", Title: "Existing Worker", ShortName: "existing-worker", State: store.StateSpawning, MergeMode: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"start", "--brief", fixture.brief, "--name", "launch-probe", "--queue"}); code != 0 {
		t.Fatalf("queued start exit=%d output=%s", code, output.String())
	}
	db, err = store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tasks, err := db.Tasks(context.Background(), fixture.project.ID, true)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("Tasks=%#v err=%v", tasks, err)
	}
	queued := tasks[1]
	if queued.State != store.StateQueued || queued.Branch != "" || queued.WorktreePath != "" || queued.SandboxID != 0 || queued.PaneID != "" {
		t.Fatalf("queued Task allocated resources: %+v", queued)
	}
	plan, err := queued.AdmittedPlan()
	briefBytes, readErr := os.ReadFile(fixture.brief)
	if err != nil || readErr != nil || plan == nil || !bytes.Equal(plan.Inputs[0].Bytes, briefBytes) {
		t.Fatalf("queued Task did not retain its admitted Brief: plan=%#v err=%v readErr=%v", plan, err, readErr)
	}
	frozenPreference := false
	for _, input := range plan.Inputs {
		if input.Role == "preferences" && string(input.Bytes) == "Admission-time Worker guidance." {
			frozenPreference = true
		}
	}
	if !frozenPreference {
		t.Fatalf("queued Task did not retain its admission-time Worker guidance: %#v", plan.Inputs)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if code := cli.Run([]string{"teardown", taskIDString(queued.Seq)}); code != 0 {
		t.Fatalf("cancel queued Task exit=%d output=%s", code, output.String())
	}
	db, err = store.OpenReadOnly(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	canceled, err := db.TaskByID(context.Background(), fixture.project.ID, queued.ID)
	if err != nil || canceled.State != store.StateTornDown || canceled.Branch != "" || canceled.WorktreePath != "" {
		t.Fatalf("queued Task cancellation state=%+v err=%v", canceled, err)
	}
	if fixture.fake.CallCount("tab.create") != 0 || fixture.fake.CallCount("agent.start") != 0 {
		t.Fatalf("queued Task cancellation touched Herdr: %#v", fixture.fake.Calls)
	}
}

func TestQueuedTaskRefillsAfterRestartWithFrozenAdmission(t *testing.T) {
	fixture := newStartFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte("[defaults]\nmax_workers = 1\nmerge_mode = \"local\"\n\n[profiles.claude]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"claude\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	activeID, err := db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 1, Type: "change", Title: "Current Worker", ShortName: "current-worker", State: store.StateSpawning})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"start", "--brief", fixture.brief, "--name", "refill-probe", "--queue"}); code != 0 {
		t.Fatalf("queue admission exit=%d output=%s", code, output.String())
	}
	db, err = store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := db.Task(ctx, fixture.project.ID, "t2")
	if err != nil || queued.State != store.StateQueued || queued.Branch != "" || queued.PaneID != "" || queued.SandboxID != 0 {
		t.Fatalf("queued Task is not durably resource-free before restart: task=%+v err=%v", queued, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen the store as a cold recovery would, then release the old Worker.
	db, err = store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Transition(ctx, activeID, store.StateSpawning, store.StateFailed, "cli", "fixture Worker finished"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	service := testService(fixture.home, fixture.fake)
	project, err := db.ProjectByID(ctx, fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.retryPendingLaunches(ctx, db, project, cfg, fixture.fake.SnapshotValue); err != nil {
		t.Fatalf("refill queued Task: %v", err)
	}
	started, err := db.Task(ctx, fixture.project.ID, "t2")
	if err != nil || started.State != store.StateWorking || started.Branch != store.TaskBranchName(queued.Seq) || started.PaneID == "" || started.SandboxID == 0 {
		t.Fatalf("queued Task was not launched through its admitted identity: task=%+v err=%v", started, err)
	}
	if got, err := db.ActiveWorkerCount(ctx, fixture.project.ID); err != nil || got != 1 {
		t.Fatalf("refill exceeded max_workers: active=%d err=%v", got, err)
	}
	if tasks, err := db.Tasks(ctx, fixture.project.ID, true); err != nil || len(tasks) != 2 || tasks[1].ID != queued.ID {
		t.Fatalf("refill recreated rather than resumed queued Task: tasks=%#v err=%v", tasks, err)
	}
	if _, err := db.IntentByTask(ctx, queued.ID); !store.IsNotFound(err) {
		t.Fatalf("successful refill left a start intent behind: %v", err)
	}
}

func TestStartWaitsOnTypedLandedPrerequisiteWithoutAllocatingResources(t *testing.T) {
	fixture := newStartFixture(t)
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	predecessorID, err := db.CreateTask(context.Background(), fixture.project.ID, store.Task{Seq: 1, Type: "change", Title: "Predecessor", ShortName: "predecessor", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), predecessorID, store.StateSpawning, store.StateWorking, "cli", "fixture Worker started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	brief := `---
type: change
title: Dependent change
done_when: predecessor has landed
after: t1:landed
---
Implement after the prerequisite.
`
	if err := os.WriteFile(fixture.brief, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"start", "--brief", fixture.brief}); code != 0 {
		t.Fatalf("dependent start exit=%d output=%s", code, output.String())
	}
	db, err = store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dependent, err := db.Task(context.Background(), fixture.project.ID, "t2")
	if err != nil || dependent.State != store.StateQueued || dependent.Branch != "" || dependent.WorktreePath != "" {
		t.Fatalf("dependent Task=%+v err=%v", dependent, err)
	}
	plan, err := dependent.AdmittedPlan()
	if err != nil || plan == nil || len(plan.Dependencies) != 1 || plan.Dependencies[0].Milestone != "landed" || plan.Execution.Base != "main" || plan.Execution.BaseCommit != "" {
		t.Fatalf("dependency was not frozen in admitted plan: plan=%+v err=%v", plan, err)
	}
	unmet, err := db.UnmetTaskDependencies(context.Background(), fixture.project.ID, plan.Dependencies)
	if err != nil || len(unmet) != 1 || unmet[0] != "t1:landed" {
		t.Fatalf("unmet dependencies=%v err=%v predecessor=%d", unmet, err, predecessorID)
	}
	output.Reset()
	if code := cli.Run([]string{"show", "t2"}); code != 0 || !strings.Contains(output.String(), "waiting for t1:landed") {
		t.Fatalf("queued Task wait is not inspectable: code=%d output=%s", code, output.String())
	}
}

func TestFailedPrerequisiteDecisionCanDropEdgeAndRunQueuedTask(t *testing.T) {
	fixture := newStartFixture(t)
	ctx := context.Background()
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	predecessorID, err := db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 1, Type: "change", Title: "Predecessor", ShortName: "predecessor", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, predecessorID, store.StateSpawning, store.StateWorking, "cli", "fixture Worker started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, predecessorID, store.StateWorking, store.StateFailed, "worker", "fixture failed"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	brief := `---
type: change
title: Dependent change
done_when: prerequisite is resolved
after: t1:landed
---
Wait for the prerequisite.
`
	if err := os.WriteFile(fixture.brief, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = &bytes.Buffer{}, &bytes.Buffer{}
	if code := cli.Run([]string{"start", "--brief", fixture.brief}); code != 0 {
		t.Fatalf("dependent start exit=%d output=%s", code, cli.Out)
	}
	db, err = store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.refillQueuedTasks(ctx, db, fixture.project, cfg); err != nil {
		t.Fatal(err)
	}
	decisions, err := db.Decisions(ctx, fixture.project.ID, true)
	if err != nil || len(decisions) != 1 || decisions[0].Kind != "dependency" || strings.Join(decisions[0].Options, ",") != "cancel,drop-prerequisite-and-run" {
		t.Fatalf("dependency Decision=%#v err=%v", decisions, err)
	}
	queued, err := db.Task(ctx, fixture.project.ID, "t2")
	if err != nil || queued.State != store.StateQueued || queued.Branch != "" {
		t.Fatalf("queued Task changed before Decision: task=%+v err=%v", queued, err)
	}
	decision, err := db.AnswerDecision(ctx, fixture.project.ID, decisions[0].ID, "drop-prerequisite-and-run", "User approved dropping the failed prerequisite")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	cli = fixture.service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"apply", fmt.Sprint(decision.ID)}); code != 0 {
		t.Fatalf("apply dependency Decision exit=%d output=%s", code, output.String())
	}
	db, err = store.OpenReadOnly(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	updated, err := db.Task(ctx, fixture.project.ID, "t2")
	plan, planErr := updated.AdmittedPlan()
	if err != nil || planErr != nil || updated.State != store.StateWorking || len(plan.Dependencies) != 0 || plan.Revision != 2 {
		t.Fatalf("applied dependency Decision did not revise and dispatch Task: task=%+v plan=%+v err=%v planErr=%v output=%s", updated, plan, err, planErr, output.String())
	}
}

func TestStartRejectsWrongTypedPrerequisiteAtomically(t *testing.T) {
	fixture := newStartFixture(t)
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	predecessorID, err := db.CreateTask(context.Background(), fixture.project.ID, store.Task{Seq: 1, Type: "change", Title: "Predecessor", ShortName: "predecessor", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), predecessorID, store.StateSpawning, store.StateWorking, "cli", "fixture Worker started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	brief := `---
type: research
title: Invalid consumer
done_when: dependency is rejected
after: t1:reported
---
The predecessor is not a Report.
`
	if err := os.WriteFile(fixture.brief, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"start", "--brief", fixture.brief}); code == 0 {
		t.Fatalf("wrong dependency type was admitted: %s", output.String())
	}
	db, err = store.OpenReadOnly(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tasks, err := db.Tasks(context.Background(), fixture.project.ID, true)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("invalid dependency was partially admitted: tasks=%+v err=%v", tasks, err)
	}
}

func TestStartRejectsTaskBoundBranchCollisionBeforeTaskCreation(t *testing.T) {
	fixture := newStartFixture(t)
	gitTest(t, fixture.repo, "branch", "posse/t1")
	output := &bytes.Buffer{}
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"start", "--brief", fixture.brief, "--name", "independent-label"}); code != 1 || !strings.Contains(output.String(), "branch_exists") || !strings.Contains(output.String(), "posse/t1") {
		t.Fatalf("Task-bound branch collision exit=%d output=%s", code, output.String())
	}
	db, err := store.OpenReadOnly(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tasks, err := db.Tasks(context.Background(), fixture.project.ID, true)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("branch collision allocated a Task: %#v, %v", tasks, err)
	}
	sandboxes, err := db.Sandboxes(context.Background(), fixture.project.ID)
	if err != nil || len(sandboxes) != 0 {
		t.Fatalf("branch collision acquired a Sandbox: %#v, %v", sandboxes, err)
	}
}

func TestStartRejectsNoMistakesOnNonDefaultTargetBeforeAllocatingResources(t *testing.T) {
	fixture := newStartFixture(t)
	origin := filepath.Join(filepath.Dir(fixture.repo), "origin.git")
	gitTest(t, filepath.Dir(fixture.repo), "init", "--bare", origin)
	gitTest(t, fixture.repo, "remote", "add", "origin", origin)
	gitTest(t, fixture.repo, "push", "origin", "main")
	gitTest(t, fixture.repo, "switch", "-c", "staging")
	gitTest(t, fixture.repo, "push", "origin", "staging")
	gitTest(t, fixture.repo, "switch", "main")
	installReadyNoMistakes(t)
	brief := "---\ntype: change\ntitle: Launch probe\ndone_when: the Worker acts\nmerge_mode: no-mistakes\ntarget: staging\n---\nCreate a committed file.\n"
	if err := os.WriteFile(fixture.brief, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output := fixture.start(t)
	if code != 1 || !strings.Contains(output, "no_mistakes_target_unsupported") {
		t.Fatalf("start did not refuse unsupported no-mistakes target: code=%d output=%s", code, output)
	}
	if fixture.fake.CallCount("tab.create") != 0 || fixture.fake.CallCount("agent.start") != 0 {
		t.Fatalf("unsupported no-mistakes target allocated Herdr resources: calls=%#v", fixture.fake.Calls)
	}
	db, err := store.OpenReadOnly(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tasks, err := db.Tasks(context.Background(), fixture.project.ID, true)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("unsupported no-mistakes target allocated Tasks: %#v, %v", tasks, err)
	}
	sandboxes, err := db.Sandboxes(context.Background(), fixture.project.ID)
	if err != nil || len(sandboxes) != 0 {
		t.Fatalf("unsupported no-mistakes target acquired Sandboxes: %#v, %v", sandboxes, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.home, "scratch", "shop", "t1")); !os.IsNotExist(err) {
		t.Fatalf("unsupported no-mistakes target allocated Task scratch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.home, "projects", "shop", "tasks", "t1")); !os.IsNotExist(err) {
		t.Fatalf("unsupported no-mistakes target allocated Task files: %v", err)
	}
	if _, err := gitOutput(context.Background(), fixture.repo, "rev-parse", "--verify", "refs/heads/posse/t1"); err == nil {
		t.Fatal("unsupported no-mistakes target allocated a Task branch")
	}
}

func TestStartAdmitsDefaultTargetNoMistakesWithDeprecationWarning(t *testing.T) {
	fixture := newStartFixture(t)
	origin := filepath.Join(filepath.Dir(fixture.repo), "origin.git")
	gitTest(t, filepath.Dir(fixture.repo), "init", "--bare", origin)
	gitTest(t, fixture.repo, "remote", "add", "origin", origin)
	gitTest(t, fixture.repo, "push", "origin", "main")
	installReadyNoMistakes(t)
	brief := "---\ntype: change\ntitle: Launch probe\ndone_when: the Worker acts\nmerge_mode: no-mistakes\n---\nCreate a committed file.\n"
	if err := os.WriteFile(fixture.brief, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output := fixture.start(t)
	if code != 0 || !strings.Contains(output, "deprecated") {
		t.Fatalf("default-target no-mistakes Task was not admitted with warning: code=%d output=%s", code, output)
	}
	db, err := store.OpenReadOnly(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tasks, err := db.Tasks(context.Background(), fixture.project.ID, true)
	if err != nil || len(tasks) != 1 || tasks[0].MergeMode != "no-mistakes" || tasks[0].TargetBranch != "main" || tasks[0].State != store.StateWorking {
		t.Fatalf("default-target Task behavior changed: %#v, %v", tasks, err)
	}
}

func installReadyNoMistakes(t *testing.T) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "no-mistakes"), []byte("#!/bin/sh\necho initialized\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestStartReportsSandboxSetupFailureAndReleasesSandbox(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(filepath.Join(home, "projects", "shop"), 0o700); err != nil {
		t.Fatal(err)
	}
	globalConfig := "[defaults]\nmerge_mode = \"local\"\n\n[profiles.fast]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"fast\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(globalConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "projects", "shop", "config.toml"), []byte("[pool]\nsetup = [\"false\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	briefPath := filepath.Join(root, "brief.md")
	if err := os.WriteFile(briefPath, []byte("---\ntype: change\ntitle: Setup failure\ndone_when: setup passes\n---\nCreate a committed file.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"}}}
	service := testService(home, fake)
	output := &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, output
	t.Chdir(repo)
	if code := cli.Run([]string{"start", "--brief", briefPath}); code != 1 || !strings.Contains(output.String(), "sandbox_setup_failed") {
		t.Fatalf("start setup failure code=%d output=%s", code, output.String())
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil || len(tasks) != 1 || tasks[0].State != store.StateFailed || tasks[0].SandboxID != 0 || tasks[0].ShortName != "setup-failure" || tasks[0].Branch != "posse/t1" {
		t.Fatalf("failed setup Task = %#v, %v", tasks, err)
	}
	output.Reset()
	if code := cli.Run([]string{"start", "--brief", briefPath, "--name", "opaque"}); code != 1 || !strings.Contains(output.String(), "sandbox_setup_failed") {
		t.Fatalf("independent display name exit=%d output=%s", code, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"start", "--brief", briefPath}); code != 1 || !strings.Contains(output.String(), "sandbox_setup_failed") {
		t.Fatalf("repeated title refused: code=%d output=%s", code, output.String())
	}
	if tasks, err := db.Tasks(ctx, project.ID, true); err != nil || len(tasks) != 3 || tasks[0].ShortName != "setup-failure" || tasks[1].ShortName != "opaque" || tasks[2].ShortName != "setup-failure" || tasks[0].Branch != "posse/t1" || tasks[1].Branch != "posse/t2" || tasks[2].Branch != "posse/t3" {
		t.Fatalf("repeated titles did not get independent display names and Task branches: %#v, %v", tasks, err)
	}
	sandboxes, err := db.Sandboxes(ctx, project.ID)
	if err != nil || len(sandboxes) != 1 || sandboxes[0].State != "idle" || sandboxes[0].TaskID != 0 {
		t.Fatalf("Sandbox after setup failure = %#v, %v", sandboxes, err)
	}
}

func TestRelaunchResumesLegacySessionWithFullProfileArgumentsInSameSandbox(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	sandbox := filepath.Join(root, "sandbox")
	gitTest(t, repo, "worktree", "add", "-b", "posse/t1", sandbox, "main")
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(filepath.Join(home, "projects", "shop", "tasks", "t1"), 0o700); err != nil {
		t.Fatal(err)
	}
	configText := "[kinds.claude]\nauto_approve_args = [\"--yes\"]\nmodel_args = [\"--model\", \"{model}\"]\neffort_args = [\"--effort\", \"{effort}\"]\nresume_args = [\"--resume\", \"{session}\"]\n\n[profiles.deep]\nkind = \"claude\"\nmodel = \"opus\"\neffort = \"high\"\nargs = [\"--profile-arg\"]\n\n[profiles.fast]\nkind = \"claude\"\nmodel = \"sonnet\"\neffort = \"low\"\nargs = [\"--fast-arg\"]\n\n[profiles.codex-fast]\nkind = \"codex\"\nmodel = \"gpt-5\"\neffort = \"high\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "projects", "shop", "tasks", "t1", "launch.md"), []byte("worker instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w3", "w3:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project.HerdrWorkspaceID, project.LeadPaneID, project.LeadLabel = "w3", "w3:p1", "posse:shop:lead"
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Resume", ShortName: "worker-tree", Profile: "deep", MergeMode: "local", Branch: "posse/t1", BaseRef: "main", WorktreePath: sandbox, HerdrWorkspaceID: "w2", PaneID: "w2:p1", PaneLabel: "posse:shop:t1", AgentSession: `{"session_id":"session-9"}`})
	if err != nil {
		t.Fatal(err)
	}
	created, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []struct {
		body      string
		createdAt int64
		status    string
	}{
		{body: "before the Brief", createdAt: created.CreatedAt - 1, status: "delivered"},
		{body: "first instruction", createdAt: created.CreatedAt + 1, status: "delivered"},
		{body: "queued instruction", createdAt: created.CreatedAt + 2, status: "queued"},
		{body: "later instruction supersedes the first", createdAt: created.CreatedAt + 3, status: "delivered"},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO messages(task_id,body,created_at,delivered_at,status,wait_for_idle) VALUES(?,?,?,?,?,0)`, taskID, message.body, message.createdAt, message.createdAt, message.status); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateWorking, store.StateLost, "cli", "lost"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, project.ID, sandbox, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, taskID, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	gitDir := strings.TrimSpace(gitTest(t, sandbox, "rev-parse", "--git-dir"))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(sandbox, gitDir)
	}
	indexLock := filepath.Join(gitDir, "index.lock")
	if err := os.WriteFile(indexLock, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	lead := herdr.Pane{PaneID: "w3:p1", WorkspaceID: "w3", TabID: "w3:t0", Label: "posse:shop:lead"}
	fake := herdr.NewFake()
	fake.SnapshotValue.Panes = []herdr.Pane{lead}
	fake.Results["tab.create"] = json.RawMessage(`{"tab":{"tab_id":"w3:t1","workspace_id":"w3"},"root_pane":{"pane_id":"w3:p2","tab_id":"w3:t1"}}`)
	fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	fake.BeforeCall = func(method string) {
		if method == "agent.start" {
			kind := "claude"
			if fake.CallCount("agent.start") > 0 {
				kind = "codex"
			}
			fake.SnapshotValue.Panes = []herdr.Pane{lead, {PaneID: "w3:p2", WorkspaceID: "w3", TabID: "w3:t1", Label: "posse:shop:t1", Agent: kind}}
		}
	}
	service := testService(home, fake)
	t.Chdir(repo)
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out = &output
	if code := cli.Run([]string{"relaunch", "t1", "--profile="}); code != 2 || !strings.Contains(output.String(), "non-empty Profile name") {
		t.Fatalf("relaunch accepted an empty Profile: code=%d output=%s", code, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"relaunch", "t1", "--profile", "missing"}); code != 1 || !strings.Contains(output.String(), "profile_unknown") {
		t.Fatalf("relaunch accepted an unknown Profile: code=%d output=%s", code, output.String())
	}
	output.Reset()
	if code := cli.Run([]string{"relaunch", "t1", "--profile", "fast"}); code != 0 || !strings.Contains(output.String(), "working") {
		t.Fatalf("relaunch failed: code=%d output=%s", code, output.String())
	}
	if _, err := os.Stat(indexLock); !os.IsNotExist(err) {
		t.Fatalf("stale Git index lock was not removed: %v", err)
	}
	skillRoots, err := filepath.Glob(filepath.Join(home, "projects", "shop", "launch-skills", "*"))
	if err != nil || len(skillRoots) != 1 {
		t.Fatalf("skill snapshot roots = %q, %v", skillRoots, err)
	}
	started, typedRelaunch := false, false
	for _, call := range fake.Calls {
		switch call.Method {
		case "agent.start":
			args := call.Params["args"].([]string)
			started = call.Params["kind"] == "claude" && call.Params["pane_id"] == "w3:p2" && len(args) == 14 && equalStrings(args[:12], []string{"--settings", claudeNoAttributionSettings, "--yes", "--model", "sonnet", "--effort", "low", "--fast-arg", "--resume", "session-9", "--plugin-dir", skillRoots[0]}) && args[12] == "--plugin-dir" && args[13] == filepath.Join(home, "projects", "shop", "claude-research") && !strings.Contains(strings.Join(args, " "), "relaunch.md")
		case "agent.prompt":
			text, _ := call.Params["text"].(string)
			typedRelaunch = typedRelaunch || strings.Contains(text, "relaunch.md")
		}
	}
	if !started || !typedRelaunch {
		t.Fatalf("relaunch args or focused typed prompt missing: started=%t typed=%t calls=%#v", started, typedRelaunch, fake.Calls)
	}
	workspaceRenamed, tabCreated, metadataBeforeStart, metadataAfterStart := false, false, false, false
	startedIndex, firstMetadataIndex, lastMetadataIndex := -1, -1, -1
	for index, call := range fake.Calls {
		switch call.Method {
		case "workspace.rename":
			workspaceRenamed = true
		case "tab.create":
			tabCreated = call.Params["label"] == "worker-tree" && call.Params["cwd"] == sandbox && call.Params["workspace_id"] == "w3"
		case "agent.start":
			startedIndex = index
		case "pane.report_metadata":
			if firstMetadataIndex < 0 {
				firstMetadataIndex = index
			}
			lastMetadataIndex = index
			metadataReported := call.Params["clear_display_agent"] == nil && call.Params["display_agent"] == "claude" && strings.Contains(call.Params["title"].(string), sandbox)
			metadataBeforeStart = metadataBeforeStart || (metadataReported && (startedIndex < 0 || index < startedIndex))
			metadataAfterStart = metadataAfterStart || (metadataReported && startedIndex >= 0 && index > startedIndex)
		}
	}
	if workspaceRenamed || !tabCreated || !metadataBeforeStart || !metadataAfterStart || startedIndex < 0 || firstMetadataIndex >= startedIndex || lastMetadataIndex <= startedIndex {
		t.Fatalf("relaunch relabeled a workspace or missed early harness metadata: workspace renamed=%v tab_created=%v metadata_before=%v metadata_after=%v calls=%#v", workspaceRenamed, tabCreated, metadataBeforeStart, metadataAfterStart, fake.Calls)
	}
	relaunchText, err := os.ReadFile(filepath.Join(home, "projects", "shop", "tasks", "t1", "relaunch.md"))
	if err != nil || !strings.Contains(string(relaunchText), "Re-read `launch.md`") || !strings.Contains(string(relaunchText), "posse signal") || !strings.Contains(string(relaunchText), "first instruction") || !strings.Contains(string(relaunchText), "later instruction supersedes the first") {
		t.Fatalf("relaunch instructions were not written: %q, %v", relaunchText, err)
	}
	if strings.Index(string(relaunchText), "first instruction") > strings.Index(string(relaunchText), "later instruction supersedes the first") || strings.Contains(string(relaunchText), "before the Brief") || strings.Contains(string(relaunchText), "queued instruction") || !strings.Contains(string(relaunchText), "Later messages supersede earlier ones") {
		t.Fatalf("relaunch Lead messages were not ordered and filtered correctly: %q", relaunchText)
	}
	observer, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	updated, err := observer.Task(ctx, project.ID, "t1")
	if err != nil || updated.State != store.StateWorking || updated.WorktreePath != sandbox || updated.HerdrWorkspaceID != "w3" || updated.PaneID != "w3:p2" || updated.Profile != "fast" || updated.DispatchRule != "relaunch --profile fast" {
		t.Fatalf("relaunch changed Task Sandbox/state incorrectly: %#v, %v", updated, err)
	}
	transitions, err := observer.TaskTransitions(ctx, taskID, 10)
	profileChangeRecorded := false
	for _, transition := range transitions {
		if transition.Note == "Profile changed from deep to fast" && transition.From == transition.To {
			profileChangeRecorded = true
		}
	}
	if err != nil || !profileChangeRecorded {
		t.Fatalf("Profile change history = %#v, %v", transitions, err)
	}
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex-home"))
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		t.Fatal(err)
	}
	codexRelaunchStart := len(fake.Calls)
	if _, err := service.relaunchTask(ctx, observer, home, project, cfg, updated, "codex-fast"); err != nil {
		t.Fatalf("relaunch with a different harness failed: %v", err)
	}
	freshStart := false
	for _, call := range fake.Calls {
		if call.Method == "agent.start" && call.Params["kind"] == "codex" {
			args := call.Params["args"].([]string)
			freshStart = len(args) == 7 && equalStrings(args[:5], []string{"--dangerously-bypass-approvals-and-sandbox", "-m", "gpt-5", "-c", "model_reasoning_effort=high"}) && args[5] == "-c" && strings.HasPrefix(args[6], "developer_instructions=") && strings.Contains(args[6], filepath.Join(skillRoots[0], "skills", "posse-setup", "SKILL.md"))
		}
	}
	deliveredRelaunch := false
	for _, call := range fake.Calls[codexRelaunchStart:] {
		if call.Method == "agent.prompt" && strings.Contains(fmt.Sprint(call.Params["text"]), "relaunch.md") {
			deliveredRelaunch = true
		}
	}
	if !freshStart || !deliveredRelaunch {
		t.Fatalf("kind-changing relaunch resumed the old session, omitted Profile args, or failed to deliver its prompt: started=%t delivered=%t calls=%#v", freshStart, deliveredRelaunch, fake.Calls)
	}
	codexMetadataIndex, codexStartIndex := -1, -1
	for index, call := range fake.Calls[codexRelaunchStart:] {
		if call.Method == "pane.report_metadata" && call.Params["display_agent"] == "codex" && codexMetadataIndex < 0 {
			codexMetadataIndex = codexRelaunchStart + index
		}
		if call.Method == "agent.start" && call.Params["kind"] == "codex" {
			codexStartIndex = codexRelaunchStart + index
		}
	}
	if codexMetadataIndex < 0 || codexStartIndex < 0 || codexMetadataIndex >= codexStartIndex {
		t.Fatalf("kind-changing relaunch did not set the harness subtitle before agent.start: %#v", fake.Calls)
	}
	codexConfig, err := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"))
	if err != nil || !strings.Contains(string(codexConfig), `trust_level = "trusted"`) || !strings.Contains(string(codexConfig), sandbox) {
		t.Fatalf("Codex Sandbox preparation was not run: %q, %v", codexConfig, err)
	}
	updated, err = observer.Task(ctx, project.ID, "t1")
	if err != nil || updated.Profile != "codex-fast" || updated.DispatchRule != "relaunch --profile codex-fast" {
		t.Fatalf("kind-changing relaunch did not update the Task Profile: %#v, %v", updated, err)
	}
	transitions, err = observer.TaskTransitions(ctx, taskID, 10)
	foundProfileChanges := map[string]bool{}
	for _, transition := range transitions {
		foundProfileChanges[transition.Note] = true
	}
	if err != nil || !foundProfileChanges["Profile changed from deep to fast"] || !foundProfileChanges["Profile changed from fast to codex-fast"] {
		t.Fatalf("Profile history lost a change: %#v, %v", transitions, err)
	}
	if err := observer.StartIntent(ctx, project.ID, taskID, "relaunch", "done:agent.start", `{}`, deadIntentProcessID); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(home, project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.reconcileIntents(ctx, observer, project, cfg, fake.SnapshotValue); err != nil {
		t.Fatalf("interrupted relaunch did not recover: %v", err)
	}
	updated, err = observer.Task(ctx, project.ID, "t1")
	if err != nil || updated.State != store.StateWorking || updated.WorktreePath != sandbox || updated.Launches != 3 {
		t.Fatalf("recovered relaunch changed Task Sandbox/state incorrectly: %#v, %v", updated, err)
	}
	launchIdentities, err := observer.TaskLaunchIdentities(ctx, taskID)
	wantLaunchIdentities := []store.TaskLaunchIdentity{
		{TaskID: taskID, LaunchNumber: 1, Profile: "fast", ConfiguredModel: "sonnet", ModelKnown: false},
		{TaskID: taskID, LaunchNumber: 2, Profile: "codex-fast", ConfiguredModel: "gpt-5", ModelKnown: true},
		{TaskID: taskID, LaunchNumber: 3, Profile: "codex-fast", ConfiguredModel: "gpt-5", ModelKnown: true},
	}
	if err != nil || !reflect.DeepEqual(launchIdentities, wantLaunchIdentities) {
		t.Fatalf("relaunch identity history = %#v, %v; want %#v", launchIdentities, err, wantLaunchIdentities)
	}
	if _, err := observer.IntentByTask(ctx, taskID); !store.IsNotFound(err) {
		t.Fatalf("relaunch intent remains after recovery: %v", err)
	}
}

func TestRelaunchInstructionsBoundDeliveredLeadMessages(t *testing.T) {
	messages := make([]store.Message, maxRelaunchMessages+2)
	for index := range messages {
		messages[index] = store.Message{Body: fmt.Sprintf("message-%d: %s", index, strings.Repeat("x", maxRelaunchMessageBytes+20))}
	}
	instructions := string(relaunchInstructions("", messages))
	if len(instructions) > maxRelaunchMessages*(maxRelaunchMessageBytes+64)+2048 {
		t.Fatalf("relaunch prompt exceeded its message bound: %d bytes", len(instructions))
	}
	if !strings.Contains(instructions, "Earlier delivered Lead messages are omitted") || !strings.Contains(instructions, "message-2") || !strings.Contains(instructions, "message-33") || strings.Contains(instructions, "message-0") || !strings.Contains(instructions, "[truncated]") {
		t.Fatalf("relaunch prompt did not apply message bounds: %q", instructions)
	}
}

func TestRelaunchReportsInProgressGitStateWithoutChangingIt(t *testing.T) {
	for _, operation := range []string{"merge", "rebase", "git-process"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			initRepo(t, repo)
			sandboxPath := filepath.Join(root, "sandbox")
			gitTest(t, repo, "worktree", "add", "-b", "posse/t1", sandboxPath, "main")
			gitDirText := strings.TrimSpace(gitTest(t, sandboxPath, "rev-parse", "--git-dir"))
			gitDir := gitDirText
			if !filepath.IsAbs(gitDir) {
				gitDir = filepath.Join(sandboxPath, gitDir)
			}
			var marker string
			var process *exec.Cmd
			switch operation {
			case "merge":
				marker = filepath.Join(gitDir, "MERGE_HEAD")
				if err := os.WriteFile(marker, []byte(strings.TrimSpace(gitTest(t, repo, "rev-parse", "main"))+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "rebase":
				marker = filepath.Join(gitDir, "rebase-merge")
				if err := os.Mkdir(marker, 0o700); err != nil {
					t.Fatal(err)
				}
			case "git-process":
				marker = filepath.Join(gitDir, "index.lock")
				if err := os.WriteFile(marker, []byte("locked"), 0o600); err != nil {
					t.Fatal(err)
				}
				process = exec.Command("bash", "-c", "exec -a git sleep 30")
				process.Dir = sandboxPath
				if err := process.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
				deadline := time.Now().Add(2 * time.Second)
				for !processInSandbox(process.Process.Pid, sandboxPath) && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
			}
			warning, err := inspectSandboxGitState(context.Background(), sandboxPath)
			if err != nil {
				t.Fatalf("relaunch refused an in-progress Git operation: %v", err)
			}
			if warning == "" || !strings.Contains(string(relaunchInstructions(warning)), warning) {
				t.Fatalf("relaunch report omitted the in-progress Git state warning: %q", warning)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("relaunch changed in-progress Git state %s: %v", marker, err)
			}
		})
	}
}

type relaunchFixture struct {
	db      *store.DB
	fake    *herdr.Fake
	service *Service
	home    string
	project store.Project
	cfg     config.Config
	task    store.Task
}

func newRelaunchFixture(t *testing.T, initial store.State) relaunchFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	sandboxPath := filepath.Join(root, "sandbox")
	gitTest(t, repo, "worktree", "add", "-b", "posse/t1", sandboxPath, "main")
	home := filepath.Join(root, "posse")
	taskHome := filepath.Join(home, "projects", "shop", "tasks", "t1")
	if err := os.MkdirAll(taskHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskHome, "launch.md"), []byte("# Worker protocol\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configText := "[kinds.claude]\n\n[profiles.deep]\nkind = \"claude\"\nmodel = \"sonnet\"\n\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Resume attention state", Profile: "deep", MergeMode: "local", Branch: "posse/t1", BaseRef: "main", WorktreePath: sandboxPath, HerdrWorkspaceID: "w1", PaneID: "w1:p1", PaneLabel: "posse:shop:t1", AgentName: "posse-shop-t1-1"})
	if err == nil {
		err = db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started")
	}
	if err == nil {
		_, err = db.ExecContext(ctx, `UPDATE tasks SET state=? WHERE id=?`, string(initial), taskID)
	}
	if err == nil {
		_, err = db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, project.ID, sandboxPath, taskID)
	}
	if err == nil {
		_, err = db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, taskID, taskID)
	}
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "working"}}}
	fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	service := testService(home, fake)
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	return relaunchFixture{db: db, fake: fake, service: service, home: home, project: project, cfg: cfg, task: task}
}

func TestRelaunchDoneChangeSuggestsSend(t *testing.T) {
	fixture := newRelaunchFixture(t, store.StateDone)
	t.Chdir(filepath.Join(filepath.Dir(fixture.home), "repo"))
	var output bytes.Buffer
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"relaunch", "t1"}); code != 1 || !strings.Contains(output.String(), "relaunch_refused") || !strings.Contains(output.String(), "posse send t1 <message>") {
		t.Fatalf("done Change relaunch did not refuse with a send step: code=%d output=%s", code, output.String())
	}
	if len(fixture.fake.Calls) != 0 {
		t.Fatalf("done Change relaunch contacted Herdr before refusing: %#v", fixture.fake.Calls)
	}
}

func TestCodexWorkerSkillInjectionPreservesDeveloperInstructions(t *testing.T) {
	const (
		globalRule  = "Fixture safety rule: never delete user data without explicit consent."
		profileRule = "Profile safety rule: preserve existing User changes."
	)

	t.Run("spawn", func(t *testing.T) {
		f := newFirstOutcomeFixture(t)
		setupFollowupAgentPath(t, "pi", "codex")
		codexHome := filepath.Join(f.root, "codex-home")
		writeCodexInstructionsFixture(t, codexHome, globalRule)
		writeCodexWorkerProfileFixture(t, f.home, "worker", profileRule, true, false)
		t.Setenv("CODEX_HOME", codexHome)
		f.fake.BeforeCall = func(method string) {
			if method == "agent.start" {
				f.fake.SnapshotValue.Panes = append(f.fake.SnapshotValue.Panes, herdr.Pane{PaneID: "fake:child:p1", WorkspaceID: "fake:child", Label: "posse:shop:t1", Agent: "codex", AgentStatus: "working"})
			}
		}
		t.Setenv("HERDR_ENV", "1")
		t.Setenv("HERDR_PANE_ID", "w1:p1")
		t.Setenv("HERDR_WORKSPACE_ID", "w1")
		outcomeCLI(t, f.service, 0, "start", "--brief", f.brief, "--name", "first-outcome")
		assertCodexWorkerDeveloperInstructions(t, codexStartArgs(t, f.fake), globalRule, profileRule)
	})

	t.Run("relaunch", func(t *testing.T) {
		f := newRelaunchFixture(t, store.StateLost)
		codexHome := filepath.Join(filepath.Dir(f.home), "codex-home")
		writeCodexInstructionsFixture(t, codexHome, globalRule)
		writeCodexWorkerProfileFixture(t, f.home, "deep", profileRule, false, false)
		t.Setenv("CODEX_HOME", codexHome)
		cfg, err := config.Load(f.home, f.project.Name)
		if err != nil {
			t.Fatal(err)
		}
		task, err := f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.relaunchTask(context.Background(), f.db, f.home, f.project, cfg, task, ""); err != nil {
			t.Fatalf("relaunch Codex Worker: %v", err)
		}
		assertCodexWorkerDeveloperInstructions(t, codexStartArgs(t, f.fake), globalRule, profileRule)
	})
}

func writeCodexInstructionsFixture(t *testing.T, codexHome, instructions string) {
	t.Helper()
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("developer_instructions = "+mustJSONString(t, instructions)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeCodexWorkerProfileFixture(t *testing.T, home, name, instructions string, dispatchDefault, raw bool) {
	t.Helper()
	encodedInstructions := mustJSONString(t, instructions)
	if raw {
		encodedInstructions = instructions
	}
	profileArgs, err := json.Marshal([]string{"-c", "developer_instructions=" + encodedInstructions})
	if err != nil {
		t.Fatal(err)
	}
	configText := "[defaults]\nmerge_mode = \"local\"\n\n[profiles." + name + "]\nkind = \"codex\"\nmodel = \"gpt-5\"\neffort = \"high\"\nargs = " + string(profileArgs) + "\n"
	if dispatchDefault {
		configText += "\n[dispatch.default]\nuse = \"" + name + "\"\n"
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
}

func codexStartArgs(t *testing.T, fake *herdr.Fake) []string {
	t.Helper()
	for index := len(fake.Calls) - 1; index >= 0; index-- {
		call := fake.Calls[index]
		if call.Method == "agent.start" && call.Params["kind"] == "codex" {
			args, ok := call.Params["args"].([]string)
			if !ok {
				t.Fatalf("Codex launch args have type %T", call.Params["args"])
			}
			return args
		}
	}
	t.Fatal("no Codex Worker launch was recorded")
	return nil
}

func containsArgPair(args []string, flag, value string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == flag && args[index+1] == value {
			return true
		}
	}
	return false
}

func assertCodexWorkerDeveloperInstructions(t *testing.T, args []string, globalRule, profileRule string) {
	t.Helper()
	var developerInstructions []string
	for index := 0; index+1 < len(args); index++ {
		if args[index] != "-c" {
			continue
		}
		value, found := strings.CutPrefix(args[index+1], "developer_instructions=")
		if !found {
			continue
		}
		var instruction string
		if err := json.Unmarshal([]byte(value), &instruction); err != nil {
			t.Fatalf("Codex developer_instructions is not a quoted string: %q: %v", args[index+1], err)
		}
		developerInstructions = append(developerInstructions, instruction)
	}
	if len(developerInstructions) != 1 {
		t.Fatalf("Codex launch has %d developer_instructions overrides, want one merged value: %#v", len(developerInstructions), args)
	}
	if !containsArgPair(args, "-m", "gpt-5") || !containsArgPair(args, "-c", "model_reasoning_effort=high") {
		t.Fatalf("Codex launch lost non-developer config overrides: %#v", args)
	}
	for _, required := range []string{globalRule, profileRule, "launch-skills", filepath.Join("skills", "posse", "SKILL.md"), filepath.Join("skills", "posse-setup", "SKILL.md"), filepath.Join("skills", "posse-down", "SKILL.md"), filepath.Join("skills", "posse-update", "SKILL.md")} {
		if !strings.Contains(developerInstructions[0], required) {
			t.Fatalf("Codex developer_instructions omitted %q: %s", required, developerInstructions[0])
		}
	}
}

func TestCodexWorkerSkillInjectionPreservesRawDeveloperInstructions(t *testing.T) {
	const (
		globalRule  = "Fixture safety rule: never delete user data without explicit consent."
		profileRule = "Profile safety rule: preserve existing User changes."
	)

	t.Run("spawn", func(t *testing.T) {
		f := newFirstOutcomeFixture(t)
		setupFollowupAgentPath(t, "pi", "codex")
		codexHome := filepath.Join(f.root, "codex-home")
		writeCodexInstructionsFixture(t, codexHome, globalRule)
		writeCodexWorkerProfileFixture(t, f.home, "worker", profileRule, true, true)
		t.Setenv("CODEX_HOME", codexHome)
		f.fake.BeforeCall = func(method string) {
			if method == "agent.start" {
				f.fake.SnapshotValue.Panes = append(f.fake.SnapshotValue.Panes, herdr.Pane{PaneID: "fake:child:p1", WorkspaceID: "fake:child", Label: "posse:shop:t1", Agent: "codex", AgentStatus: "working"})
			}
		}
		t.Setenv("HERDR_ENV", "1")
		t.Setenv("HERDR_PANE_ID", "w1:p1")
		t.Setenv("HERDR_WORKSPACE_ID", "w1")
		outcomeCLI(t, f.service, 0, "start", "--brief", f.brief, "--name", "first-outcome")
		assertCodexWorkerDeveloperInstructions(t, codexStartArgs(t, f.fake), globalRule, profileRule)
	})

	t.Run("relaunch", func(t *testing.T) {
		f := newRelaunchFixture(t, store.StateLost)
		codexHome := filepath.Join(filepath.Dir(f.home), "codex-home")
		writeCodexInstructionsFixture(t, codexHome, globalRule)
		writeCodexWorkerProfileFixture(t, f.home, "deep", profileRule, false, true)
		t.Setenv("CODEX_HOME", codexHome)
		cfg, err := config.Load(f.home, f.project.Name)
		if err != nil {
			t.Fatal(err)
		}
		task, err := f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.relaunchTask(context.Background(), f.db, f.home, f.project, cfg, task, ""); err != nil {
			t.Fatalf("relaunch Codex Worker with raw developer instructions: %v", err)
		}
		assertCodexWorkerDeveloperInstructions(t, codexStartArgs(t, f.fake), globalRule, profileRule)
	})
}

func TestRelaunchRecoversNeedsDecisionAndBlockedWorkers(t *testing.T) {
	for _, initial := range []store.State{store.StateNeedsDecision, store.StateBlocked} {
		t.Run(string(initial), func(t *testing.T) {
			ctx := context.Background()
			fixture := newRelaunchFixture(t, initial)
			if _, err := fixture.service.relaunchTask(ctx, fixture.db, fixture.home, fixture.project, fixture.cfg, fixture.task, ""); err != nil {
				t.Fatalf("relaunch %s Worker: %v", initial, err)
			}
			updated, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
			if err != nil || updated.State != store.StateWorking || updated.SandboxID == 0 || updated.Launches != 1 {
				t.Fatalf("relaunch lost Task state or Sandbox: %#v, %v", updated, err)
			}
		})
	}
}

func TestRelaunchRecoversTaskMarkedLostWhileRelaunching(t *testing.T) {
	ctx := context.Background()
	fixture := newRelaunchFixture(t, store.StateWorking)
	marked := false
	fixture.fake.BeforeCall = func(method string) {
		if method != "agent.start" || marked {
			return
		}
		marked = true
		if err := fixture.db.Transition(ctx, fixture.task.ID, store.StateWorking, store.StateLost, "cli", "Worker pane and label are absent from the Herdr snapshot"); err != nil {
			t.Errorf("mark Task lost during relaunch: %v", err)
		}
	}
	if _, err := fixture.service.relaunchTask(ctx, fixture.db, fixture.home, fixture.project, fixture.cfg, fixture.task, ""); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	updated, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil || !marked || updated.State != store.StateWorking {
		t.Fatalf("relaunch left a Task marked lost mid-relaunch in %q (marked=%v, err=%v)", updated.State, marked, err)
	}
}

func TestTaskSequenceDoesNotDependOnLegacyBranch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.test"}} {
		if _, err := gitOutput(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "base"}, {"checkout", "-b", "posse/legacy-work"}} {
		if _, err := gitOutput(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "unmerged.txt"), []byte("keep this commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, repo, "add", "unmerged.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, repo, "commit", "-m", "unmerged task work"); err != nil {
		t.Fatal(err)
	}
	wantCommit, err := gitOutput(ctx, repo, "rev-parse", "refs/heads/posse/legacy-work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, repo, "checkout", "main"); err != nil {
		t.Fatal(err)
	}

	home := filepath.Join(root, ".posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	id, sequence, err := createTaskWithSequence(ctx, db, project, home, store.Task{Type: "change", Title: "New work", ShortName: "new-work", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if sequence != 1 {
		t.Fatalf("allocated Task sequence = %d, want 1", sequence)
	}
	task, err := db.TaskByID(ctx, project.ID, id)
	if err != nil || task.Seq != 1 || task.Branch != "posse/t1" {
		t.Fatalf("new Task = %#v, %v", task, err)
	}
	gotCommit, err := gitOutput(ctx, repo, "rev-parse", "refs/heads/posse/legacy-work")
	if err != nil || gotCommit != wantCommit {
		t.Fatalf("legacy branch moved from %s to %s: %v", wantCommit, gotCommit, err)
	}
}

func TestTaskBranchAvailabilityIgnoresRedirectWarning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/group/repo/info/refs" {
			http.Redirect(w, r, "/group/repo.git/info/refs?service=git-upload-pack", http.StatusMovedPermanently)
			return
		}
		if r.URL.Path == "/group/repo.git/info/refs" {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_, _ = w.Write([]byte("001e# service=git-upload-pack\n00000000"))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	gitTest(t, repo, "remote", "add", "origin", server.URL+"/group/repo")
	db, err := store.Open(filepath.Join(root, "posse"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}

	output, err := commandOutputArgs(ctx, "", "git", "-C", repo, "ls-remote", "--heads", "origin", "refs/heads/posse/fresh-name")
	if err != nil || !strings.Contains(output, "warning: redirecting to") {
		t.Fatalf("ls-remote did not reproduce redirect warning: output=%q err=%v", output, err)
	}
	stdout, err := gitOutput(ctx, repo, "ls-remote", "--heads", "origin", "refs/heads/posse/fresh-name")
	if err != nil || stdout != "" {
		t.Fatalf("git output includes redirect warning: stdout=%q err=%v", stdout, err)
	}
	if err := taskBranchAvailableForRef(ctx, db, project, "posse/fresh-name", []repoTarget{{Name: project.Name, Root: project.Root}}); err != nil {
		t.Fatalf("fresh branch rejected because of redirect warning: %v", err)
	}
}

func TestTaskBranchAvailabilityReportsUnreachableOriginAsCheckFailure(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	remote := server.URL + "/group/repo"
	server.Close()

	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	gitTest(t, repo, "remote", "add", "origin", remote)
	db, err := store.Open(filepath.Join(root, "posse"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}

	err = taskBranchAvailableForRef(ctx, db, project, "posse/fresh-name", []repoTarget{{Name: project.Name, Root: project.Root}})
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "branch_check_failed" || !strings.Contains(failure.Message, "shop") || !strings.Contains(failure.Message, "refs/heads/posse/fresh-name") {
		t.Fatalf("unreachable origin result = %#v; want branch_check_failed for repository and ref", err)
	}
}

func TestTaskBranchAvailabilityAllowsWorkspaceMemberWithoutOrigin(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	workspace := workspaceFixture(t, root)
	detected, err := detectProject(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	members := make([]store.ProjectRepo, 0, len(detected.Repos))
	for _, member := range detected.Repos {
		members = append(members, store.ProjectRepo{Name: member.Name, Path: member.Path, DefaultBranch: member.DefaultBranch, Status: store.RepoActive, OriginHost: savedOriginHost(member.Remote)})
	}
	db, err := store.Open(filepath.Join(root, "posse"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateWorkspaceProject(ctx, detected.Name, workspace, members)
	if err != nil {
		t.Fatal(err)
	}
	selected := []taskMember{{repoTarget: repoTarget{Name: "e2e-tool", Root: filepath.Join(workspace, "e2e-tool"), DefaultBranch: "main"}, Path: "e2e-tool"}}
	if err := taskBranchAvailableForRef(ctx, db, project, "posse/fresh-name", []repoTarget{selected[0].repoTarget}); err != nil {
		t.Fatalf("fresh branch rejected for workspace member without origin: %v", err)
	}
}

func TestWorkspaceStartChecksOnlySelectedMembers(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		brief     dispatch.Brief
		wantError bool
	}{
		{name: "unrequested collision", brief: dispatch.Brief{Type: "change", Repos: []string{"worker"}}},
		{name: "unrequested unreachable origin", brief: dispatch.Brief{Type: "change", Repos: []string{"worker"}}},
		{name: "research without editable members", brief: dispatch.Brief{Type: "research"}},
		{name: "selected collision", brief: dispatch.Brief{Type: "change", Repos: []string{"backend"}}, wantError: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"worker"}, []string{"worker"})
			backendRoot := filepath.Join(f.workspace, "backend")
			if testCase.name == "unrequested unreachable origin" {
				gitTest(t, backendRoot, "remote", "set-url", "origin", "https://127.0.0.1:1/acme/backend.git")
			} else {
				gitTest(t, backendRoot, "branch", store.TaskBranchName(2))
			}
			members, _, err := f.service.planTaskMembers(context.Background(), f.db, f.project, f.config(), testCase.brief, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = taskBranchAvailableForBranch(context.Background(), f.db, f.project, store.TaskBranchName(2), members)
			if (err != nil) != testCase.wantError {
				t.Fatalf("selected scope branch check err=%v, want error %v", err, testCase.wantError)
			}
		})
	}
}

func TestTaskBranchAvailabilityChecksLocalAndOrigin(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	db, err := store.Open(filepath.Join(root, "posse"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "branch", "posse/used-name")
	if err := taskBranchAvailableForRef(ctx, db, project, "posse/used-name", []repoTarget{{Name: project.Name, Root: project.Root}}); err == nil || !strings.Contains(err.Error(), "shop") || !strings.Contains(err.Error(), "refs/heads/posse/used-name") {
		t.Fatalf("local branch collision = %v", err)
	}
	remote := filepath.Join(root, "origin.git")
	gitTest(t, root, "init", "--bare", remote)
	gitTest(t, repo, "remote", "add", "origin", remote)
	gitTest(t, repo, "push", "origin", "refs/heads/main:refs/heads/posse/remote-name")
	gitTest(t, repo, "update-ref", "-d", "refs/remotes/origin/posse/remote-name")
	if err := taskBranchAvailableForRef(ctx, db, project, "posse/remote-name", []repoTarget{{Name: project.Name, Root: project.Root}}); err == nil || !strings.Contains(err.Error(), "shop origin") || !strings.Contains(err.Error(), "refs/heads/posse/remote-name") {
		t.Fatalf("origin branch collision = %v", err)
	}
}

func TestFailSpawnDeliversTaskFailedNoticeToIdleLead(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Broken spawn", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: project.LeadLabel, Agent: "claude", AgentStatus: "idle"}}}
	service := testService(home, fake)
	if err := service.failSpawn(ctx, db, project, taskID, "Broken spawn", "Sandbox setup failed"); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("agent.prompt") != 1 {
		t.Fatalf("task_failed Notice was not delivered after spawn failure: %#v", fake.Calls)
	}
	task, err := db.Task(ctx, project.ID, "t1")
	if err != nil || task.State != store.StateFailed {
		t.Fatalf("spawn Task state = %#v, %v", task, err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "task_failed" || notices[0].DeliveredAt == 0 {
		t.Fatalf("spawn failure Notice = %#v, %v", notices, err)
	}
}

func TestDoneChangeTaskKeepsWorkerSlotAgainstQueuedRefill(t *testing.T) {
	fixture := newStartFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte("[defaults]\nmax_workers = 1\nmerge_mode = \"local\"\n\n[profiles.claude]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"claude\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	changeID, err := db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 1, Type: "change", Title: "Current Worker", ShortName: "current-worker", State: store.StateSpawning})
	if err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	cli := fixture.service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"start", "--brief", fixture.brief, "--name", "refill-probe", "--queue"}); code != 0 {
		t.Fatalf("queue admission exit=%d output=%s", code, output.String())
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.ProjectByID(ctx, fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	service := testService(fixture.home, fixture.fake)
	for _, step := range []struct {
		from, to store.State
		source   string
	}{
		{store.StateSpawning, store.StateWorking, "cli"},
		{store.StateWorking, store.StateDone, "worker"},
	} {
		if err := db.Transition(ctx, changeID, step.from, step.to, step.source, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	// The done Change Task keeps its Worker, so the refill must not reuse its slot.
	if err := service.retryPendingLaunches(ctx, db, project, cfg, fixture.fake.SnapshotValue); err != nil {
		t.Fatalf("refill: %v", err)
	}
	if queued, err := db.Task(ctx, fixture.project.ID, "t2"); err != nil || queued.State != store.StateQueued {
		t.Fatalf("refill admitted a Task into a done Worker's slot: task=%+v err=%v", queued, err)
	}
	// A follow-up reopens the done Task without exceeding max_workers.
	messageID, err := db.QueueMessage(ctx, changeID, "fix CI", false)
	if err != nil {
		t.Fatal(err)
	}
	token := "fixture-claim"
	if _, err := db.ExecContext(ctx, `UPDATE messages SET status='claimed',claim_token=?,claimed_at=1 WHERE id=?`, token, messageID); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkMessageSubmitting(ctx, messageID, token); err != nil {
		t.Fatal(err)
	}
	if got, err := db.ActiveWorkerCount(ctx, fixture.project.ID); err != nil || got != 1 {
		t.Fatalf("follow-up exceeded max_workers: active=%d err=%v", got, err)
	}
	// Releasing the Worker frees the slot for the queued Task.
	if err := db.Transition(ctx, changeID, store.StateWorking, store.StateFailed, "worker", "fixture Worker exited"); err != nil {
		t.Fatal(err)
	}
	if err := service.retryPendingLaunches(ctx, db, project, cfg, fixture.fake.SnapshotValue); err != nil {
		t.Fatalf("refill after teardown: %v", err)
	}
	if started, err := db.Task(ctx, fixture.project.ID, "t2"); err != nil || started.State != store.StateWorking {
		t.Fatalf("queued Task did not start after the Worker exited: task=%+v err=%v", started, err)
	}
}
