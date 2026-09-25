package prepare

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestMain(m *testing.M) {
	_ = os.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Exit(m.Run())
}

func TestClaudeTrustPreservesExistingValuesAndCarriesOnlyApprovedImports(t *testing.T) {
	project, worktree := gitProjectAndWorktree(t)
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	storePath := filepath.Join(configDir, ".claude.json")
	if err := os.WriteFile(storePath, []byte(`{"other":{"keep":true},"projects":{"`+project+`":{"custom":"value","hasClaudeMdExternalIncludesApproved":true,"hasClaudeMdExternalIncludesWarningShown":false}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ClaudeTrust(worktree, project); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]any {
		t.Helper()
		data, err := os.ReadFile(storePath)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	result := read()
	if result["other"].(map[string]any)["keep"] != true {
		t.Fatalf("unrelated Claude config changed: %#v", result)
	}
	projects := result["projects"].(map[string]any)
	primary := projects[project].(map[string]any)
	worker := projects[worktree].(map[string]any)
	if primary["hasTrustDialogAccepted"] != true || primary["custom"] != "value" {
		t.Fatalf("primary trust config = %#v", primary)
	}
	if worker["hasTrustDialogAccepted"] != true || worker["hasClaudeMdExternalIncludesApproved"] != true {
		t.Fatalf("worktree trust config = %#v", worker)
	}
	if worker["hasClaudeMdExternalIncludesWarningShown"] != false {
		t.Fatalf("import warning state was not carried exactly: %#v", worker)
	}
	if err := ClaudeTrust(worktree, project); err != nil {
		t.Fatal(err)
	}
	_ = read()
}

func TestClaudeTrustDoesNotCarryRecordedImportDecline(t *testing.T) {
	project, worktree := gitProjectAndWorktree(t)
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	storePath := filepath.Join(configDir, ".claude.json")
	contents := `{"projects":{"` + project + `":{"hasClaudeMdExternalIncludesApproved":false,"hasClaudeMdExternalIncludesWarningShown":true}}}`
	if err := os.WriteFile(storePath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ClaudeTrust(worktree, project); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Projects map[string]map[string]json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	primary := result.Projects[project]
	worker := result.Projects[worktree]
	if string(primary["hasClaudeMdExternalIncludesApproved"]) != "false" || string(primary["hasTrustDialogAccepted"]) != "true" {
		t.Fatalf("primary user decision changed: %s", data)
	}
	if _, exists := worker["hasClaudeMdExternalIncludesApproved"]; exists {
		t.Fatalf("recorded No was copied or overwritten: %s", data)
	}
}

func TestClaudeTrustNeverOverwritesRecordedFalseOrMountImportAnswers(t *testing.T) {
	project, worktree := gitProjectAndWorktree(t)
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	storePath := filepath.Join(configDir, ".claude.json")
	contents := `{"projects":{"` + project + `":{"hasTrustDialogAccepted":false,"hasClaudeMdExternalIncludesApproved":true,"hasClaudeMdExternalIncludesWarningShown":true},"` + worktree + `":{"hasTrustDialogAccepted":false,"hasClaudeMdExternalIncludesApproved":false,"hasClaudeMdExternalIncludesWarningShown":true}}}`
	if err := os.WriteFile(storePath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ClaudeTrust(worktree, project); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Projects map[string]map[string]json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	primary := result.Projects[project]
	mount := result.Projects[worktree]
	if string(primary["hasTrustDialogAccepted"]) != "false" || string(mount["hasTrustDialogAccepted"]) != "false" {
		t.Fatalf("recorded trust decisions were overwritten: %s", data)
	}
	if string(mount["hasClaudeMdExternalIncludesApproved"]) != "false" || string(mount["hasClaudeMdExternalIncludesWarningShown"]) != "true" {
		t.Fatalf("Mount import answers were overwritten: %s", data)
	}
}

func TestClaudeTrustRejectsUnrelatedDirectoryAndSymlinkedStore(t *testing.T) {
	project, worktree := gitProjectAndWorktree(t)
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	plain := t.TempDir()
	if err := ClaudeTrust(plain, project); err == nil {
		t.Fatal("trusted a plain directory")
	}
	storePath := filepath.Join(configDir, ".claude.json")
	target := filepath.Join(configDir, "target.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, storePath); err != nil {
		t.Fatal(err)
	}
	if err := ClaudeTrust(worktree, project); err == nil {
		t.Fatal("modified a symlinked Claude config store")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != `{}` {
		t.Fatalf("symlink target changed: %q %v", data, err)
	}
}

func TestCodexTrustPreservesOtherConfigTables(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	path := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(path, []byte("model = 'gpt-5-codex'\n\n[profiles.fast]\nmodel = 'gpt-5-mini'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(t.TempDir(), "worker")
	if err := CodexTrust(worktree); err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if _, err := toml.DecodeFile(path, &config); err != nil {
		t.Fatal(err)
	}
	if config["model"] != "gpt-5-codex" || config["profiles"].(map[string]any)["fast"].(map[string]any)["model"] != "gpt-5-mini" || config["projects"].(map[string]any)[worktree].(map[string]any)["trust_level"] != "trusted" {
		t.Fatalf("Codex config = %#v", config)
	}
}

func TestCodexTrustAppendsWithoutReencodingCommentsOrKeyOrder(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	path := filepath.Join(codexHome, "config.toml")
	original := "# preserve this comment\nmodel = 'gpt-5-codex'\n\n[profiles.fast]\nmodel = 'gpt-5-mini'\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(t.TempDir(), "mount with \"quotes\"")
	if err := CodexTrust(worktree); err != nil {
		t.Fatal(err)
	}
	quoted, err := json.Marshal(worktree)
	if err != nil {
		t.Fatal(err)
	}
	want := original + "\n[projects." + string(quoted) + "]\ntrust_level = \"trusted\"\n"
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("Codex config changed beyond the appended block:\nwant: %q\ngot:  %q\nerr: %v", want, got, err)
	}
	if err := CodexTrust(worktree); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("second Codex trust run changed bytes: %q %v", got, err)
	}
}

func gitProjectAndWorktree(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	worktree := filepath.Join(root, "worktree")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, project, "init", "-b", "main")
	runGit(t, project, "config", "user.name", "Test User")
	runGit(t, project, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(project, "README.md"), []byte("project\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, project, "add", "README.md")
	runGit(t, project, "commit", "-m", "initial")
	runGit(t, project, "worktree", "add", "-b", "posse/t1", worktree)
	return project, worktree
}

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"-C", root}, args...)
	command := exec.Command("git", commandArgs...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v: %s", args, err, output)
	}
}
