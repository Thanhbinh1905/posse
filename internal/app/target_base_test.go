package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// stagingAhead publishes a staging branch one commit ahead of main and returns its tip.
func stagingAhead(t *testing.T, repo string) string {
	t.Helper()
	gitTest(t, repo, "checkout", "-b", "staging")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("feature only on staging\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", "feature.txt")
	gitTest(t, repo, "commit", "-m", "staging feature")
	gitTest(t, repo, "push", "origin", "staging")
	gitTest(t, repo, "checkout", "main")
	return strings.TrimSpace(gitTest(t, repo, "rev-parse", "staging"))
}

func addOrigin(t *testing.T, repo string) {
	t.Helper()
	remote := filepath.Join(filepath.Dir(repo), "origin.git")
	gitTest(t, filepath.Dir(repo), "init", "--bare", "-b", "main", remote)
	gitTest(t, repo, "remote", "add", "origin", remote)
	gitTest(t, repo, "push", "-u", "origin", "main")
}

func TestRepositoryTaskWithOnlyTargetStartsFromFrozenTarget(t *testing.T) {
	f := newStartFixture(t)
	addOrigin(t, f.repo)
	staging := stagingAhead(t, f.repo)
	if err := os.WriteFile(f.brief, []byte("---\ntype: change\ntitle: Hotfix\ndone_when: patched\ntarget: staging\n---\nPatch staging.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output := f.start(t)
	if code != 0 {
		t.Fatalf("start: %d %s", code, output)
	}
	task, _ := f.onlyTask(t)
	if head := strings.TrimSpace(gitTest(t, task.WorktreePath, "rev-parse", "HEAD")); head != staging {
		t.Fatalf("Task started at %s, want staging tip %s", head, staging)
	}
	if !strings.Contains(output, "shop,staging,"+staging+",staging") || strings.Contains(output, "differs") {
		t.Fatalf("start did not print the resolved base and target:\n%s", output)
	}
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil || plan.Execution.BaseCommit != staging || task.BaseRef != staging {
		t.Fatalf("target base not frozen: base_ref=%s plan=%#v err=%v", task.BaseRef, plan, err)
	}
	if err := os.WriteFile(filepath.Join(task.WorktreePath, "fix.txt"), []byte("fix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, task.WorktreePath, "add", "fix.txt")
	gitTest(t, task.WorktreePath, "commit", "-m", "hotfix")
	code, diff := runCLI(t, f.service, "diff", taskIDString(task.Seq), "--stat")
	if code != 0 || !strings.Contains(diff, "fix.txt") || strings.Contains(diff, "feature.txt") {
		t.Fatalf("diff is not scoped to the target base: %d\n%s", code, diff)
	}
}

func TestLandedPrerequisiteAllowsExplicitExistingFeatureBase(t *testing.T) {
	f := newStartFixture(t)
	gitTest(t, f.repo, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(f.repo, "feature.txt"), []byte("prerequisite landed\\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.repo, "add", "feature.txt")
	gitTest(t, f.repo, "commit", "-m", "land prerequisite on feature")
	featureTip := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "feature"))
	gitTest(t, f.repo, "checkout", "main")
	ctx := context.Background()
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	predecessorID, err := db.CreateTask(ctx, f.project.ID, store.Task{Seq: 1, Type: "change", Title: "Prerequisite", ShortName: "prerequisite", MergeMode: "local", Branch: "posse/t1", BaseRef: "main", TargetBranch: "feature"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, predecessorID, store.StateSpawning, store.StateWorking, "cli", "fixture Worker started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, predecessorID, store.StateWorking, store.StateDone, "worker", "fixture Worker completed"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, predecessorID, store.StateDone, store.StateMerged, "cli", "fixture prerequisite landed"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	brief := "---\ntype: change\ntitle: Follow-up\ndone_when: follow-up is ready\nbase: feature\ntarget: feature\nafter: t1:landed\n---\nBuild on the landed feature.\n"
	if err := os.WriteFile(f.brief, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output := f.start(t)
	if code != 0 {
		t.Fatalf("start with a landed feature prerequisite: %d %s", code, output)
	}
	db, err = store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.Task(ctx, f.project.ID, "t2")
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(gitTest(t, task.WorktreePath, "rev-parse", "HEAD")); got != featureTip {
		t.Fatalf("follow-up started at %s, want feature tip", got)
	}
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil || len(plan.Dependencies) != 1 || plan.Dependencies[0].TaskID != "t1" || plan.Execution.Base != "refs/heads/feature" {
		t.Fatalf("landed prerequisite was not retained with the explicit base: plan=%#v err=%v", plan, err)
	}
}

func TestQueuedFeatureBaseAndPrerequisiteSurviveStoreRestart(t *testing.T) {
	f := newStartFixture(t)
	if err := os.WriteFile(filepath.Join(f.repo, "feature.txt"), []byte("prerequisite landed\\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.repo, "add", "feature.txt")
	gitTest(t, f.repo, "commit", "-m", "land prerequisite on feature")
	featureCommit := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "HEAD"))
	addOrigin(t, f.repo)
	ctx := context.Background()
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	predecessorID, err := db.CreateTask(ctx, f.project.ID, store.Task{Seq: 1, Type: "change", Title: "Prerequisite", ShortName: "prerequisite", MergeMode: "local", TargetBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, predecessorID, store.StateSpawning, store.StateWorking, "cli", "fixture Worker started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	brief := "---\ntype: change\ntitle: Follow-up\ndone_when: follow-up is ready\nafter: t1:landed\n---\nBuild on the landed feature.\n"
	if err := os.WriteFile(f.brief, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output := runCLI(t, f.service, "start", "--brief", f.brief, "--name", "follow-up")
	if code != 0 {
		t.Fatalf("queue feature-base Task: %d %s", code, output)
	}
	checkPlan := func() {
		t.Helper()
		db, err := store.Open(f.home)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		task, err := db.Task(ctx, f.project.ID, "t2")
		if err != nil {
			t.Fatal(err)
		}
		plan, err := task.AdmittedPlan()
		if err != nil || plan == nil || task.State != store.StateQueued || task.Branch != "" || task.WorktreePath != "" || task.SandboxID != 0 || plan.Execution.BaseCommit != "" || len(plan.Dependencies) != 1 || plan.Dependencies[0].TaskID != "t1" {
			t.Fatalf("queued plan lost its feature base or prerequisite: task=%+v plan=%#v err=%v", task, plan, err)
		}
	}
	checkPlan()
	if f.fake.CallCount("tab.create") != 0 || f.fake.CallCount("agent.start") != 0 {
		t.Fatalf("queued dependent acquired Worker resources before its prerequisite landed: %#v", f.fake.Calls)
	}
	db, err = store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := db.Task(ctx, f.project.ID, "t2")
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		from, to store.State
		source   string
	}{{store.StateWorking, store.StateDone, "worker"}, {store.StateDone, store.StateMerged, "cli"}} {
		if err := db.Transition(ctx, predecessorID, transition.from, transition.to, transition.source, "fixture prerequisite landed"); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, f.repo, "checkout", "-b", "target-release")
	if err := os.WriteFile(filepath.Join(f.repo, "released-target.txt"), []byte("target advanced before release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.repo, "add", "released-target.txt")
	gitTest(t, f.repo, "commit", "-m", "advance target before release")
	latestFeature := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "HEAD"))
	gitTest(t, f.repo, "push", "origin", "HEAD:refs/heads/main")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	service := testService(f.home, f.fake)
	db, err = store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.ProjectByID(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.retryPendingLaunches(ctx, db, project, cfg, f.fake.SnapshotValue); err != nil {
		t.Fatalf("reconcile queued feature-base Task after restart: %v", err)
	}
	started, err := db.Task(ctx, f.project.ID, "t2")
	if err != nil || started.State != store.StateWorking || started.Branch != store.TaskBranchName(queued.Seq) || started.SandboxID == 0 {
		t.Fatalf("queued dependent did not launch after restart: task=%+v err=%v", started, err)
	}
	if got := strings.TrimSpace(gitTest(t, started.WorktreePath, "rev-parse", "HEAD")); got != latestFeature {
		t.Fatalf("restarted dependent started at %s, want target tip at release %s (admission tip was %s)", got, latestFeature, featureCommit)
	}
	plan, err := started.AdmittedPlan()
	if err != nil || plan == nil || len(plan.Dependencies) != 1 || plan.Dependencies[0].TaskID != "t1" || plan.Execution.BaseCommit != "" {
		t.Fatalf("restarted dependent lost its admitted base or prerequisite: plan=%#v err=%v", plan, err)
	}
	unmet, err := db.UnmetTaskDependencies(ctx, project.ID, plan.Dependencies)
	if err != nil || len(unmet) != 0 {
		t.Fatalf("landed prerequisite remains unmet: %#v %v", unmet, err)
	}
}

func TestMissingExplicitBaseRefusesLandedDependencyBeforeEffects(t *testing.T) {
	f := newStartFixture(t)
	ctx := context.Background()
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	predecessorID, err := db.CreateTask(ctx, f.project.ID, store.Task{Seq: 1, Type: "change", Title: "Prerequisite", ShortName: "prerequisite", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		from, to store.State
		source   string
	}{{store.StateSpawning, store.StateWorking, "cli"}, {store.StateWorking, store.StateDone, "worker"}, {store.StateDone, store.StateMerged, "cli"}} {
		if err := db.Transition(ctx, predecessorID, transition.from, transition.to, transition.source, "fixture prerequisite landed"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	brief := "---\ntype: change\ntitle: Follow-up\ndone_when: follow-up is ready\nbase: missing\ntarget: main\nafter: t1:landed\n---\nBuild on the landed feature.\n"
	if err := os.WriteFile(f.brief, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output := runCLI(t, f.service, "start", "--brief", f.brief, "--name", "follow-up")
	if code == 0 || !strings.Contains(output, "base branch") || !strings.Contains(output, "does not exist") {
		t.Fatalf("start accepted a missing base: code=%d output=%s", code, output)
	}
	if f.fake.CallCount("tab.create") != 0 || f.fake.CallCount("agent.start") != 0 {
		t.Fatalf("invalid base acquired Worker resources: calls=%#v", f.fake.Calls)
	}
	db, err = store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if tasks, err := db.Tasks(ctx, f.project.ID, true); err != nil || len(tasks) != 1 {
		t.Fatalf("invalid base left Tasks %#v (%v)", tasks, err)
	}
	if sandboxes, err := db.Sandboxes(ctx, f.project.ID); err != nil || len(sandboxes) != 0 {
		t.Fatalf("invalid base acquired Sandboxes %#v (%v)", sandboxes, err)
	}
	if branches := strings.TrimSpace(gitTest(t, f.repo, "branch", "--list", "posse/t2")); branches != "" {
		t.Fatalf("invalid base claimed a Task branch: %q", branches)
	}
}

func TestRepositoryTaskExplicitBaseWinsOverTargetAndWarns(t *testing.T) {
	f := newStartFixture(t)
	addOrigin(t, f.repo)
	stagingAhead(t, f.repo)
	main := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "main"))
	if err := os.WriteFile(f.brief, []byte("---\ntype: change\ntitle: Backport\ndone_when: patched\nbase: main\ntarget: staging\n---\nPatch staging.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output := f.start(t)
	if code != 0 {
		t.Fatalf("start: %d %s", code, output)
	}
	if !strings.Contains(output, "shop,main,"+main+",staging") || !strings.Contains(output, "base main differs from target staging") {
		t.Fatalf("start did not warn about a base that differs from the target:\n%s", output)
	}
	task, _ := f.onlyTask(t)
	if head := strings.TrimSpace(gitTest(t, task.WorktreePath, "rev-parse", "HEAD")); head != main {
		t.Fatalf("explicit base lost: Task started at %s, want main %s", head, main)
	}
}

func TestRepositoryTaskWithoutBranchSelectionStillStartsFromDefaultBranch(t *testing.T) {
	f := newStartFixture(t)
	code, output := f.start(t)
	if code != 0 {
		t.Fatalf("start: %d %s", code, output)
	}
	if !strings.Contains(output, "shop,main,\"\",main") || strings.Contains(output, "differs") {
		t.Fatalf("default branch plan was not printed:\n%s", output)
	}
	task, _ := f.onlyTask(t)
	if plan, err := task.AdmittedPlan(); err != nil || plan == nil || plan.Execution.BaseCommit != "" || task.BaseRef != "main" {
		t.Fatalf("default branch base changed: base_ref=%s plan=%#v err=%v", task.BaseRef, plan, err)
	}
}

func TestWorkspaceLandedPrerequisiteAllowsExplicitMemberBase(t *testing.T) {
	fastLaunchRetries(t)
	ctx := context.Background()
	root := t.TempDir()
	workspace := workspaceFixture(t, root)
	workerRoot := filepath.Join(workspace, "worker")
	stagingAhead(t, workerRoot)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(filepath.Join(home, "projects", "stack"), 0o700); err != nil {
		t.Fatal(err)
	}
	configText := "[defaults]\nmerge_mode = \"local\"\n\n[profiles.claude]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"claude\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	detected, err := detectProject(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	project, err := registerDetected(ctx, db, detected)
	if err != nil {
		t.Fatal(err)
	}
	predecessorID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Prerequisite", ShortName: "prerequisite", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, predecessorID, store.StateSpawning, store.StateWorking, "cli", "fixture prerequisite started"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTaskRepos(ctx, predecessorID, []store.TaskRepo{{Repo: "worker", TargetBranch: "staging", MergeMode: "local"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:stack:lead"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:stack:lead", Agent: "claude", AgentStatus: "idle"}}}
	fake.Results["tab.create"] = json.RawMessage(`{"tab":{"tab_id":"w1:t2","workspace_id":"w1"},"root_pane":{"pane_id":"w1:p2","tab_id":"w1:t2"}}`)
	fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	service := testService(home, fake)
	t.Chdir(workspace)
	brief := filepath.Join(root, "brief.md")
	text := "---\ntype: change\ntitle: Follow-up\ndone_when: follow-up is ready\nrepos: [worker]\nbase: {worker: staging}\ntarget: {worker: staging}\nafter: t1:landed\n---\nBuild on the landed feature.\n"
	if err := os.WriteFile(brief, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output := runCLI(t, service, "start", "--brief", brief, "--name", "follow-up")
	if code != 0 {
		t.Fatalf("queue workspace Task with a landed prerequisite pending: %d %s", code, output)
	}
	if fake.CallCount("tab.create") != 0 || fake.CallCount("agent.start") != 0 {
		t.Fatalf("queued workspace Task acquired Worker resources before its prerequisite landed: %#v", fake.Calls)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := db.Task(ctx, project.ID, "t2")
	if err != nil || queued.State != store.StateQueued || queued.Branch != "" || queued.WorktreePath != "" || queued.SandboxID != 0 {
		t.Fatalf("workspace Task was not queued without resources: task=%+v err=%v", queued, err)
	}
	plan, err := queued.AdmittedPlan()
	if err != nil || plan == nil || len(plan.Dependencies) != 1 || plan.Dependencies[0].TaskID != "t1" {
		t.Fatalf("queued workspace plan lost its prerequisite: plan=%#v err=%v", plan, err)
	}
	for _, transition := range []struct {
		from, to store.State
		source   string
	}{{store.StateWorking, store.StateDone, "worker"}, {store.StateDone, store.StateMerged, "cli"}} {
		if err := db.Transition(ctx, predecessorID, transition.from, transition.to, transition.source, "fixture prerequisite landed"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		t.Fatal(err)
	}
	service = testService(home, fake)
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.retryPendingLaunches(ctx, db, project, cfg, fake.SnapshotValue); err != nil {
		t.Fatalf("reconcile queued workspace Task after restart: %v", err)
	}
	started, err := db.Task(ctx, project.ID, "t2")
	if err != nil || started.State != store.StateWorking || started.SandboxID == 0 {
		t.Fatalf("queued workspace Task did not launch: task=%+v err=%v", started, err)
	}
	repos, err := db.TaskRepos(ctx, started.ID)
	if err != nil {
		t.Fatal(err)
	}
	featureCommit := strings.TrimSpace(gitTest(t, workerRoot, "rev-parse", "staging"))
	if len(repos) != 1 || repos[0].BaseRef != featureCommit || repos[0].TargetBranch != "staging" {
		t.Fatalf("launched workspace member lost its feature base: %#v", repos)
	}
	if got := strings.TrimSpace(gitTest(t, filepath.Join(started.WorktreePath, "worker"), "rev-parse", "HEAD")); got != featureCommit {
		t.Fatalf("workspace Member started at %s, want admitted feature commit %s", got, featureCommit)
	}
	plan, err = started.AdmittedPlan()
	if err != nil || plan == nil || len(plan.Dependencies) != 1 || plan.Dependencies[0].TaskID != "t1" {
		t.Fatalf("restarted workspace Task lost its prerequisite: plan=%#v err=%v", plan, err)
	}
}

// The clouddbv1 t32 shape: a workspace Change Brief selects one Member's
// target and no base.
func TestWorkspaceTaskWithOnlyMemberTargetStartsFromFrozenTarget(t *testing.T) {
	fastLaunchRetries(t)
	ctx := context.Background()
	root := t.TempDir()
	workspace := workspaceFixture(t, root)
	workerRoot := filepath.Join(workspace, "worker")
	staging := stagingAhead(t, workerRoot)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(filepath.Join(home, "projects", "stack"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "[defaults]\nmerge_mode = \"local\"\n\n[profiles.claude]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"claude\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	detected, err := detectProject(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	project, err := registerDetected(ctx, db, detected)
	if err == nil {
		err = db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:stack:lead")
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:stack:lead", Agent: "claude", AgentStatus: "idle"}}}
	fake.Results["tab.create"] = json.RawMessage(`{"tab":{"tab_id":"w1:t2","workspace_id":"w1"},"root_pane":{"pane_id":"w1:p2","tab_id":"w1:t2"}}`)
	fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	service := testService(home, fake)
	t.Chdir(workspace)
	brief := filepath.Join(root, "brief.md")
	text := "---\ntype: change\ntitle: Hotfix on staging\ndone_when: patched\nrepos: [worker, backend]\ntarget: {worker: staging}\n---\nPatch staging.\n"
	if err := os.WriteFile(brief, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output := runCLI(t, service, "start", "--brief", brief, "--name", "hotfix-staging")
	if code != 0 {
		t.Fatalf("start: %d %s", code, output)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("Tasks = %#v, %v", tasks, err)
	}
	worktree := filepath.Join(tasks[0].WorktreePath, "worker")
	if head := strings.TrimSpace(gitTest(t, worktree, "rev-parse", "HEAD")); head != staging {
		t.Fatalf("worker started at %s, want staging tip %s", head, staging)
	}
	if !strings.Contains(output, "worker,staging,"+staging+",staging") || !strings.Contains(output, "backend,main,\"\",main") || strings.Contains(output, "differs") {
		t.Fatalf("start did not print each Member's base and target:\n%s", output)
	}
	repos, err := db.TaskRepos(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range repos {
		if repo.Repo == "worker" && (repo.BaseRef != staging || repo.TargetBranch != "staging") {
			t.Fatalf("worker branch plan = %+v", repo)
		}
	}
	if err := os.WriteFile(filepath.Join(worktree, "fix.txt"), []byte("fix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, worktree, "add", "fix.txt")
	gitTest(t, worktree, "commit", "-m", "hotfix")
	code, diff := runCLI(t, service, "diff", taskIDString(tasks[0].Seq), "--stat")
	if code != 0 || !strings.Contains(diff, "fix.txt") || strings.Contains(diff, "feature.txt") {
		t.Fatalf("diff is not scoped to the target base: %d\n%s", code, diff)
	}
}

func TestBaseFollowsTargetOnlyWithoutExplicitBase(t *testing.T) {
	cfg := config.Config{Repositories: map[string]config.Repository{"api": {TargetBranch: "staging"}}}
	for _, testCase := range []struct {
		name       string
		brief      dispatch.Brief
		repo       string
		fromTarget bool
		pinned     bool
	}{
		{name: "brief target", brief: dispatch.Brief{Type: "change", Target: "staging"}, fromTarget: true, pinned: true},
		{name: "member config target", brief: dispatch.Brief{Type: "change"}, repo: "api", fromTarget: true, pinned: true},
		{name: "default target", brief: dispatch.Brief{Type: "change", Target: "main"}},
		{name: "no target", brief: dispatch.Brief{Type: "change"}},
		{name: "explicit base wins", brief: dispatch.Brief{Type: "change", Base: "main", Target: "staging"}, pinned: true},
		{name: "member base wins", brief: dispatch.Brief{Type: "change", BaseByRepo: map[string]string{"api": "main"}}, repo: "api", pinned: true},
		{name: "landed prerequisite stays live", brief: dispatch.Brief{Type: "change", Target: "staging", After: []dispatch.Dependency{{Milestone: "landed"}}}, fromTarget: true},
		{name: "review", brief: dispatch.Brief{Type: "review", Target: "staging"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := baseFromTarget(cfg, testCase.brief, testCase.repo, "main"); got != testCase.fromTarget {
				t.Fatalf("baseFromTarget = %v, want %v", got, testCase.fromTarget)
			}
			if got := pinsBase(cfg, testCase.brief, testCase.repo, "main"); got != testCase.pinned {
				t.Fatalf("pinsBase = %v, want %v", got, testCase.pinned)
			}
		})
	}
}
