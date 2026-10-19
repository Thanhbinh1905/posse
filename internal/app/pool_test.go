package app

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestTeardownFailedTaskDoesNotResetSandboxReusedByAnotherTask(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nmerge_mode = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	task1ID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "First", MergeMode: "local", Branch: "posse/t1", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task1, err := db.TaskByID(ctx, project.ID, task1ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireSandbox(ctx, db, project, task1, home, "warm", nil); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "working"}}}
	service := testService(home, fake)
	if err := service.failSpawn(ctx, db, project, task1ID, "First", "start failed"); err != nil {
		t.Fatal(err)
	}
	failedTask, err := db.TaskByID(ctx, project.ID, task1ID)
	if reason, lockErr := sandboxLockReason(ctx, repo, failedTask.WorktreePath); lockErr != nil || reason != "" {
		t.Fatalf("failed spawn left Sandbox locked: %q, %v", reason, lockErr)
	}
	if err != nil || failedTask.SandboxID != 0 {
		t.Fatalf("failed Task retained its released Sandbox: %#v, %v", failedTask, err)
	}
	task2ID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 2, Type: "change", Title: "Second", MergeMode: "local", Branch: "posse/t2", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task2, err := db.TaskByID(ctx, project.ID, task2ID)
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := acquireSandbox(ctx, db, project, task2, home, "warm", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reason, lockErr := sandboxLockReason(ctx, repo, sandbox.Path); lockErr != nil || reason != "posse: held by t2" {
		t.Fatalf("reused Sandbox lock: %q, %v", reason, lockErr)
	}
	work := filepath.Join(sandbox.Path, "second-task-work.txt")
	if err := os.WriteFile(work, []byte("keep this work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	output := &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"teardown", "t1", "--discard", "--user-approved", "discard first task"}); code != 0 {
		t.Fatalf("teardown failed Task exit = %d; output=%s", code, strings.TrimSpace(output.String()))
	}
	contents, err := os.ReadFile(work)
	if err != nil || string(contents) != "keep this work\n" {
		t.Fatalf("second Task work after teardown = %q, %v", contents, err)
	}
	sandboxes, err := db.Sandboxes(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sandboxes) != 1 || sandboxes[0].State != "held" || sandboxes[0].TaskID != task2ID {
		t.Fatalf("Sandbox after teardown = %#v, want held by second Task %d", sandboxes, task2ID)
	}
}

func TestProcMapPathPreservesSpacesAndRejectsAnonymousMappings(t *testing.T) {
	line := "7f100000-7f101000 r--s 00000000 00:33 12345                   /tmp/cache  with spaces/file (deleted)"
	if got, want := procMapPath(line), "/tmp/cache  with spaces/file (deleted)"; got != want {
		t.Fatalf("mapped path = %q, want %q", got, want)
	}
	if got := procMapPath("7f100000-7f101000 r--p 00000000 00:00 0"); got != "" {
		t.Fatalf("anonymous mapping path = %q, want empty", got)
	}
}

func TestProcessReferencesRootsUsesCanonicalProcPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sandbox")
	alias := filepath.Join(t.TempDir(), "sandbox-alias")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sleep", "30")
	command.Dir = alias
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})

	cwdPath := filepath.Join("/proc", strconv.Itoa(command.Process.Pid), "cwd")
	cwd, err := os.Readlink(cwdPath)
	if err != nil {
		t.Fatal(err)
	}
	if cwd != root {
		t.Fatalf("kernel-reported cwd = %q, want canonical path %q", cwd, root)
	}
	if !processReferencesRoots(command.Process.Pid, []ownedProcessRoot{{path: root}}) {
		t.Fatalf("process with symlinked cwd %q was not found inside canonical root %q", alias, root)
	}
	if processPathInsideRoot(root+"-sibling/file", root) {
		t.Fatalf("sibling path was treated as inside root %q", root)
	}
	if !processPathInsideRoot(root+"/deleted (deleted)", root) {
		t.Fatalf("deleted path under root %q was not recognized", root)
	}
}

func TestPruneCandidatesRemoveOnlyExcessIdleAndCleanBrokenSandboxes(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	idle1 := filepath.Join(root, "sandbox-1")
	idle2 := filepath.Join(root, "sandbox-2")
	cleanBroken := filepath.Join(root, "sandbox-3")
	dirtyBroken := filepath.Join(root, "sandbox-4")
	readOnlyBroken := filepath.Join(root, "sandbox-5")
	outside := filepath.Join(root, "outside")
	gitTest(t, repo, "worktree", "add", "--detach", idle1, "main")
	gitTest(t, repo, "worktree", "add", "--detach", idle2, "main")
	gitTest(t, repo, "worktree", "add", "--detach", cleanBroken, "main")
	gitTest(t, repo, "worktree", "add", "--detach", dirtyBroken, "main")
	gitTest(t, repo, "worktree", "add", "--detach", readOnlyBroken, "main")
	gitTest(t, repo, "worktree", "add", "--detach", outside, "main")
	if err := os.Symlink(outside, filepath.Join(root, "sandbox-7")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirtyBroken, "README.md"), []byte("tracked change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	readOnlyCache := filepath.Join(readOnlyBroken, "cache", "module")
	if err := os.MkdirAll(readOnlyCache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readOnlyCache, "cache.bin"), []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readOnlyCache, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(readOnlyCache), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(readOnlyCache, "cache.bin"), 0o444); err != nil {
		t.Fatal(err)
	}
	candidates := pruneCandidates(context.Background(), []store.Sandbox{
		{ID: 1, Number: 1, State: "idle", Path: idle1},
		{ID: 2, Number: 2, State: "idle", Path: idle2},
		{ID: 3, Number: 3, State: "broken", Path: cleanBroken},
		{ID: 4, Number: 4, State: "broken", Path: dirtyBroken},
		{ID: 5, Number: 5, State: "broken", Path: readOnlyBroken},
		{ID: 6, Number: 6, State: "held", Path: filepath.Join(root, "sandbox-6")},
		{ID: 7, Number: 7, State: "broken", Path: filepath.Join(root, "sandbox-7")},
	}, 1, sandboxWorktreeClean)
	got := make([]int, len(candidates))
	for i, sandbox := range candidates {
		got[i] = sandbox.Number
	}
	if !reflect.DeepEqual(got, []int{5, 3, 2}) {
		t.Fatalf("prune candidates = %v, want read-only broken sandbox 5, clean broken sandbox 3 and excess idle sandbox 2", got)
	}
	zeroKeepCandidates := pruneCandidates(context.Background(), []store.Sandbox{
		{ID: 1, Number: 1, State: "idle", Path: idle1},
		{ID: 2, Number: 2, State: "idle", Path: idle2},
	}, 0, sandboxWorktreeClean)
	if len(zeroKeepCandidates) != 2 {
		t.Fatalf("keep_idle=0 prune candidates = %#v, want both idle Sandboxes", zeroKeepCandidates)
	}
	if err := makeSandboxUntrackedWritable(context.Background(), readOnlyBroken, "pristine"); err != nil {
		t.Fatal(err)
	}
}

func TestResetWorktreeUsesLocalDefaultWhenOriginRefIsNotCached(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	remote := filepath.Join(root, "remote.git")
	gitTest(t, root, "clone", "--bare", repo, remote)
	gitTest(t, repo, "remote", "add", "origin", remote)
	sandbox := filepath.Join(root, "sandbox")
	gitTest(t, repo, "worktree", "add", "-b", "posse/t1", sandbox, "main")
	if err := os.WriteFile(filepath.Join(sandbox, "task-work.txt"), []byte("task work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, sandbox, "add", "task-work.txt")
	gitTest(t, sandbox, "commit", "-m", "Task work")
	defaultHead := strings.TrimSpace(gitTest(t, repo, "rev-parse", "refs/heads/main"))
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", repo, "show-ref", "--verify", "refs/remotes/origin/main").CombinedOutput(); err == nil {
		t.Fatalf("fixture unexpectedly has cached origin ref: %s", output)
	}
	if err := resetWorktree(context.Background(), sandbox, repoTarget{Root: repo, DefaultBranch: "main"}, "warm", false); err != nil {
		t.Fatalf("reset without cached origin ref: %v", err)
	}
	if got := strings.TrimSpace(gitTest(t, sandbox, "rev-parse", "HEAD")); got != defaultHead {
		t.Fatalf("reset HEAD=%s, want local default %s", got, defaultHead)
	}
	if branch := strings.TrimSpace(gitTest(t, sandbox, "branch", "--show-current")); branch != "" {
		t.Fatalf("reset Sandbox remains on branch %q, want detached HEAD", branch)
	}
	if status := strings.TrimSpace(gitTest(t, sandbox, "status", "--porcelain")); status != "" {
		t.Fatalf("reset Sandbox is dirty: %s", status)
	}
}

func TestReleaseSandboxRemovesReadOnlyUntrackedCache(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	sandboxPath := filepath.Join(root, "sandbox")
	gitTest(t, repo, "worktree", "add", "-b", "posse/t1", sandboxPath, "main")
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
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Discard cache", MergeMode: "local", Branch: "posse/t1", BaseRef: "main", WorktreePath: sandboxPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, project.ID, sandboxPath, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, taskID, taskID); err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(sandboxPath, ".cache", "pkg", "mod", "module")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "go.mod"), []byte("module cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cache, filepath.Dir(cache), filepath.Dir(filepath.Dir(cache)), filepath.Join(sandboxPath, ".cache")} {
		if err := os.Chmod(path, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(cache, "go.mod"), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := releaseSandbox(ctx, db, project, task, "warm", false); err != nil {
		t.Fatalf("release failed on read-only untracked cache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "go.mod")); !os.IsNotExist(err) {
		t.Fatalf("read-only cache remained after release: %v", err)
	}
	sandboxes, err := db.Sandboxes(ctx, project.ID)
	if err != nil || len(sandboxes) != 1 || sandboxes[0].State != "idle" || sandboxes[0].TaskID != 0 {
		t.Fatalf("Sandbox after release = %#v, %v", sandboxes, err)
	}
}

func TestPoolPruneRecoversBrokenSandboxWithReadOnlyUntrackedCache(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	sandboxPath := filepath.Join(home, "pool", "shop", "sandbox-1")
	if err := os.MkdirAll(filepath.Dir(sandboxPath), 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "worktree", "add", "--detach", sandboxPath, "main")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[pool]\nkeep_idle = 1\n"), 0o600); err != nil {
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
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state) VALUES(?,1,?,'broken')`, project.ID, sandboxPath); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(sandboxPath, ".cache", "go-path", "pkg", "mod")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "module.zip"), []byte("cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path := cache; path != sandboxPath; path = filepath.Dir(path) {
		if err := os.Chmod(path, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(cache, "module.zip"), 0o444); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"}}}
	service := testService(home, fake)
	t.Chdir(repo)
	output := &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"pool", "prune", "--yes"}); code != 0 {
		t.Fatalf("prune failed to recover broken Sandbox: code=%d output=%s", code, output.String())
	}
	if _, err := os.Stat(sandboxPath); !os.IsNotExist(err) {
		t.Fatalf("broken Sandbox still exists after prune: %v", err)
	}
	observer, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	sandboxes, err := observer.Sandboxes(ctx, project.ID)
	if err != nil || len(sandboxes) != 0 {
		t.Fatalf("Sandbox rows after prune = %#v, %v", sandboxes, err)
	}
}

func TestPoolPruneClaimsSandboxBeforeGitRemoval(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	home := filepath.Join(root, "posse")
	sandboxRoot := filepath.Join(home, "pool", "shop")
	sandbox1Path := filepath.Join(sandboxRoot, "sandbox-1")
	sandbox2Path := filepath.Join(sandboxRoot, "sandbox-2")
	initRepo(t, repo)
	if err := os.MkdirAll(sandboxRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "worktree", "add", "--detach", sandbox1Path, "refs/heads/main")
	gitTest(t, repo, "worktree", "add", "--detach", sandbox2Path, "refs/heads/main")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[pool]\nkeep_idle = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	for index, sandboxPath := range []string{sandbox1Path, sandbox2Path} {
		if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state) VALUES(?,?,?,'idle')`, project.ID, index+1, sandboxPath); err != nil {
			t.Fatal(err)
		}
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"}}}
	service := testService(home, fake)
	stateAtRemoval := ""
	service.removeSandbox = func(ctx context.Context, projectRoot, path string) error {
		if err := db.QueryRowContext(ctx, `SELECT state FROM mounts WHERE path=?`, path).Scan(&stateAtRemoval); err != nil {
			return err
		}
		_, err := gitOutput(ctx, projectRoot, "worktree", "remove", path)
		return err
	}
	t.Chdir(repo)
	output := &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"pool", "prune", "--yes"}); code != 0 {
		t.Fatalf("prune exit=%d output=%s", code, output.String())
	}
	if stateAtRemoval != "pruning" {
		t.Fatalf("Sandbox state at Git removal = %q, want pruning", stateAtRemoval)
	}
}

func TestStopSandboxProcessesResolvesSandboxRootBeforeMatchingCWD(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	worktree := filepath.Join(root, "sandbox")
	sandboxAlias := filepath.Join(root, "sandbox-alias")
	initRepo(t, repo)
	gitTest(t, repo, "worktree", "add", "--detach", worktree, "main")
	if err := os.Symlink(worktree, sandboxAlias); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sleep", "60")
	command.Dir = worktree
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	authorization := newSandboxProcessAuthorization()
	if _, err := authorization.bind(command.Process.Pid); err != nil {
		t.Fatal(err)
	}
	defer authorization.Close()
	killed, err := stopSandboxProcessesAuthorized(sandboxAlias, authorization)
	if err != nil {
		t.Fatal(err)
	}
	pid := strconv.Itoa(command.Process.Pid)
	if !slices.Contains(killed, pid) {
		t.Fatalf("Sandbox process list = %v, want process %s whose cwd is under the real Sandbox path", killed, pid)
	}
}
