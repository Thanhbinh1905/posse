package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
)

func TestRepositoryForgeProbesAreDeduplicatedAndConcurrentByHost(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	gate := filepath.Join(root, "started")
	logPath := filepath.Join(root, "probe.log")
	for _, directory := range []string{bin, gate} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("POSSE_HOME", filepath.Join(root, "posse"))
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("POSSE_PROBE_GATE", gate)
	t.Setenv("POSSE_PROBE_LOG", logPath)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := `#!/bin/sh
host=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--hostname" ] && [ "$#" -gt 1 ]; then host=$2; shift 2; else shift; fi
done
printf '%s %s\n' "${0##*/}" "$host" >> "$POSSE_PROBE_LOG"
: > "$POSSE_PROBE_GATE/$host"
i=0
while [ "$i" -lt 100 ]; do
  if [ -e "$POSSE_PROBE_GATE/gitlab.example" ] && [ -e "$POSSE_PROBE_GATE/github.example" ]; then break; fi
  /bin/sleep 0.005
  i=$((i + 1))
done
if [ ! -e "$POSSE_PROBE_GATE/gitlab.example" ] || [ ! -e "$POSSE_PROBE_GATE/github.example" ]; then exit 1; fi
case "${0##*/}:$host" in
  glab:gitlab.example|gh:github.example) exit 0 ;;
  *) exit 1 ;;
esac
`
	for _, cli := range []string{"glab", "gh"} {
		if err := os.WriteFile(filepath.Join(bin, cli), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	targets := []repoTarget{}
	for _, item := range []struct{ name, host string }{
		{"api", "gitlab.example"}, {"worker", "gitlab.example"}, {"tools", "github.example"},
	} {
		repo := filepath.Join(root, item.name)
		initRepo(t, repo)
		gitTest(t, repo, "remote", "add", "origin", "https://"+item.host+"/acme/"+item.name+".git")
		targets = append(targets, repoTarget{Name: item.name, Root: repo, DefaultBranch: "main"})
	}

	started := time.Now()
	checks := repositoryDoctorChecks(context.Background(), config.Config{Defaults: config.Defaults{LandingMode: "pr"}}, targets)
	elapsed := time.Since(started)
	if elapsed >= forgeProbeTimeout {
		t.Fatalf("distinct hosts were not probed concurrently: took %s, timeout is %s", elapsed, forgeProbeTimeout)
	}
	for _, check := range checks {
		if check.GapCode == "forge_auth" && check.Status != "ok" {
			t.Errorf("forge auth check failed: %+v", check)
		}
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"gitlab.example", "github.example"} {
		if count := strings.Count(string(calls), host); count != 2 {
			t.Errorf("host %s had %d CLI probes, want one per CLI:\n%s", host, count, calls)
		}
	}
}

func TestExplicitForgeConfigSkipsAutoDetectionAndDeduplicatesReadiness(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	logPath := filepath.Join(root, "probe.log")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_HOME", filepath.Join(root, "posse"))
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("POSSE_PROBE_LOG", logPath)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, cli := range []string{"glab", "gh"} {
		exitCode := "0"
		if cli == "gh" {
			exitCode = "1"
		}
		script := "#!/bin/sh\nprintf '%s\\n' \"${0##*/}\" >> \"$POSSE_PROBE_LOG\"\nexit " + exitCode + "\n"
		if err := os.WriteFile(filepath.Join(bin, cli), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	targets := []repoTarget{}
	for _, member := range []string{"api", "worker"} {
		repo := filepath.Join(root, member)
		initRepo(t, repo)
		gitTest(t, repo, "remote", "add", "origin", "https://git.example/acme/"+member+".git")
		targets = append(targets, repoTarget{Name: member, Root: repo, DefaultBranch: "main"})
	}
	cfg := config.Config{Defaults: config.Defaults{LandingMode: "pr", Forge: "gitlab"}}
	checks := repositoryDoctorChecks(context.Background(), cfg, targets)
	for _, check := range checks {
		if check.GapCode == "forge_auth" && check.Status != "ok" {
			t.Errorf("explicit GitLab configuration did not authenticate: %+v", check)
		}
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(calls)) != "glab" {
		t.Fatalf("explicit forge config should probe only glab once, got %q", calls)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	docs := filepath.Join(root, "docs")
	initRepo(t, docs)
	gitTest(t, docs, "remote", "add", "origin", "https://git.example/acme/docs.git")
	memberTargets := append(append([]repoTarget(nil), targets...), repoTarget{Name: "docs", Root: docs, DefaultBranch: "main"})
	memberConfig := config.Config{
		Defaults: config.Defaults{LandingMode: "pr"},
		Repositories: map[string]config.Repository{
			"api": {Forge: "gitlab"}, "worker": {Forge: "gitlab"},
		},
	}
	checks = repositoryDoctorChecks(context.Background(), memberConfig, memberTargets)
	for _, check := range checks {
		if check.GapCode == "forge_auth" && check.Status != "ok" {
			t.Errorf("explicit Member forge configuration did not authenticate: %+v", check)
		}
	}
	calls, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "glab\n") != 1 || strings.Count(string(calls), "gh\n") != 1 {
		t.Fatalf("explicit Member forge config and auto-detection duplicated same-host probes: %q", calls)
	}
}

func TestForgeProbeCacheHasShortTTLAndInvalidatesOnConfigChange(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	logPath := filepath.Join(root, "probe.log")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_HOME", filepath.Join(root, "posse"))
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("POSSE_PROBE_LOG", logPath)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, cli := range []string{"glab", "gh"} {
		script := "#!/bin/sh\nprintf '%s\\n' \"${0##*/}\" >> \"$POSSE_PROBE_LOG\"\nexit 1\n"
		if err := os.WriteFile(filepath.Join(bin, cli), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	previousTTL := forgeProbeCacheTTL
	forgeProbeCacheTTL = time.Second
	t.Cleanup(func() { forgeProbeCacheTTL = previousTTL })

	cfg := config.Config{}
	if _, err := cachedForgeProbe(context.Background(), root, "git.example", cfg, "auto"); err != nil {
		t.Fatal(err)
	}
	// A later CLI process shares only the on-disk cache, not this memory map.
	forgeProbeState.Lock()
	forgeProbeState.memory = map[string]forgeProbeCacheEntry{}
	forgeProbeState.Unlock()
	if _, err := cachedForgeProbe(context.Background(), root, "git.example", cfg, "auto"); err != nil {
		t.Fatal(err)
	}
	changed := cfg
	changed.Defaults.Gate = []string{"go test ./..."}
	if _, err := cachedForgeProbe(context.Background(), root, "git.example", changed, "auto"); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(calls), "\n"); count != 4 {
		t.Fatalf("cache did not reuse the same config result or invalidate after config change: %d calls\n%s", count, calls)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := cachedForgeProbe(context.Background(), root, "git.example", changed, "auto"); err != nil {
		t.Fatal(err)
	}
	calls, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(calls), "\n"); count != 6 {
		t.Fatalf("expired forge cache was reused: %d calls\n%s", count, calls)
	}
}

func TestForgeProbeTimeoutIsDistinctFromUnknownForge(t *testing.T) {
	host := "git.example"
	err := forgeProbeTimeoutError(host)
	if !strings.Contains(err.Error(), "forge CLI timed out for host "+host) {
		t.Fatalf("timeout error = %v", err)
	}
}
