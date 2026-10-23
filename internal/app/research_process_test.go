package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/research"
	"github.com/thanhbinh1905/posse/internal/store"
)

// processResearch runs process-launched children in-process with the fake
// harness binaries from testdata/research on PATH.
type processResearch struct {
	*researchFixture
	log     string
	started []int64
	mu      sync.Mutex
	wg      sync.WaitGroup
	version map[research.Harness]string
}

func newProcessResearch(t *testing.T) *processResearch {
	t.Helper()
	bin, err := filepath.Abs(filepath.Join("testdata", "research"))
	if err != nil {
		t.Fatal(err)
	}
	f := &processResearch{researchFixture: newResearchFixture(t), version: map[research.Harness]string{research.Codex: "codex-cli 0.163.0", research.Pi: "1.3.0"}}
	f.identities[os.Getpid()] = "boot/test-runner"
	f.log = filepath.Join(f.root, "harness.log")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_RESEARCH_LOG", f.log)
	t.Setenv("CODEX_HOME", filepath.Join(f.root, "codex-home"))
	t.Setenv("POSSE_HOME", f.home)
	previousVersion, previousStart, previousVerify := researchHarnessVersion, startResearchRunner, codexVerifyTimeout
	researchHarnessVersion = func(_ context.Context, harness research.Harness) string { return f.version[harness] }
	codexVerifyTimeout = 3 * time.Second
	startResearchRunner = func(home string, id int64) error {
		f.mu.Lock()
		f.started = append(f.started, id)
		f.mu.Unlock()
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			if err := f.service.runResearchAttempt(context.Background(), id); err != nil {
				t.Errorf("runner %d: %v", id, err)
			}
		}()
		return nil
	}
	t.Cleanup(func() {
		f.wg.Wait()
		researchHarnessVersion, startResearchRunner, codexVerifyTimeout = previousVersion, previousStart, previousVerify
	})
	return f
}

func (f *processResearch) runs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

func (f *processResearch) harnessLog() string {
	data, _ := os.ReadFile(f.log)
	return string(data)
}

func (f *processResearch) attempt(id int64) store.ResearchAttempt {
	f.t.Helper()
	attempt, err := f.db.ResearchAttempt(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return attempt
}

func (f *processResearch) waitState(id int64, state string) store.ResearchAttempt {
	f.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		attempt := f.attempt(id)
		if attempt.State == state {
			return attempt
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("attempt %d state=%s want %s: %+v", id, attempt.State, state, attempt)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCodexResearchThreadsAreVerifiedReadOnlyAndFollowedUp(t *testing.T) {
	f := newProcessResearch(t)
	code, output := runCLI(t, f.service, "research", "start", "--harness", "codex", "--scope", f.repo, "--prompt", "map the store", "--wait", "--timeout", "20000")
	if code != 0 || !strings.Contains(output, "completed") || !strings.Contains(output, "answer to: map the store") {
		t.Fatalf("start: %d %s", code, output)
	}
	first := f.attempts()[0]
	if first.ChildID != "0199a213-81c0-7800-8aa1-bbab2a035a53" || first.ParentRole != "lead" || first.Scope != f.repo || first.HarnessVersion != "0.163.0" || !first.Resumable {
		t.Fatalf("first %+v", first)
	}
	if usage := research.ParseUsage(first.Usage); usage.InputTokens != 120 || usage.OutputTokens != 30 || usage.Turns != 1 {
		t.Fatalf("usage %s", first.Usage)
	}
	invocation := f.harnessLog()
	for _, want := range []string{"exec --json", "--sandbox read-only", `sandbox_mode="read-only"`, `approval_policy="never"`, "--cd " + f.repo} {
		if !strings.Contains(invocation, want) {
			t.Fatalf("codex invocation %q missing %q", invocation, want)
		}
	}
	// The Lead's own danger-full-access sandbox is never inherited.
	if strings.Contains(invocation, "danger") || strings.Contains(invocation, "bypass") {
		t.Fatalf("codex child inherited a write-capable setting: %s", invocation)
	}

	// A follow-up continues the same thread as a linked attempt.
	code, output = runCLI(t, f.service, "research", "follow-up", research.AttemptID(first.ID), "--prompt", "and the queue", "--wait", "--timeout", "20000")
	if code != 0 || !strings.Contains(output, "answer to: and the queue") {
		t.Fatalf("follow-up: %d %s", code, output)
	}
	second := f.attempts()[0]
	if second.PreviousID != first.ID || second.ChildID != first.ChildID || second.State != "completed" {
		t.Fatalf("follow-up %+v", second)
	}
	if !strings.Contains(f.harnessLog(), "exec resume --json --skip-git-repo-check -c sandbox_mode=\"read-only\" -c approval_policy=\"never\" "+first.ChildID+" -") {
		t.Fatalf("resume invocation %s", f.harnessLog())
	}
	// A repeated keyed start returns the same attempt without another child.
	for range 2 {
		if code, output := runCLI(t, f.service, "research", "start", "--harness", "codex", "--scope", f.repo, "--prompt", "again", "--key", "call-7", "--wait", "--timeout", "20000"); code != 0 {
			t.Fatal(output)
		}
	}
	if f.runs() != 3 {
		t.Fatalf("runner starts=%d want 3", f.runs())
	}
}

func TestCodexResearchRefusesInheritedDangerAndUnsupportedVersions(t *testing.T) {
	f := newProcessResearch(t)
	t.Setenv("FAKE_CODEX_SANDBOX", "danger-full-access")
	code, output := runCLI(t, f.service, "research", "start", "--harness", "codex", "--scope", f.repo, "--prompt", "map", "--wait", "--timeout", "20000")
	if code != 0 || !strings.Contains(output, "refused") {
		t.Fatalf("danger start: %d %s", code, output)
	}
	refused := f.attempts()[0]
	if refused.State != "refused" || refused.ResultPath != "" || !strings.Contains(refused.Note, "danger-full-access") {
		t.Fatalf("danger attempt %+v", refused)
	}

	f.version[research.Codex] = "codex-cli 0.120.0"
	code, output = runCLI(t, f.service, "research", "start", "--harness", "codex", "--scope", f.repo, "--prompt", "map")
	if code == 0 || !strings.Contains(output, "research_unsupported") {
		t.Fatalf("old codex: %d %s", code, output)
	}
	f.version[research.Codex] = ""
	if code, output := runCLI(t, f.service, "research", "start", "--harness", "codex", "--scope", f.repo, "--prompt", "map"); code == 0 || !strings.Contains(output, "research_unsupported") {
		t.Fatalf("unknown codex: %d %s", code, output)
	}
	if f.runs() != 1 {
		t.Fatalf("an unsupported Codex launched a child: runs=%d", f.runs())
	}
	if code, output := runCLI(t, f.service, "research", "start", "--harness", "codex", "--scope", t.TempDir(), "--prompt", "map"); code == 0 || !strings.Contains(output, "research_scope_refused") {
		t.Fatalf("foreign scope: %d %s", code, output)
	}
	if code, output := runCLI(t, f.service, "research", "start", "--harness", "codex", "--prompt", "map"); code == 0 || !strings.Contains(output, "--scope") {
		t.Fatalf("missing scope: %d %s", code, output)
	}
	if code, output := runCLI(t, f.service, "research", "start", "--harness", "claude", "--scope", f.repo, "--prompt", "map"); code == 0 || !strings.Contains(output, "Agent tool") {
		t.Fatalf("claude start: %d %s", code, output)
	}
	states := map[string]int{}
	for _, attempt := range f.attempts() {
		states[attempt.State]++
	}
	if states["refused"] != 4 {
		t.Fatalf("refusals are not all recorded: %v", states)
	}
}

func TestCodexResearchCancelBudgetAndRunnerLoss(t *testing.T) {
	f := newProcessResearch(t)
	if code, output := runCLI(t, f.service, "config", "set", "research.max_active", "1"); code != 0 {
		t.Fatal(output)
	}
	t.Setenv("FAKE_CODEX_MODE", "sleep")
	code, output := runCLI(t, f.service, "research", "start", "--harness", "codex", "--scope", f.repo, "--prompt", "long")
	if code != 0 {
		t.Fatal(output)
	}
	running := f.attempts()[0]
	running = f.waitState(running.ID, "running")
	deadline := time.Now().Add(10 * time.Second)
	for running.ChildPID == 0 || running.ChildID == "" {
		if time.Now().After(deadline) {
			t.Fatalf("child never recorded: %+v", running)
		}
		time.Sleep(20 * time.Millisecond)
		running = f.attempt(running.ID)
	}
	if code, output := runCLI(t, f.service, "research", "start", "--harness", "codex", "--scope", f.repo, "--prompt", "second"); code == 0 || !strings.Contains(output, "research_budget") {
		t.Fatalf("budget: %d %s", code, output)
	}
	code, output = runCLI(t, f.service, "research", "cancel", research.AttemptID(running.ID))
	if code != 0 || !strings.Contains(output, "cancelled") {
		t.Fatalf("cancel: %d %s", code, output)
	}
	f.wg.Wait()
	cancelled := f.attempt(running.ID)
	if cancelled.State != "cancelled" || cancelled.ResultPath != "" {
		t.Fatalf("cancelled %+v", cancelled)
	}
	if researchProcessAlive(cancelled.ChildPID, cancelled.ChildIdentity) {
		t.Fatalf("cancelled child %d still runs", cancelled.ChildPID)
	}

	// A runner that died with its child leaves a resumable interrupted thread.
	lost, _, err := f.db.CreateResearchAttempt(context.Background(), store.ResearchAttempt{ProjectID: f.project.ID, ParentRole: "lead", Harness: "codex", Scope: f.repo, State: "registered"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.identities[424242] = ""
	pid, identity, child := 424242, "boot/gone", "0199a213-81c0-7800-8aa1-bbab2a035a53"
	if _, err := f.db.UpdateResearchAttempt(context.Background(), lost.ID, nil, store.ResearchChange{State: researchState(research.Running), RunnerPID: &pid, RunnerIdentity: &identity, ChildID: &child}); err != nil {
		t.Fatal(err)
	}
	if code, output := runCLI(t, f.service, "research", "list", "--all"); code != 0 || !strings.Contains(output, "interrupted") {
		t.Fatalf("list: %d %s", code, output)
	}
	interrupted := f.attempt(lost.ID)
	if interrupted.State != "interrupted" || !interrupted.Resumable {
		t.Fatalf("lost runner %+v", interrupted)
	}
	t.Setenv("FAKE_CODEX_MODE", "")
	if code, output := runCLI(t, f.service, "research", "follow-up", research.AttemptID(lost.ID), "--prompt", "resume", "--wait", "--timeout", "20000"); code != 0 || !strings.Contains(output, "completed") {
		t.Fatalf("resume lost thread: %d %s", code, output)
	}
}

func TestCodexResearchBoundsOutput(t *testing.T) {
	f := newProcessResearch(t)
	for _, setting := range [][]string{{"research.max_result_bytes", "16"}, {"research.max_output_bytes", "4096"}} {
		if code, output := runCLI(t, f.service, "config", "set", setting[0], setting[1]); code != 0 {
			t.Fatal(output)
		}
	}
	t.Setenv("FAKE_CODEX_MODE", "flood")
	code, output := runCLI(t, f.service, "research", "start", "--harness", "codex", "--scope", f.repo, "--prompt", "flood", "--wait", "--timeout", "20000")
	if code != 0 || !strings.Contains(output, "failed") {
		t.Fatalf("flood: %d %s", code, output)
	}
	flooded := f.attempts()[0]
	if !strings.Contains(flooded.Note, "max_output_bytes") || flooded.ResultPath != "" {
		t.Fatalf("flooded %+v", flooded)
	}
	if info, err := os.Stat(filepath.Join(researchDirectory(f.home, f.project.Name, flooded.ID), "events.jsonl")); err != nil || info.Size() > 4096 {
		t.Fatalf("events log size %v err=%v", info, err)
	}
	t.Setenv("FAKE_CODEX_MODE", "")
	code, output = runCLI(t, f.service, "research", "start", "--harness", "codex", "--scope", f.repo, "--prompt", "a long prompt that yields a long answer", "--wait", "--timeout", "20000")
	if code != 0 {
		t.Fatal(output)
	}
	truncated := f.attempts()[0]
	if truncated.State != "completed" || !truncated.ResultTruncated || truncated.ResultBytes != 16 {
		t.Fatalf("truncated %+v", truncated)
	}
}

func TestPiResearchPersistsSessionIdentityAndVerifiesResume(t *testing.T) {
	f := newProcessResearch(t)
	code, output := runCLI(t, f.service, "research", "start", "--harness", "pi", "--scope", f.repo, "--prompt", "map", "--key", "pi-tool:s1:call-1", "--wait", "--timeout", "20000")
	if code != 0 || !strings.Contains(output, "pi answer to: map") {
		t.Fatalf("start: %d %s", code, output)
	}
	first := f.attempts()[0]
	dir := researchDirectory(f.home, f.project.Name, first.ID)
	if first.State != "completed" || first.ChildID == "" || !strings.HasPrefix(first.SessionRef, filepath.Join(dir, "sessions")) || !regularFileExists(first.SessionRef) || !first.Resumable || first.Model != "fake-model" {
		t.Fatalf("first %+v", first)
	}
	if usage := research.ParseUsage(first.Usage); usage.InputTokens != 20 || usage.CostUSD != 0.002 {
		t.Fatalf("usage %s", first.Usage)
	}
	invocation := f.harnessLog()
	for _, want := range []string{"--mode json -p", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-mcp", "--no-approve", "--tools read,grep,find,ls", "--session-id " + first.ChildID} {
		if !strings.Contains(invocation, want) {
			t.Fatalf("pi invocation %q missing %q", invocation, want)
		}
	}
	if strings.Contains(invocation, "--no-session") {
		t.Fatalf("pi child ran without a persistent session: %s", invocation)
	}
	// A reloaded parent repeating the same tool call reuses the attempt.
	if code, output := runCLI(t, f.service, "research", "start", "--harness", "pi", "--scope", f.repo, "--prompt", "map", "--key", "pi-tool:s1:call-1", "--wait"); code != 0 || f.runs() != 1 {
		t.Fatalf("repeated start: runs=%d %s", f.runs(), output)
	}

	// Resume continues the same Posse-owned session file.
	code, output = runCLI(t, f.service, "research", "follow-up", research.AttemptID(first.ID), "--prompt", "deeper", "--wait", "--timeout", "20000")
	if code != 0 || !strings.Contains(output, "pi answer to: deeper") {
		t.Fatalf("follow-up: %d %s", code, output)
	}
	second := f.attempts()[0]
	if second.PreviousID != first.ID || second.ChildID != first.ChildID || second.SessionRef != first.SessionRef || second.State != "completed" {
		t.Fatalf("second %+v", second)
	}
	if !strings.Contains(f.harnessLog(), "--session "+first.SessionRef) {
		t.Fatalf("resume invocation %s", f.harnessLog())
	}

	// A resume that does not continue the session is reported interrupted.
	t.Setenv("FAKE_PI_MODE", "no-append")
	code, output = runCLI(t, f.service, "research", "follow-up", research.AttemptID(second.ID), "--prompt", "again", "--wait", "--timeout", "20000")
	if code != 0 || !strings.Contains(output, "interrupted") || strings.Contains(output, "result_text") {
		t.Fatalf("unverified resume: %d %s", code, output)
	}

	// A child that opens a different session is refused.
	t.Setenv("FAKE_PI_MODE", "wrong-session")
	code, output = runCLI(t, f.service, "research", "start", "--harness", "pi", "--scope", f.repo, "--prompt", "map", "--wait", "--timeout", "20000")
	if code != 0 || !strings.Contains(output, "refused") || !strings.Contains(output, "Posse-owned session") {
		t.Fatalf("wrong session: %d %s", code, output)
	}
}

func TestPiResearchWithoutPersistedSessionCannotResume(t *testing.T) {
	f := newProcessResearch(t)
	ctx := context.Background()
	lost, _, err := f.db.CreateResearchAttempt(ctx, store.ResearchAttempt{ProjectID: f.project.ID, ParentRole: "lead", Harness: "pi", Scope: f.repo, State: "registered"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.identities[434343] = ""
	pid, identity, child := 434343, "boot/gone", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if _, err := f.db.UpdateResearchAttempt(ctx, lost.ID, nil, store.ResearchChange{State: researchState(research.Running), RunnerPID: &pid, RunnerIdentity: &identity, ChildID: &child}); err != nil {
		t.Fatal(err)
	}
	if code, output := runCLI(t, f.service, "research", "list", "--all"); code != 0 {
		t.Fatal(output)
	}
	interrupted := f.attempt(lost.ID)
	if interrupted.State != "interrupted" || interrupted.Resumable || !strings.Contains(interrupted.Note, "no persisted Pi session") {
		t.Fatalf("lost pi %+v", interrupted)
	}
	if code, output := runCLI(t, f.service, "research", "follow-up", research.AttemptID(lost.ID), "--prompt", "resume"); code == 0 || !strings.Contains(output, "research_not_resumable") || !strings.Contains(output, "remains interrupted") {
		t.Fatalf("resume without session: %d %s", code, output)
	}
	if f.runs() != 0 {
		t.Fatalf("a non-resumable child was relaunched: runs=%d", f.runs())
	}

	// Cancelling a running Pi child stops exactly that child.
	t.Setenv("FAKE_PI_MODE", "sleep")
	if code, output := runCLI(t, f.service, "research", "start", "--harness", "pi", "--scope", f.repo, "--prompt", "long"); code != 0 {
		t.Fatal(output)
	}
	running := f.attempts()[0]
	deadline := time.Now().Add(10 * time.Second)
	for running.ChildPID == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("child never recorded: %+v", running)
		}
		time.Sleep(20 * time.Millisecond)
		running = f.attempt(running.ID)
	}
	if code, output := runCLI(t, f.service, "research", "cancel", research.AttemptID(running.ID)); code != 0 {
		t.Fatal(output)
	}
	f.wg.Wait()
	if cancelled := f.attempt(running.ID); cancelled.State != "cancelled" || cancelled.ResultPath != "" || researchProcessAlive(cancelled.ChildPID, cancelled.ChildIdentity) {
		t.Fatalf("cancelled %+v", cancelled)
	}
}

func TestPiResearchToolTracksOwnedAttempts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable to load the Pi extension")
	}
	root := t.TempDir()
	log := filepath.Join(root, "posse.log")
	fake := filepath.Join(root, "posse")
	script := `#!/bin/sh
printf '%s\n' "$*" >>"` + log + `"
case "$1 $2" in
"research start")
	case "$*" in
	*call-3*) id=r9 ;;
	*) id=r7 ;;
	esac
	printf '{"research":{"id":"%s","state":"registered"}}\n' "$id" ;;
"research wait")
	if [ "$FAKE_POSSE_WAIT" = hang ]; then
		i=0
		while [ ! -e "` + log + `.cancel-$3" ] && [ $i -lt 100 ]; do sleep 0.05; i=$((i + 1)); done
		printf '{"research":{"id":"%s","state":"cancelled","note":"cancelled by posse research cancel"}}\n' "$3"
	else
		printf '{"research":{"id":"%s","state":"completed","result_text":"the store lives in internal/store","usage":"in=1 out=1"}}\n' "$3"
	fi ;;
"research cancel")
	: >"` + log + `.cancel-$3"
	printf '{"research":{"id":"%s","state":"cancelled"}}\n' "$3" ;;
*)
	printf '{}\n' ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	extension, err := filepath.Abs("lead_pi_extension.ts")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, filepath.Join("testdata", "pi-research-tool.mjs"))
	command.Env = append(os.Environ(), "POSSE_PI_EXTENSION="+extension, "POSSE_FAKE_BIN="+fake)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("node harness: %v\n%s", err, output)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var results struct {
		Answer       string         `json:"answer"`
		Details      map[string]any `json:"details"`
		MissingScope string         `json:"missingScope"`
		Aborted      string         `json:"aborted"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &results); err != nil {
		t.Fatalf("harness output %s: %v", output, err)
	}
	if results.Answer != "the store lives in internal/store" || results.Details["attempt"] != "r7" {
		t.Fatalf("answer %+v", results)
	}
	if !strings.Contains(results.MissingScope, "explicit scope") {
		t.Fatalf("missing scope accepted: %+v", results)
	}
	if !strings.Contains(results.Aborted, "r9 cancelled") {
		t.Fatalf("abort did not cancel the owned attempt: %+v", results)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, line := range strings.Split(string(calls), "\n") {
		if strings.HasPrefix(line, "research start") {
			starts++
			for _, want := range []string{"--harness pi", "--scope=/repo", "--key=pi-tool:session-1:call-", "--json"} {
				if !strings.Contains(line, want) {
					t.Fatalf("start %q missing %q", line, want)
				}
			}
		}
	}
	// Both executions of call-1 used one key; Posse returns the same attempt.
	if starts != 3 || strings.Count(string(calls), "--key=pi-tool:session-1:call-1 ") != 2 {
		t.Fatalf("starts=%d calls:\n%s", starts, calls)
	}
	if strings.Count(string(calls), "research cancel r9") < 1 || strings.Contains(string(calls), "research cancel r7") {
		t.Fatalf("cancel did not bind to the owned attempt:\n%s", calls)
	}
}
