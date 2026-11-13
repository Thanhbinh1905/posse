package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/greenhouse"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestParallelRequestFailureIsRecordedOnlyAfterPendingRequestEnds(t *testing.T) {
	pendingPath := filepath.Join(t.TempDir(), "request.pending.json")
	if err := os.WriteFile(pendingPath, []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	pending, err := parallelReviewPending(pendingPath)
	if err != nil || !pending || shouldRecordParallelRequestFailure(pending) {
		t.Fatalf("pending parallel request should remain unrecorded: pending=%v record=%v err=%v", pending, shouldRecordParallelRequestFailure(pending), err)
	}
	if err := os.Remove(pendingPath); err != nil {
		t.Fatal(err)
	}
	pending, err = parallelReviewPending(pendingPath)
	if err != nil || pending || !shouldRecordParallelRequestFailure(pending) {
		t.Fatalf("ended parallel request should record its outcome: pending=%v record=%v err=%v", pending, shouldRecordParallelRequestFailure(pending), err)
	}
}

func TestParallelReviewRetriesTransportAsOneRequestObservation(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
	ctx := context.Background()
	head, err := gitOutput(ctx, f.task.WorktreePath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	grant := "shop/t1/shop/r1"
	calls := 0
	result, attempts, err := reviewGreenhouseWithRetries(ctx, true, time.Millisecond, func(time.Duration) (greenhouse.RoundResult, error) {
		calls++
		if calls == 1 {
			return greenhouse.RoundResult{}, errors.New("transport reset after request delivery")
		}
		return greenhouse.RoundResult{Outcome: greenhouse.Settled, Grant: grant}, nil
	})
	if err != nil || calls != 2 || attempts != 2 || result.Outcome != greenhouse.Settled {
		t.Fatalf("retried review = %+v after %d calls and %d attempts, %v", result, calls, attempts, err)
	}
	verdict := store.Verdict{TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2", Grant: grant, Head: head, Target: head, ReviewBase: head, Outcome: "clean", Mode: "parallel"}
	if recorded, err := f.db.RecordGreenhouseVerdict(ctx, verdict, func(context.Context) (string, error) { return head, nil }); err != nil || !recorded {
		t.Fatalf("record final Verdict = %v, %v", recorded, err)
	}
	request := store.GreenhouseReviewRequest{ProjectID: f.project.ID, TaskID: f.task.ID, Repository: f.project.Name, RequestedHead: head, Provider: store.GreenhouseReviewProvider, RequestedAt: 100, SettledAt: 300, Outcome: "verdict", Grant: result.Grant}
	if err := f.db.RecordGreenhouseReviewRequest(ctx, request); err != nil {
		t.Fatal(err)
	}
	requests, err := f.db.GreenhouseReviewRequests(ctx, f.project.ID)
	if err != nil || len(requests) != 1 || requests[0].RequestedAt != 100 || requests[0].SettledAt != 300 {
		t.Fatalf("one retried request observation = %+v, %v", requests, err)
	}
}

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
