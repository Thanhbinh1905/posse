package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
