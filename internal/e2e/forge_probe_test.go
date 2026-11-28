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

type multiMemberForgeFixture struct {
	binDir, binary, home, workspace, logPath string
	env                                      []string
}

func newMultiMemberForgeFixture(t *testing.T) multiMemberForgeFixture {
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
		script := "#!/bin/sh\nprintf '%s %s\\n' '" + cli + "' \"$*\" >> \"" + logPath + "\"\nexit 1\n"
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
		repos = append(repos, store.ProjectRepo{Name: member, Path: member, DefaultBranch: "main", Status: store.RepoActive, OriginHost: "git.paas.vn"})
	}

	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(filepath.Join(home, "projects", "clouddbv1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nmerge_mode = \"pr\"\n"), 0o600); err != nil {
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
	now := time.Now().UnixMilli()
	if err := db.RecordCheckoutAttempt(context.Background(), project.ID, now); err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if err := db.RecordRepoCheckout(context.Background(), project.ID, member, now, "current", ""); err != nil {
			t.Fatal(err)
		}
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
	return multiMemberForgeFixture{binDir: binDir, binary: binary, home: home, workspace: workspace, logPath: logPath, env: env}
}

func TestForgeConfigVisibility(t *testing.T) {
	f := newMultiMemberForgeFixture(t)
	run := func(args ...string) string {
		t.Helper()
		return runPosse(t, f.binary, f.workspace, f.env, args...)
	}
	for _, all := range [][]string{nil, {"--all"}} {
		output := run(append([]string{"config", "schema"}, all...)...)
		if strings.Contains(output, "defaults.forge") {
			t.Fatalf("removed defaults.forge is still in config schema (flags %v)", all)
		}
		if visible := strings.Contains(output, "repositories.<repository>.forge"); visible != (len(all) > 0) {
			t.Fatalf("repository forge override visibility = %v with flags %v", visible, all)
		}
	}
	run("config", "set", "repositories.api.forge", "gitlab", "--project", "clouddbv1")
	for _, effective := range [][]string{nil, {"--effective"}} {
		args := append([]string{"config", "show", "--project", "clouddbv1", "--json"}, effective...)
		for _, all := range [][]string{nil, {"--all"}} {
			output := run(append(args, all...)...)
			if visible := strings.Contains(output, `"forge"`) || strings.Contains(output, "repositories.api.forge"); visible != (len(all) > 0) {
				t.Fatalf("repository forge override visibility = %v with flags %v %v: %s", visible, effective, all, output)
			}
		}
	}
}

func TestForgeDetectionAndOverridesInRepositoryProject(t *testing.T) {
	f := newPRLifecycleFixtureWithManualWatchOwner(t)
	defer f.db.Close()
	const remote = "https://enterprise.example.test/acme/shop.git"
	gitTest(t, f.env, f.repo, "remote", "set-url", "origin", remote)
	gitTest(t, f.env, f.repo, "config", "url.file://"+f.remote+".insteadOf", remote)
	for _, cli := range []string{"gh", "glab"} {
		if err := os.WriteFile(filepath.Join(f.root, "bin", cli), []byte("#!/bin/sh\n[ \"$1 $2\" = 'auth status' ]\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) string {
		t.Helper()
		return runPosse(t, f.binary, f.repo, setEnv(f.leadEnv, "TMPDIR", filepath.Join(f.root, "tmp")), args...)
	}
	const ambiguous = "origin host is authenticated with both gh and glab: enterprise.example.test"
	const key = "repositories.shop.forge"
	if output := run("doctor", "--json"); !strings.Contains(output, ambiguous) || !strings.Contains(output, key) {
		t.Fatalf("doctor did not name %s for the ambiguous origin: %s", key, output)
	}

	path := filepath.Join(f.home, "projects", f.project.Name, "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[defaults]\nforge = 'gitlab'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const gitlabAuth = "glab auth enterprise.example.test"
	if output := run("doctor", "--json"); strings.Contains(output, ambiguous) || !strings.Contains(output, gitlabAuth) {
		t.Fatalf("doctor did not honor legacy GitLab override: %s", output)
	}
	run("config", "set", key, "github", "--project", "shop")
	if output := run("doctor", "--json"); strings.Contains(output, ambiguous) || strings.Contains(output, gitlabAuth) {
		t.Fatalf("doctor did not honor the repository-specific GitHub override: %s", output)
	}
	run("config", "set", key, "auto", "--project", "shop")
	if output := run("doctor", "--json"); !strings.Contains(output, ambiguous) {
		t.Fatalf("repository auto did not replace the explicit legacy forge: %s", output)
	}
	run("config", "unset", key, "--project", "shop")
	run("config", "unset", "defaults.forge", "--project", "shop")
	if output := run("doctor", "--json"); !strings.Contains(output, ambiguous) || !strings.Contains(output, key) {
		t.Fatalf("doctor did not return to auto-detection: %s", output)
	}
}

func TestForgeReadinessUsesCacheInMultiMemberWorkspace(t *testing.T) {
	f := newMultiMemberForgeFixture(t)
	binary, workspace, env, logPath := f.binary, f.workspace, f.env, f.logPath
	command := exec.Command(binary, "--json")
	command.Dir = workspace
	command.Env = env
	started := time.Now()
	output, err := command.CombinedOutput()
	elapsed := time.Since(started)
	t.Logf("posse --json with uncached shared-host auth completed in %s", elapsed)
	if err != nil {
		t.Fatalf("posse status failed after %s: %v\n%s", elapsed, err, output)
	}
	var result struct {
		Readiness []struct {
			Code          string `json:"code"`
			Consequence   string `json:"consequence"`
			Fix           string `json:"fix"`
			Informational bool   `json:"informational"`
		} `json:"readiness"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode posse status: %v\n%s", err, output)
	}
	if elapsed > 2*time.Second {
		t.Errorf("posse status took %s with slow forge CLIs, want at most 2s", elapsed)
	}
	foundUnknown := false
	for _, gap := range result.Readiness {
		if gap.Code == "forge_auth" && strings.Contains(gap.Consequence, "unknown") && strings.Contains(gap.Consequence, "git.paas.vn") && strings.Contains(gap.Fix, "resolve forge access when a Task needs") && gap.Informational {
			foundUnknown = true
		}
	}
	if !foundUnknown {
		t.Errorf("readiness did not report the uncached forge state as informational: %s", output)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read forge probe log: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("readiness invoked a forge CLI despite having no cached auth result:\n%s", calls)
	}

	doctorEnv := setEnv(env, "TMPDIR", filepath.Join(filepath.Dir(f.home), "tmp"))
	outputText := runPosse(t, binary, workspace, doctorEnv, "doctor", "--json")
	if !strings.Contains(outputText, "origin host has no configured forge: git.paas.vn") || !strings.Contains(outputText, "Set `repositories.api.forge` to github or gitlab") {
		t.Fatalf("doctor did not name the Member override: %s", outputText)
	}
	outputText = runPosse(t, binary, workspace, env, "--json")
	if !strings.Contains(outputText, "Set `repositories.api.forge` to github or gitlab") {
		t.Fatalf("readiness did not name the Member override: %s", outputText)
	}
}
