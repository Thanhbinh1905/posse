//go:build e2e

package e2e

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// TestInstallerSavedDataFolderReachesFirstTask installs a real release
// fixture with install.sh into a custom data folder, then starts a Lead and a
// Worker Task from a Herdr server that was running before the folder was
// chosen. No process gets POSSE_HOME; everything resolves the home pointer.
func TestInstallerSavedDataFolderReachesFirstTask(t *testing.T) {
	root := newFixtureRoot(t, herdr.TestRootName())
	bin := filepath.Join(root, "bin")
	// posse setup refuses a binary under /tmp, so install where users would.
	prefix := newFixtureRootAt(t, "/var/tmp", fixturePrefix("installed-"))
	data := filepath.Join(root, "data folder")
	for _, dir := range []string{bin, filepath.Join(root, "home"), filepath.Join(root, "claude"), filepath.Join(root, "codex"), filepath.Join(root, "pi")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	releases := writeReleaseFixture(t, root, "v9.9.9")
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(herdrBinary, filepath.Join(bin, "herdr")); err != nil {
		t.Fatal(err)
	}
	agent := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'claude 0.0.0'; exit 0; fi
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
while IFS= read -r line; do
 printf '%s\n' "$line" >> "$POSSE_TEST_ROOT/prompts.log"
 case "$PWD/" in */pool/*) herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1 & ;; esac
done
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(agent), 0o700); err != nil {
		t.Fatal(err)
	}
	env := isolatedE2EEnv(t, root)
	env = unsetEnv(env, "POSSE_HOME")
	env = setEnv(env, "POSSE_TEST_ROOT", root)
	env = setEnv(env, "PATH", prefix+":"+bin+":/usr/bin:/bin")
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	client := herdr.NewWithEnv("herdr", env)
	startServer(t, client)

	installer := filepath.Join(moduleRoot(t), "install.sh")
	installEnv := append(append([]string(nil), env...), "POSSE_PREFIX="+prefix, "POSSE_VERSION=v9.9.9", "POSSE_RELEASE_BASE=file://"+releases, "NO_COLOR=1")
	install := func(args ...string) string {
		t.Helper()
		command := exec.Command("sh", append([]string{installer}, args...)...)
		command.Dir = root
		command.Env = installEnv
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("install.sh %v: %v\n%s", args, err, output)
		}
		return string(output)
	}
	output := install("--yes", "--home", data)
	for _, want := range []string{"[6/6] Prepare your agents for posse", "posse keeps its data in " + data, "✓  Herdr plugin and agent integrations", "✓  Recommended workflow skills", "| posse is ready"} {
		if !strings.Contains(output, want) {
			t.Fatalf("installer output omitted %q:\n%s", want, output)
		}
	}
	pointer := filepath.Join(root, "xdg", "posse", "home")
	if contents, err := os.ReadFile(pointer); err != nil || string(contents) != data+"\n" {
		t.Fatalf("home pointer = %q %v", contents, err)
	}
	configured, err := config.ConfiguredSkills(filepath.Join(data, "config.toml"))
	if err != nil || len(config.MissingRecommendedSkills(configured)) != 0 {
		t.Fatalf("fresh install config lacks the recommended skills: %v %v", configured, err)
	}
	if _, err := os.Stat(filepath.Join(root, "home", ".posse")); !os.IsNotExist(err) {
		t.Fatalf("install wrote the legacy ~/.posse: %v", err)
	}
	plugins, err := client.Run(context.Background(), "plugin", "list", "--json")
	if err != nil || !strings.Contains(string(plugins), filepath.Join(data, "plugin")) {
		t.Fatalf("Herdr does not list the posse plugin from the data folder: %s %v", plugins, err)
	}
	settings, err := os.ReadFile(filepath.Join(root, "claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	contextHook := hookCommandFor(t, settings, "SessionStart", " _context")

	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(root, "origin.git")
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.test"}, {"commit", "--allow-empty", "-m", "Initial"}, {"init", "--bare", origin}, {"remote", "add", "origin", origin}, {"push", "-u", "origin", "main"}} {
		if out, err := gitCommand(env, repo, args...); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	workspace, err := createWorkspace(client, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "pane", "run", workspace.RootPane.PaneID, "posse up --name shop --yes"); err != nil {
		t.Fatal(err)
	}
	var project store.Project
	if !waitForCondition(35*time.Second, func() bool {
		db, err := store.OpenReadOnly(data)
		if err != nil {
			return false
		}
		defer db.Close()
		project, err = db.ProjectByName(context.Background(), "shop")
		return err == nil && project.LeadPaneID != ""
	}) {
		pane, _ := client.Run(context.Background(), "pane", "read", workspace.RootPane.PaneID, "--source", "recent-unwrapped", "--lines", "80")
		t.Fatalf("posse up in a pane without POSSE_HOME did not register the Project in the data folder: %s", pane)
	}
	leadEnv := setEnv(env, "HERDR_ENV", "1")
	leadEnv = setEnv(leadEnv, "HERDR_PANE_ID", project.LeadPaneID)
	leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", project.HerdrWorkspaceID)
	hookEnv := setEnv(env, "HERDR_PANE_ID", project.LeadPaneID)
	// The startup finalizer adopts the Lead shortly after up registers it.
	var hookOutput []byte
	if !waitForCondition(35*time.Second, func() bool {
		hook := exec.Command("sh", "-c", contextHook)
		hook.Dir = repo
		hook.Env = hookEnv
		hookOutput, err = hook.CombinedOutput()
		return err == nil && strings.Contains(string(hookOutput), "role: lead") && strings.Contains(string(hookOutput), "project: shop")
	}) {
		explicit, _ := exec.Command("sh", "-c", "POSSE_HOME='"+data+"' "+contextHook).CombinedOutput()
		pane, _ := client.Run(context.Background(), "pane", "read", project.LeadPaneID, "--source", "recent-unwrapped", "--lines", "60")
		snap, snapErr := client.Snapshot(context.Background())
		snapshot, _ := json.Marshal(snap.Panes)
		snapshot = append(snapshot, []byte(fmt.Sprint(snapErr))...)
		t.Fatalf("installed SessionStart hook %q did not find the Lead through the pointer: %v\n%s\nwith POSSE_HOME: %s\npane: %s\nagents: %s", contextHook, err, hookOutput, explicit, pane, snapshot)
	}

	binary := filepath.Join(prefix, "posse")
	brief := filepath.Join(root, "change.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: First change\ndone_when: change verified\n---\nMake a change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, binary, repo, leadEnv, "start", "--brief", brief, "--name", "first-change"); !strings.Contains(output, "working") {
		t.Fatalf("start: %s", output)
	}
	if _, err := os.Stat(filepath.Join(data, "projects", "shop", "tasks", "t1", "launch.md")); err != nil {
		t.Fatalf("first Task was not written to the data folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "home", ".posse")); !os.IsNotExist(err) {
		t.Fatalf("Lead or Worker wrote the legacy ~/.posse: %v", err)
	}

	// A reinstall keeps the saved folder and a customized skill list.
	configPath := filepath.Join(data, "config.toml")
	custom := "# mine\n[skills]\nchange = [\"posse-tdd\"]\n"
	if err := os.WriteFile(configPath, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}
	output = install("--yes")
	if !strings.Contains(output, "posse keeps its data in "+data) {
		t.Fatalf("reinstall did not keep the saved folder:\n%s", output)
	}
	if contents, _ := os.ReadFile(configPath); string(contents) != custom {
		t.Fatalf("reinstall changed a custom skill list:\n%s", contents)
	}
	if contents, _ := os.ReadFile(pointer); string(contents) != data+"\n" {
		t.Fatalf("reinstall changed the pointer: %q", contents)
	}

	// An explicit POSSE_HOME wins and never touches the pointer.
	explicit := filepath.Join(root, "explicit")
	if output := runPosse(t, binary, repo, setEnv(env, "POSSE_HOME", explicit), "_home"); output != explicit+"\nenv\n"+pointer+"\n" {
		t.Fatalf("explicit POSSE_HOME = %q", output)
	}
	if contents, _ := os.ReadFile(pointer); string(contents) != data+"\n" {
		t.Fatalf("explicit POSSE_HOME changed the pointer: %q", contents)
	}
}

func writeReleaseFixture(t *testing.T, root, version string) string {
	t.Helper()
	releases := filepath.Join(root, "releases")
	dir := filepath.Join(releases, version)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "build", "posse")
	build := exec.Command("go", "build", "-ldflags", "-X main.version="+version, "-o", binary, "./cmd/posse")
	build.Dir = moduleRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse: %v\n%s", err, output)
	}
	contents, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	asset := "posse_" + strings.TrimPrefix(version, "v") + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	archive, err := os.Create(filepath.Join(dir, asset))
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(archive)
	writer := tar.NewWriter(compressed)
	if err := writer.WriteHeader(&tar.Header{Name: "posse", Mode: 0o755, Size: int64(len(contents))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(contents); err != nil {
		t.Fatal(err)
	}
	for _, closer := range []interface{ Close() error }{writer, compressed, archive} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	packed, err := os.ReadFile(filepath.Join(dir, asset))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(packed)
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(hex.EncodeToString(sum[:])+"  "+asset+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return releases
}

func hookCommandFor(t *testing.T, settings []byte, event, suffix string) string {
	t.Helper()
	var parsed struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(settings, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, group := range parsed.Hooks[event] {
		for _, hook := range group.Hooks {
			if strings.HasSuffix(hook.Command, suffix) {
				return hook.Command
			}
		}
	}
	t.Fatalf("settings have no posse %s hook: %s", event, settings)
	return ""
}

func unsetEnv(env []string, key string) []string {
	kept := env[:0:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, key+"=") {
			kept = append(kept, entry)
		}
	}
	return kept
}

// TestInstallerWithoutAgentIsNotReady installs a real release with no
// supported agent on PATH. Preparing the machine succeeds, first while setup
// applies and then while it is already complete, but neither run may tell the
// User that posse up is ready.
func TestInstallerWithoutAgentIsNotReady(t *testing.T) {
	root := newFixtureRoot(t, herdr.TestRootName())
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{bin, filepath.Join(root, "home"), filepath.Join(root, "posse"), filepath.Join(root, "claude"), filepath.Join(root, "codex")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	releases := writeReleaseFixture(t, root, "v9.9.9")
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(herdrBinary, filepath.Join(bin, "herdr")); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"claude", "codex", "pi", "opencode"} {
		if path, err := exec.LookPath(filepath.Join("/usr/bin", agent)); err == nil {
			t.Skipf("%s is installed system-wide at %s", agent, path)
		}
	}
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	env := isolatedE2EEnv(t, root)
	env = setEnv(env, "PATH", bin+":/usr/bin:/bin")
	env = append(env, "POSSE_PREFIX="+newFixtureRootAt(t, "/var/tmp", fixturePrefix("installed-")), "POSSE_VERSION=v9.9.9", "POSSE_RELEASE_BASE=file://"+releases, "NO_COLOR=1")
	for _, run := range []string{"setup applies", "setup already complete"} {
		command := exec.Command("sh", filepath.Join(moduleRoot(t), "install.sh"), "--yes")
		command.Dir = root
		command.Env = env
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: install.sh: %v\n%s", run, err, output)
		}
		text := string(output)
		if run == "setup already complete" && !strings.Contains(text, "Your agents are already prepared.") {
			t.Fatalf("second run did not find setup complete:\n%s", text)
		}
		for _, want := range []string{"No supported agent found", "| posse is installed", "Next: install Claude Code, Codex, Pi or OpenCode."} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s: output omitted %q:\n%s", run, want, text)
			}
		}
		if strings.Contains(text, "posse is ready") {
			t.Fatalf("%s: installer reported ready without an agent:\n%s", run, text)
		}
	}
}
