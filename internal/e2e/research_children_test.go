//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/research"
	"github.com/thanhbinh1905/posse/internal/store"
)

// researchE2EEnv puts the fake Codex and Pi harnesses on the Lead's PATH.
func researchE2EEnv(t *testing.T, f *prLifecycleFixture) []string {
	t.Helper()
	bin := filepath.Join(moduleRoot(t), "internal", "app", "testdata", "research")
	env := setEnv(f.leadEnv, "PATH", bin+string(os.PathListSeparator)+envValue(f.leadEnv, "PATH"))
	env = setEnv(env, "FAKE_RESEARCH_LOG", filepath.Join(f.root, "research-harness.log"))
	return setEnv(env, "CODEX_HOME", filepath.Join(f.root, "codex-research-home"))
}

func runResearchCLI(t *testing.T, f *prLifecycleFixture, env []string, args ...string) (map[string]any, string, error) {
	t.Helper()
	command := exec.Command(f.binary, append(args, "--json")...)
	command.Dir, command.Env = f.repo, env
	output, err := command.CombinedOutput()
	var decoded map[string]any
	_ = json.Unmarshal(output, &decoded)
	return decoded, string(output), err
}

func TestResearchChildrenRunThroughTheDetachedRunner(t *testing.T) {
	f := newPRLifecycleFixtureWithLead(t, true)
	defer f.db.Close()
	env := researchE2EEnv(t, f)
	ctx := context.Background()

	decoded, output, err := runResearchCLI(t, f, env, "research", "start", "--harness", "codex", "--scope", f.repo, "--prompt", "map the store", "--wait", "--timeout", "60000")
	if err != nil {
		t.Fatalf("codex start: %v\n%s", err, output)
	}
	attempt, _ := decoded["research"].(map[string]any)
	if attempt["state"] != "completed" || attempt["result_text"] != "answer to: map the store" || attempt["parent"] != "Lead" {
		t.Fatalf("codex attempt: %s", output)
	}
	attempts, err := f.db.ResearchAttempts(ctx, store.ResearchAttemptFilter{ProjectID: f.project.ID})
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts=%+v err=%v", attempts, err)
	}
	recorded := attempts[0]
	if recorded.RunnerPID == 0 || recorded.RunnerPID == os.Getpid() || recorded.ChildID == "" {
		t.Fatalf("runner identity not recorded: %+v", recorded)
	}
	if err := syscall.Kill(recorded.RunnerPID, 0); err == nil {
		if identity, identityErr := processIdentity(recorded.RunnerPID); identityErr == nil && identity == recorded.RunnerIdentity {
			t.Fatalf("runner %d outlived its finished child", recorded.RunnerPID)
		}
	}
	if _, err := os.Stat(filepath.Join(f.home, "research-runs", "r1.log")); err != nil {
		t.Fatalf("runner log: %v", err)
	}

	// Pi uses a Posse-owned persistent session that a follow-up continues.
	decoded, output, err = runResearchCLI(t, f, env, "research", "start", "--harness", "pi", "--scope", f.repo, "--prompt", "where is the queue", "--wait", "--timeout", "60000")
	if err != nil {
		t.Fatalf("pi start: %v\n%s", err, output)
	}
	attempt, _ = decoded["research"].(map[string]any)
	if attempt["state"] != "completed" || attempt["resumable"] != true {
		t.Fatalf("pi attempt: %s", output)
	}
	id, _ := attempt["id"].(string)
	decoded, output, err = runResearchCLI(t, f, env, "research", "follow-up", id, "--prompt", "and its tests", "--wait", "--timeout", "60000")
	if err != nil {
		t.Fatalf("pi follow-up: %v\n%s", err, output)
	}
	if attempt, _ = decoded["research"].(map[string]any); attempt["state"] != "completed" || attempt["follows"] != id {
		t.Fatalf("pi follow-up: %s", output)
	}

	// Cancel stops the exact running child and its runner.
	sleeping := setEnv(env, "FAKE_CODEX_MODE", "sleep")
	decoded, output, err = runResearchCLI(t, f, sleeping, "research", "start", "--harness", "codex", "--scope", f.repo, "--prompt", "long")
	if err != nil {
		t.Fatalf("long start: %v\n%s", err, output)
	}
	attempt, _ = decoded["research"].(map[string]any)
	id, _ = attempt["id"].(string)
	var running store.ResearchAttempt
	deadline := time.Now().Add(30 * time.Second)
	for {
		all, _ := f.db.ResearchAttempts(ctx, store.ResearchAttemptFilter{ProjectID: f.project.ID})
		for _, candidate := range all {
			if research.AttemptID(candidate.ID) == id {
				running = candidate
			}
		}
		if running.ChildPID != 0 && running.ChildID != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child never started: %+v", running)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, output, err := runResearchCLI(t, f, env, "research", "cancel", id); err != nil {
		t.Fatalf("cancel: %v\n%s", err, output)
	}
	deadline = time.Now().Add(15 * time.Second)
	for alive(running.ChildPID, running.ChildIdentity) || alive(running.RunnerPID, running.RunnerIdentity) {
		if time.Now().After(deadline) {
			t.Fatalf("cancelled child %d or runner %d still runs", running.ChildPID, running.RunnerPID)
		}
		time.Sleep(50 * time.Millisecond)
	}
	decoded, output, err = runResearchCLI(t, f, env, "research", "show", id)
	if err != nil {
		t.Fatalf("show: %v\n%s", err, output)
	}
	if decoded["state"] != "cancelled" || decoded["result"] != nil {
		t.Fatalf("cancelled attempt: %s", output)
	}
}

func TestResearchHookRefusesWriteCapableClaudeChildren(t *testing.T) {
	f := newPRLifecycleFixtureWithLead(t, true)
	defer f.db.Close()
	hook := func(event map[string]any) (int, string) {
		event["session_id"], event["cwd"] = "parent-session", f.repo
		data, _ := json.Marshal(event)
		command := exec.Command(f.binary, "_research-hook", "claude", "--version", "2.1.308")
		command.Dir, command.Env, command.Stdin = f.repo, f.leadEnv, bytes.NewReader(data)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		err := command.Run()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, stderr.String()
	}
	agent := func(id, agentType string) map[string]any {
		return map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Agent", "tool_use_id": id, "tool_input": map[string]any{"subagent_type": agentType, "prompt": "look"}}
	}
	if code, stderr := hook(agent("t1", "general-purpose")); code != 2 || !strings.Contains(stderr, "refused") {
		t.Fatalf("general-purpose: %d %s", code, stderr)
	}
	if code, stderr := hook(agent("t2", "Explore")); code != 0 {
		t.Fatalf("Explore: %d %s", code, stderr)
	}
	if code, stderr := hook(map[string]any{"hook_event_name": "SubagentStart", "agent_id": "a1", "agent_type": "Explore"}); code != 0 {
		t.Fatalf("start: %d %s", code, stderr)
	}
	for tool, input := range map[string]map[string]any{"Write": {"file_path": "x"}, "Bash": {"command": "touch x"}} {
		if code, stderr := hook(map[string]any{"hook_event_name": "PreToolUse", "agent_id": "a1", "agent_type": "Explore", "permission_mode": "bypassPermissions", "tool_name": tool, "tool_input": input}); code != 2 {
			t.Fatalf("%s in child: %d %s", tool, code, stderr)
		}
	}
	if code, stderr := hook(map[string]any{"hook_event_name": "PreToolUse", "agent_id": "a1", "agent_type": "Explore", "tool_name": "Read", "tool_input": map[string]any{"file_path": "README.md"}}); code != 0 {
		t.Fatalf("Read in child: %d %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "x")); !os.IsNotExist(err) {
		t.Fatalf("refused write left an effect: %v", err)
	}
	attempts, err := f.db.ResearchAttempts(context.Background(), store.ResearchAttemptFilter{ProjectID: f.project.ID})
	if err != nil || len(attempts) != 2 || attempts[0].State != "running" || attempts[0].ChildID != "a1" || attempts[1].State != "refused" {
		t.Fatalf("attempts=%+v err=%v", attempts, err)
	}
}

func alive(pid int, identity string) bool {
	if pid < 1 {
		return false
	}
	current, err := processIdentity(pid)
	return err == nil && current == identity
}

func processIdentity(pid int) (string, error) {
	boot, start, err := store.ProcessIdentityForPID(pid)
	if err != nil {
		return "", err
	}
	return boot + "/" + start, nil
}
