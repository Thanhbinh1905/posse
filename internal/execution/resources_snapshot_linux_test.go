//go:build linux

package execution

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/thanhbinh1905/posse/internal/store"
	"golang.org/x/sys/unix"
)

// snapshotRewrites counts renames into dir, the final step of every Project
// snapshot rewrite. Each drain returns the count since the previous one;
// inotify merges identical unread events, so drain after every call.
func snapshotRewrites(t *testing.T, dir string) (drain func() int) {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_MOVED_TO); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return func() int {
		count := 0
		buf := make([]byte, 64*1024)
		for {
			n, err := unix.Read(fd, buf)
			if err != nil || n <= 0 {
				return count
			}
			for offset := 0; offset < n; {
				event := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))
				offset += unix.SizeofInotifyEvent + int(event.Len)
				count++
			}
		}
	}
}

func TestAcquireAllAndReleaseAllRewriteProjectSnapshotOncePerCall(t *testing.T) {
	ctx := context.Background()
	db, project, _ := resourceDB(t)
	repo := initResourceGitRepo(t)
	manager := NewManager(db)
	if err := db.PersistProject(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	requests := []LeaseRequest{
		{Path: repo, Request: store.ResourceLeaseRequest{OwnerProjectID: project.ID, Owner: "sync", SourceKey: "s:path"}},
		{Repository: repo, Ref: "refs/heads/main", Request: store.ResourceLeaseRequest{OwnerProjectID: project.ID, Owner: "sync", SourceKey: "s:local"}},
		{Repository: repo, Ref: "refs/heads/main", Request: store.ResourceLeaseRequest{OwnerProjectID: project.ID, Owner: "sync", SourceKey: "s:other"}},
	}
	requests[2].Ref = "refs/heads/other"
	if output, err := exec.Command("git", "-C", repo, "branch", "other").CombinedOutput(); err != nil {
		t.Fatalf("git branch: %v\n%s", err, output)
	}

	drain := snapshotRewrites(t, filepath.Dir(db.ProjectSnapshotPath(project.Name)))
	count := func(run func()) int {
		drain()
		run()
		return drain()
	}
	var leases []store.ResourceLease
	if rewrites := count(func() {
		var err error
		if leases, err = manager.AcquireAll(ctx, requests); err != nil {
			t.Fatal(err)
		}
	}); rewrites != 1 {
		t.Fatalf("AcquireAll of %d leases rewrote the Project snapshot %d times, want 1", len(requests), rewrites)
	}
	if held, err := db.ResourceLeases(ctx, project.ID); err != nil || len(held) != len(requests) {
		t.Fatalf("leases after AcquireAll = %v, %v; want %d", held, err, len(requests))
	}
	if rewrites := count(func() {
		if err := manager.ReleaseAll(ctx, leases); err != nil {
			t.Fatal(err)
		}
	}); rewrites > 1 {
		t.Fatalf("ReleaseAll rewrote the Project snapshot %d times, want at most 1", rewrites)
	}
	if held, err := db.ResourceLeases(ctx, project.ID); err != nil || len(held) != 0 {
		t.Fatalf("leases after release = %v, %v; want none", held, err)
	}

	// One at a time, the same leases rewrite the snapshot per call.
	sequential := 0
	if rewrites := count(func() {
		first, err := manager.AcquirePath(ctx, repo, requests[0].Request)
		if err != nil {
			t.Fatal(err)
		}
		total := drain()
		second, err := manager.AcquireGitRef(ctx, repo, "refs/heads/main", requests[1].Request)
		if err != nil {
			t.Fatal(err)
		}
		total += drain()
		_ = manager.Release(ctx, second)
		total += drain()
		_ = manager.Release(ctx, first)
		sequential = total
	}) + sequential; rewrites < 3 {
		t.Fatalf("sequential acquire and release rewrote the snapshot %d times, want at least 3 to confirm the counter sees per-lease rewrites", rewrites)
	}
}

func TestAcquireAllIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	db, project, other := resourceDB(t)
	repo := initResourceGitRepo(t)
	manager := NewManager(db)
	blocker, err := manager.AcquireGitRef(ctx, repo, "refs/heads/main", store.ResourceLeaseRequest{OwnerProjectID: other.ID, Owner: "blocker", SourceKey: "blocker"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.AcquireAll(ctx, []LeaseRequest{
		{Path: repo, Request: store.ResourceLeaseRequest{OwnerProjectID: project.ID, Owner: "sync", SourceKey: "s:path"}},
		{Repository: repo, Ref: "refs/heads/main", Request: store.ResourceLeaseRequest{OwnerProjectID: project.ID, Owner: "sync", SourceKey: "s:local"}},
	})
	var conflict *store.ResourceLeaseConflict
	if !errors.As(err, &conflict) || conflict.Lease.Owner != "blocker" {
		t.Fatalf("AcquireAll error = %v, want conflict naming blocker", err)
	}
	if held, err := db.ResourceLeases(ctx, project.ID); err != nil || len(held) != 0 {
		t.Fatalf("partial leases after refused AcquireAll = %v, %v; want none", held, err)
	}
	if err := manager.Release(ctx, blocker); err != nil {
		t.Fatal(err)
	}
}
