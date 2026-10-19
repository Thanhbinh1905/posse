package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func TestWorkspaceRootContextIsInvalidForRepositoryProjects(t *testing.T) {
	service := &Service{}
	_, _, err := service.planTaskMembers(context.Background(), nil, store.Project{}, config.Config{}, dispatch.Brief{Type: "research", RootContext: "read-only"}, nil)
	if err == nil || !strings.Contains(err.Error(), "root_context: is valid only in a workspace") {
		t.Fatalf("repository project accepted root_context: %v", err)
	}
}

func TestWorkspaceTaskAcquiresOnlySelectedMemberDespiteUnrelatedProblems(t *testing.T) {
	f := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"worker"}, []string{"worker"})
	ctx := context.Background()
	workerRoot := filepath.Join(f.workspace, "worker")
	gitTest(t, workerRoot, "branch", "posse/t2")
	gitTest(t, workerRoot, "remote", "set-url", "origin", "https://unreachable.invalid/worker.git")
	if err := os.RemoveAll(filepath.Join(f.workspace, "e2e-tool")); err != nil {
		t.Fatal(err)
	}

	brief := dispatch.Brief{Type: "research", Repos: []string{"backend"}}
	members, _, err := f.service.planTaskMembers(ctx, f.db, f.project, f.config(), brief, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := taskBranchAvailableForBranch(ctx, f.db, f.project, store.TaskBranchName(2), members); err != nil {
		t.Fatalf("unrequested collision or unavailable repository blocked branch admission: %v", err)
	}
	taskID, _, err := createTaskWithSequence(ctx, f.db, f.project, f.home, store.Task{Type: "research", Title: "Inspect one member", ShortName: "scoped-single-member", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.db.TaskByID(ctx, f.project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.acquireWorkspaceSandbox(ctx, f.db, f.project, task, members, f.home, "warm", nil, "none"); err != nil {
		t.Fatalf("acquire selected Member: %v", err)
	}
	task, err = f.db.TaskByID(ctx, f.project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{"backend"} {
		if _, err := os.Stat(filepath.Join(task.WorktreePath, member, ".git")); err != nil {
			t.Fatalf("selected Member %s is not mounted: %v", member, err)
		}
	}
	for _, member := range []string{"worker", "e2e-tool"} {
		if _, err := os.Stat(filepath.Join(task.WorktreePath, member)); !os.IsNotExist(err) {
			t.Fatalf("unselected Member %s is mounted: %v", member, err)
		}
	}
	if _, err := os.Stat(filepath.Join(task.WorktreePath, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatalf("implicit shared-root context was copied: %v", err)
	}
	taskRepos, err := f.db.TaskRepos(ctx, task.ID)
	if err != nil || len(taskRepos) != 1 || taskRepos[0].Repo != "backend" {
		t.Fatalf("Task Member scope = %#v, %v", taskRepos, err)
	}
}

func TestWorkspaceResearchCanSelectOnlyBoundedReadOnlyRootContext(t *testing.T) {
	f := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"worker"}, []string{"worker"})
	ctx := context.Background()
	brief := dispatch.Brief{Type: "research", RootContext: "read-only"}
	members, _, err := f.service.planTaskMembers(ctx, f.db, f.project, f.config(), brief, nil)
	if err != nil || len(members) != 0 {
		t.Fatalf("root-only Research members = %#v, %v", members, err)
	}
	taskID, _, err := createTaskWithSequence(ctx, f.db, f.project, f.home, store.Task{Type: "research", Title: "Inspect shared context", ShortName: "inspect-shared-context", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.db.TaskByID(ctx, f.project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.acquireWorkspaceSandbox(ctx, f.db, f.project, task, members, f.home, "warm", nil, "read-only"); err != nil {
		t.Fatalf("acquire root-only Research Sandbox: %v", err)
	}
	task, err = f.db.TaskByID(ctx, f.project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = makeSandboxTreeWritable(task.WorktreePath) })
	rootFile := filepath.Join(task.WorktreePath, "CLAUDE.md")
	info, err := os.Stat(rootFile)
	if err != nil || info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("root context is not read-only: mode=%v err=%v", info, err)
	}
	if contents, err := os.ReadFile(rootFile); err != nil || string(contents) != "how the repos fit together\n" {
		t.Fatalf("read root context: contents=%q err=%v", contents, err)
	}
	for _, member := range []string{"backend", "worker", "e2e-tool"} {
		if _, err := os.Stat(filepath.Join(task.WorktreePath, member, ".git")); !os.IsNotExist(err) {
			t.Fatalf("root-only Research received editable Member %s: %v", member, err)
		}
	}
	if err := validateWorkspaceRootContext(ctx, f.db, f.home, f.project, task, "read-only"); err != nil {
		t.Fatalf("unchanged root context rejected: %v", err)
	}
	if err := os.WriteFile(filepath.Join(task.WorktreePath, "unexpected.txt"), []byte("write\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkspaceRootContext(ctx, f.db, f.home, f.project, task, "read-only"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("changed root context accepted: %v", err)
	}
	if err := os.Remove(rootFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(task.WorktreePath, "unexpected-empty-directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkspaceRootContext(ctx, f.db, f.home, f.project, task, "read-only"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("new directory in shared root accepted: %v", err)
	}
}

func TestWorkspaceSandboxDrainsOnlyCleanUnrequestedLegacyWorktrees(t *testing.T) {
	f := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"backend"}, []string{"backend"})
	ctx := context.Background()
	targets, err := workspaceSandboxTargets(ctx, f.db, f.project)
	if err != nil {
		t.Fatal(err)
	}
	sandbox := filepath.Join(f.root, "legacy-sandbox")
	if err := os.MkdirAll(sandbox, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if target.Name == "backend" || target.Name == "worker" {
			if err := addMemberWorktree(ctx, target.repoTarget, filepath.Join(sandbox, target.Path)); err != nil {
				t.Fatalf("seed legacy %s worktree: %v", target.Name, err)
			}
		}
	}
	var selected taskMember
	for _, target := range targets {
		if target.Name == "backend" {
			selected = target
		}
	}
	if err := f.service.prepareWorkspaceSandbox(ctx, f.project, targets, []taskMember{selected}, sandbox, "warm", "none"); err != nil {
		t.Fatalf("prepare scoped Sandbox: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sandbox, "backend", ".git")); err != nil {
		t.Fatalf("selected Member was drained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sandbox, "worker")); !os.IsNotExist(err) {
		t.Fatalf("clean legacy Member was not drained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sandbox, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatalf("unselected root context was copied: %v", err)
	}
}

func TestWorkspaceDrainRefusesSymlinkedSandboxPaths(t *testing.T) {
	f := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"backend"}, []string{"backend"})
	ctx := context.Background()
	targets, err := workspaceSandboxTargets(ctx, f.db, f.project)
	if err != nil {
		t.Fatal(err)
	}
	sandbox := filepath.Join(f.root, "symlink-sandbox")
	if err := os.MkdirAll(sandbox, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.workspace, "worker"), filepath.Join(sandbox, "worker")); err != nil {
		t.Fatal(err)
	}
	var selected taskMember
	for _, target := range targets {
		if target.Name == "backend" {
			selected = target
		}
	}
	if err := f.service.prepareWorkspaceSandbox(ctx, f.project, targets, []taskMember{selected}, sandbox, "warm", "none"); err == nil {
		t.Fatal("draining an unselected Member symlink succeeded")
	}
	if _, err := os.Stat(filepath.Join(f.workspace, "worker", ".git")); err != nil {
		t.Fatalf("the source repository behind the symlink was changed: %v", err)
	}

	external := filepath.Join(f.root, "external-sandbox-target")
	if err := os.MkdirAll(external, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(external, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("outside Sandbox\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sandboxLink := filepath.Join(f.root, "linked-sandbox")
	if err := os.Symlink(external, sandboxLink); err != nil {
		t.Fatal(err)
	}
	if err := f.service.prepareWorkspaceSandbox(ctx, f.project, targets, []taskMember{selected}, sandboxLink, "warm", "none"); err == nil {
		t.Fatal("preparing a symlinked Sandbox root succeeded")
	}
	if contents, err := os.ReadFile(sentinel); err != nil || string(contents) != "outside Sandbox\n" {
		t.Fatalf("symlinked Sandbox preparation changed external content: %q, %v", contents, err)
	}
}

func TestWorkspaceScopedCleanupPreservesDirtyUnselectedMember(t *testing.T) {
	f := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"backend"}, []string{"backend"})
	ctx := context.Background()
	members, _, err := f.service.planTaskMembers(ctx, f.db, f.project, f.config(), dispatch.Brief{Type: "change", Repos: []string{"backend"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _, err := createTaskWithSequence(ctx, f.db, f.project, f.home, store.Task{Type: "change", Title: "Scoped cleanup", ShortName: "scoped-cleanup", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.db.TaskByID(ctx, f.project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	briefBytes := []byte("---\ntype: change\ntitle: Scoped cleanup\ndone_when: cleanup is safe\nrepos: [backend]\n---\n")
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief: taskplan.Select("brief", "brief.md", briefBytes),
		Execution: taskplan.Execution{Mode: "sandbox", Scope: "workspace", Members: []taskplan.Member{{
			Name: "backend", Path: "backend", Base: members[0].BaseRef, Target: "main", Delivery: "local",
		}}},
		Launch: taskplan.Launch{ProfileName: "test", Profile: config.Profile{Kind: "pi"}},
		Review: "none", Delivery: "local", Autonomy: config.Autonomy{Review: "ask", Merge: "ask"},
		Paths: taskplan.Paths{Scratch: filepath.Join(f.home, "scratch", "stack", "{task}"), Output: filepath.Join(f.home, "projects", "stack", "tasks", "{task}")},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan = plan.Bind(taskIDString(task.Seq))
	task.PlanJSON, err = plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET plan_json=? WHERE id=?`, task.PlanJSON, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `INSERT INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,?,?)`, task.ID, plan.Revision, task.PlanJSON); err != nil {
		t.Fatal(err)
	}
	sandbox, err := f.service.acquireWorkspaceSandbox(ctx, f.db, f.project, task, members, f.home, "warm", nil, "none")
	if err != nil {
		t.Fatal(err)
	}
	workerTarget := taskMember{repoTarget: repoTarget{Name: "worker", Root: filepath.Join(f.workspace, "worker"), DefaultBranch: "main"}, Path: "worker"}
	workerPath := filepath.Join(sandbox.Path, workerTarget.Path)
	if err := addMemberWorktree(ctx, workerTarget.repoTarget, workerPath); err != nil {
		t.Fatal(err)
	}
	dirtyPath := filepath.Join(workerPath, "preserve-me.txt")
	if err := os.WriteFile(dirtyPath, []byte("unselected work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := releaseWorkspaceSandbox(ctx, f.db, f.project, sandbox, "warm", removeGitWorktree); err == nil {
		t.Fatal("cleanup reset or removed a dirty unselected Member")
	}
	if contents, err := os.ReadFile(dirtyPath); err != nil || string(contents) != "unselected work\n" {
		t.Fatalf("unselected Member work was lost: contents=%q err=%v", contents, err)
	}
}
