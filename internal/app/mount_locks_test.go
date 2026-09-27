package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestHeldMountLockSurvivesReconcileAndUnlocksOnRelease(t *testing.T) {
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
	id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Lock", LandingMode: "local", Branch: "posse/t1", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	mount, err := acquireMount(ctx, db, project, task, home, "warm", nil)
	if err != nil {
		t.Fatal(err)
	}
	reason, err := mountLockReason(ctx, repo, mount.Path)
	if err != nil || reason != "posse: held by t1" {
		t.Fatalf("held lock = %q %v", reason, err)
	}
	if err := ensureHeldMountLocks(ctx, db, project); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, repo, "worktree", "unlock", mount.Path); err != nil {
		t.Fatal(err)
	}
	if err := ensureHeldMountLocks(ctx, db, project); err != nil {
		t.Fatal(err)
	}
	reason, err = mountLockReason(ctx, repo, mount.Path)
	if err != nil || reason != "posse: held by t1" {
		t.Fatalf("reconciled lock = %q %v", reason, err)
	}
	task, err = db.TaskByID(ctx, project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releaseMount(ctx, db, project, task, "warm", false); err != nil {
		t.Fatal(err)
	}
	reason, err = mountLockReason(ctx, repo, mount.Path)
	if err != nil || reason != "" {
		t.Fatalf("released lock = %q %v", reason, err)
	}
	// Simulate a crash that left the former Task's lock on an idle Mount.
	id2, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 2, Type: "ship", Title: "Reuse", LandingMode: "local", Branch: "posse/t2", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.TaskByID(ctx, project.ID, id2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, repo, "worktree", "lock", "--reason", "posse: held by t1", mount.Path); err != nil {
		t.Fatal(err)
	}
	reused, err := acquireMount(ctx, db, project, second, home, "warm", nil)
	if err != nil || reused.ID != mount.ID {
		t.Fatalf("stale Posse lock blocked reuse: %#v %v", reused, err)
	}
	if reason, err = mountLockReason(ctx, repo, mount.Path); err != nil || reason != "posse: held by t2" {
		t.Fatalf("reused lock = %q %v", reason, err)
	}
	second, err = db.TaskByID(ctx, project.ID, id2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releaseMount(ctx, db, project, second, "warm", true); err != nil {
		t.Fatal(err)
	}
	if reason, err = mountLockReason(ctx, repo, mount.Path); err != nil || reason != "" {
		t.Fatalf("discard lock = %q %v", reason, err)
	}
	if _, err := gitOutput(ctx, repo, "worktree", "lock", "--reason", "foreign owner", mount.Path); err != nil {
		t.Fatal(err)
	}
	id3, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 3, Type: "ship", Title: "Foreign", LandingMode: "local", Branch: "posse/t3", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	third, err := db.TaskByID(ctx, project.ID, id3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireMount(ctx, db, project, third, home, "warm", nil); err == nil {
		t.Fatal("acquire replaced a foreign lock")
	}
	if reason, err = mountLockReason(ctx, repo, mount.Path); err != nil || reason != "foreign owner" {
		t.Fatalf("foreign lock changed: %q %v", reason, err)
	}
}

func TestReconcileCannotRelockDuringRelease(t *testing.T) {
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
	id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Race", LandingMode: "local", Branch: "posse/t1", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	mount, err := acquireMount(ctx, db, project, task, home, "warm", nil)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	reconciled := make(chan error, 1)
	go func() {
		<-started
		reconciled <- ensureHeldMountLocks(ctx, db, project)
	}()
	err = withMountStateLock(ctx, db, func() error {
		if err := unlockMount(ctx, repo, mount.Path); err != nil {
			return err
		}
		close(started)
		// Reconcile begins while Git is unlocked but ownership remains held.
		time.Sleep(50 * time.Millisecond)
		return db.ReleaseMount(ctx, mount.ID, task.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-reconciled; err != nil {
		t.Fatal(err)
	}
	if reason, err := mountLockReason(ctx, repo, mount.Path); err != nil || reason != "" {
		t.Fatalf("reconcile relocked idle Mount: %q, %v", reason, err)
	}
}

func TestBrokenMountUnlocksAndAcquireReplacesOnlyStalePosseLock(t *testing.T) {
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
	makeTask := func(seq int) store.Task {
		t.Helper()
		id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: seq, Type: "ship", Title: "Reuse", LandingMode: "local", Branch: fmt.Sprintf("posse/t%d", seq), BaseRef: "main"})
		if err != nil {
			t.Fatal(err)
		}
		task, err := db.TaskByID(ctx, project.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	first := makeTask(1)
	mount, err := acquireMount(ctx, db, project, first, home, "warm", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := breakMount(ctx, db, project, first, mount, "test broken checkout"); err != nil {
		t.Fatal(err)
	}
	if reason, err := mountLockReason(ctx, repo, mount.Path); err != nil || reason != "" {
		t.Fatalf("broken Mount lock = %q, %v", reason, err)
	}
	// Simulate a crash after an ownership transition but before Git unlock.
	second := makeTask(2)
	idle, err := db.AcquireMount(ctx, project.ID, second.ID, filepath.Join(home, "remuda", project.Name))
	if err != nil {
		t.Fatal(err)
	}
	if idle.Path == mount.Path {
		t.Fatal("broken Mount was reused")
	}
	if _, err := gitOutput(ctx, repo, "worktree", "add", "--detach", idle.Path, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, repo, "worktree", "lock", "--reason", "posse: held by t1", idle.Path); err != nil {
		t.Fatal(err)
	}
	if err := lockClaimedMount(ctx, db, project, idle, second); err != nil {
		t.Fatal(err)
	}
	if reason, err := mountLockReason(ctx, repo, idle.Path); err != nil || reason != "posse: held by t2" {
		t.Fatalf("reclaimed lock = %q, %v", reason, err)
	}
	if _, err := gitOutput(ctx, repo, "worktree", "unlock", idle.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, repo, "worktree", "lock", "--reason", "foreign owner", idle.Path); err != nil {
		t.Fatal(err)
	}
	if err := lockClaimedMount(ctx, db, project, idle, second); err == nil {
		t.Fatal("foreign lock was replaced")
	}
	if reason, err := mountLockReason(ctx, repo, idle.Path); err != nil || reason != "foreign owner" {
		t.Fatalf("foreign lock changed: %q, %v", reason, err)
	}
}

func TestMissingHeldMountRaisesNoticeWithoutRecreatingCheckout(t *testing.T) {
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
	id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Missing", LandingMode: "local", Branch: "posse/t1", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	mount, err := acquireMount(ctx, db, project, task, home, "warm", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, repo, "worktree", "remove", "--force", "--force", mount.Path); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := ensureHeldMountLocks(ctx, db, project); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(mount.Path); !os.IsNotExist(err) {
		t.Fatalf("missing Mount was recreated: %v", err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, notice := range notices {
		if notice.Kind == "mount_missing" && strings.Contains(notice.Summary, "t1") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("missing Mount Notices = %#v", notices)
	}
}

func TestFailedTaskHeldMountIsRelockedByReconcile(t *testing.T) {
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
	id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Failed launch", LandingMode: "local", Branch: "posse/t1", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	mount, err := acquireMount(ctx, db, project, task, home, "warm", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, store.StateSpawning, store.StateFailed, "cli", "launch failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, repo, "worktree", "unlock", mount.Path); err != nil {
		t.Fatal(err)
	}
	if err := ensureHeldMountLocks(ctx, db, project); err != nil {
		t.Fatal(err)
	}
	reason, err := mountLockReason(ctx, repo, mount.Path)
	if err != nil || reason != "posse: held by t1" {
		t.Fatalf("failed Task's held Mount lock = %q, %v", reason, err)
	}
}

func TestMountIsLockedBeforeAcquireCanExposeHeldCheckout(t *testing.T) {
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
	id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Lock timing", LandingMode: "local", Branch: "posse/t1", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	entered, release := filepath.Join(root, "before-lock"), filepath.Join(root, "release-lock")
	shim := fmt.Sprintf("#!/bin/sh\nif [ \"$3\" = worktree ] && [ \"$4\" = lock ]; then\n  : > %s\n  while [ ! -e %s ]; do sleep 0.01; done\nfi\nexec %s \"$@\"\n", shellQuote(entered), shellQuote(release), shellQuote(realGit))
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	done := make(chan error, 1)
	go func() { _, err := acquireMount(ctx, db, project, task, home, "warm", nil); done <- err }()
	defer func() {
		_ = os.WriteFile(release, nil, 0600)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("acquire ended before git worktree lock: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("acquire did not reach git worktree lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mount, err := db.MountByTask(ctx, task.ID)
	if err != nil || mount.State != "held" {
		t.Fatalf("Mount before lock = %+v, %v", mount, err)
	}
	reason, err := mountLockReason(ctx, repo, mount.Path)
	if err != nil || reason != "posse: held by t1" {
		t.Fatalf("held checkout before lock command = %q, %v", reason, err)
	}
}

func TestReleaseSnapshotFailureDoesNotRelockIdleMount(t *testing.T) {
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
	id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Release snapshot", LandingMode: "local", Branch: "posse/t1", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	mount, err := acquireMount(ctx, db, project, task, home, "warm", nil)
	if err != nil {
		t.Fatal(err)
	}
	task, err = db.TaskByID(ctx, project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	snapshotDir := filepath.Dir(db.TaskSnapshotPath(project.Name, task.Seq))
	if err := os.Chmod(snapshotDir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(snapshotDir, 0700)
	if _, err := releaseMount(ctx, db, project, task, "warm", false); err == nil {
		t.Fatal("expected snapshot persistence failure after release commit")
	}
	mounts, err := db.Mounts(ctx, project.ID)
	if err != nil || len(mounts) != 1 || mounts[0].State != "idle" {
		t.Fatalf("Mount after committed release = %+v, %v", mounts, err)
	}
	reason, err := mountLockReason(ctx, repo, mount.Path)
	if err != nil || reason != "" {
		t.Fatalf("idle Mount retained lock %q, %v", reason, err)
	}
}
