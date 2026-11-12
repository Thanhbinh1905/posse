//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

// Reproduce #966: a User opened a research child's transcript in the Claude
// Worker pane and left it there. Claude Code sends typed text to the agent
// whose transcript it shows, so Lead messages resumed the child instead of
// reaching the Worker, both while the child ran and after it ended.
func TestClaudeWorkerMessageSkipsOpenResearchChildView(t *testing.T) {
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("real Claude executable is not installed")
	}
	f := newPRLifecycleFixture(t)
	t.Cleanup(func() { _ = f.db.Close() })
	const childPrompt = "CHILD-PROMPT-4412 read nothing"
	const running, ended = "LEAD-MESSAGE-WHILE-CHILD-RUNS", "LEAD-MESSAGE-AFTER-CHILD-ENDED"
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseChild := func() { releaseOnce.Do(func() { close(release) }) }
	var mu sync.Mutex
	var parentBodies, childBodies []string
	server := claudeLocalModel(t, func(_ int, body string) []map[string]any {
		// Only the parent's requests carry its launch prompt.
		if !strings.Contains(body, "launch.md and") {
			mu.Lock()
			childBodies = append(childBodies, body)
			first := len(childBodies) == 1
			mu.Unlock()
			if first {
				<-release
			}
			return []map[string]any{{"type": "text", "text": "child result"}}
		}
		mu.Lock()
		parentBodies = append(parentBodies, body)
		n := len(parentBodies)
		mu.Unlock()
		if n == 1 {
			return []map[string]any{{"type": "tool_use", "id": "spawn_child", "name": "Agent", "input": map[string]any{"description": "Research child", "subagent_type": "Explore", "prompt": childPrompt, "run_in_background": true}}}
		}
		return []map[string]any{{"type": "text", "text": fmt.Sprintf("parent turn %d", n)}}
	})
	// Server cleanup waits for the child's held request.
	t.Cleanup(releaseChild)
	wrapper := fmt.Sprintf("#!/bin/sh\nexport ANTHROPIC_BASE_URL=%s ANTHROPIC_API_KEY=local-test-only CLAUDE_CODE_NO_FLICKER=1 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1\nexec %s \"$@\"\n", shellQuote(server.URL), shellQuote(claude))
	if err := os.WriteFile(filepath.Join(f.root, "bin", "claude"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	state := `{"hasCompletedOnboarding":true,"theme":"dark","bypassPermissionsModeAccepted":true,"customApiKeyResponses":{"approved":["local-test-only"],"rejected":[]}}`
	if err := os.MkdirAll(filepath.Join(f.root, "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "claude", ".claude.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	client := herdr.NewWithEnv("herdr", f.env)
	if _, err := client.Run(context.Background(), "integration", "install", "claude"); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "profiles.deep.kind", "claude")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "profiles.deep.model", "claude-sonnet-4-6")
	brief := filepath.Join(f.root, "child-view.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Research child view\ndone_when: Lead messages reach the Worker\n---\nStart one research child.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "start", "--brief", brief, "--name", "child-view")
	task := f.mustTask(t, "t1")
	visible := func() string { return paneVisibleText(client, task.PaneID) }
	sendKeys := func(keys ...string) {
		t.Helper()
		for _, key := range keys {
			if _, err := client.Call(context.Background(), "agent.send_keys", map[string]any{"target": task.PaneID, "keys": []string{key}}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Second)
		}
	}
	childViewShown := func() bool { return strings.Contains(visible(), "Message @Explore") }
	openChildView := func() {
		t.Helper()
		sendKeys("down", "down", "enter")
		if !waitForCondition(10*time.Second, childViewShown) {
			t.Fatalf("the research child's transcript view did not open:\n%s", visible())
		}
	}
	reached := func(bodies *[]string, text string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, body := range *bodies {
			if strings.Contains(body, text) {
				return true
			}
		}
		return false
	}
	expectParentTurn := func(text string) {
		t.Helper()
		if !waitForCondition(30*time.Second, func() bool { return reached(&parentBodies, text) || reached(&childBodies, text) }) {
			t.Fatalf("message %q did not reach the Worker's own turn:\n%s", text, visible())
		}
		if reached(&childBodies, text) {
			t.Fatalf("message %q reached the research child instead of the Worker:\n%s", text, visible())
		}
		if childViewShown() {
			t.Fatalf("the Worker pane still shows the research child's view:\n%s", visible())
		}
	}

	if !waitForCondition(60*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(parentBodies) >= 2 && len(childBodies) == 1
	}) {
		t.Fatalf("the Worker did not start its research child:\n%s", visible())
	}
	openChildView()
	runPosse(t, f.binary, f.repo, f.leadEnv, "send", "t1", running)
	expectParentTurn(running)

	// Claude lists the child only while it runs, and its view stays open after
	// it ends, as in #966 where message 2551 arrived after the child ended.
	openChildView()
	releaseChild()
	if !waitForCondition(30*time.Second, func() bool {
		return paneIdle(client, task.PaneID) && childViewShown() && strings.Contains(visible(), "child result")
	}) {
		t.Fatalf("the research child did not end with its view open:\n%s", visible())
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "send", "t1", "--queue", ended)
	expectParentTurn(ended)
}
