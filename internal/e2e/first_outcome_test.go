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

// Drive the installed CLI from a fresh home, without a setup interview or config edits.
func TestFirstOutcomeFreshHome(t *testing.T) {
	root := newFixtureRoot(t, herdr.TestRootName())
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(root, "installed")
	if err := os.MkdirAll(installed, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(installed, "posse")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"herdr": herdrBinary, "posse": binary} {
		if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	agent := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'claude 0.0.0'; exit 0; fi
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
while IFS= read -r line; do
 printf '%s\n' "$line" >> "$POSSE_TEST_ROOT/prompts.log"
 case "$PWD/" in "$POSSE_HOME/remuda/"*) herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1 & ;; esac
done
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(agent), 0700); err != nil {
		t.Fatal(err)
	}
	env := isolatedE2EEnv(t, root)
	env = setEnv(env, "POSSE_TEST_ROOT", root)
	// Only the fake Claude harness is discoverable, regardless of the caller's installed agents.
	env = setEnv(env, "PATH", bin+":/usr/bin:/bin")
	for _, dir := range []string{"home", "posse", "claude", "codex", "pi"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.test"}, {"commit", "--allow-empty", "-m", "Initial"}} {
		if out, err := gitCommand(env, repo, args...); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	client := herdr.NewWithEnv("herdr", env)
	startServer(t, client)
	workspace, err := createWorkspace(client, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "pane", "run", workspace.RootPane.PaneID, "posse up --name shop --yes"); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "posse"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var project store.Project
	if !waitForCondition(35*time.Second, func() bool {
		project, err = db.ProjectByName(context.Background(), "shop")
		if err != nil || project.LeadPaneID == "" {
			return false
		}
		snapshot, err := client.Snapshot(context.Background())
		if err != nil {
			return false
		}
		for _, pane := range snapshot.Panes {
			if pane.PaneID != project.LeadPaneID || pane.Agent != "claude" {
				continue
			}
			for _, agent := range snapshot.Agents {
				if agent.PaneID == pane.PaneID && agent.Name == "posse-shop-lead-1" {
					return true // The detached startup finalizer has adopted the Lead.
				}
			}
		}
		return false
	}) {
		pane, _ := client.Run(context.Background(), "pane", "read", workspace.RootPane.PaneID, "--source", "recent-unwrapped", "--lines", "80")
		t.Fatalf("fresh-home up did not start the only available harness: %s", pane)
	}
	pane, paneErr := client.Run(context.Background(), "pane", "read", project.LeadPaneID, "--source", "recent-unwrapped", "--lines", "80")
	if paneErr != nil {
		t.Fatalf("read startup output: %v %s", paneErr, pane)
	}
	for _, want := range []string{"kind: claude", "kind_source: only available kind", "gate_empty", "autonomy_ask"} {
		if !strings.Contains(string(pane), want) {
			t.Fatalf("up omitted %q: %s", want, pane)
		}
	}
	leadEnv := setEnv(env, "HERDR_ENV", "1")
	leadEnv = setEnv(leadEnv, "HERDR_PANE_ID", project.LeadPaneID)
	leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", project.HerdrWorkspaceID)
	for _, command := range [][]string{nil, {"lead"}} {
		output := runPosse(t, binary, repo, leadEnv, command...)
		for _, want := range []string{"gate_empty", "autonomy_ask"} {
			if !strings.Contains(output, want) {
				t.Fatalf("%v omitted %q: %s", command, want, output)
			}
		}
	}
	var leadResult struct {
		Instructions string `json:"instructions"`
	}
	leadOutput := runPosse(t, binary, repo, leadEnv, "lead", "--json")
	if err := json.Unmarshal([]byte(leadOutput), &leadResult); err != nil {
		t.Fatalf("decode posse lead instructions: %v\n%s", err, leadOutput)
	}
	startupInstructions, err := os.ReadFile(filepath.Join(root, "posse", "projects", "shop", "lead.md"))
	if err != nil {
		t.Fatal(err)
	}
	if leadResult.Instructions != string(startupInstructions) {
		t.Fatalf("startup and posse lead instructions differ:\nstartup:\n%s\nlead:\n%s", startupInstructions, leadResult.Instructions)
	}
	for _, want := range []string{"one Ship Brief and dispatch it directly", "Scouts, specifications, tickets and separate review Tasks are optional", "## Runtime obligations", "Never edit the Project repository", "Posse does not support those paths yet", "This is a status update, not another approval step"} {
		if !strings.Contains(leadResult.Instructions, want) {
			t.Fatalf("composed instructions omit %q: %s", want, leadResult.Instructions)
		}
	}
	preferenceFile := filepath.Join(root, "project-preferences.md")
	const preferenceText = "For this Project, choose concise implementation Briefs and skip unnecessary exploration.\n"
	if err := os.WriteFile(preferenceFile, []byte(preferenceText), 0600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, binary, repo, env, "preferences", "set", "lead", "--file", preferenceFile, "--project", "shop")
	leadOutput = runPosse(t, binary, repo, leadEnv, "lead", "--json")
	if err := json.Unmarshal([]byte(leadOutput), &leadResult); err != nil {
		t.Fatalf("decode updated posse lead instructions: %v\n%s", err, leadOutput)
	}
	if !strings.Contains(leadResult.Instructions, preferenceText) || !strings.Contains(leadResult.Instructions, "## Runtime obligations") || !strings.Contains(leadResult.Instructions, "Never edit the Project repository") {
		t.Fatalf("Project preferences did not affect guidance while retaining runtime obligations: %s", leadResult.Instructions)
	}
	riderPreferences := filepath.Join(root, "rider-preferences.md")
	const originalRiderPreferences = "Original Rider preferences.\n"
	if err := os.WriteFile(riderPreferences, []byte(originalRiderPreferences), 0600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, binary, repo, env, "preferences", "set", "rider", "--file", riderPreferences, "--project", "shop")
	brief := filepath.Join(root, "ship.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: First change\ndone_when: change verified\n---\nMake a change.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := runPosse(t, binary, repo, leadEnv, "dispatch", "--brief", brief)
	if !strings.Contains(output, "implicit: lead kind") || !strings.Contains(output, "claude") {
		t.Fatalf("dispatch: %s", output)
	}
	output = runPosse(t, binary, repo, leadEnv, "ride", "--brief", brief, "--name", "first-change")
	if !strings.Contains(output, "working") {
		t.Fatalf("ride: %s", output)
	}
	task, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil || task.State != store.StateWorking || !strings.Contains(task.Profile, "claude") {
		t.Fatalf("implicit Task: %#v %v", task, err)
	}
	launchPath := filepath.Join(root, "posse", "projects", "shop", "tasks", "t1", "launch.md")
	launchSnapshot, err := os.ReadFile(launchPath)
	if err != nil || !strings.Contains(string(launchSnapshot), originalRiderPreferences) {
		t.Fatalf("Rider launch omitted its admitted preferences: %s, %v", launchSnapshot, err)
	}
	const updatedRiderPreferences = "Updated Rider preferences for future Tasks.\n"
	if err := os.WriteFile(riderPreferences, []byte(updatedRiderPreferences), 0600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, binary, repo, env, "preferences", "set", "rider", "--file", riderPreferences, "--project", "shop")
	runPosse(t, binary, repo, leadEnv, "relaunch", "t1")
	afterUpdate, err := os.ReadFile(launchPath)
	if err != nil || string(afterUpdate) != string(launchSnapshot) || strings.Contains(string(afterUpdate), updatedRiderPreferences) {
		t.Fatalf("editing preferences changed an existing Rider snapshot during relaunch: before=%q after=%q err=%v", launchSnapshot, afterUpdate, err)
	}
	for _, path := range []string{filepath.Join(root, "posse", "config.toml"), filepath.Join(root, "posse", "projects", "shop", "config.toml")} {
		content, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if strings.Contains(string(content), "claude") || strings.Contains(string(content), "implicit") {
			t.Fatalf("detected selection persisted: %s", content)
		}
	}
}
