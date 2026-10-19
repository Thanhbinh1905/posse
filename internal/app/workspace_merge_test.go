package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type workspaceMergeFixture struct {
	t         *testing.T
	root      string
	workspace string
	home      string
	db        *store.DB
	service   *Service
	project   store.Project
	task      store.Task
	out       bytes.Buffer
}

// newWorkspaceMergeFixture registers the workspace fixture, gives Task t1 a real
// workspace Sandbox for the named members, commits in the changed ones and moves
// the Task to done.
func newWorkspaceMergeFixture(t *testing.T, config string, repos []string, changed []string) *workspaceMergeFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &workspaceMergeFixture{t: t, root: root, workspace: workspaceFixture(t, root), home: filepath.Join(root, "posse")}
	if err := os.MkdirAll(fixture.home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fixture.db = db
	ctx := context.Background()
	detected, err := detectProject(ctx, fixture.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.project, err = registerDetected(ctx, db, detected); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, fixture.project.ID, "w1", "w1:p1", "posse:stack:lead"); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:stack:lead", Agent: "claude", AgentStatus: "idle"}}}
	fixture.service = testService(fixture.home, fake)
	cfg := fixture.config()
	members, _, err := fixture.service.planTaskMembers(ctx, db, fixture.project, cfg, dispatch.Brief{Type: "change", Repos: repos}, nil)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _, err := createTaskWithSequence(ctx, db, fixture.project, fixture.home, store.Task{Type: "change", Title: "Span members", ShortName: "span-members", MergeMode: "local", AutonomyMerge: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, fixture.project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.acquireWorkspaceSandbox(ctx, db, fixture.project, task, members, fixture.home, "warm", nil, "legacy"); err != nil {
		t.Fatal(err)
	}
	if fixture.task, err = db.TaskByID(ctx, fixture.project.ID, taskID); err != nil {
		t.Fatal(err)
	}
	for _, name := range changed {
		worktree := filepath.Join(fixture.task.WorktreePath, name)
		if err := os.WriteFile(filepath.Join(worktree, "change.txt"), []byte(name+" change\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, worktree, "add", "change.txt")
		gitTest(t, worktree, "commit", "-m", "change "+name)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkspaceChangeSignal(ctx, db, fixture.task); err != nil {
		t.Fatalf("done Signal refused: %v", err)
	}
	if _, err := db.RecordWorkerSignal(ctx, fixture.task, "done", "changed "+strings.Join(changed, " and "), map[string]string{}, "task_done"); err != nil {
		t.Fatal(err)
	}
	briefPath := filepath.Join(fixture.home, "projects", "stack", "tasks", "t1", "brief.md")
	if err := writeFile(briefPath, []byte("---\ntype: change\ntitle: Span members\ndone_when: members changed\nrepos: ["+strings.Join(repos, ", ")+"]\n---\nChange the members.\n")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(fixture.workspace)
	return fixture
}

func (f *workspaceMergeFixture) config() config.Config {
	cfg, err := config.Load(f.home, "stack")
	if err != nil {
		f.t.Fatal(err)
	}
	return cfg
}

func (f *workspaceMergeFixture) run(args ...string) int {
	f.out.Reset()
	cli := f.service.CLI()
	cli.Out, cli.ErrOut = &f.out, &f.out
	return cli.Run(args)
}

func (f *workspaceMergeFixture) repos() map[string]store.TaskRepo {
	repos, err := f.db.TaskRepos(context.Background(), f.task.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	byName := map[string]store.TaskRepo{}
	for _, repo := range repos {
		byName[repo.Repo] = repo
	}
	return byName
}

func (f *workspaceMergeFixture) state() store.State {
	task, err := f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return task.State
}

const localWorkspaceConfig = "[defaults]\nmerge_mode = \"local\"\nauto_teardown = \"never\"\n"

func TestWorkspacePRPollAndDiscardSkipMemberlessResearch(t *testing.T) {
	f := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"worker"}, []string{"worker"})
	ctx := context.Background()
	researchID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{
		Seq: 2, Type: "research", Title: "Root-only Research", State: store.StateSpawning,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, researchID, store.StateSpawning, store.StateWorking, "cli", "fixture Worker started"); err != nil {
		t.Fatal(err)
	}
	research, err := f.db.TaskByID(ctx, f.project.ID, researchID)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.service.pollWorkspacePullRequests(ctx, f.db, f.project, f.config(), true); err != nil {
		t.Fatalf("memberless Research broke workspace PR polling: %v", err)
	}
	requests, err := f.service.discardPullRequests(ctx, f.db, f.project, research)
	if err != nil || len(requests) != 0 {
		t.Fatalf("memberless Research discard PRs = %#v, %v; want no PRs and no error", requests, err)
	}
	if err := f.db.Transition(ctx, research.ID, store.StateWorking, store.StateDone, "worker", "fixture completed"); err != nil {
		t.Fatal(err)
	}
	research, err = f.db.TaskByID(ctx, f.project.ID, researchID)
	if err != nil {
		t.Fatal(err)
	}
	err = f.service.mergeWorkspace(&axi.Context{Context: ctx}, f.db, f.project, f.config(), research, false, "")
	var refusal *axi.Error
	if !errors.As(err, &refusal) || refusal.Code != "merge_refused" {
		t.Fatalf("Merge with no selected Members error = %v, want merge_refused", err)
	}
}

func TestWorkspacePRMergeQueuesFreshnessBeforeMemberGate(t *testing.T) {
	f := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"pr\"\nauto_teardown = \"never\"\ngate = [\"touch gate-ran\"]\n", []string{"worker"}, []string{"worker"})
	ctx := context.Background()
	workerRoot := filepath.Join(f.workspace, "worker")
	advanceTree := filepath.Join(f.root, "worker-target-advance")
	gitTest(t, workerRoot, "worktree", "add", "-b", "target-advance", advanceTree, "refs/heads/main")
	if err := os.WriteFile(filepath.Join(advanceTree, "target-only.txt"), []byte("target update\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, advanceTree, "add", "target-only.txt")
	gitTest(t, advanceTree, "commit", "-m", "advance target")
	targetTip := strings.TrimSpace(gitTest(t, advanceTree, "rev-parse", "HEAD"))
	gitTest(t, advanceTree, "push", "origin", targetTip+":refs/heads/main")

	if code := f.run("merge", "t1"); code == 0 || !strings.Contains(f.out.String(), "pr_branch_behind") || !strings.Contains(f.out.String(), targetTip) {
		t.Fatalf("workspace Merge did not name its missing target tip: exit=%d output=%s", code, f.out.String())
	}
	member := f.repos()["worker"]
	if member.GatedSHA != "" || member.State != store.TaskRepoOpen || f.state() != store.StateDone {
		t.Fatalf("stale workspace member was gated or merged: member=%#v task=%s", member, f.state())
	}
	if _, err := os.Stat(filepath.Join(member.WorktreePath, "gate-ran")); !os.IsNotExist(err) {
		t.Fatalf("Gate ran before target freshness was established: %v", err)
	}
	var instruction string
	if err := f.db.QueryRowContext(ctx, `SELECT body FROM messages WHERE task_id=? AND status='queued' ORDER BY id DESC LIMIT 1`, f.task.ID).Scan(&instruction); err != nil || !strings.Contains(instruction, targetTip) || !strings.Contains(instruction, "Workspace member worker") || !strings.Contains(instruction, "--repo worker") {
		t.Fatalf("workspace Worker did not receive a member-specific sync instruction: body=%q err=%v", instruction, err)
	}
}

func TestWorkspaceDiscardCaptureBundlesEveryApprovedMemberTip(t *testing.T) {
	f := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"backend", "worker"}, []string{"backend", "worker"})
	ctx := context.Background()
	tips, err := f.service.workspaceBranchTips(ctx, f.db, f.project, f.task)
	if err != nil || tips == "" {
		t.Fatalf("approved workspace tips = %q, %v", tips, err)
	}
	if err := f.db.RecordApprovalWithBranch(ctx, f.task.ID, "discard", "approved workspace discard", tips); err != nil {
		t.Fatal(err)
	}
	if err := f.service.captureDiscardTips(ctx, f.db, f.home, f.project, f.task); err != nil {
		t.Fatalf("capture workspace discard tips: %v", err)
	}
	artifactDir, err := ensureDiscardArtifactDirectory(f.home, f.project, f.task)
	if err != nil {
		t.Fatal(err)
	}
	pairs := strings.Split(tips, ",")
	if len(pairs) != 2 {
		t.Fatalf("workspace approved tips = %q, want two member tips", tips)
	}
	for _, pair := range pairs {
		repository, commit, ok := strings.Cut(pair, "=")
		if !ok {
			t.Fatalf("invalid workspace tip %q", pair)
		}
		target, err := f.service.projectTarget(ctx, f.db, f.project, repository)
		if err != nil {
			t.Fatal(err)
		}
		bundle := filepath.Join(artifactDir, discardTipName(repository, commit))
		heads, err := gitOutput(ctx, target.Root, "bundle", "list-heads", bundle)
		if err != nil || !bundleHasCommit(heads, commit) {
			t.Fatalf("bundle for %s does not capture approved tip %s: heads=%q err=%v", repository, commit, heads, err)
		}
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET state=? WHERE id=?`, string(store.StateTornDown), f.task.ID); err != nil {
		t.Fatal(err)
	}
	task, err := f.db.TaskByID(ctx, f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releaseSandbox(ctx, f.db, f.project, task, "", false); err != nil {
		t.Fatalf("release workspace Sandbox before pruning its merged branches: %v", err)
	}
	task, err = f.db.TaskByID(ctx, f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	items, err := mergedOrDiscardedBranchItems(ctx, f.db, f.home, f.project, []store.Task{task})
	if err != nil || len(items) != 2 {
		t.Fatalf("captured workspace branch prune candidates = %#v, %v; want both approved member tips", items, err)
	}
	for _, item := range items {
		if err := f.service.applyPruneItem(ctx, f.db, f.home, f.project, item); err != nil {
			t.Fatalf("remove captured workspace branch %s in %s: %v", item.label, item.path, err)
		}
		if _, err := gitOutput(ctx, item.path, "rev-parse", "--verify", "refs/heads/"+item.label); !isMissingGitRef(err) {
			t.Errorf("workspace branch %s remains in %s: %v", item.label, item.path, err)
		}
	}
}

func TestWorkspaceTeardownRemovesOnlyRecordedMergedPRHeads(t *testing.T) {
	f := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"pr\"\nauto_teardown = \"never\"\n", []string{"backend", "worker"}, []string{"backend", "worker"})
	ctx := context.Background()
	repos, err := f.db.TaskRepos(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range repos {
		target, err := f.service.projectTarget(ctx, f.db, f.project, repo.Repo)
		if err != nil {
			t.Fatal(err)
		}
		branchSHA := strings.TrimSpace(gitTest(t, target.Root, "rev-parse", "refs/heads/"+f.task.Branch))
		defaultSHA := strings.TrimSpace(gitTest(t, target.Root, "rev-parse", "refs/heads/"+target.DefaultBranch))
		if repo.Repo == "worker" {
			gitTest(t, target.Root, "branch", "staging", target.DefaultBranch)
			gitTest(t, target.Root, "push", "origin", "staging")
			gitTest(t, repo.WorktreePath, "push", "origin", "HEAD:refs/heads/staging")
			repo.TargetBranch = "staging"
			defaultSHA = branchSHA
		}
		repo.State = store.TaskRepoMerged
		repo.MergeMode = "pr"
		repo.MergedRef = defaultSHA
		repo.GatedSHA = branchSHA
		if repo.Repo == "backend" {
			// A later commit on the Task branch must survive the recorded PR head.
			followUp := filepath.Join(repo.WorktreePath, "after-pr.txt")
			if err := os.WriteFile(followUp, []byte("follow-up\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitTest(t, repo.WorktreePath, "add", "after-pr.txt")
			gitTest(t, repo.WorktreePath, "commit", "-m", "follow-up after merged PR")
		}
		if err := f.db.UpdateTaskRepo(ctx, repo); err != nil {
			t.Fatal(err)
		}
		if repo.Repo == "worker" {
			if _, err := f.db.ExecContext(ctx, `UPDATE task_repos SET target_branch='staging' WHERE task_id=? AND repo='worker'`, f.task.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	removed, err := f.service.removeWorkspaceBranches(ctx, f.db, f.project, f.task, "")
	if err != nil || !removed {
		t.Fatalf("remove merged workspace PR branch: removed=%v err=%v", removed, err)
	}
	for _, repo := range repos {
		target, err := f.service.projectTarget(ctx, f.db, f.project, repo.Repo)
		if err != nil {
			t.Fatal(err)
		}
		_, err = gitOutput(ctx, target.Root, "rev-parse", "--verify", "refs/heads/"+f.task.Branch)
		if repo.Repo == "worker" && !isMissingGitRef(err) {
			t.Errorf("recorded merged PR branch remains in worker: %v", err)
		}
		if repo.Repo == "backend" && err != nil {
			t.Errorf("follow-up work on advanced backend branch was removed: %v", err)
		}
	}
}

func TestWorkspaceSandboxMirrorsSharedFilesAndRequestedMembers(t *testing.T) {
	f := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"backend", "worker"}, []string{"worker"})
	sandbox := f.task.WorktreePath
	for _, path := range []string{"CLAUDE.md", "docs/arch.md", ".env"} {
		if _, err := os.Stat(filepath.Join(sandbox, path)); err != nil {
			t.Fatalf("shared file %s was not copied: %v", path, err)
		}
	}
	for _, path := range []string{"e2e-tool", "backend-side", ".hidden"} {
		if _, err := os.Stat(filepath.Join(sandbox, path)); !os.IsNotExist(err) {
			t.Fatalf("%s must not be in the Sandbox: %v", path, err)
		}
	}
	for _, member := range []string{"backend", "worker"} {
		if branch := strings.TrimSpace(gitTest(t, filepath.Join(sandbox, member), "branch", "--show-current")); branch != "posse/t1" {
			t.Fatalf("%s is on %q", member, branch)
		}
	}
	// A Worker's edit to a shared copy never reaches the workspace root.
	if err := os.WriteFile(filepath.Join(sandbox, "CLAUDE.md"), []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if contents, _ := os.ReadFile(filepath.Join(f.workspace, "CLAUDE.md")); string(contents) != "how the repos fit together\n" {
		t.Fatalf("workspace CLAUDE.md changed: %q", contents)
	}
}

func TestWorkspaceChangeSignalRefusesChangesOutsideRequestedMembers(t *testing.T) {
	f := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"worker"}, []string{"worker"})
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(f.task.WorktreePath, "worker", "stray.txt"), []byte("stray\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkspaceChangeSignal(ctx, f.db, f.task); err == nil || !strings.Contains(err.Error(), "worker: worktree has uncommitted changes") {
		t.Fatalf("err = %v", err)
	}
}

func TestWorkspaceLocalMergeMergesEveryChangedMember(t *testing.T) {
	f := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"backend", "worker", "e2e-tool"}, []string{"backend", "e2e-tool"})
	if code := f.run("merge", "t1"); code != 0 {
		t.Fatalf("merge: %d\n%s", code, f.out.String())
	}
	repos := f.repos()
	if repos["backend"].State != store.TaskRepoGated || repos["e2e-tool"].State != store.TaskRepoGated || repos["worker"].State != store.TaskRepoUnchanged {
		t.Fatalf("members after merge = %#v", repos)
	}
	if f.state() != store.StateMerging {
		t.Fatalf("state = %s", f.state())
	}
	if code := f.run("merge", "t1", "--execute"); code == 0 || !strings.Contains(f.out.String(), "merge_approval_required") {
		t.Fatalf("merge without approval: %d\n%s", code, f.out.String())
	}
	if code := f.run("merge", "t1", "--execute", "--user-approved", "merge both"); code != 0 {
		t.Fatalf("merge --execute: %d\n%s", code, f.out.String())
	}
	if f.state() != store.StateMerged {
		t.Fatalf("state = %s\n%s", f.state(), f.out.String())
	}
	for _, member := range []string{"backend", "e2e-tool"} {
		log := gitTest(t, filepath.Join(f.workspace, member), "log", "--format=%s", "-1", "refs/heads/main")
		if strings.TrimSpace(log) != "change "+member {
			t.Fatalf("%s main = %q", member, log)
		}
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, notice := range notices {
		found = found || (notice.Kind == "merge_ready" && strings.Contains(notice.Summary, "backend, e2e-tool"))
	}
	if !found {
		t.Fatalf("merge_ready does not name the members: %#v", notices)
	}
	if code := f.run("teardown", "t1"); code != 0 || !strings.Contains(f.out.String(), "branch_removed: true") {
		t.Fatalf("teardown: %d\n%s", code, f.out.String())
	}
	for _, member := range []string{"backend", "worker", "e2e-tool"} {
		if branches := gitTest(t, filepath.Join(f.workspace, member), "branch", "--list", "posse/t1"); strings.TrimSpace(branches) != "" {
			t.Fatalf("%s kept posse/t1", member)
		}
	}
	if _, err := os.Stat(filepath.Join(f.task.WorktreePath, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatalf("released Sandbox kept shared copies: %v", err)
	}
}

func TestWorkspaceMergeRefusesMovedMemberBranch(t *testing.T) {
	f := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"backend", "worker"}, []string{"backend", "worker"})
	if code := f.run("merge", "t1"); code != 0 {
		t.Fatalf("merge: %d\n%s", code, f.out.String())
	}
	worker := filepath.Join(f.task.WorktreePath, "worker")
	if err := os.WriteFile(filepath.Join(worker, "late.txt"), []byte("late\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, worker, "add", "late.txt")
	gitTest(t, worker, "commit", "-m", "late")
	if code := f.run("merge", "t1", "--execute", "--user-approved", "go"); code == 0 || !strings.Contains(f.out.String(), "worker: Task branch moved") {
		t.Fatalf("merge after move: %d\n%s", code, f.out.String())
	}
	if f.state() != store.StateDone || f.repos()["worker"].State != store.TaskRepoOpen {
		t.Fatalf("state = %s repos = %#v", f.state(), f.repos())
	}
}

func TestWorkspacePullRequestMemberMergesWhenMerged(t *testing.T) {
	f := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"pr\"\nauto_teardown = \"never\"\n", []string{"worker", "e2e-tool"}, []string{"worker", "e2e-tool"})
	bin := filepath.Join(f.root, "bin")
	state := filepath.Join(f.root, "gh-view.json")
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$POSSE_TEST_GH_LOG\"\ncase \"$1 $2\" in\n" +
		"  \"pr list\") printf '[]\\n' ;;\n" +
		"  \"pr create\") printf 'https://github.com/acme/worker/pull/7\\n' ;;\n" +
		"  \"pr view\") cat \"$POSSE_TEST_GH_VIEW\" ;;\n" +
		"  \"pr merge\") printf 'Merged\\n' ;;\n" +
		"  *) exit 90 ;;\nesac\n"
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GH_LOG", filepath.Join(f.root, "gh.log"))
	t.Setenv("POSSE_TEST_GH_VIEW", state)
	worker := filepath.Join(f.workspace, "worker")
	gitTest(t, worker, "remote", "set-url", "origin", "https://github.com/acme/worker.git")
	remote := filepath.Join(f.root, "remotes", "worker.git")
	gitTest(t, worker, "config", "remote.origin.pushurl", remote)
	gitTest(t, worker, "config", "url.file://"+remote+".insteadOf", "https://github.com/acme/worker.git")
	head := strings.TrimSpace(gitTest(t, filepath.Join(f.task.WorktreePath, "worker"), "rev-parse", "HEAD"))
	writeView := func(prState, mergeCommit string) {
		view := map[string]any{"url": "https://github.com/acme/worker/pull/7", "state": prState, "headRefOid": head, "baseRefName": "main", "mergeable": "MERGEABLE", "reviewDecision": "APPROVED", "statusCheckRollup": []any{map[string]any{"__typename": "CheckRun", "name": "unit", "status": "COMPLETED", "conclusion": "SUCCESS"}}}
		if mergeCommit != "" {
			view["mergeCommit"] = map[string]any{"oid": mergeCommit}
		}
		encoded, _ := json.Marshal(view)
		if err := os.WriteFile(state, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeView("OPEN", "")
	if code := f.run("merge", "t1"); code != 0 {
		t.Fatalf("merge: %d\n%s", code, f.out.String())
	}
	repos := f.repos()
	if repos["worker"].State != store.TaskRepoMerge || repos["worker"].PRURL != "https://github.com/acme/worker/pull/7" || repos["e2e-tool"].State != store.TaskRepoGated {
		t.Fatalf("members = %#v\n%s", repos, f.out.String())
	}
	if code := f.run("merge", "t1", "--execute", "--user-approved", "change it"); code != 0 {
		t.Fatalf("merge --execute: %d\n%s", code, f.out.String())
	}
	if f.state() != store.StateMerging || f.repos()["e2e-tool"].State != store.TaskRepoMerged {
		t.Fatalf("after merge state = %s repos = %#v", f.state(), f.repos())
	}
	writeView("MERGED", head)
	ctx := context.Background()
	if err := f.db.Transition(ctx, f.task.ID, store.StateMerging, store.StateWorking, "lead", "follow-up after PR opened"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.pollWorkspacePullRequests(ctx, f.db, f.project, f.config(), true); err != nil {
		t.Fatal(err)
	}
	if f.state() != store.StateMerged || f.repos()["worker"].MergedRef != head {
		t.Fatalf("after PR merge state = %s repos = %#v", f.state(), f.repos())
	}
	notices, err := f.db.Notices(ctx, f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, notice := range notices {
		if strings.HasPrefix(notice.Kind, "pr_") {
			kinds = append(kinds, notice.Kind+":"+notice.Summary)
		}
	}
	joined := strings.Join(kinds, "|")
	if !strings.Contains(joined, "pr_opened:worker: Span members") || !strings.Contains(joined, "pr_merged:worker: Span members") {
		t.Fatalf("PR Notices do not name the member: %s", joined)
	}
	orphanPath := filepath.Join(f.task.WorktreePath, "worker", "untracked-follow-up.txt")
	if err := os.WriteFile(orphanPath, []byte("Member Orphan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.service.autoTeardownMergedTasks(ctx, f.db, f.project, f.config()); err != nil {
		t.Fatal(err)
	}
	if f.state() != store.StateTornDown {
		t.Fatalf("workspace PR did not tear down: %s", f.state())
	}
	if content := gitTest(t, worker, "show", "refs/heads/"+f.task.Branch+"-orphan:untracked-follow-up.txt"); content != "Member Orphan\n" {
		t.Fatalf("Member Orphan missing: %q", content)
	}
	decisions, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	orphans := 0
	for _, d := range decisions {
		if d.Kind == "orphan" {
			orphans++
		}
	}
	if orphans != 1 {
		t.Fatalf("Member Orphan Decisions=%d: %#v", orphans, decisions)
	}
}

func TestWorkspaceBriefsNameKnownMembersAndReviewsInheritThem(t *testing.T) {
	f := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"backend", "worker"}, []string{"worker"})
	ctx := context.Background()
	cfg := f.config()
	if _, _, err := f.service.planTaskMembers(ctx, f.db, f.project, cfg, dispatch.Brief{Type: "change"}, nil); err == nil || !strings.Contains(err.Error(), "must list the members") {
		t.Fatalf("change without repos: %v", err)
	}
	if _, _, err := f.service.planTaskMembers(ctx, f.db, f.project, cfg, dispatch.Brief{Type: "change", Repos: []string{"nova"}}, nil); err == nil || !strings.Contains(err.Error(), "nova is not a member") {
		t.Fatalf("unknown member: %v", err)
	}
	if _, _, err := f.service.planTaskMembers(ctx, f.db, f.project, cfg, dispatch.Brief{Type: "change", Repos: []string{"e2e-tool"}, MergeMode: "pr"}, nil); err == nil || !strings.Contains(err.Error(), "has no origin") {
		t.Fatalf("pr on a member without origin: %v", err)
	}
	members, _, err := f.service.planTaskMembers(ctx, f.db, f.project, cfg, dispatch.Brief{Type: "review"}, &f.task)
	if err != nil || len(members) != 2 || members[0].BaseRef != f.task.Branch {
		t.Fatalf("review members = %#v, %v", members, err)
	}
	if _, _, err := f.service.planTaskMembers(ctx, f.db, f.project, cfg, dispatch.Brief{Type: "review", Repos: []string{"e2e-tool"}}, &f.task); err == nil {
		t.Fatal("a review may not name a member the reviewed Task does not touch")
	}
}
