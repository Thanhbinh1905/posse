//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestIdleFullscreenClaudeWorkerKeepsItsViewDuringAutomaticReads(t *testing.T) {
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("real Claude executable is not installed")
	}
	base := newPRLifecycleFixture(t)
	t.Cleanup(func() { _ = base.db.Close() })
	var reply strings.Builder
	for line := 1; line <= 150; line++ {
		fmt.Fprintf(&reply, "line %d\n\n", line)
	}
	server := claudeLocalModel(t, func(int, string) []map[string]any {
		// Herdr must observe the Worker working before the reply lands.
		time.Sleep(2 * time.Second)
		return []map[string]any{{"type": "text", "text": reply.String()}}
	})
	// The fixture's fake claude shadows the real one on PATH.
	wrapper := fmt.Sprintf("#!/bin/sh\nexport ANTHROPIC_BASE_URL=%s ANTHROPIC_API_KEY=local-test-only CLAUDE_CODE_NO_FLICKER=1 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1\nexec %s \"$@\"\n", shellQuote(server.URL), shellQuote(claude))
	if err := os.WriteFile(filepath.Join(base.root, "bin", "claude"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	state := `{"hasCompletedOnboarding":true,"theme":"dark","bypassPermissionsModeAccepted":true,"customApiKeyResponses":{"approved":["local-test-only"],"rejected":[]}}`
	if err := os.MkdirAll(filepath.Join(base.root, "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base.root, "claude", ".claude.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	client := herdr.NewWithEnv("herdr", base.env)
	if _, err := client.Run(context.Background(), "integration", "install", "claude"); err != nil {
		t.Fatal(err)
	}
	runPosse(t, base.binary, base.repo, base.env, "config", "set", "profiles.deep.kind", "claude")
	runPosse(t, base.binary, base.repo, base.env, "config", "set", "profiles.deep.model", "claude-sonnet-4-6")
	brief := filepath.Join(base.root, "idle-scroll.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Idle scroll\ndone_when: view kept\n---\nSay hello.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, base.binary, base.repo, base.leadEnv, "start", "--brief", brief, "--name", "idle-scroll")
	task := base.mustTask(t, "t1")

	visible := func() string { return paneVisibleText(client, task.PaneID) }
	idle := func() bool { return paneIdle(client, task.PaneID) }
	lastLine := regexp.MustCompile(`line 150`)
	var settled string
	if !waitForCondition(60*time.Second, func() bool {
		screen := visible()
		if !lastLine.MatchString(screen) || !idle() || screen != settled {
			settled = screen
			return false
		}
		return true
	}) {
		t.Fatalf("Claude never settled after its reply:\n%s", visible())
	}

	assertIdleViewStaysPut(t, base, task, visible, "line 150")
}
