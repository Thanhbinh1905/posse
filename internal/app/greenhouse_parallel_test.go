package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/greenhouse"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func hasPendingDecision(decisions []store.Decision, id int64) bool {
	for _, decision := range decisions {
		if decision.ID == id {
			return true
		}
	}
	return false
}

func TestGreenhouseStaleHeadInstructionRequestsReviewWithoutPR(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateDone)
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
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := queueTargetMergeInstruction(context.Background(), f.db, f.task, "", prTargetFreshness{Branch: "main", Tip: strings.Repeat("a", 40)}, f.headSHA); err != nil {
		t.Fatal(err)
	}
	instructions, err := f.db.PendingTaskInstructions(context.Background(), f.task.ID)
	if err != nil || len(instructions) != 1 || !strings.Contains(instructions[0].Body, "posse signal done \"<summary>\" --head <sha>") || !strings.Contains(instructions[0].Body, "without --pr") || !strings.Contains(instructions[0].Body, "posse publish") {
		t.Fatalf("stale Greenhouse instruction = %+v, %v", instructions, err)
	}
}

func TestGreenhousePassQueuesPublicationForNeedsDecisionWorker(t *testing.T) {
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
	if _, err := f.db.AddSignal(context.Background(), f.task.ID, "done", "Review requested", map[string]string{"greenhouse_pending_head": f.headSHA}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(context.Background(), f.task.ID, store.StateWorking, store.StateDone, "worker", "Review requested"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(context.Background(), f.task.ID, store.StateDone, store.StateWorking, "worker", "Publish was refused"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.AddSignal(context.Background(), f.task.ID, "needs-decision", "Waiting for direction", nil); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(context.Background(), f.task.ID, store.StateWorking, store.StateNeedsDecision, "worker", "Waiting for direction"); err != nil {
		t.Fatal(err)
	}
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `INSERT INTO verdicts(task_id,repo,provider,protocol,head_sha,target_sha,review_base_sha,verdict,blocking,mode,recorded_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, f.task.ID, f.project.Name, store.BuiltinReviewProvider, "builtin-v1", f.headSHA, f.headSHA, f.headSHA, "clean", 0, "gate", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcileGreenhouseRepositoryPrepublication(context.Background(), f.db, f.home, f.project, cfg); err != nil {
		t.Fatal(err)
	}
	messages, err := f.db.UndeliveredTaskMessages(context.Background(), f.task.ID)
	if err != nil || len(messages) != 0 {
		t.Fatalf("builtin Verdict queued publication: %+v, %v", messages, err)
	}
	verdict := store.Verdict{TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2", Grant: "shop/t1/shop/r1", Head: f.headSHA, Target: f.headSHA, ReviewBase: f.headSHA, Outcome: "clean", Mode: "gate"}
	if recorded, err := f.db.RecordGreenhouseVerdict(context.Background(), verdict, func(context.Context) (string, error) { return f.headSHA, nil }); err != nil || !recorded {
		t.Fatalf("record Gate pass: recorded=%v err=%v", recorded, err)
	}
	cfg, err = config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcileGreenhouseRepositoryPrepublication(context.Background(), f.db, f.home, f.project, cfg); err != nil {
		t.Fatal(err)
	}
	messages, err = f.db.UndeliveredTaskMessages(context.Background(), f.task.ID)
	if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Body, "Greenhouse passed Task head") || !strings.Contains(messages[0].Body, "posse publish") {
		t.Fatalf("publication message = %+v, %v", messages, err)
	}
}

func TestGreenhouseBlockingVerdictWithdrawsMergeReadinessAndQueuesFix(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
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
	if _, err := f.db.ExecContext(context.Background(), `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=?`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := f.db.RaiseDecision(context.Background(), store.DecisionRequest{
		ProjectID: f.project.ID, TaskID: f.task.ID, Origin: "test:merge-ready", Kind: "merge_ready",
		Question: "Merge Task?", Options: []string{"merge", "wait"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pendingBefore, err := f.db.PendingDecisions(context.Background(), f.project.ID)
	if err != nil || !hasPendingDecision(pendingBefore, decision.ID) {
		t.Fatalf("merge-ready Decision before Verdict = %+v, %v", pendingBefore, err)
	}
	verdict := store.Verdict{
		TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2",
		Grant: "shop/t1/shop/r1", Head: f.headSHA, Target: f.headSHA, ReviewBase: f.headSHA,
		Outcome: "findings", Blocking: 1, FindingsPath: "findings.md", Mode: "gate",
		Findings: []store.ProviderFinding{{Key: "blocking", Severity: "P1", Origin: "correctness", Path: "main.go", Line: 1, Excerpt: "bad", Guidance: "fix", Head: f.headSHA}},
	}
	if recorded, err := f.db.RecordGreenhouseVerdict(context.Background(), verdict, func(context.Context) (string, error) { return f.headSHA, nil }); err != nil || !recorded {
		t.Fatalf("record findings Verdict: recorded=%v err=%v", recorded, err)
	}
	if err := f.service.handleGreenhouseBlockingVerdict(context.Background(), f.db, f.project, f.task, verdict); err != nil {
		t.Fatal(err)
	}
	current, err := f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil || current.State != store.StateDone {
		t.Fatalf("Task state = %s, %v; want merge readiness withdrawn to done", current.State, err)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	withdrawn, namedFindings := false, false
	for _, notice := range notices {
		withdrawn = withdrawn || notice.Kind == "merge_readiness_withdrawn"
		namedFindings = namedFindings || strings.Contains(notice.Summary, "findings.md")
	}
	if !withdrawn || !namedFindings {
		t.Fatalf("notices = %+v, want withdrawal and findings file", notices)
	}
	pendingAfter, err := f.db.PendingDecisions(context.Background(), f.project.ID)
	if err != nil || hasPendingDecision(pendingAfter, decision.ID) {
		t.Fatalf("stale merge-ready Decision remains pending: %+v, %v", pendingAfter, err)
	}
	pending, err := f.db.ReviewFixInstructionPending(context.Background(), f.task.ID)
	if err != nil || !pending {
		t.Fatalf("Review fix pending = %v, %v; want queued fix", pending, err)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireSelectedReviewAt(context.Background(), f.db, f.project, cfg, current, f.headSHA); err == nil || !strings.Contains(err.Error(), "no current passing Greenhouse Verdict") {
		t.Fatalf("merge review gate error = %v, want missing passing Verdict for this head", err)
	}
	code, output, _ := f.run("merge", "t1", "--execute", "--user-approved", "approved")
	if code == 0 || !strings.Contains(output, "no current passing Greenhouse Verdict") {
		t.Fatalf("merge --execute = %d %s; want refusal until a passing Verdict exists", code, output)
	}
}

func TestGreenhouseReviewDoesNotStartDuringOrAfterTeardown(t *testing.T) {
	for _, tornDown := range []bool{false, true} {
		t.Run(map[bool]string{false: "teardown in progress", true: "already torn down"}[tornDown], func(t *testing.T) {
			f := newPRMergeFixture(t, "local", store.StateDone)
			if tornDown {
				if err := f.db.TransitionWithApproval(context.Background(), f.task.ID, store.StateDone, store.StateTornDown, "user", "discarded", "discard", "approved"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := f.db.StartIntent(context.Background(), f.project.ID, f.task.ID, "teardown", "started", `{}`, os.Getpid()); err != nil {
					t.Fatal(err)
				}
			}
			roundDir := filepath.Join(f.home, "projects", f.project.Name, "tasks", taskIDString(f.task.Seq), "greenhouse")
			if err := f.service.startParallelGreenhouseReviews(context.Background(), f.db, f.home, f.project, f.task, []store.ReviewTarget{{Repository: f.project.Name, Commit: f.headSHA}}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(parallelReviewOwnerPath(roundDir, f.project.Name)); !os.IsNotExist(err) {
				t.Fatalf("review owner started during teardown: %v", err)
			}
		})
	}
}

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

func TestPinGreenhouseReviewCheckoutIsScopedByRepository(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
	checkouts := make(map[string]string)
	for _, repository := range []string{"shop", "other"} {
		member := greenhouseMember{name: repository, root: f.repo, worktree: f.worktree}
		pinned, err := pinGreenhouseReviewCheckout(context.Background(), f.db, f.home, f.project, f.task, member, f.headSHA)
		if err != nil {
			t.Fatal(err)
		}
		if actual, err := gitOutput(context.Background(), pinned.worktree, "rev-parse", "HEAD"); err != nil || actual != f.headSHA {
			t.Fatalf("%s checkout head = %q, want %s, err=%v", repository, actual, f.headSHA, err)
		}
		checkouts[repository] = pinned.worktree
	}
	if checkouts["shop"] == checkouts["other"] {
		t.Fatalf("repository members share review checkout %q", checkouts["shop"])
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

func TestWorkspaceGreenhouseRecordsExactHeadEvenWhenHostMarksItNonCurrent(t *testing.T) {
	f := newWorkspaceMergeFixture(t, "review_provider = \"greenhouse\"\n[defaults]\nmerge_mode = \"local\"\n", []string{"worker"}, []string{"worker"})
	ctx := context.Background()
	repos, err := f.db.TaskRepos(ctx, f.task.ID)
	if err != nil || len(repos) != 1 {
		t.Fatalf("workspace Task repos = %+v, %v", repos, err)
	}
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief:     taskplan.Select("brief", "workspace.md", []byte("---\ntype: change\ntitle: Greenhouse workspace\ndone_when: reviewed\nrepos: [worker]\n---\n")),
		Execution: taskplan.Execution{Mode: "sandbox", Scope: "workspace", Members: []taskplan.Member{{Name: "worker", Path: "worker", Base: "main", Target: "main", Delivery: "local", Review: "full-pipeline", ReviewProvider: store.GreenhouseReviewProvider}}},
		Launch:    taskplan.Launch{ProfileName: "test", Profile: config.Profile{Kind: "pi"}},
		Review:    "full-pipeline", ReviewProvider: store.GreenhouseReviewProvider, Delivery: "local",
		Autonomy:        config.Autonomy{Review: "ask", Merge: "ask"},
		Paths:           taskplan.Paths{Scratch: filepath.Join(f.home, "scratch", "stack", "t1"), Output: filepath.Join(f.home, "projects", "stack", "tasks", "t1")},
		ValidationLoops: &taskplan.ValidationLoops{Review: config.ReviewLoop{Enabled: true, MaxRounds: 3, BlockAt: "P1"}, ReviewProvider: store.GreenhouseReviewProvider},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET plan_json=? WHERE id=?`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `INSERT OR REPLACE INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,?,?)`, f.task.ID, plan.Revision, encoded); err != nil {
		t.Fatal(err)
	}
	f.task.PlanJSON = encoded
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("review_provider = \"greenhouse\"\n[review_loop]\ngreenhouse_timeout = \"1s\"\n[defaults]\nmerge_mode = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	head, err := gitOutput(ctx, filepath.Join(f.task.WorktreePath, "worker"), "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.service.greenhouseRound = func(_ context.Context, round greenhouse.Round) (greenhouse.RoundResult, error) {
		calls++
		grant := fmt.Sprintf("stack/t1/worker/r%d", calls)
		return greenhouse.RoundResult{
			Outcome: greenhouse.Settled, Grant: grant, RunID: "workspace-run",
			Result: &greenhouse.HostResult{Grant: grant, Head: head, Target: round.Target, Verdict: "findings", Current: false,
				Findings: []greenhouse.Finding{{ID: "finding-1", Priority: "P1", Origin: "correctness", File: "change.txt", Start: 1, End: 1, Explanation: "Finding", Action: "Fix it", Head: head}}},
		}, nil
	}
	binDir := t.TempDir()
	if err := os.Symlink("/usr/bin/true", filepath.Join(binDir, "greenhouse")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := f.db.AddSignal(ctx, f.task.ID, "done", "Workspace review requested", map[string]any{"greenhouse_pending_members": []greenhousePendingMember{{Repository: "worker", Head: head}}}); err != nil {
		t.Fatal(err)
	}
	decision, err := f.db.RaiseDecision(ctx, store.DecisionRequest{
		ProjectID: f.project.ID, TaskID: f.task.ID, Origin: "test:workspace-merge-ready", Kind: "merge_ready",
		Question: "Merge Task?", Options: []string{"merge", "wait"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := f.run("_greenhouse", "review", "t1", "--repo", "worker"); code != 0 {
		t.Fatalf("workspace Greenhouse review: %d\n%s", code, f.out.String())
	}
	requests, err := f.db.GreenhouseReviewRequests(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(requests) != 1 || requests[0].Outcome != "verdict" || requests[0].RequestedHead != head {
		t.Fatalf("workspace review calls=%d requests=%+v, want one exact-head Verdict", calls, requests)
	}
	verdicts, err := f.db.VerdictsFor(ctx, f.task.ID)
	if err != nil || len(verdicts) != 1 || verdicts[0].Head != head || verdicts[0].Outcome != "findings" {
		t.Fatalf("workspace Verdicts=%+v err=%v, want findings for %s", verdicts, err, head)
	}
	findings, err := f.db.ProviderFindingsForTask(ctx, f.task.ID)
	if err != nil || len(findings) != 1 || findings[0].Repository != "worker" || findings[0].HeadSHA != head {
		t.Fatalf("workspace provider findings=%+v err=%v, want one finding for %s", findings, err, head)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcileGreenhouseWorkspacePrepublication(ctx, f.db, f.home, f.project, cfg); err != nil {
		t.Fatal(err)
	}
	pending, err := f.db.ReviewFixInstructionPending(ctx, f.task.ID)
	if err != nil || !pending {
		t.Fatalf("workspace findings fix pending=%t err=%v, want queued repair", pending, err)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil || countNoticeKind(notices, "greenhouse_review_failed") != 1 {
		t.Fatalf("workspace Greenhouse notices=%+v err=%v, want failure Notice", notices, err)
	}
	withdrawn := false
	for _, notice := range notices {
		withdrawn = withdrawn || notice.Kind == "merge_readiness_withdrawn" && strings.Contains(notice.Summary, "findings")
	}
	if !withdrawn {
		t.Fatalf("workspace Greenhouse notices=%+v, want merge readiness withdrawal naming findings", notices)
	}
	pendingDecisions, err := f.db.PendingDecisions(ctx, f.project.ID)
	if err != nil || hasPendingDecision(pendingDecisions, decision.ID) {
		t.Fatalf("workspace stale merge-ready Decision remains pending: %+v, %v", pendingDecisions, err)
	}
}

func TestWorkspaceGreenhouseMovedPendingHeadRaisesNotice(t *testing.T) {
	f := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"worker"}, []string{"worker"})
	ctx := context.Background()
	worktree := filepath.Join(f.task.WorktreePath, "worker")
	pendingHead, err := gitOutput(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "later.txt"), []byte("later work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, worktree, "add", "later.txt")
	gitTest(t, worktree, "commit", "-m", "move member head")
	if _, err := f.db.AddSignal(ctx, f.task.ID, "done", "Workspace head moved", map[string]any{"greenhouse_pending_members": []greenhousePendingMember{{Repository: "worker", Head: pendingHead}}}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcileGreenhouseWorkspacePrepublication(ctx, f.db, f.home, f.project, cfg); err != nil {
		t.Fatal(err)
	}
	requests, err := f.db.GreenhouseReviewRequests(ctx, f.project.ID)
	if err != nil || len(requests) != 0 {
		t.Fatalf("moved pending head requests=%+v err=%v, want no review request", requests, err)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil || countNoticeKind(notices, "greenhouse_review_failed") != 1 {
		t.Fatalf("moved pending head notices=%+v err=%v, want one Greenhouse failure Notice", notices, err)
	}
	for _, notice := range notices {
		if notice.Kind == "greenhouse_review_failed" && strings.Contains(notice.Summary, "posse review start t1") {
			return
		}
	}
	t.Fatalf("moved pending head Notice has no working next step: %+v", notices)
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
