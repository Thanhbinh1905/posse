package app

import (
	"context"
	"fmt"
	"os"
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
