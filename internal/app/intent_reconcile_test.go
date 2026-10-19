package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

const deadIntentProcessID = 1 << 30

func TestInterruptedStartWithoutPaneFailsAndReleasesSandbox(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Interrupted start", MergeMode: "local", Branch: "posse/t1", WorktreePath: sandboxPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, project.ID, sandboxPath, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?),created_at=? WHERE id=?`, taskID, time.Now().Add(-6*time.Minute).UnixMilli(), taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.StartIntent(ctx, project.ID, taskID, "start", "done:sandbox.acquire", `{}`, deadIntentProcessID); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		t.Fatal(err)
	}
	service := testService(home, herdr.NewFake())
	if err := service.reconcileIntents(ctx, db, project, cfg, herdr.Snapshot{}); err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil || task.State != store.StateFailed || task.SandboxID != 0 {
		t.Fatalf("interrupted start state and Sandbox = %#v, %v", task, err)
	}
	sandboxes, err := db.Sandboxes(ctx, project.ID)
	if err != nil || len(sandboxes) != 1 || sandboxes[0].State != "idle" || sandboxes[0].TaskID != 0 {
		t.Fatalf("Sandbox after interrupted start = %#v, %v", sandboxes, err)
	}
	if _, err := db.IntentByTask(ctx, taskID); !store.IsNotFound(err) {
		t.Fatalf("start intent remains after recovery: %v", err)
	}
}

func TestInterruptedMergeIntentFinishesOrUndoesFromRepositoryState(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmerged", true: "merged"}[merged], func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			initRepo(t, repo)
			worktree := filepath.Join(root, "sandbox")
			gitTest(t, repo, "worktree", "add", "-b", "posse/t1", worktree, "main")
			if err := os.WriteFile(filepath.Join(worktree, "change.txt"), []byte("work\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitTest(t, worktree, "add", "change.txt")
			gitTest(t, worktree, "commit", "-m", "worker change")
			gatedSHA := strings.TrimSpace(gitTest(t, worktree, "rev-parse", "HEAD"))
			if merged {
				gitTest(t, repo, "merge", "--ff-only", "refs/heads/posse/t1")
			}
			home := filepath.Join(root, "posse")
			db, err := store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nauto_teardown = \"never\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			project, err := db.CreateProject(ctx, "shop", repo, "main")
			if err != nil {
				t.Fatal(err)
			}
			taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Interrupted merge", MergeMode: "local", Branch: "posse/t1", BaseRef: "main", WorktreePath: worktree})
			if err != nil {
				t.Fatal(err)
			}
			for _, transition := range []struct {
				from, to store.State
				source   string
			}{{store.StateSpawning, store.StateWorking, "cli"}, {store.StateWorking, store.StateDone, "worker"}} {
				if err := db.Transition(ctx, taskID, transition.from, transition.to, transition.source, "fixture"); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.SetTaskGatedSHA(ctx, taskID, gatedSHA); err != nil {
				t.Fatal(err)
			}
			if err := db.Transition(ctx, taskID, store.StateDone, store.StateMerging, "cli", "gate passed"); err != nil {
				t.Fatal(err)
			}
			if err := db.StartIntent(ctx, project.ID, taskID, "merge --execute", "done:merge", `{}`, deadIntentProcessID); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(home, project.Name)
			if err != nil {
				t.Fatal(err)
			}
			if err := testService(home, nil).reconcileIntents(ctx, db, project, cfg, herdr.Snapshot{}); err != nil {
				t.Fatal(err)
			}
			task, err := db.TaskByID(ctx, project.ID, taskID)
			if err != nil {
				t.Fatal(err)
			}
			if merged {
				if task.State != store.StateMerged || task.MergedRef == "" {
					t.Fatalf("merged recovery state = %#v", task)
				}
			} else if task.State != store.StateDone || task.GatedSHA != "" {
				t.Fatalf("unmerged recovery did not undo merging: %#v", task)
			}
			if _, err := db.IntentByTask(ctx, taskID); !store.IsNotFound(err) {
				t.Fatalf("merge intent remains after recovery: %v", err)
			}
		})
	}
}

func TestInterruptedTeardownRerunsThroughSandboxRelease(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Interrupted teardown", MergeMode: "local", Branch: "posse/t1", BaseRef: "main", WorktreePath: sandboxPath})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		from, to store.State
		source   string
	}{{store.StateSpawning, store.StateWorking, "cli"}, {store.StateWorking, store.StateDone, "worker"}, {store.StateDone, store.StateMerging, "cli"}, {store.StateMerging, store.StateMerged, "cli"}} {
		if err := db.Transition(ctx, taskID, transition.from, transition.to, transition.source, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, project.ID, sandboxPath, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, taskID, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.StartIntent(ctx, project.ID, taskID, "teardown", "done:panes.close", `{}`, deadIntentProcessID); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := testService(home, nil).reconcileIntents(ctx, db, project, cfg, herdr.Snapshot{}); err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil || task.State != store.StateTornDown || task.SandboxID != 0 {
		t.Fatalf("interrupted teardown state = %#v, %v", task, err)
	}
	sandboxes, err := db.Sandboxes(ctx, project.ID)
	if err != nil || len(sandboxes) != 1 || sandboxes[0].State != "idle" || sandboxes[0].TaskID != 0 {
		t.Fatalf("Sandbox after interrupted teardown = %#v, %v", sandboxes, err)
	}
	if _, err := db.IntentByTask(ctx, taskID); !store.IsNotFound(err) {
		t.Fatalf("teardown intent remains after recovery: %v", err)
	}
}

func TestStartupTeardownRecoveryDoesNotFetchWhileReleasingSandbox(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	remote := filepath.Join(root, "remote.git")
	gitTest(t, root, "clone", "--bare", repo, remote)
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
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Interrupted teardown", MergeMode: "local", Branch: "posse/t1", BaseRef: "main", WorktreePath: sandboxPath})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		from, to store.State
		source   string
	}{
		{store.StateSpawning, store.StateWorking, "cli"},
		{store.StateWorking, store.StateDone, "worker"},
		{store.StateDone, store.StateMerging, "cli"},
		{store.StateMerging, store.StateMerged, "cli"},
	} {
		if err := db.Transition(ctx, taskID, transition.from, transition.to, transition.source, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, project.ID, sandboxPath, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, taskID, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.StartIntent(ctx, project.ID, taskID, "teardown", "done:panes.close", `{}`, deadIntentProcessID); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "remote", "add", "origin", "https://blackhole.invalid/acme/repo.git")
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	fetchLog := filepath.Join(root, "fetch.log")
	wrapper := "#!/bin/sh\nfor arg do if [ \"$arg\" = fetch ]; then printf '%s\\n' \"$*\" >> \"$POSSE_TEST_GIT_FETCH_LOG\"; exit 86; fi; done\nexec \"$POSSE_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GIT_FETCH_LOG", fetchLog)
	t.Setenv("POSSE_REAL_GIT", gitPath)
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	if err := service.reconcileIntentsMode(ctx, db, project, cfg, herdr.Snapshot{}, true, false); err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil || task.State != store.StateTornDown || task.SandboxID != 0 {
		t.Fatalf("startup recovery Task = %#v, %v", task, err)
	}
	sandboxes, err := db.Sandboxes(ctx, project.ID)
	if err != nil || len(sandboxes) != 1 || sandboxes[0].State != "idle" || sandboxes[0].TaskID != 0 {
		t.Fatalf("startup recovery Sandbox = %#v, %v", sandboxes, err)
	}
	if _, err := os.Stat(fetchLog); !os.IsNotExist(err) {
		contents, _ := os.ReadFile(fetchLog)
		t.Fatalf("startup recovery fetched origin: %q (%v)", contents, err)
	}
	if _, err := db.IntentByTask(ctx, taskID); !store.IsNotFound(err) {
		t.Fatalf("teardown intent remains after local recovery: %v", err)
	}
}

func TestInterruptedCompletionTeardownCannotReleaseFailedTaskSandbox(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Failed Task", MergeMode: "local", Branch: "posse/t1", WorktreePath: sandboxPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateWorking, store.StateFailed, "worker", "failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, project.ID, sandboxPath, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, taskID, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.StartIntent(ctx, project.ID, taskID, "teardown", "done:panes.close", `{}`, deadIntentProcessID); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := testService(home, nil).reconcileIntents(ctx, db, project, cfg, herdr.Snapshot{}); err != nil {
		t.Fatal(err)
	}
	sandboxes, err := db.Sandboxes(ctx, project.ID)
	if err != nil || len(sandboxes) != 1 || sandboxes[0].State != "held" || sandboxes[0].TaskID != taskID {
		t.Fatalf("recovery released a failed Task without discard approval: %#v, %v", sandboxes, err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil || task.State != store.StateFailed || task.SandboxID == 0 {
		t.Fatalf("recovery changed the failed Task without discard approval: %#v, %v", task, err)
	}
}
