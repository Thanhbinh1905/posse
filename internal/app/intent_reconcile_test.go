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

func TestInterruptedRideWithoutPaneFailsAndReleasesMount(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Interrupted ride", LandingMode: "local", Branch: "posse/t1", WorktreePath: mountPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, project.ID, mountPath, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?),created_at=? WHERE id=?`, taskID, time.Now().Add(-6*time.Minute).UnixMilli(), taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.StartIntent(ctx, project.ID, taskID, "ride", "done:mount.acquire", `{}`, deadIntentProcessID); err != nil {
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
	if err != nil || task.State != store.StateFailed || task.MountID != 0 {
		t.Fatalf("interrupted ride state and Mount = %#v, %v", task, err)
	}
	mounts, err := db.Mounts(ctx, project.ID)
	if err != nil || len(mounts) != 1 || mounts[0].State != "idle" || mounts[0].TaskID != 0 {
		t.Fatalf("Mount after interrupted ride = %#v, %v", mounts, err)
	}
	if _, err := db.IntentByTask(ctx, taskID); !store.IsNotFound(err) {
		t.Fatalf("ride intent remains after recovery: %v", err)
	}
}

func TestInterruptedLandIntentFinishesOrUndoesFromRepositoryState(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmerged", true: "merged"}[merged], func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			initRepo(t, repo)
			worktree := filepath.Join(root, "mount")
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
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nauto_unsaddle = \"never\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			project, err := db.CreateProject(ctx, "shop", repo, "main")
			if err != nil {
				t.Fatal(err)
			}
			taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Interrupted land", LandingMode: "local", Branch: "posse/t1", BaseRef: "main", WorktreePath: worktree})
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
			if err := db.Transition(ctx, taskID, store.StateDone, store.StateLanding, "cli", "gate passed"); err != nil {
				t.Fatal(err)
			}
			if err := db.StartIntent(ctx, project.ID, taskID, "land --merge", "done:merge", `{}`, deadIntentProcessID); err != nil {
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
				if task.State != store.StateLanded || task.LandedRef == "" {
					t.Fatalf("merged recovery state = %#v", task)
				}
			} else if task.State != store.StateDone || task.GatedSHA != "" {
				t.Fatalf("unmerged recovery did not undo landing: %#v", task)
			}
			if _, err := db.IntentByTask(ctx, taskID); !store.IsNotFound(err) {
				t.Fatalf("land intent remains after recovery: %v", err)
			}
		})
	}
}

func TestInterruptedUnsaddleRerunsThroughMountRelease(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Interrupted unsaddle", LandingMode: "local", Branch: "posse/t1", BaseRef: "main", WorktreePath: mountPath})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		from, to store.State
		source   string
	}{{store.StateSpawning, store.StateWorking, "cli"}, {store.StateWorking, store.StateDone, "worker"}, {store.StateDone, store.StateLanding, "cli"}, {store.StateLanding, store.StateLanded, "cli"}} {
		if err := db.Transition(ctx, taskID, transition.from, transition.to, transition.source, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, project.ID, mountPath, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, taskID, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.StartIntent(ctx, project.ID, taskID, "unsaddle", "done:panes.close", `{}`, deadIntentProcessID); err != nil {
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
	if err != nil || task.State != store.StateTornDown || task.MountID != 0 {
		t.Fatalf("interrupted unsaddle state = %#v, %v", task, err)
	}
	mounts, err := db.Mounts(ctx, project.ID)
	if err != nil || len(mounts) != 1 || mounts[0].State != "idle" || mounts[0].TaskID != 0 {
		t.Fatalf("Mount after interrupted unsaddle = %#v, %v", mounts, err)
	}
	if _, err := db.IntentByTask(ctx, taskID); !store.IsNotFound(err) {
		t.Fatalf("unsaddle intent remains after recovery: %v", err)
	}
}

func TestStartupUnsaddleRecoveryDoesNotFetchWhileReleasingMount(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	remote := filepath.Join(root, "remote.git")
	gitTest(t, root, "clone", "--bare", repo, remote)
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
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Interrupted unsaddle", LandingMode: "local", Branch: "posse/t1", BaseRef: "main", WorktreePath: mountPath})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		from, to store.State
		source   string
	}{
		{store.StateSpawning, store.StateWorking, "cli"},
		{store.StateWorking, store.StateDone, "worker"},
		{store.StateDone, store.StateLanding, "cli"},
		{store.StateLanding, store.StateLanded, "cli"},
	} {
		if err := db.Transition(ctx, taskID, transition.from, transition.to, transition.source, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, project.ID, mountPath, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, taskID, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.StartIntent(ctx, project.ID, taskID, "unsaddle", "done:panes.close", `{}`, deadIntentProcessID); err != nil {
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
	if err != nil || task.State != store.StateTornDown || task.MountID != 0 {
		t.Fatalf("startup recovery Task = %#v, %v", task, err)
	}
	mounts, err := db.Mounts(ctx, project.ID)
	if err != nil || len(mounts) != 1 || mounts[0].State != "idle" || mounts[0].TaskID != 0 {
		t.Fatalf("startup recovery Mount = %#v, %v", mounts, err)
	}
	if _, err := os.Stat(fetchLog); !os.IsNotExist(err) {
		contents, _ := os.ReadFile(fetchLog)
		t.Fatalf("startup recovery fetched origin: %q (%v)", contents, err)
	}
	if _, err := db.IntentByTask(ctx, taskID); !store.IsNotFound(err) {
		t.Fatalf("unsaddle intent remains after local recovery: %v", err)
	}
}

func TestInterruptedCompletionUnsaddleCannotReleaseFailedTaskMount(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Failed Task", LandingMode: "local", Branch: "posse/t1", WorktreePath: mountPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateWorking, store.StateFailed, "worker", "failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, project.ID, mountPath, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET mount_id=(SELECT id FROM mounts WHERE task_id=?) WHERE id=?`, taskID, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.StartIntent(ctx, project.ID, taskID, "unsaddle", "done:panes.close", `{}`, deadIntentProcessID); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := testService(home, nil).reconcileIntents(ctx, db, project, cfg, herdr.Snapshot{}); err != nil {
		t.Fatal(err)
	}
	mounts, err := db.Mounts(ctx, project.ID)
	if err != nil || len(mounts) != 1 || mounts[0].State != "held" || mounts[0].TaskID != taskID {
		t.Fatalf("recovery released a failed Task without discard approval: %#v, %v", mounts, err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil || task.State != store.StateFailed || task.MountID == 0 {
		t.Fatalf("recovery changed the failed Task without discard approval: %#v, %v", task, err)
	}
}
