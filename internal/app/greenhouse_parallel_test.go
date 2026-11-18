package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/greenhouse"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestGreenhousePrepublicationNoticesSkippedPendingIntent(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		provider   string
		verb       string
		data       any
		wantReason string
	}{
		{name: "missing done signal", provider: store.GreenhouseReviewProvider, wantReason: "no latest done Signal"},
		{name: "latest signal not done", provider: store.GreenhouseReviewProvider, verb: "working", data: map[string]string{}, wantReason: "no latest done Signal"},
		{name: "invalid signal payload", provider: store.GreenhouseReviewProvider, verb: "done", data: "invalid", wantReason: "invalid or missing greenhouse_pending_head"},
		{name: "empty pending head", provider: store.GreenhouseReviewProvider, verb: "done", data: map[string]string{"other": "value"}, wantReason: "invalid or missing greenhouse_pending_head"},
		{name: "incompatible admitted plan", provider: "builtin", verb: "done", data: map[string]string{"greenhouse_pending_head": strings.Repeat("a", 40)}, wantReason: "does not select Greenhouse full-pipeline"},
		{name: "changed task head", provider: store.GreenhouseReviewProvider, verb: "done", data: map[string]string{"greenhouse_pending_head": strings.Repeat("a", 40)}, wantReason: "Task head moved"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newPRMergeFixture(t, "local", store.StateWorking)
			freezeCILoopWithReview(t, f, 0, 0, "full-pipeline")
			plan, err := f.task.AdmittedPlan()
			if err != nil {
				t.Fatal(err)
			}
			plan.ReviewProvider = testCase.provider
			encoded, err := plan.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.ExecContext(context.Background(), `UPDATE tasks SET plan_json=? WHERE id=?`, encoded, f.task.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.ExecContext(context.Background(), `INSERT OR REPLACE INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,1,?)`, f.task.ID, encoded); err != nil {
				t.Fatal(err)
			}
			if testCase.verb != "" {
				if _, err := f.db.AddSignal(context.Background(), f.task.ID, testCase.verb, "fixture signal", testCase.data); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.db.Transition(context.Background(), f.task.ID, store.StateWorking, store.StateDone, "worker", "done"); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(f.home, f.project.Name)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := f.service.reconcileGreenhouseRepositoryPrepublication(context.Background(), f.db, f.home, f.project, cfg); err != nil {
					t.Fatal(err)
				}
			}
			notices, err := f.db.Notices(context.Background(), f.project.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(notices) != 1 || notices[0].Kind != "greenhouse_review_failed" || !strings.Contains(notices[0].Summary, testCase.wantReason) || !strings.Contains(notices[0].Summary, "posse review start t1") {
				t.Fatalf("skip Notice = %+v, want one Notice containing %q and a working next step", notices, testCase.wantReason)
			}
		})
	}
}

func TestGreenhouseWatchPassSpawnsAuthorizedReviewChild(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateWorking)
	freezeCILoopWithReview(t, f, 0, 0, "full-pipeline")
	plan, err := f.task.AdmittedPlan()
	if err != nil {
		t.Fatal(err)
	}
	plan.ReviewProvider = store.GreenhouseReviewProvider
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `UPDATE tasks SET plan_json=? WHERE id=?`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `INSERT OR REPLACE INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,1,?)`, f.task.ID, encoded); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("review_provider = \"greenhouse\"\n[defaults]\nmerge_mode = \"pr\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetProjectLead(context.Background(), f.project.ID, "fixture", "lead-pane", "lead"); err != nil {
		t.Fatal(err)
	}
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	head, err := gitOutput(context.Background(), f.task.WorktreePath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := f.run("signal", "done", "Greenhouse review requested", "--head", head); code != 0 {
		t.Fatalf("done Signal: %d %s %s", code, output, stderr)
	}
	taskScratch := os.Getenv("TMPDIR")
	if taskScratch == "" {
		taskScratch = t.TempDir()
	}
	shortDir, err := os.MkdirTemp(taskScratch, "gh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortDir) })
	shortHome := filepath.Join(shortDir, "p")
	if err := os.Symlink(f.home, shortHome); err != nil {
		t.Fatal(err)
	}
	f.home = shortHome
	f.service = testService(f.home, f.service.Herdr)
	greenhouseHome := greenhouse.HomeFor(f.home)
	t.Cleanup(func() {
		pid, live := greenhouse.LivePID(greenhouseHome)
		if live {
			if pid < 2 {
				t.Errorf("Greenhouse daemon returned unsafe PID %d", pid)
				return
			}
			if err := greenhouse.Stop(greenhouseHome, 30*time.Second); err != nil {
				t.Errorf("stop Greenhouse daemon %d: %v", pid, err)
			}
		}
		if pid, live := greenhouse.LivePID(greenhouseHome); live {
			t.Errorf("Greenhouse daemon %d is still alive after cleanup", pid)
		}
	})
	binDir, err := os.MkdirTemp(taskScratch, "gb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(binDir) })
	greenhouseBinary := os.Getenv("POSSE_TEST_GREENHOUSE_BINARY")
	realGreenhouse := greenhouseBinary != ""
	if !realGreenhouse {
		greenhouseBinary = "/usr/bin/false"
	}
	if err := os.Symlink(greenhouseBinary, filepath.Join(binDir, "greenhouse")); err != nil {
		t.Fatal(err)
	}
	if realGreenhouse {
		fakeReviewer := `#!/usr/bin/python3
import json, sys
if "--list-models" in sys.argv:
    print("provider model context")
    print("openai gpt-5.6-luna 128K")
    sys.exit(0)
request = json.load(sys.stdin)
path = request["Paths"][0]
head = request["Head"]
verdict = {"head": head, "verdict": "findings", "coverage": request["Paths"], "findings": [{"id": "watch-finding", "priority": "P1", "origin": "correctness", "file": path, "start": 1, "end": 1, "explanation": "Fixture review finding", "action": "Fix the reviewed issue", "rule": "", "head": head}]}
print(json.dumps({"type": "message_end", "message": {"role": "assistant", "provider": "openai", "model": "gpt-5.6-luna", "stopReason": "stop", "content": [{"type": "text", "text": json.dumps(verdict)}]}}))
print(json.dumps({"type": "agent_settled", "aborted": False}))
`
		if err := os.WriteFile(filepath.Join(binDir, "pi"), []byte(fakeReviewer), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	binary := filepath.Join(f.root, "bin", "posse")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = filepath.Clean(filepath.Join(workingDir, "../.."))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Posse CLI: %v\\n%s", err, output)
	}
	previousVerifier := watchExecutableVerifier
	watchExecutableVerifier = func(string) bool { return true }
	t.Cleanup(func() { watchExecutableVerifier = previousVerifier })
	if err := f.service.startWatchRuntime(context.Background(), f.db, f.home, f.project, binary); err != nil {
		t.Fatal(err)
	}
	watch, err := f.db.WatchRuntime(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var process watchProcess
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		process, _ = inspectWatchProcess(f.home, watch.PID)
		if process.PID == watch.PID {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if process.PID != watch.PID {
		t.Fatalf("Project Watch PID %d did not start", watch.PID)
	}
	t.Cleanup(func() {
		if err := stopStoredWatchRuntime(context.Background(), f.home, process); err != nil {
			t.Errorf("stop Project Watch: %v", err)
		}
	})
	passToken := "greenhouse-watch-pass"
	if err := writeFile(watchPassRequestPath(f.home, f.project), []byte(passToken+"\n")); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for readWatchPassToken(watchPassDonePath(f.home, f.project)) != passToken && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if readWatchPassToken(watchPassDonePath(f.home, f.project)) != passToken {
		t.Fatal("Project Watch did not serve the requested pass")
	}
	deadline = time.Now().Add(20 * time.Second)
	var requests []store.GreenhouseReviewRequest
	var notices []store.Notice
	for time.Now().Before(deadline) {
		requests, err = f.db.GreenhouseReviewRequests(context.Background(), f.project.ID)
		if err != nil {
			t.Fatal(err)
		}
		notices, err = f.db.Notices(context.Background(), f.project.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if (len(requests) > 0 && (requests[0].Outcome == "verdict" || requests[0].Outcome == "failed")) || countNoticeKind(notices, "greenhouse_review_failed") > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(requests) != 1 || requests[0].RequestedHead != head {
		t.Fatalf("watch pass did not reach the spawned review child: requests=%+v notices=%+v", requests, notices)
	}
	if realGreenhouse {
		if requests[0].Outcome != "verdict" || requests[0].VerdictID == nil {
			t.Fatalf("real Greenhouse request = %+v, want a persisted, linked Verdict", requests[0])
		}
		deadline := time.Now().Add(15 * time.Second)
		var pending bool
		for time.Now().Before(deadline) {
			pending, err = f.db.ReviewFixInstructionPending(context.Background(), f.task.ID)
			if err != nil || pending {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		verdicts, err := f.db.VerdictsFor(context.Background(), f.task.ID)
		if err != nil || len(verdicts) != 1 {
			t.Fatalf("Greenhouse Verdicts = %+v, %v", verdicts, err)
		}
		verdict := verdicts[0]
		wantBase, err := greenhouseReviewBase(context.Background(), f.task.WorktreePath, verdict.Target, head)
		if err != nil || verdict.Head != head || verdict.ReviewBase != wantBase || len(verdict.ReviewBase) != 40 && len(verdict.ReviewBase) != 64 || verdict.Outcome != "findings" || verdict.Blocking < 1 {
			t.Fatalf("persisted Greenhouse Verdict = %+v, expected blocking findings at head=%s base=%s, err=%v", verdict, head, wantBase, err)
		}
		providerFindings, err := f.db.ProviderFindingsForTask(context.Background(), f.task.ID)
		if err != nil || len(providerFindings) != 1 || providerFindings[0].HeadSHA != head {
			t.Fatalf("persisted provider findings = %+v, err=%v", providerFindings, err)
		}
		if err != nil || !pending {
			notices, noticeErr := f.db.Notices(context.Background(), f.project.ID, false)
			messages, messageErr := f.db.PendingTaskInstructions(context.Background(), f.task.ID)
			t.Fatalf("Greenhouse finding did not queue a Fix Round: pending=%t err=%v notices=%+v noticeErr=%v messages=%+v messageErr=%v", pending, err, notices, noticeErr, messages, messageErr)
		}
		var fixRounds int
		if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='review'`, f.task.ID).Scan(&fixRounds); err != nil || fixRounds != 1 {
			t.Fatalf("queued review Fix Rounds = %d, err=%v", fixRounds, err)
		}
		t.Logf("E2E: done Signal head=%s -> Watch spawned _greenhouse review -> one real Greenhouse request and one findings Verdict (review_base_sha=%s) -> one queued Fix Round", head, verdict.ReviewBase)
	} else if requests[0].Outcome != "failed" {
		t.Fatalf("failed child request outcome = %q, want failed", requests[0].Outcome)
	}
	for _, notice := range notices {
		if notice.Kind == "greenhouse_review_failed" && strings.Contains(notice.Summary, "lead_only") {
			t.Fatalf("verified Watch child was rejected as Lead-only: %s", notice.Summary)
		}
	}
}

func TestGreenhouseVerdictComputesBaseAndChecksProviderBase(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
	member := greenhouseMember{name: f.project.Name, root: f.task.WorktreePath, worktree: f.task.WorktreePath}
	ctx := context.Background()
	base, err := greenhouseReviewBase(ctx, member.worktree, f.headSHA, f.headSHA)
	if err != nil {
		t.Fatal(err)
	}
	result := greenhouse.RoundResult{Grant: "shop/t1/shop/r1", RunID: "run-without-base", Result: &greenhouse.HostResult{Head: f.headSHA, Target: f.headSHA, Verdict: "findings"}}
	if recorded, verdict, err := recordGreenhouseVerdict(ctx, f.db, f.task, member, t.TempDir(), "P1", config.Config{}, result); err != nil || !recorded || verdict.ReviewBase != base {
		t.Fatalf("Verdict with Posse-computed base = %+v, %v", verdict, err)
	}
	result.Grant = "shop/t1/shop/r2"
	result.Result.ReviewBase = strings.Repeat("f", 40)
	if _, _, err := recordGreenhouseVerdict(ctx, f.db, f.task, member, t.TempDir(), "P1", config.Config{}, result); err == nil || !strings.Contains(err.Error(), "differs from Posse-computed") {
		t.Fatalf("mismatched Greenhouse review_base error = %v", err)
	}
	verdicts, err := f.db.VerdictsFor(ctx, f.task.ID)
	if err != nil || len(verdicts) != 1 || verdicts[0].ReviewBase != base {
		t.Fatalf("Greenhouse Verdicts = %+v, %v", verdicts, err)
	}
}

func TestParallelPendingGrantIdentifiesRequestForFailureRecording(t *testing.T) {
	pendingPath := filepath.Join(t.TempDir(), "request.pending.json")
	if err := os.WriteFile(pendingPath, []byte(`{"grant":"shop/t1/shop/r1","request":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	grant, err := parallelPendingGrant(pendingPath)
	if err != nil || grant != "shop/t1/shop/r1" {
		t.Fatalf("pending grant = %q, %v", grant, err)
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
