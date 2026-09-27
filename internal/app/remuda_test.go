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

func TestUnsaddleFailedTaskDoesNotResetMountReusedByAnotherTask(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nlanding_mode = \"local\"\n"), 0o600); err != nil {
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
	task1ID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "First", LandingMode: "local", Branch: "posse/t1", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task1, err := db.TaskByID(ctx, project.ID, task1ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireMount(ctx, db, project, task1, home, "warm", nil); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "working"}}}
	service := testService(home, fake)
	if err := service.failSpawn(ctx, db, project, task1ID, "First", "start failed"); err != nil {
		t.Fatal(err)
	}
	failedTask, err := db.TaskByID(ctx, project.ID, task1ID)
	if err != nil || failedTask.MountID != 0 {
		t.Fatalf("failed Task retained its released Mount: %#v, %v", failedTask, err)
	}
	task2ID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 2, Type: "ship", Title: "Second", LandingMode: "local", Branch: "posse/t2", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task2, err := db.TaskByID(ctx, project.ID, task2ID)
	if err != nil {
		t.Fatal(err)
	}
	mount, err := acquireMount(ctx, db, project, task2, home, "warm", nil)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(mount.Path, "second-task-work.txt")
	if err := os.WriteFile(work, []byte("keep this work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	output := &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"unsaddle", "t1", "--discard", "--user-approved", "discard first task"}); code != 0 {
		t.Fatalf("unsaddle failed Task exit = %d; output=%s", code, strings.TrimSpace(output.String()))
	}
	contents, err := os.ReadFile(work)
	if err != nil || string(contents) != "keep this work\n" {
		t.Fatalf("second Task work after unsaddle = %q, %v", contents, err)
	}
	mounts, err := db.Mounts(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 || mounts[0].State != "held" || mounts[0].TaskID != task2ID {
		t.Fatalf("Mount after unsaddle = %#v, want held by second Task %d", mounts, task2ID)
	}
}

func TestPruneCandidatesRemoveOnlyExcessIdleAndCleanBrokenMounts(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	cleanBroken := filepath.Join(root, "mount-3")
	dirtyBroken := filepath.Join(root, "mount-4")
	readOnlyBroken := filepath.Join(root, "mount-5")
	gitTest(t, repo, "worktree", "add", "--detach", cleanBroken, "main")
	gitTest(t, repo, "worktree", "add", "--detach", dirtyBroken, "main")
	gitTest(t, repo, "worktree", "add", "--detach", readOnlyBroken, "main")
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
	candidates := pruneCandidates(context.Background(), []store.Mount{
		{ID: 1, Number: 1, State: "idle", Path: filepath.Join(root, "mount-1")},
		{ID: 2, Number: 2, State: "idle", Path: filepath.Join(root, "mount-2")},
		{ID: 3, Number: 3, State: "broken", Path: cleanBroken},
		{ID: 4, Number: 4, State: "broken", Path: dirtyBroken},
		{ID: 5, Number: 5, State: "broken", Path: readOnlyBroken},
		{ID: 6, Number: 6, State: "held", Path: filepath.Join(root, "mount-6")},
	}, 1, mountWorktreeClean)
	got := make([]int, len(candidates))
	for i, mount := range candidates {
		got[i] = mount.Number
	}
	if !reflect.DeepEqual(got, []int{5, 3, 2}) {
		t.Fatalf("prune candidates = %v, want read-only broken mount 5, clean broken mount 3 and excess idle mount 2", got)
	}
	if err := makeMountUntrackedWritable(context.Background(), readOnlyBroken, "pristine"); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseMountRemovesReadOnlyUntrackedCache(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	mountPath := filepath.Join(root, "mount")
	gitTest(t, repo, "worktree", "add", "-b", "posse/t1", mountPath, "main")
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
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Discard cache", LandingMode: "local", Branch: "posse/t1", BaseRef: "main", WorktreePath: mountPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, project.ID, mountPath, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, taskID, taskID); err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(mountPath, ".cache", "pkg", "mod", "module")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "go.mod"), []byte("module cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cache, filepath.Dir(cache), filepath.Dir(filepath.Dir(cache)), filepath.Join(mountPath, ".cache")} {
		if err := os.Chmod(path, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(cache, "go.mod"), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := releaseMount(ctx, db, project, task, "warm", false); err != nil {
		t.Fatalf("release failed on read-only untracked cache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "go.mod")); !os.IsNotExist(err) {
		t.Fatalf("read-only cache remained after release: %v", err)
	}
	mounts, err := db.Mounts(ctx, project.ID)
	if err != nil || len(mounts) != 1 || mounts[0].State != "idle" || mounts[0].TaskID != 0 {
		t.Fatalf("Mount after release = %#v, %v", mounts, err)
	}
}

func TestRemudaPruneRecoversBrokenMountWithReadOnlyUntrackedCache(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	mountPath := filepath.Join(root, "mount")
	gitTest(t, repo, "worktree", "add", "--detach", mountPath, "main")
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[remuda]\nkeep_idle = 1\n"), 0o600); err != nil {
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
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state) VALUES(?,1,?,'broken')`, project.ID, mountPath); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(mountPath, ".cache", "go-path", "pkg", "mod")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "module.zip"), []byte("cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path := cache; path != mountPath; path = filepath.Dir(path) {
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
	if code := cli.Run([]string{"remuda", "prune", "--yes"}); code != 0 {
		t.Fatalf("prune failed to recover broken Mount: code=%d output=%s", code, output.String())
	}
	if _, err := os.Stat(mountPath); !os.IsNotExist(err) {
		t.Fatalf("broken Mount still exists after prune: %v", err)
	}
	observer, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	mounts, err := observer.Mounts(ctx, project.ID)
	if err != nil || len(mounts) != 0 {
		t.Fatalf("Mount rows after prune = %#v, %v", mounts, err)
	}
}

func TestRemudaPruneClaimsMountBeforeGitRemoval(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	mount1Path := filepath.Join(root, "mount-1")
	mount2Path := filepath.Join(root, "mount-2")
	initRepo(t, repo)
	gitTest(t, repo, "worktree", "add", "--detach", mount1Path, "refs/heads/main")
	gitTest(t, repo, "worktree", "add", "--detach", mount2Path, "refs/heads/main")
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[remuda]\nkeep_idle = 1\n"), 0o600); err != nil {
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
	for index, mountPath := range []string{mount1Path, mount2Path} {
		if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state) VALUES(?,?,?,'idle')`, project.ID, index+1, mountPath); err != nil {
			t.Fatal(err)
		}
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"}}}
	service := testService(home, fake)
	stateAtRemoval := ""
	service.removeMount = func(ctx context.Context, projectRoot, path string) error {
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
	if code := cli.Run([]string{"remuda", "prune", "--yes"}); code != 0 {
		t.Fatalf("prune exit=%d output=%s", code, output.String())
	}
	if stateAtRemoval != "pruning" {
		t.Fatalf("Mount state at Git removal = %q, want pruning", stateAtRemoval)
	}
}

func TestStopMountProcessesResolvesMountRootBeforeMatchingCWD(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	worktree := filepath.Join(root, "mount")
	mountAlias := filepath.Join(root, "mount-alias")
	initRepo(t, repo)
	gitTest(t, repo, "worktree", "add", "--detach", worktree, "main")
	if err := os.Symlink(worktree, mountAlias); err != nil {
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
	killed, err := stopMountProcesses(mountAlias)
	if err != nil {
		t.Fatal(err)
	}
	pid := strconv.Itoa(command.Process.Pid)
	if !slices.Contains(killed, pid) {
		t.Fatalf("Mount process list = %v, want process %s whose cwd is under the real Mount path", killed, pid)
	}
}
