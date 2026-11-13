package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestParallelReviewLockDetectsLiveAndAbandonedOwners(t *testing.T) {
	if lockPath := os.Getenv("POSSE_REVIEW_LOCK_CHILD"); lockPath != "" {
		lock, err := inheritedParallelReviewLock(3, lockPath)
		if err != nil {
			t.Fatal(err)
		}
		defer releaseParallelReviewLock(lock)
		ready, done := os.Getenv("POSSE_REVIEW_LOCK_READY"), os.Getenv("POSSE_REVIEW_LOCK_DONE")
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(done); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("timed out waiting for parent")
	}
	path := filepath.Join(t.TempDir(), "member.parallel.lock")
	first, busy, err := acquireParallelReviewLock(path)
	if err != nil || busy {
		t.Fatalf("acquire first lock = busy %v, err %v", busy, err)
	}
	if inFlight, err := parallelReviewInFlight(path); err != nil || !inFlight {
		t.Fatalf("active lock = %v, %v; want active", inFlight, err)
	}
	readyPath, donePath := filepath.Join(t.TempDir(), "ready"), filepath.Join(t.TempDir(), "done")
	command := exec.Command(os.Args[0], "-test.run=^TestParallelReviewLockDetectsLiveAndAbandonedOwners$")
	command.Env = append(os.Environ(), "POSSE_REVIEW_LOCK_CHILD="+path, "POSSE_REVIEW_LOCK_READY="+readyPath, "POSSE_REVIEW_LOCK_DONE="+donePath)
	command.ExtraFiles = []*os.File{first}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(readyPath); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("child did not acquire inherited lock: %v", err)
	}
	if inFlight, err := parallelReviewInFlight(path); err != nil || !inFlight {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("inherited child lock = %v, %v; want active", inFlight, err)
	}
	if err := os.WriteFile(donePath, []byte("done"), 0o600); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("lock child: %v", err)
	}
	if inFlight, err := parallelReviewInFlight(path); err != nil || inFlight {
		t.Fatalf("released lock = %v, %v; want inactive", inFlight, err)
	}
}

func TestAbandonedParallelReviewResumesItsOriginalHeadBeforeCurrentHead(t *testing.T) {
	oldHead := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	newHead := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	owner := &parallelReviewOwner{PID: 42, Head: oldHead, RequestedAt: 123}
	head, requestedAt := parallelRequestMetadata(newHead, 456, true, owner)
	if head != oldHead || requestedAt != 123 {
		t.Fatalf("resumed request = %s at %d, want old head %s at 123", head, requestedAt, oldHead)
	}
	head, requestedAt = parallelRequestMetadata(newHead, 789, false, owner)
	if head != newHead || requestedAt != 789 {
		t.Fatalf("new request = %s at %d, want current head %s at 789", head, requestedAt, newHead)
	}
}

func TestAbandonedParallelReviewKeepsItsHeadUntilAdapterResume(t *testing.T) {
	dir := t.TempDir()
	ownerPath := filepath.Join(dir, "shop.parallel-owner.json")
	pendingPath := filepath.Join(dir, "shop.pending.json")
	owner := parallelReviewOwner{PID: os.Getpid(), Head: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RequestedAt: 123}
	if err := writeParallelReviewOwner(ownerPath, owner); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pendingPath, []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clearParallelReviewOwner(ownerPath, pendingPath, owner.PID); err != nil {
		t.Fatal(err)
	}
	got, err := readParallelReviewOwner(ownerPath)
	if err != nil || got == nil || got.Head != owner.Head || got.RequestedAt != owner.RequestedAt {
		t.Fatalf("abandoned owner = %+v, %v; want original request metadata", got, err)
	}
	if gotAction := nextParallelReviewAction(false, true); gotAction != parallelReviewResume {
		t.Fatalf("abandoned pending action = %q, want resume", gotAction)
	}
	if err := os.Remove(pendingPath); err != nil {
		t.Fatal(err)
	}
	if err := clearParallelReviewOwner(ownerPath, pendingPath, owner.PID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ownerPath); !os.IsNotExist(err) {
		t.Fatalf("settled owner remains: %v", err)
	}
}
