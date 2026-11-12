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
request = json.load(sys.stdin)
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
		c.setMode(t, "clean")
		result := c.review(t)
		c.requireRecorded(t, result, head, "clean", 0)
		if result["grant"] == spent {
			t.Fatalf("Posse reused grant %s", spent)
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

	t.Run("inconclusive verdict with zero findings never satisfies the gate", func(t *testing.T) {
		head := c.authorCommit(t)
		c.setMode(t, "inconclusive")
		c.requireRecorded(t, c.review(t), head, "inconclusive", 0)
		verdicts := c.verdicts(t)
		gate := verdicts[len(verdicts)-1]
		gate.Mode = "gate"
		if gate.Qualifies(head) {
			t.Fatalf("an inconclusive Verdict with blocking = 0 qualifies: %+v", gate)
		}
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
		output, err := c.posse(env, f.repo, c.reviewArgs()...)
		if err == nil || !strings.Contains(output, "greenhouse_protocol") || !strings.Contains(output, "speaks protocol 1 and Posse needs protocol 2") {
			t.Fatalf("posse did not refuse a protocol 1 daemon naming both ranges: %v\n%s", err, output)
		}
		if got := len(c.verdicts(t)); got != before {
			t.Fatalf("a refused daemon produced a Verdict")
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
	install := exec.Command("go", "install", greenhouseModule+"@"+greenhouseVersion)
	install.Env = setEnv(os.Environ(), "GOBIN", filepath.Join(f.root, "bin"))
	if output, err := install.CombinedOutput(); err != nil {
		t.Fatalf("install pinned Greenhouse: %v\n%s", err, output)
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
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(c.f.root, "bin", "greenhouse"), "axi")
	command.Dir = c.change.WorktreePath
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
