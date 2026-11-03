//go:build linux

package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
)

func TestForgeProbeEntryExpiringDuringDiskReadIsDiscarded(t *testing.T) {
	root := t.TempDir()
	home, bin := filepath.Join(root, "posse"), filepath.Join(root, "bin")
	for _, path := range []string{home, bin} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("POSSE_HOME", home)
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FORGE_PROBE_LOG", filepath.Join(root, "calls.log"))
	t.Setenv("FORGE_PROBE_READY", filepath.Join(root, "ready"))
	for _, cli := range []string{"gh", "glab"} {
		script := `#!/bin/sh
while [ ! -e "$FORGE_PROBE_READY" ]; do /bin/sleep 0.001; done
printf '%s\n' "${0##*/}" >> "$FORGE_PROBE_LOG"
exit 1
`
		if err := os.WriteFile(filepath.Join(bin, cli), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	autoRepo := filepath.Join(root, "tools")
	initRepo(t, autoRepo)
	gitTest(t, autoRepo, "remote", "add", "origin", "https://git.example/acme/tools.git")
	cfg := config.Config{}
	key, _, err := forgeProbeCacheKey("git.example", cfg)
	if err != nil {
		t.Fatal(err)
	}
	previousTTL := forgeProbeCacheTTL
	forgeProbeCacheTTL = time.Hour
	t.Cleanup(func() { forgeProbeCacheTTL = previousTTL })
	baseTime := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	currentTime := baseTime
	clockReached := make(chan struct{})
	advanceClock := make(chan struct{})
	clockCalls := 0
	previousNow := forgeProbeNow
	forgeProbeNow = func() time.Time {
		clockMu.Lock()
		clockCalls++
		waitForAdvance := clockCalls == 2
		if waitForAdvance {
			close(clockReached)
		}
		clockMu.Unlock()
		if waitForAdvance {
			<-advanceClock
		}
		clockMu.Lock()
		defer clockMu.Unlock()
		return currentTime
	}
	t.Cleanup(func() { forgeProbeNow = previousNow })
	entry := forgeProbeCacheEntry{CheckedAt: baseTime.Add(-100 * time.Millisecond), Outcome: forgeProbeOutcome{
		GitHubChecked: true, GitLabChecked: true, GitHubAuthenticated: true, GitLabAuthenticated: true,
	}}
	data, err := json.Marshal(forgeProbeDiskCache{Entries: map[string]forgeProbeCacheEntry{key: entry}})
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(home, forgeProbeCache)
	if err := syscall.Mkfifo(cachePath, 0o600); err != nil {
		t.Fatal(err)
	}
	writerDone := make(chan error, 1)
	go func() {
		writer, err := os.OpenFile(cachePath, os.O_WRONLY, 0o600)
		if err != nil {
			writerDone <- err
			return
		}
		if _, err := writer.Write(data); err != nil {
			_ = writer.Close()
			writerDone <- err
			return
		}
		if err := writer.Close(); err != nil {
			writerDone <- err
			return
		}
		replacement := filepath.Join(home, "regular-cache")
		if err := os.WriteFile(replacement, []byte(`{"entries":{}}`), 0o600); err != nil {
			writerDone <- err
			return
		}
		if err := os.Rename(replacement, cachePath); err != nil {
			writerDone <- err
			return
		}
		writerDone <- os.WriteFile(os.Getenv("FORGE_PROBE_READY"), nil, 0o600)
	}()

	type probeResult struct {
		outcome forgeProbeOutcome
		err     error
	}
	firstDone := make(chan probeResult, 1)
	go func() {
		first, err := cachedForgeProbe(context.Background(), root, "git.example", cfg, "gitlab")
		firstDone <- probeResult{outcome: first, err: err}
	}()
	<-clockReached
	clockMu.Lock()
	currentTime = baseTime.Add(forgeProbeCacheTTL)
	clockMu.Unlock()
	close(advanceClock)
	firstResult := <-firstDone
	first, err := firstResult.outcome, firstResult.err
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	second, err := cachedForgeProbe(context.Background(), root, "git.example", cfg, "github")
	if err != nil {
		t.Fatal(err)
	}
	autoForge, autoErr := repositoryForgeReadiness(context.Background(), autoRepo, cfg, "tools")
	if autoErr == nil || !strings.Contains(autoErr.Error(), "origin host has no configured forge") {
		t.Errorf("auto Member consumed expired GitHub authentication: forge=%+v err=%v", autoForge, autoErr)
	}
	calls, err := os.ReadFile(os.Getenv("FORGE_PROBE_LOG"))
	if err != nil {
		t.Fatal(err)
	}
	if first.GitHubChecked || first.GitHubAuthenticated || first.GitHubTimedOut {
		t.Errorf("entry expired during disk read, partial refresh retained stale GitHub outcome: %+v", first)
	}
	if second.GitHubAuthenticated || !strings.Contains(string(calls), "gh\n") {
		t.Errorf("partial refresh revived expired GitHub authentication: result=%+v calls=%q", second, calls)
	}
	if strings.Count(string(calls), "glab\n") != 1 || strings.Count(string(calls), "gh\n") != 1 {
		t.Errorf("each expired CLI result should be refreshed once: %q", calls)
	}
}
