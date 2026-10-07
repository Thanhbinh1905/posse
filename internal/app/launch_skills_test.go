package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	setupassets "github.com/thanhbinh1905/posse/internal/setup"
)

func TestFreezePosseLaunchSkillsBuildsExactHarnessInputs(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "posse-home")
	projectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	skills, err := freezePosseLaunchSkills(home, "shop", projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	claudeArgs, err := posseSkillLaunchArgs("claude", skills)
	if err != nil || !equalStrings(claudeArgs, []string{"--plugin-dir", skills.ClaudePluginDir}) {
		t.Fatalf("Claude skill args = %#v, %v", claudeArgs, err)
	}
	piArgs, err := posseSkillLaunchArgs("pi", skills)
	if err != nil || len(piArgs) != 4 || piArgs[0] != "--skill" || piArgs[2] != "--skill" {
		t.Fatalf("Pi skill args = %#v, %v", piArgs, err)
	}
	codexArgs, err := posseSkillLaunchArgs("codex", skills)
	if err != nil || len(codexArgs) != 2 || codexArgs[0] != "-c" {
		t.Fatalf("Codex skill args = %#v, %v", codexArgs, err)
	}
	value, found := strings.CutPrefix(codexArgs[1], "developer_instructions=")
	var instructions string
	if !found || json.Unmarshal([]byte(value), &instructions) != nil {
		t.Fatalf("Codex developer_instructions is not a quoted string: %#v", codexArgs)
	}
	for _, name := range []string{"posse", "posse-setup"} {
		want, err := setupassets.Skill(name)
		if err != nil {
			t.Fatal(err)
		}
		frozenPath := filepath.Join(skills.ClaudePluginDir, "skills", name, "SKILL.md")
		got, err := os.ReadFile(frozenPath)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("frozen skill %s = %q, %v; want embedded bytes", name, got, err)
		}
		if !strings.Contains(instructions, frozenPath) {
			t.Fatalf("Codex developer instructions omitted exact frozen skill path %s", frozenPath)
		}
		if piArgs[1+2*indexOfString([]string{"posse", "posse-setup"}, name)] != frozenPath {
			t.Fatalf("Pi skill path for %s = %q, want %q", name, piArgs, frozenPath)
		}
	}
	if _, err := os.Stat(filepath.Join(skills.ClaudePluginDir, ".claude-plugin", "plugin.json")); err != nil {
		t.Fatalf("Claude plugin manifest is missing: %v", err)
	}
	if entries, err := os.ReadDir(projectRoot); err != nil || len(entries) != 0 {
		t.Fatalf("skill injection wrote into the Project: entries=%v err=%v", entries, err)
	}

	modifiedPath := filepath.Join(skills.ClaudePluginDir, "skills", "posse", "SKILL.md")
	if err := os.WriteFile(modifiedPath, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := freezePosseLaunchSkills(home, "shop", projectRoot); err == nil || !strings.Contains(err.Error(), "snapshot was modified") {
		t.Fatalf("modified frozen snapshot was silently replaced: %v", err)
	}
}

func TestFreezePosseLaunchSkillsRejectsSnapshotSymlinkIntoProject(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "posse-home")
	projectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(home, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(projectRoot, filepath.Join(home, "projects", "shop")); err != nil {
		t.Fatal(err)
	}
	if _, err := freezePosseLaunchSkills(home, "shop", projectRoot); err == nil || !strings.Contains(err.Error(), "managed directory") {
		t.Fatalf("snapshot symlink into Project was not rejected: %v", err)
	}
	if entries, err := os.ReadDir(projectRoot); err != nil || len(entries) != 0 {
		t.Fatalf("snapshot symlink created Project files: entries=%v err=%v", entries, err)
	}
}

func indexOfString(values []string, wanted string) int {
	for index, value := range values {
		if value == wanted {
			return index
		}
	}
	return -1
}
