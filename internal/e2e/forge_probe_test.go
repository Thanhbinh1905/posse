//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestForgeProbeTimeoutInMultiMemberWorkspace(t *testing.T) {
	root := newFixtureRoot(t, herdr.TestRootName())
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	env := isolatedE2EEnv(t, root)
	env = setEnv(env, "POSSE_TEST_ROOT", root)
	env = setEnv(env, "PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	client := herdr.NewWithEnv("herdr", env)
	startServer(t, client)

	logPath := filepath.Join(root, "forge-probes.log")
	for _, cli := range []string{"glab", "gh"} {
		script := "#!/bin/sh\nprintf '%s %s\\n' '" + cli + "' \"$*\" >> \"" + logPath + "\"\n/bin/sleep 0.8\nexit 1\n"
		if err := os.WriteFile(filepath.Join(binDir, cli), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	workspace := filepath.Join(root, "clouddbv1")
	members := []string{"api", "frontend", "worker", "tools"}
	repos := make([]store.ProjectRepo, 0, len(members))
	for _, member := range members {
		repo := filepath.Join(workspace, member)
		if err := os.MkdirAll(repo, 0o700); err != nil {
			t.Fatal(err)
		}
		gitTest(t, env, repo, "init", "-q", "-b", "main")
		gitTest(t, env, repo, "config", "user.name", "Posse E2E")
		gitTest(t, env, repo, "config", "user.email", "posse-e2e@example.test")
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte(member+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, env, repo, "add", "README.md")
		gitTest(t, env, repo, "commit", "-qm", "fixture")
		gitTest(t, env, repo, "remote", "add", "origin", "https://git.paas.vn/acme/"+member+".git")
		repos = append(repos, store.ProjectRepo{Name: member, Path: member, DefaultBranch: "main", Status: store.RepoActive})
	}

	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(filepath.Join(home, "projects", "clouddbv1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nlanding_mode = \"pr\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateWorkspaceProject(context.Background(), "clouddbv1", workspace, repos)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordCheckoutAttempt(context.Background(), project.ID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	binary := filepath.Join(binDir, "posse")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = moduleRoot(t)
	build.Env = env
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse: %v\n%s", err, output)
	}
	command := exec.Command(binary, "--json")
	command.Dir = workspace
	command.Env = env
	started := time.Now()
	output, err := command.CombinedOutput()
	elapsed := time.Since(started)
	t.Logf("posse --json with shared-host CLI timeouts completed in %s", elapsed)
	if err != nil {
		t.Fatalf("posse status failed after %s: %v\n%s", elapsed, err, output)
	}
	var result struct {
		Readiness []struct {
			Code        string `json:"code"`
			Consequence string `json:"consequence"`
			Fix         string `json:"fix"`
		} `json:"readiness"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode posse status: %v\n%s", err, output)
	}
	if elapsed > 2*time.Second {
		t.Errorf("posse status took %s with slow forge CLIs, want at most 2s", elapsed)
	}
	foundTimeout := false
	for _, gap := range result.Readiness {
		if gap.Code == "forge_auth" && strings.Contains(gap.Consequence, "forge CLI timed out for host git.paas.vn") && strings.Contains(gap.Fix, "Check forge CLI connectivity") {
			foundTimeout = true
		}
	}
	if !foundTimeout {
		t.Errorf("readiness did not identify the forge probe timeout: %s", output)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read forge probe log: %v", err)
	}
	if count := strings.Count(string(calls), "auth status --hostname git.paas.vn"); count != 2 {
		t.Errorf("probed the shared host %d times, want one call per forge CLI:\n%s", count, calls)
	}
}
