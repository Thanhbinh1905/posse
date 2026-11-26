//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

const (
	greenhouseModule = "github.com/Thanhbinh1905/greenhouse/cmd/greenhouse"
	// greenhouseVersion pins the Greenhouse release the G0 contract runs against.
	greenhouseVersion = "v0.0.0-20261112171046-615b4358845d"
	// greenhouseProtocolOneCommit predates protocol negotiation and host mode,
	// so its daemon speaks only protocol 1.
	greenhouseProtocolOneCommit = "a55c3f2b36d85485919456b2bdf0d50240b41cdc"
)

// The fake reviewer stands in for the native `pi` agent that Greenhouse runs.
// Greenhouse confines it with Landlock, so it writes only to its control
// directory, which backends.json grants.
const greenhouseFakeReviewer = `#!/usr/bin/env python3
import json, os, sys, time
ctl = %q
if '--list-models' in sys.argv:
    print('provider model context')
    print('fixture model 128K')
    sys.exit(0)
raw = sys.stdin.read()
if os.path.exists(os.path.join(ctl, 'prefix-input')):
    request, _ = json.JSONDecoder().raw_decode(raw.lstrip())
else:
    request = json.loads(raw)
with open(os.path.join(ctl, 'invocations'), 'a') as log:
    log.write(request['Head'] + '\n')
while os.path.exists(os.path.join(ctl, 'hold')):
    time.sleep(0.05)
mode_path = os.path.join(ctl, 'mode')
mode = open(mode_path).read().strip() if os.path.exists(mode_path) else 'clean'
def emit(message):
    print(json.dumps({'type': 'message_end', 'message': message}))
if mode == 'quota':
    emit({'role': 'assistant', 'provider': 'fixture', 'model': 'model', 'stopReason': 'error', 'errorMessage': '429: quota exceeded', 'content': []})
    sys.exit(1)
if mode == 'invalid-output':
    emit({'role': 'assistant', 'provider': 'fixture', 'model': 'model', 'stopReason': 'stop', 'content': [{'type': 'text', 'text': 'not a review result'}]})
    print(json.dumps({'type': 'agent_settled', 'aborted': False}))
    sys.exit(0)
verdict = {'head': request['Head'], 'verdict': 'clean', 'coverage': request['Paths'], 'findings': []}
if mode == 'inconclusive':
    verdict['verdict'] = 'inconclusive'
if mode == 'findings':
    verdict['verdict'] = 'findings'
    verdict['findings'] = [{'id': 'fixture-p1', 'priority': 'P1', 'origin': 'correctness', 'file': request['Paths'][0], 'start': 1, 'end': 1, 'explanation': 'The fixture flags this line', 'action': 'Change the line', 'rule': '', 'head': request['Head']}]
emit({'role': 'assistant', 'provider': 'fixture', 'model': 'model', 'stopReason': 'stop', 'content': [{'type': 'text', 'text': json.dumps(verdict)}]})
print(json.dumps({'type': 'agent_settled', 'aborted': False}))
`

type greenhouseContract struct {
	f      *workerTabsFixture
	ctl    string
	home   string
	env    []string
	change store.Task
	commit int
}

// TestGreenhouseAdapterContract is stage G0 of the Direction 2 design: each
// case of its fault table runs against the real greenhouse binary with a fake
// reviewer, an isolated POSSE_HOME and an isolated Herdr server. Every case
// passes only if no Verdict is recorded for a head that was not reviewed.
func TestGreenhouseFullPipelineMergeOnlyTaskRecordsEmptyDiffVerdict(t *testing.T) {
	f := newWorkerTabsFixture(t)
	c := newGreenhouseContract(t, f)
	c.env = setEnv(c.env, "GREENHOUSE_HOME", c.home)
	projectDB, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := projectDB.ProjectByID(context.Background(), f.projectID)
	projectDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(f.home, "projects", project.Name)
	if err := os.WriteFile(filepath.Join(projectDir, "config.toml"), []byte(`review_provider = "greenhouse"
[defaults]
review = "full-pipeline"
merge_mode = "local"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	c.change = f.start(t, "t1", "Merge-only Greenhouse Task", "merge-only-greenhouse")
	gitTest(t, f.env, c.change.WorktreePath, "checkout", "-b", "source")
	gitTest(t, f.env, c.change.WorktreePath, "commit", "--allow-empty", "-q", "-m", "source history")
	gitTest(t, f.env, c.change.WorktreePath, "checkout", c.change.Branch)
	gitTest(t, f.env, c.change.WorktreePath, "merge", "--no-ff", "--no-edit", "source")
	head := strings.TrimSpace(gitTest(t, f.env, c.change.WorktreePath, "rev-parse", "HEAD"))
	gitTest(t, f.env, c.change.WorktreePath, "diff", "--quiet", "main", head)
	if parents := strings.Fields(gitTest(t, f.env, c.change.WorktreePath, "rev-list", "--parents", "-n", "1", head)); len(parents) != 3 {
		t.Fatalf("merge-only head has %d parents; want a merge commit", len(parents)-1)
	}
	workerEnv := setEnv(c.env, "POSSE_TASK_ID", c.change.ContainmentMarker)
	workerEnv = setEnv(workerEnv, "HERDR_PANE_ID", c.change.PaneID)
	if output, err := c.posse(workerEnv, c.change.WorktreePath, "signal", "done", "Merge-only Task", "--head", head); err != nil {
		t.Fatalf("Worker done Signal: %v\n%s", err, output)
	}
	if output, err := c.posse(c.env, f.repo, "watch", "--poll-only", "--timeout", "3000"); err != nil {
		t.Fatalf("Lead Watch: %v\n%s", err, output)
	}
	verdicts := c.verdicts(t)
	if len(verdicts) != 1 || verdicts[0].Head != head || verdicts[0].Outcome != "clean" || verdicts[0].Mode != "gate" || verdicts[0].Target != strings.TrimSpace(gitTest(t, f.env, f.repo, "rev-parse", "main")) {
		t.Fatalf("empty-diff Verdicts = %+v; want one exact-head gate Verdict", verdicts)
	}
	if _, err := os.Stat(filepath.Join(c.ctl, "invocations")); !os.IsNotExist(err) {
		t.Fatalf("Greenhouse reviewer was invoked for an empty diff: %v", err)
	}
	newHead := c.authorCommit(t)
	result := c.review(t)
	if result["head"] != newHead || result["verdict"] != "clean" || result["recorded"] != true {
		t.Fatalf("changed-head review = %v; want a recorded clean Verdict for %s", result, newHead)
	}
	if len(c.verdicts(t)) != 2 {
		t.Fatalf("Verdicts after changed head = %+v; want the empty-diff and normal review Verdicts", c.verdicts(t))
	}
	if c.invocations(t) != 1 {
		t.Fatalf("Greenhouse reviewer invocations = %d; want one for the changed head", c.invocations(t))
	}
}

func TestGreenhouseFullPipelinePrepublicationReviewRunsFromWatch(t *testing.T) {
	f := newWorkerTabsFixture(t)
	c := newGreenhouseContract(t, f)
	c.env = setEnv(c.env, "GREENHOUSE_HOME", c.home)
	t.Cleanup(func() {
		find := func() ([]isolatedHerdrServerProcess, error) {
			processes, err := findSpawnedProcesses(func(_ []byte, cwd string) bool { return strings.HasPrefix(cwd, f.root) })
			var daemons []isolatedHerdrServerProcess
			for _, process := range processes {
				if strings.Contains(process.Args, "/bin/greenhouse daemon") {
					daemons = append(daemons, process)
				}
			}
			return daemons, err
		}
		for attempt := 0; attempt < 4; attempt++ {
			if err := stopProcesses("Greenhouse daemon", find, 3*time.Second); err != nil {
				t.Errorf("stop fixture Greenhouse daemon: %v", err)
				return
			}
			time.Sleep(250 * time.Millisecond)
			processes, err := find()
			if err != nil || len(processes) == 0 {
				return
			}
		}
		t.Errorf("Greenhouse daemon restarted during cleanup")
	})
	projectDB, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := projectDB.ProjectByID(context.Background(), f.projectID)
	projectDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(f.home, "projects", project.Name)
	if err := os.WriteFile(filepath.Join(projectDir, "config.toml"), []byte("review_provider = \"greenhouse\"\n[defaults]\nreview = \"full-pipeline\"\nmerge_mode = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.change = f.start(t, "t1", "Greenhouse prepublication change", "greenhouse-prepubl")
	head := c.authorCommit(t)
	workerEnv := setEnv(c.env, "POSSE_TASK_ID", c.change.ContainmentMarker)
	workerEnv = setEnv(workerEnv, "HERDR_PANE_ID", c.change.PaneID)
	if output, err := c.posse(workerEnv, c.change.WorktreePath, "signal", "done", "Greenhouse prepublication change", "--head", head); err != nil {
		t.Fatalf("Worker done Signal: %v\n%s", err, output)
	}
	if task := f.task(t, "t1"); task.State != store.StateDone || task.PRURL != "" {
		t.Fatalf("prepublication Task state = %#v", task)
	}
	if output, err := c.posse(c.env, f.repo, "watch", "--poll-only", "--timeout", "3000"); err != nil {
		t.Fatalf("Lead Watch: %v\n%s", err, output)
	}
	if !waitForCondition(60*time.Second, func() bool { return len(c.verdicts(t)) > 0 }) {
		t.Fatal("Watch did not request a Greenhouse Verdict")
	}
	if output, err := c.posse(c.env, f.repo, "watch", "--poll-only", "--timeout", "3000"); err != nil {
		t.Fatalf("Lead Watch follow-up pass: %v\n%s", err, output)
	}
	verdicts := c.verdicts(t)
	verdict := verdicts[len(verdicts)-1]
	if verdict.Mode != "gate" || verdict.Head != head || !verdict.Qualifies(head) {
		t.Fatalf("Watch Verdict = %+v, want passing gate for %s", verdict, head)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "tasks", "t1", "candidate")); err == nil {
		t.Fatal("Greenhouse full-pipeline created a private candidate")
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	var reviews int
	err = db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM tasks WHERE reviews_task_id=?`, c.change.ID).Scan(&reviews)
	var handoffs int
	if err == nil && reviews == 0 {
		err = db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE task_id=? AND body LIKE '%Publish this exact head%'`, c.change.ID).Scan(&handoffs)
	}
	_ = db.Close()
	if err != nil || reviews != 0 || handoffs != 1 {
		t.Fatalf("Review Tasks = %d, publication handoffs = %d (want 1): %v", reviews, handoffs, err)
	}
	if output, err := c.posse(c.env, f.repo, "merge", "t1", "--execute", "--user-approved", "User approved the Greenhouse-reviewed change"); err != nil {
		t.Fatalf("Lead merge: %v\n%s", err, output)
	}
	merged := f.task(t, "t1")
	if merged.State != store.StateMerged || merged.MergedRef != head || merged.GatedSHA != head {
		t.Fatalf("merged head is not the passing Greenhouse head %s: %#v", head, merged)
	}
}

func TestGreenhousePrepublicationReconcilesMultipleDoneTasks(t *testing.T) {
	f := newWorkerTabsFixture(t)
	c := newGreenhouseContract(t, f)
	c.env = setEnv(c.env, "GREENHOUSE_HOME", c.home)
	t.Cleanup(func() {
		find := func() ([]isolatedHerdrServerProcess, error) {
			processes, err := findSpawnedProcesses(func(_ []byte, cwd string) bool { return strings.HasPrefix(cwd, f.root) })
			var daemons []isolatedHerdrServerProcess
			for _, process := range processes {
				if strings.Contains(process.Args, "/bin/greenhouse daemon") {
					daemons = append(daemons, process)
				}
			}
			return daemons, err
		}
		for attempt := 0; attempt < 4; attempt++ {
			if err := stopProcesses("Greenhouse daemon", find, 3*time.Second); err != nil {
				t.Errorf("stop fixture Greenhouse daemon: %v", err)
				return
			}
			time.Sleep(250 * time.Millisecond)
			processes, err := find()
			if err != nil || len(processes) == 0 {
				return
			}
		}
		t.Errorf("Greenhouse daemon restarted during cleanup")
	})
	projectDB, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := projectDB.ProjectByID(context.Background(), f.projectID)
	projectDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(f.home, "projects", project.Name)
	if err := os.WriteFile(filepath.Join(projectDir, "config.toml"), []byte("review_provider = \"greenhouse\"\n[defaults]\nreview = \"full-pipeline\"\nmerge_mode = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.change = f.start(t, "t1", "First Greenhouse prepublication change", "first-greenhouse-prepubl")
	firstHead := c.authorCommit(t)
	firstEnv := setEnv(c.env, "POSSE_TASK_ID", c.change.ContainmentMarker)
	firstEnv = setEnv(firstEnv, "HERDR_PANE_ID", c.change.PaneID)
	if output, err := c.posse(firstEnv, c.change.WorktreePath, "signal", "done", "first change", "--head", firstHead); err != nil {
		t.Fatalf("first Worker done Signal: %v\n%s", err, output)
	}
	firstTask := c.change
	firstDB, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	gateVerdict := store.Verdict{TaskID: firstTask.ID, Repository: project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2", Grant: "shop/t1/shop/preexisting-gate", Head: firstHead, Target: firstHead, ReviewBase: firstHead, Outcome: "clean", Mode: "gate"}
	if recorded, err := firstDB.RecordGreenhouseVerdict(context.Background(), gateVerdict, func(context.Context) (string, error) { return firstHead, nil }); err != nil || !recorded {
		t.Fatalf("record preexisting Gate pass: recorded=%v err=%v", recorded, err)
	}
	parallelVerdict := gateVerdict
	parallelVerdict.Grant = "shop/t1/shop/preexisting-parallel"
	parallelVerdict.Mode = "parallel"
	if recorded, err := firstDB.RecordGreenhouseVerdict(context.Background(), parallelVerdict, func(context.Context) (string, error) { return firstHead, nil }); err != nil || !recorded {
		t.Fatalf("record preexisting parallel Verdict: recorded=%v err=%v", recorded, err)
	}
	now := time.Now().UnixMilli()
	if err := firstDB.RecordGreenhouseReviewRequest(context.Background(), store.GreenhouseReviewRequest{ProjectID: f.projectID, TaskID: firstTask.ID, Repository: project.Name, RequestedHead: firstHead, Provider: store.GreenhouseReviewProvider, RequestedAt: now, SettledAt: now, Outcome: "verdict", Grant: parallelVerdict.Grant}); err != nil {
		t.Fatal(err)
	}
	if err := firstDB.Close(); err != nil {
		t.Fatal(err)
	}
	second := f.start(t, "t2", "Second Greenhouse prepublication change", "second-greenhouse")
	secondPath := filepath.Join(second.WorktreePath, "second-change.txt")
	if err := os.WriteFile(secondPath, []byte("second change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "second-change.txt"}, {"commit", "-q", "-m", "second author change"}, {"rev-parse", "HEAD"}} {
		command := exec.Command("git", args...)
		command.Dir, command.Env = second.WorktreePath, f.env
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		if args[0] == "rev-parse" {
			secondHead := strings.TrimSpace(string(output))
			secondEnv := setEnv(c.env, "POSSE_TASK_ID", second.ContainmentMarker)
			secondEnv = setEnv(secondEnv, "HERDR_PANE_ID", second.PaneID)
			if result, err := c.posse(secondEnv, second.WorktreePath, "signal", "done", "second change", "--head", secondHead); err != nil {
				t.Fatalf("second Worker done Signal: %v\n%s", err, result)
			}
		}
	}
	if output, err := c.posse(c.env, f.repo, "watch", "--poll-only", "--timeout", "3000"); err != nil {
		t.Fatalf("Lead Watch: %v\n%s", err, output)
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if !waitForCondition(60*time.Second, func() bool {
		verdicts, err := db.VerdictsFor(context.Background(), second.ID)
		return err == nil && len(verdicts) > 0
	}) {
		t.Fatalf("Watch did not review the second pending Task; Verdicts=%+v", mustVerdicts(t, db, second.ID))
	}
	var firstGatePass bool
	for _, verdict := range mustVerdicts(t, db, firstTask.ID) {
		firstGatePass = firstGatePass || verdict.Mode == "gate" && verdict.Qualifies(firstHead)
	}
	if !firstGatePass {
		t.Fatalf("first Task has no passing Gate Verdict: %+v", mustVerdicts(t, db, firstTask.ID))
	}
	var failedNotices int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='greenhouse_review_failed'`, firstTask.ID).Scan(&failedNotices); err != nil || failedNotices != 0 {
		t.Fatalf("passing first Task has %d Greenhouse failure notices, err=%v", failedNotices, err)
	}
}

func mustVerdicts(t *testing.T, db *store.DB, taskID int64) []store.Verdict {
	t.Helper()
	verdicts, err := db.VerdictsFor(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	return verdicts
}

func TestGreenhousePrepublicationFindingsQueueReviewFix(t *testing.T) {
	f := newWorkerTabsFixture(t)
	c := newGreenhouseContract(t, f)
	c.env = setEnv(c.env, "GREENHOUSE_HOME", c.home)
	t.Cleanup(func() {
		find := func() ([]isolatedHerdrServerProcess, error) {
			processes, err := findSpawnedProcesses(func(_ []byte, cwd string) bool { return strings.HasPrefix(cwd, f.root) })
			var daemons []isolatedHerdrServerProcess
			for _, process := range processes {
				if strings.Contains(process.Args, "/bin/greenhouse daemon") {
					daemons = append(daemons, process)
				}
			}
			return daemons, err
		}
		for attempt := 0; attempt < 4; attempt++ {
			if err := stopProcesses("Greenhouse daemon", find, 3*time.Second); err != nil {
				t.Errorf("stop fixture Greenhouse daemon: %v", err)
				return
			}
			time.Sleep(250 * time.Millisecond)
			processes, err := find()
			if err != nil || len(processes) == 0 {
				return
			}
		}
		t.Errorf("Greenhouse daemon restarted during cleanup")
	})
	projectDB, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := projectDB.ProjectByID(context.Background(), f.projectID)
	projectDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(f.home, "projects", project.Name)
	if err := os.WriteFile(filepath.Join(projectDir, "config.toml"), []byte("review_provider = \"greenhouse\"\n[defaults]\nreview = \"full-pipeline\"\nmerge_mode = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.change = f.start(t, "t1", "Greenhouse findings change", "greenhouse-findings")
	head := c.authorCommit(t)
	c.setMode(t, "findings")
	workerEnv := setEnv(c.env, "POSSE_TASK_ID", c.change.ContainmentMarker)
	workerEnv = setEnv(workerEnv, "HERDR_PANE_ID", c.change.PaneID)
	if output, err := c.posse(workerEnv, c.change.WorktreePath, "signal", "done", "Greenhouse findings change", "--head", head); err != nil {
		t.Fatalf("Worker done Signal: %v\n%s", err, output)
	}
	if output, err := c.posse(c.env, f.repo, "watch", "--poll-only", "--timeout", "3000"); err != nil {
		t.Fatalf("Lead Watch: %v\n%s", err, output)
	}
	if !waitForCondition(60*time.Second, func() bool { return len(c.verdicts(t)) > 0 }) {
		t.Fatal("Watch did not record a Greenhouse Verdict")
	}
	if output, err := c.posse(c.env, f.repo, "watch", "--poll-only", "--timeout", "3000"); err != nil {
		t.Fatalf("Lead Watch follow-up pass: %v\n%s", err, output)
	}
	verdicts := c.verdicts(t)
	if len(verdicts) == 0 || verdicts[len(verdicts)-1].Outcome != "findings" || verdicts[len(verdicts)-1].Blocking == 0 {
		t.Fatalf("Watch did not record blocking Greenhouse findings: %+v", verdicts)
	}
	disputeDB, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disputeDB.ExecContext(context.Background(), `UPDATE provider_findings SET state='disputed' WHERE task_id=? AND head_sha=?`, c.change.ID, head); err != nil {
		disputeDB.Close()
		t.Fatal(err)
	}
	if err := disputeDB.Close(); err != nil {
		t.Fatal(err)
	}
	startOutput, startErr := c.posse(c.env, f.repo, "review", "start", "t1")
	disputePath := filepath.Join(f.home, "projects", "shop", "tasks", "t1", "greenhouse", "disputes", "shop-"+head+".md")
	if startErr == nil || strings.Contains(startOutput, "requested") || !strings.Contains(startOutput, disputePath) {
		t.Fatalf("review start did not refuse with the exact-head dispute path: err=%v output=%s want_path=%s", startErr, startOutput, disputePath)
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	var rounds, findingCount, reviewTasks int
	var body, status string
	err = db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='review'`, c.change.ID).Scan(&rounds)
	if err == nil {
		err = db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM provider_findings WHERE task_id=?`, c.change.ID).Scan(&findingCount)
	}
	if err == nil {
		err = db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM tasks WHERE reviews_task_id=?`, c.change.ID).Scan(&reviewTasks)
	}
	if err == nil {
		err = db.QueryRowContext(context.Background(), `SELECT body,status FROM messages WHERE task_id=? AND body LIKE '%whole-pattern Review fix%' ORDER BY id DESC LIMIT 1`, c.change.ID).Scan(&body, &status)
	}
	_ = db.Close()
	if err != nil || rounds != 1 || findingCount != 1 || reviewTasks != 0 || status != "queued" && status != "delivered" || !strings.Contains(body, "P-1") {
		t.Fatalf("provider Fix Round = rounds %d findings %d Review Tasks %d body %q status %q err=%v", rounds, findingCount, reviewTasks, body, status, err)
	}
}

func TestGreenhouseAdapterContract(t *testing.T) {
	f := newWorkerTabsFixture(t)
	c := newGreenhouseContract(t, f)
	c.change = f.start(t, "t1", "Greenhouse contract change", "greenhouse-change")
	bystander := f.start(t, "t2", "Greenhouse bystander", "greenhouse-bystander")

	t.Run("a Worker never calls Greenhouse", func(t *testing.T) {
		worker := setEnv(c.env, "POSSE_TASK_ID", c.change.ContainmentMarker)
		output, err := c.posse(worker, c.change.WorktreePath, "_greenhouse", "review", "t1")
		if err == nil || !strings.Contains(output, "worker_forbidden") && !strings.Contains(output, "lead_only") {
			t.Fatalf("a Worker reached Greenhouse: %v\n%s", err, output)
		}
		if _, err := os.Stat(filepath.Join(c.home, "pid")); err == nil {
			t.Fatal("a refused Worker call started the Greenhouse daemon")
		}
	})

	t.Run("clean review records an exact-head Verdict from a Posse-placed daemon", func(t *testing.T) {
		head := c.authorCommit(t)
		result := c.review(t)
		c.requireRecorded(t, result, head, "clean", 0)
		info, err := os.Stat(c.home)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("GREENHOUSE_HOME %s mode = %v, %v; want 0700", c.home, info.Mode().Perm(), err)
		}
		pid := c.daemonPID(t)
		environ, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(environ), "\x00GREENHOUSE_HOME="+c.home+"\x00") || strings.Contains(string(environ), "POSSE_TASK_ID=") {
			t.Fatalf("daemon %d environment lacks the Posse-owned home or carries a Task marker", pid)
		}
		for _, task := range []store.Task{c.f.task(t, "t1"), bystander} {
			if task.ContainmentMode == "cgroup" && e2eProcessInTaskCgroup(pid, task.ContainmentPath) {
				t.Fatalf("daemon %d runs in Task cgroup %s", pid, task.ContainmentPath)
			}
		}
		doctor := runPosse(t, f.binary, f.repo, c.env, "doctor", "--json")
		if !strings.Contains(doctor, "Greenhouse daemon") || !strings.Contains(doctor, "speaks protocol 1-2; Posse uses 2") {
			t.Fatalf("doctor does not report the daemon protocol range:\n%s", doctor)
		}
	})

	t.Run("findings count blocking at the frozen block_at", func(t *testing.T) {
		c.setMode(t, "findings")
		head := c.authorCommit(t)
		c.requireRecorded(t, c.review(t), head, "findings", 1)
	})

	t.Run("a reused grant is refused and records no second Verdict", func(t *testing.T) {
		before := c.verdicts(t)
		spent := before[len(before)-1].Grant
		refused := c.hostRequest(t, map[string]any{"op": "review", "grant": spent})
		if refused["outcome"] != "refused" || !strings.Contains(fmt.Sprint(refused["error"]), "grant already used") {
			t.Fatalf("Greenhouse accepted spent grant %s: %v", spent, refused)
		}
		head := c.authorCommit(t)
		workerEnv := setEnv(c.env, "POSSE_TASK_ID", c.change.ContainmentMarker)
		workerEnv = setEnv(workerEnv, "HERDR_PANE_ID", c.change.PaneID)
		if output, err := c.posse(workerEnv, c.change.WorktreePath, "signal", "done", "Fix round complete"); err != nil {
			t.Fatalf("Worker done Signal: %v\n%s", err, output)
		}
		c.setMode(t, "clean")
		result := c.review(t)
		c.requireRecorded(t, result, head, "clean", 0)
		if result["grant"] != "shop/t1/shop/r3" || result["grant"] == spent {
			t.Fatalf("new head used grant %v after spent grant %s; want r3", result["grant"], spent)
		}
		if after := c.verdicts(t); len(after) != len(before)+1 {
			t.Fatalf("Verdicts = %d, want %d", len(after), len(before)+1)
		}
	})

	t.Run("author commits while the review runs", func(t *testing.T) {
		reviewed := c.authorCommit(t)
		c.hold(t)
		invocations := c.invocations(t)
		done := c.reviewAsync(c.env)
		c.waitInvocations(t, invocations+1)
		moved := c.authorCommit(t)
		c.release(t)
		result := c.finish(t, done)
		c.requireRecorded(t, result, moved, "clean", 0)
		if refused, _ := result["refused"].([]any); len(refused) != 1 {
			t.Fatalf("the stale round was not reported: %v", result)
		}
		c.requireNoVerdictFor(t, reviewed)
	})

	t.Run("head moves between the settled result and the insert", func(t *testing.T) {
		reviewed := c.authorCommit(t)
		marker := filepath.Join(f.root, "greenhouse-settled")
		env := setEnv(setEnv(c.env, "POSSE_INTENT_PAUSE_AT", "greenhouse-review:after:settled"), "POSSE_INTENT_PAUSE_FILE", marker)
		done := c.reviewAsync(env)
		if !waitForCondition(60*time.Second, func() bool { _, err := os.Stat(marker); return err == nil }) {
			t.Fatal("posse did not stop between the settled result and the insert")
		}
		moved := c.authorCommit(t)
		if err := os.WriteFile(marker+".continue", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		result := c.finish(t, done)
		refused, _ := result["refused"].([]any)
		if len(refused) != 1 || !strings.Contains(fmt.Sprint(refused[0]), "no longer the Task head") {
			t.Fatalf("Posse's own head comparison did not refuse: %v", result)
		}
		c.requireRecorded(t, result, moved, "clean", 0)
		c.requireNoVerdictFor(t, reviewed)
	})

	t.Run("posse killed between review and reading the result", func(t *testing.T) {
		head := c.authorCommit(t)
		invocations := c.invocations(t)
		output, err := c.posse(setEnv(c.env, "POSSE_INTENT_CRASH_AT", "greenhouse-review:after:review-sent"), f.repo, c.reviewArgs()...)
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 86 {
			t.Fatalf("posse did not stop after sending the review: %v\n%s", err, output)
		}
		result := c.review(t)
		c.requireRecorded(t, result, head, "clean", 0)
		if result["replayed"] != true {
			t.Fatalf("the restarted posse did not replay its review request: %v", result)
		}
		if got := c.invocations(t); got != invocations+1 {
			t.Fatalf("reviewer invocations = %d, want exactly one more than %d", got, invocations)
		}
	})

	t.Run("daemon killed mid-review parks until recovery and spends no new grant", func(t *testing.T) {
		head := c.authorCommit(t)
		c.hold(t)
		invocations := c.invocations(t)
		grants := len(c.hostGrants(t))
		done := c.reviewAsync(c.env)
		c.waitInvocations(t, invocations+1)
		killed := c.daemonPID(t)
		if err := syscall.Kill(killed, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		parked := c.finish(t, done)
		if parked["outcome"] != "recovery-required" || parked["recovery"] == "" {
			t.Fatalf("a killed daemon did not park the run: %v", parked)
		}
		if restarted := c.daemonPID(t); restarted == killed {
			t.Fatalf("posse did not start a new daemon after %d died", killed)
		}
		again := c.review(t)
		if again["outcome"] != "recovery-required" || again["grant"] != parked["grant"] {
			t.Fatalf("posse left the parked round: %v", again)
		}
		if got := len(c.hostGrants(t)); got != grants+1 {
			t.Fatalf("host grants = %d, want %d: a parked run must not receive a new grant", got, grants+1)
		}
		if notices := c.notices(t, "greenhouse_recovery_required"); notices != 1 {
			t.Fatalf("recovery Notices = %d, want 1", notices)
		}
		c.release(t)
		var result map[string]any
		if !waitForCondition(30*time.Second, func() bool {
			result = c.review(t)
			return result["outcome"] == "settled"
		}) {
			t.Fatalf("the recovered review did not settle: %v", result)
		}
		c.requireRecorded(t, result, head, "clean", 0)
		if result["grant"] != parked["grant"] || c.invocations(t) != invocations+1 {
			t.Fatalf("recovery spent another grant or reviewer run: %v, invocations %d", result, c.invocations(t))
		}
	})

	t.Run("reviewer quota pause keeps the grant", func(t *testing.T) {
		head := c.authorCommit(t)
		c.setMode(t, "quota")
		before := len(c.verdicts(t))
		paused := c.review(t)
		if paused["outcome"] != "paused" || len(c.verdicts(t)) != before {
			t.Fatalf("a quota pause recorded a Verdict or did not pause: %v", paused)
		}
		grants := len(c.hostGrants(t))
		c.setMode(t, "clean")
		result := c.review(t)
		c.requireRecorded(t, result, head, "clean", 0)
		if result["grant"] != paused["grant"] || len(c.hostGrants(t)) != grants {
			t.Fatalf("the paused review did not resume under its grant: paused %v, result %v", paused, result)
		}
	})

	t.Run("Task Teardown while another Task's review runs", func(t *testing.T) {
		head := c.authorCommit(t)
		c.hold(t)
		invocations := c.invocations(t)
		done := c.reviewAsync(c.env)
		c.waitInvocations(t, invocations+1)
		daemon := c.daemonPID(t)
		markTaskReported(t, f, bystander)
		f.exitWorker(t, "t2")
		if output := runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t2"); !strings.Contains(output, "torn-down") {
			t.Fatalf("Teardown failed: %s", output)
		}
		if syscall.Kill(daemon, 0) != nil {
			t.Fatalf("Teardown of t2 stopped the Greenhouse daemon %d", daemon)
		}
		c.release(t)
		c.requireRecorded(t, c.finish(t, done), head, "clean", 0)
	})

	t.Run("inconclusive review never records a Verdict", func(t *testing.T) {
		head := c.authorCommit(t)
		c.setMode(t, "inconclusive")
		before := len(c.verdicts(t))
		output, err := c.posse(c.env, c.f.repo, c.reviewArgs()...)
		failure := decodeGreenhouseOutput(t, output)
		message, _ := failure["message"].(string)
		if err == nil || failure["code"] != "greenhouse_failed" || !strings.Contains(message, `Greenhouse terminal status "inconclusive": inconclusive`) {
			t.Fatalf("inconclusive review = %v, %s; want greenhouse_failed with its status and reason", err, output)
		}
		if got := len(c.verdicts(t)); got != before {
			t.Fatalf("inconclusive review recorded a Verdict: before %d, after %d", before, got)
		}
		c.requireNoVerdictFor(t, head)
		c.setMode(t, "clean")
	})

	t.Run("daemon protocol range excludes version 2", func(t *testing.T) {
		c.stopDaemon(t)
		// The protocol 1 store format predates host runs; start it on an empty home.
		if err := os.Rename(c.home, c.home+".protocol-2"); err != nil {
			t.Fatal(err)
		}
		protocolOne := buildGreenhouseProtocolOne(t, f)
		env := setEnv(c.env, "PATH", filepath.Dir(protocolOne)+string(os.PathListSeparator)+testEnvValue(c.env, "PATH"))
		before := len(c.verdicts(t))
		c.authorCommit(t)
		pendingPath := filepath.Join(f.home, "projects", "shop", "tasks", "t1", "greenhouse", "shop.pending.json")
		if err := os.MkdirAll(filepath.Dir(pendingPath), 0o700); err != nil {
			t.Fatal(err)
		}
		pendingRequest := []byte(`{"grant":"shop/t1/shop/r1","request":{}}`)
		if err := os.WriteFile(pendingPath, pendingRequest, 0o600); err != nil {
			t.Fatal(err)
		}
		output, err := c.posse(env, f.repo, c.reviewArgs()...)
		if err == nil || !strings.Contains(output, "greenhouse_protocol") || !strings.Contains(output, "speaks protocol 1 and Posse needs protocol 2") {
			t.Fatalf("posse did not refuse a protocol 1 daemon naming both ranges: %v\n%s", err, output)
		}
		if got := len(c.verdicts(t)); got != before {
			t.Fatalf("a refused daemon produced a Verdict")
		}
		if got, err := os.ReadFile(pendingPath); err != nil || string(got) != string(pendingRequest) {
			t.Fatalf("protocol refusal changed pending request: %s, %v", got, err)
		}
		state, err := os.ReadFile(filepath.Join(c.home, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		var runs struct {
			Runs map[string]any `json:"runs"`
		}
		if err := json.Unmarshal(state, &runs); err != nil || len(runs.Runs) != 0 {
			t.Fatalf("posse downgraded to protocol 1 and created runs: %v %s", err, state)
		}
		doctor := runPosse(t, f.binary, f.repo, env, "doctor", "--json")
		if !strings.Contains(doctor, "speaks protocol 1; Posse needs protocol 2") {
			t.Fatalf("doctor does not report the excluded range:\n%s", doctor)
		}
	})
}

func newGreenhouseContract(t *testing.T, f *workerTabsFixture) *greenhouseContract {
	t.Helper()
	if binary := os.Getenv("POSSE_T1258_GREENHOUSE_BIN"); binary != "" {
		if !filepath.IsAbs(binary) {
			t.Fatalf("POSSE_T1258_GREENHOUSE_BIN must be absolute: %s", binary)
		}
		if err := os.Symlink(binary, filepath.Join(f.root, "bin", "greenhouse")); err != nil {
			t.Fatalf("link real Greenhouse binary: %v", err)
		}
	} else {
		install := exec.Command("go", "install", greenhouseModule+"@"+greenhouseVersion)
		install.Env = setEnv(os.Environ(), "GOBIN", filepath.Join(f.root, "bin"))
		if output, err := install.CombinedOutput(); err != nil {
			t.Fatalf("install pinned Greenhouse: %v\n%s", err, output)
		}
	}
	c := &greenhouseContract{f: f, ctl: filepath.Join(f.root, "home", "greenhouse-reviewer"), home: filepath.Join(f.home, "greenhouse")}
	agent := filepath.Join(f.root, "home", "pi-agent")
	for _, directory := range []string{c.ctl, agent, c.home} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(agent, "models.json"), []byte(`{"providers":{"fixture":{"models":[{"id":"model","reasoning":true}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "bin", "pi"), []byte(fmt.Sprintf(greenhouseFakeReviewer, c.ctl)), 0o700); err != nil {
		t.Fatal(err)
	}
	backends := fmt.Sprintf(`{"version":2,"writable":{"reviewer":[%q]}}`, c.ctl)
	if err := os.WriteFile(filepath.Join(c.home, "backends.json"), []byte(backends), 0o600); err != nil {
		t.Fatal(err)
	}
	c.env = setEnv(f.leadEnv, "PI_CODING_AGENT_DIR", agent)
	t.Cleanup(func() { c.stopDaemon(t) })
	return c
}

func (c *greenhouseContract) reviewArgs() []string {
	return []string{"_greenhouse", "review", "t1", "--reviewer-backend", "pi", "--reviewer-model", "fixture/model", "--timeout", "60000", "--json"}
}

func (c *greenhouseContract) posse(env []string, dir string, args ...string) (string, error) {
	command := exec.Command(c.f.binary, args...)
	command.Dir, command.Env = dir, taskAuthorGenerationEnv(env)
	output, err := command.CombinedOutput()
	return string(output), err
}

func (c *greenhouseContract) review(t *testing.T) map[string]any {
	t.Helper()
	output, err := c.posse(c.env, c.f.repo, c.reviewArgs()...)
	if err != nil {
		t.Fatalf("posse _greenhouse review: %v\n%s", err, output)
	}
	return decodeGreenhouseOutput(t, output)
}

type greenhouseRun struct {
	output []byte
	err    error
}

func (c *greenhouseContract) reviewAsync(env []string) <-chan greenhouseRun {
	done := make(chan greenhouseRun, 1)
	go func() {
		output, err := c.posse(env, c.f.repo, c.reviewArgs()...)
		done <- greenhouseRun{[]byte(output), err}
	}()
	return done
}

func (c *greenhouseContract) finish(t *testing.T, done <-chan greenhouseRun) map[string]any {
	t.Helper()
	select {
	case run := <-done:
		if run.err != nil {
			t.Fatalf("posse _greenhouse review: %v\n%s", run.err, run.output)
		}
		return decodeGreenhouseOutput(t, string(run.output))
	case <-time.After(90 * time.Second):
		t.Fatal("posse _greenhouse review did not return")
		return nil
	}
}

func decodeGreenhouseOutput(t *testing.T, output string) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode review output: %v\n%s", err, output)
	}
	return result
}

func (c *greenhouseContract) requireRecorded(t *testing.T, result map[string]any, head, verdict string, blocking int) {
	t.Helper()
	if result["outcome"] != "settled" || result["head"] != head || result["verdict"] != verdict || result["recorded"] != true {
		t.Fatalf("review result = %v; want a recorded %s Verdict for %s", result, verdict, head)
	}
	verdicts := c.verdicts(t)
	last := verdicts[len(verdicts)-1]
	if last.Grant != result["grant"] || last.Head != head || last.Provider != store.GreenhouseReviewProvider || last.Mode != "parallel" || last.Outcome != verdict || last.Blocking != blocking || last.Protocol != "2" || last.Target == "" || last.ReviewBase == "" {
		t.Fatalf("recorded Verdict = %+v; want grant %v head %s %s with %d blocking", last, result["grant"], head, verdict, blocking)
	}
	findings, err := os.ReadFile(last.FindingsPath)
	if err != nil || len(findings) == 0 {
		t.Fatalf("findings file %s: %v", last.FindingsPath, err)
	}
}

func (c *greenhouseContract) requireNoVerdictFor(t *testing.T, head string) {
	t.Helper()
	for _, verdict := range c.verdicts(t) {
		if verdict.Head == head {
			t.Fatalf("a Verdict was recorded for head %s, which moved before it qualified: %+v", head, verdict)
		}
	}
}

func (c *greenhouseContract) verdicts(t *testing.T) []store.Verdict {
	t.Helper()
	db, err := store.OpenReadOnly(c.f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	verdicts, err := db.VerdictsFor(context.Background(), c.change.ID)
	if err != nil {
		t.Fatal(err)
	}
	return verdicts
}

func (c *greenhouseContract) notices(t *testing.T, kind string) int {
	t.Helper()
	db, err := store.OpenReadOnly(c.f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind=?`, c.change.ID, kind).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func (c *greenhouseContract) authorCommit(t *testing.T) string {
	t.Helper()
	c.commit++
	name := fmt.Sprintf("greenhouse-change-%d.txt", c.commit)
	if err := os.WriteFile(filepath.Join(c.change.WorktreePath, name), []byte(fmt.Sprintf("change %d\n", c.commit)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", name}, {"commit", "-q", "-m", "author change " + strconv.Itoa(c.commit)}} {
		command := exec.Command("git", args...)
		command.Dir, command.Env = c.change.WorktreePath, c.f.env
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	command := exec.Command("git", "rev-parse", "HEAD")
	command.Dir, command.Env = c.change.WorktreePath, c.f.env
	head, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(head))
}

func (c *greenhouseContract) setMode(t *testing.T, mode string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(c.ctl, "mode"), []byte(mode), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (c *greenhouseContract) hold(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(c.ctl, "hold"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (c *greenhouseContract) release(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(c.ctl, "hold")); err != nil {
		t.Fatal(err)
	}
}

func (c *greenhouseContract) invocations(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(c.ctl, "invocations"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func (c *greenhouseContract) waitInvocations(t *testing.T, want int) {
	t.Helper()
	if !waitForCondition(60*time.Second, func() bool { return c.invocations(t) >= want }) {
		t.Fatalf("reviewer invocations = %d, want %d", c.invocations(t), want)
	}
}

func (c *greenhouseContract) daemonPID(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(c.home, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func (c *greenhouseContract) stopDaemon(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(c.home, "pid"))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid < 2 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	if !waitForCondition(5*time.Second, func() bool { return syscall.Kill(pid, 0) != nil }) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		waitForCondition(3*time.Second, func() bool { return syscall.Kill(pid, 0) != nil })
	}
}

// hostRequest sends one host request for t1's run directly, as a host that
// misbehaves would.
func (c *greenhouseContract) hostRequest(t *testing.T, extra map[string]any) map[string]any {
	t.Helper()
	host := map[string]any{"name": "posse", "id": "shop/t1/shop"}
	current := c.greenhouse(t, map[string]any{"version": 2, "op": "attach", "host": host})
	request := map[string]any{"version": 2, "host": host, "run_id": current["run_id"], "input_id": current["input_id"], "gate_id": current["gate_id"], "expected": current["token"], "action_id": "contract-" + strconv.FormatInt(time.Now().UnixNano(), 10), "reason": "contract test"}
	for key, value := range extra {
		request[key] = value
	}
	return c.greenhouse(t, request)
}

func (c *greenhouseContract) hostGrants(t *testing.T) []any {
	t.Helper()
	inspected := c.greenhouse(t, map[string]any{"version": 2, "op": "inspect"})
	run, _ := inspected["run"].(map[string]any)
	grants, _ := run["host_grants"].([]any)
	return grants
}

func (c *greenhouseContract) greenhouse(t *testing.T, request map[string]any) map[string]any {
	t.Helper()
	projectDB, err := store.OpenReadOnly(c.f.home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := projectDB.ProjectByID(context.Background(), c.f.projectID)
	projectDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(c.f.home, "scratch", project.Name, "t"+strconv.Itoa(c.change.Seq), "greenhouse-review", project.Name, "worktree")
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("pinned Greenhouse review checkout %s: %v", worktree, err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(c.f.root, "bin", "greenhouse"), "axi")
	command.Dir = worktree
	command.Env = setEnv(c.env, "GREENHOUSE_HOME", c.home)
	command.Stdin = strings.NewReader(string(encoded))
	output, err := command.Output()
	if err != nil {
		t.Fatalf("greenhouse axi %s: %v\n%s", encoded, err, output)
	}
	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("greenhouse axi output: %v\n%s", err, output)
	}
	return response
}

// buildGreenhouseProtocolOne builds the last real Greenhouse whose daemon
// speaks only protocol 1. Its module path predates the GitHub one, so the Go
// module proxy cannot serve it.
func buildGreenhouseProtocolOne(t *testing.T, f *workerTabsFixture) string {
	t.Helper()
	source := filepath.Join(f.root, "greenhouse-protocol-1")
	binary := filepath.Join(f.root, "greenhouse-protocol-1-bin", "greenhouse")
	for _, args := range [][]string{
		{"init", "-q", source},
		{"-C", source, "fetch", "-q", "--depth", "1", "https://github.com/Thanhbinh1905/greenhouse.git", greenhouseProtocolOneCommit},
		{"-C", source, "checkout", "-q", "FETCH_HEAD"},
	} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = source
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build protocol 1 Greenhouse: %v\n%s", err, output)
	}
	return binary
}

// TestGreenhouseDaemonStopsAfterDownOnceReviewsSettle covers section 2.4:
// `posse down` of the last up Project stops the Posse-owned daemon only after
// the review it is running settles.
func TestGreenhouseDaemonStopsAfterDownOnceReviewsSettle(t *testing.T) {
	f := newWorkerTabsFixture(t)
	c := newGreenhouseContract(t, f)
	c.change = f.start(t, "t1", "Greenhouse shutdown change", "greenhouse-shutdown")
	head := c.authorCommit(t)
	c.hold(t)
	done := c.reviewAsync(c.env)
	c.waitInvocations(t, 1)
	daemon := c.daemonPID(t)

	markTaskReported(t, f, f.task(t, "t1"))
	f.exitWorker(t, "t1")
	output := runPosse(t, f.binary, f.repo, f.userPaneEnv(f.userBefore), "down")
	if !strings.Contains(output, "greenhouse_daemon: stops after its reviews settle") {
		t.Fatalf("down did not hand the busy daemon to the stop waiter:\n%s", output)
	}
	if waitForCondition(2*time.Second, func() bool { return syscall.Kill(daemon, 0) != nil }) {
		t.Fatal("down stopped the Greenhouse daemon while its review ran")
	}
	c.release(t)
	c.requireRecorded(t, c.finish(t, done), head, "clean", 0)
	if !waitForCondition(20*time.Second, func() bool { return syscall.Kill(daemon, 0) != nil }) {
		t.Fatalf("the Greenhouse daemon %d outlived its settled review after down", daemon)
	}
}

// TestGreenhouseDaemonDownKeepsDaemonWhileAnotherProjectIsUp covers the
// daemon shared by the Projects of one POSSE_HOME.
func TestGreenhouseDaemonDownKeepsDaemonWhileAnotherProjectIsUp(t *testing.T) {
	f := newWorkerTabsFixture(t)
	c := newGreenhouseContract(t, f)
	c.change = f.start(t, "t1", "Greenhouse shared daemon change", "greenhouse-shared")
	c.authorCommit(t)
	if result := c.review(t); result["outcome"] != "settled" {
		t.Fatalf("review = %v", result)
	}
	daemon := c.daemonPID(t)
	other := filepath.Join(f.root, "other")
	initRepository(t, other, filepath.Join(f.root, "other-origin.git"), f.env)
	runPosse(t, f.binary, other, f.env, "project", "add", "--name", "other")
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE projects SET lead_pane_id='other-lead', down_at=0 WHERE name='other'`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	markTaskReported(t, f, f.task(t, "t1"))
	f.exitWorker(t, "t1")
	output := runPosse(t, f.binary, f.repo, f.userPaneEnv(f.userBefore), "down")
	if !strings.Contains(output, "greenhouse_daemon: \"kept: Project other is up\"") && !strings.Contains(output, "greenhouse_daemon: kept: Project other is up") {
		t.Fatalf("down did not keep the daemon shared with an up Project:\n%s", output)
	}
	if syscall.Kill(daemon, 0) != nil {
		t.Fatalf("down stopped the Greenhouse daemon %d that Project other still uses", daemon)
	}
}
