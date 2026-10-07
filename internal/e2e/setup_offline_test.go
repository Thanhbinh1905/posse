//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

// TestSetupWithoutHerdrServer covers the installer path: posse setup runs
// outside any Herdr pane while no Herdr server is running.
func TestSetupWithoutHerdrServer(t *testing.T) {
	root := newFixtureRoot(t, herdr.TestRootName())
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stableDir := newFixtureRootAt(t, "/var/tmp", fixturePrefix("installed-"))
	binary := filepath.Join(stableDir, "posse")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = moduleRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse: %v\n%s", err, output)
	}
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(herdrBinary, filepath.Join(binDir, "herdr")); err != nil {
		t.Fatal(err)
	}
	env := isolatedE2EEnv(t, root)
	env = setEnv(env, "POSSE_TEST_ROOT", root)
	// Zero-config setup discovers installed harnesses. Keep the fixture's
	// fake Claude as its only harness, rather than leaking the caller's PATH.
	env = setEnv(env, "PATH", binDir+":/usr/bin:/bin")
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"home", "posse", "claude", "codex"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	client := herdr.NewWithEnv("herdr", env)
	if status, err := client.Status(context.Background()); err != nil || status.Running {
		t.Fatalf("isolated Herdr server must be stopped: status=%#v err=%v", status, err)
	}

	preview := runPosse(t, binary, root, env, "setup", "--check", "--human", "--binary", binary)
	if !strings.Contains(preview, "+  Link the posse plugin into Herdr") || strings.Contains(preview, "plan[") {
		t.Fatalf("offline setup preview is not a readable plan:\n%s", preview)
	}
	applied := runPosse(t, binary, root, env, "setup", "--human", "--binary", binary)
	for _, want := range []string{"✓  Installed the Herdr integration for claude", "✓  Linked the posse plugin into Herdr", "!  The posse skill is available only to Posse-launched sessions"} {
		if !strings.Contains(applied, want) {
			t.Fatalf("offline setup omitted %q:\n%s", want, applied)
		}
	}
	plugins, err := client.Run(context.Background(), "plugin", "list", "--json")
	if err != nil || !strings.Contains(string(plugins), filepath.Join(root, "posse", "plugin")) {
		t.Fatalf("Herdr does not list the linked posse plugin: %s err=%v", plugins, err)
	}
	for _, name := range []string{"posse", "posse-setup"} {
		if _, err := os.Stat(filepath.Join(root, "home", ".agents", "skills", name, "SKILL.md")); !os.IsNotExist(err) {
			t.Fatalf("default setup installed global skill %s: %v", name, err)
		}
	}
	// Optional global installation and explicit removal are idempotent.
	if output := runPosse(t, binary, root, env, "setup", "--global-skills", "--binary", binary); !strings.Contains(output, "global_skill:posse") {
		t.Fatalf("opt-in global skill install failed: %s", output)
	}
	for _, name := range []string{"posse", "posse-setup"} {
		if _, err := os.Stat(filepath.Join(root, "home", ".agents", "skills", name, "SKILL.md")); err != nil {
			t.Fatalf("opt-in global skill %s was not installed: %v", name, err)
		}
	}
	runPosse(t, binary, root, env, "setup", "--global-skills", "--binary", binary)
	modifiedSkill := filepath.Join(root, "home", ".agents", "skills", "posse", "SKILL.md")
	modifiedContents := []byte("user-modified Posse skill\n")
	if err := os.WriteFile(modifiedSkill, modifiedContents, 0o600); err != nil {
		t.Fatal(err)
	}
	foreignSkill := filepath.Join(root, "home", ".agents", "skills", "posse-setup", "user-notes.md")
	foreignContents := []byte("user-owned file\n")
	if err := os.WriteFile(foreignSkill, foreignContents, 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, binary, root, env, "setup", "--remove-global-skills", "--binary", binary)
	if contents, err := os.ReadFile(modifiedSkill); err != nil || string(contents) != string(modifiedContents) {
		t.Fatalf("removal changed modified Posse skill: contents=%q err=%v", contents, err)
	}
	if contents, err := os.ReadFile(foreignSkill); err != nil || string(contents) != string(foreignContents) {
		t.Fatalf("removal changed foreign file: contents=%q err=%v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(root, "home", ".agents", "skills", "posse-setup", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("removal kept unchanged Posse-owned skill: %v", err)
	}
	for _, name := range []string{"posse", "posse-setup"} {
		if _, err := os.Lstat(filepath.Join(root, "claude", "skills", name)); !os.IsNotExist(err) {
			t.Fatalf("removal kept unchanged Posse-owned Claude link %s: %v", name, err)
		}
	}
	runPosse(t, binary, root, env, "setup", "--remove-global-skills", "--binary", binary)
	// runPosse fails the test unless --exit-code reports nothing pending.
	runPosse(t, binary, root, env, "setup", "--check", "--exit-code", "--binary", binary)
	if output := runPosse(t, binary, root, env, "setup", "--uninstall"); !strings.Contains(output, "uninstalled") {
		t.Fatalf("offline setup uninstall failed: %s", output)
	}
	plugins, err = client.Run(context.Background(), "plugin", "list", "--json")
	if err != nil || strings.Contains(string(plugins), "posse.herdr") {
		t.Fatalf("offline uninstall kept the posse plugin: %s err=%v", plugins, err)
	}
}
