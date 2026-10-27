package app

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	setupassets "github.com/thanhbinh1905/posse/internal/setup"
)

func TestFreezePosseLaunchSkillsBuildsExactHarnessInputs(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "posse-home")
	userHome := filepath.Join(root, "user-home")
	piHome := filepath.Join(userHome, ".pi", "agent")
	projectRoot := filepath.Join(userHome, "project")
	t.Setenv("HOME", userHome)
	t.Setenv("PI_CODING_AGENT_DIR", piHome)
	for name, content := range map[string]string{
		filepath.Join(userHome, ".agents", "skills", "posse", "SKILL.md"):          "---\nname: posse\ndescription: old global\n---\nold\n",
		filepath.Join(userHome, ".agents", "skills", "posse-setup", "SKILL.md"):    "---\nname: posse-setup\ndescription: old global\n---\nold\n",
		filepath.Join(userHome, ".agents", "skills", "unrelated-user", "SKILL.md"): "---\nname: unrelated-user\ndescription: User skill\n---\nkeep\n",
		filepath.Join(piHome, "skills", "pi-user", "SKILL.md"):                     "---\nname: pi-user\ndescription: Pi user skill\n---\nkeep\n",
		filepath.Join(projectRoot, ".pi", "skills", "project-user", "SKILL.md"):    "---\nname: project-user\ndescription: Project skill\n---\nkeep\n",
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	skills, err := freezePosseLaunchSkills(home, "shop", projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	claudeArgs, err := appendPosseSkillLaunchArgs("claude", nil, skills)
	if err != nil || !equalStrings(claudeArgs, []string{"--plugin-dir", skills.ClaudePluginDir}) {
		t.Fatalf("Claude skill args = %#v, %v", claudeArgs, err)
	}
	piArgs, err := appendPosseSkillLaunchArgs("pi", nil, skills)
	if err != nil || len(piArgs) == 0 || piArgs[0] != "--no-skills" {
		t.Fatalf("Pi skill args = %#v, %v", piArgs, err)
	}
	var piSkillPaths []string
	for index := 1; index+1 < len(piArgs); index++ {
		if piArgs[index] == "--skill" {
			piSkillPaths = append(piSkillPaths, piArgs[index+1])
			index++
		}
	}
	for _, expected := range []string{
		filepath.Join(userHome, ".agents", "skills", "unrelated-user", "SKILL.md"),
		filepath.Join(piHome, "skills", "pi-user", "SKILL.md"),
	} {
		if !containsString(piSkillPaths, expected) {
			t.Errorf("Pi launch omitted unrelated skill %s: %#v", expected, piSkillPaths)
		}
	}
	projectSkill := filepath.Join(projectRoot, ".pi", "skills", "project-user", "SKILL.md")
	if containsString(piSkillPaths, projectSkill) {
		t.Errorf("Pi launch exposed a Project skill despite preserving only User skills: %#v", piSkillPaths)
	}
	for _, name := range setupassets.SkillNames() {
		legacy := filepath.Join(userHome, ".agents", "skills", name, "SKILL.md")
		if containsString(piSkillPaths, legacy) {
			t.Errorf("Pi launch passed legacy global %s alongside frozen copy: %#v", name, piSkillPaths)
		}
	}
	if frozenRoot := filepath.Join(skills.ClaudePluginDir, "skills"); !containsString(piSkillPaths, frozenRoot) {
		t.Errorf("Pi launch omitted the frozen skills directory %s: %#v", frozenRoot, piSkillPaths)
	}
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex-home"))
	codexArgs, err := appendPosseSkillLaunchArgs("codex", nil, skills)
	if err != nil || len(codexArgs) != 2 || codexArgs[0] != "-c" {
		t.Fatalf("Codex skill args = %#v, %v", codexArgs, err)
	}
	value, found := strings.CutPrefix(codexArgs[1], "developer_instructions=")
	var instructions string
	if !found || json.Unmarshal([]byte(value), &instructions) != nil {
		t.Fatalf("Codex developer_instructions is not a quoted string: %#v", codexArgs)
	}
	skillsRoot := filepath.Join(skills.ClaudePluginDir, "skills")
	if strings.Count(instructions, skillsRoot) != 1 || !strings.Contains(instructions, filepath.Join(skillsRoot, "{name}", "SKILL.md")) || !strings.Contains(instructions, strings.Join(setupassets.SkillNames(), ", ")) {
		t.Fatalf("Codex developer instructions must name the frozen skills directory once and list every skill: %s", instructions)
	}
	for _, name := range setupassets.SkillNames() {
		want, err := setupassets.Skill(name)
		if err != nil {
			t.Fatal(err)
		}
		if name == "posse" && !strings.Contains(string(want), "directly gives follow-up work") {
			t.Fatalf("embedded posse skill omits direct User follow-up guidance: %s", want)
		}
		frozenPath := filepath.Join(skills.ClaudePluginDir, "skills", name, "SKILL.md")
		got, err := os.ReadFile(frozenPath)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("frozen skill %s = %q, %v; want embedded bytes", name, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(skills.ClaudePluginDir, ".claude-plugin", "plugin.json")); err != nil {
		t.Fatalf("Claude plugin manifest is missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectRoot, ".pi", "skills", "project-user", "SKILL.md")); err != nil {
		t.Fatalf("Project skill was changed or removed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(projectRoot, "launch-skills")); !os.IsNotExist(err) {
		t.Fatalf("skill injection wrote a snapshot into the Project: %v", err)
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

func TestFrozenSkillSnapshotSurvivesInterruptedWrite(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "posse-home")
	userHome := filepath.Join(root, "user-home")
	piHome := filepath.Join(userHome, ".pi", "agent")
	project := filepath.Join(userHome, "project")
	t.Setenv("HOME", userHome)
	t.Setenv("PI_CODING_AGENT_DIR", piHome)
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	skills, err := freezePosseLaunchSkills(home, "shop", project)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(skills.ClaudePluginDir, "skills", "posse", "SKILL.md")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(root, "writer-ready")
	command := exec.Command(os.Args[0], "-test.run=^TestFrozenSkillInterruptionHelper$")
	command.Env = append(os.Environ(),
		"POSSE_TEST_FROZEN_SKILL_INTERRUPT=1",
		"POSSE_TEST_FROZEN_SKILL_ROOT="+skills.ClaudePluginDir,
		"POSSE_TEST_FROZEN_SKILL_PATH="+path,
		"POSSE_TEST_FROZEN_SKILL_READY="+ready,
	)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("interrupted writer did not reach its partial-write pause: %s", output.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("final skill path is visible before the write completes: stat err=%v", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	finished = true

	if _, err := freezePosseLaunchSkills(home, "shop", project); err != nil {
		t.Fatalf("retry after interrupted snapshot write: %v", err)
	}
	want, err := setupassets.Skill("posse")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("retried frozen skill differs from embedded content: err=%v", err)
	}
}

func TestFrozenSkillConcurrentFirstWritesAreAtomic(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "skills", "posse", "SKILL.md")
	contents := bytes.Repeat([]byte("frozen skill content\n"), 32_768)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	write := func(file *os.File, data []byte) error {
		middle := len(data) / 2
		if _, err := file.Write(data[:middle]); err != nil {
			return err
		}
		started <- struct{}{}
		<-release
		_, err := file.Write(data[middle:])
		return err
	}
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- writeFrozenSkillWith(root, path, contents, write) }()
	}
	arrived, received := 0, 0
	deadline := time.After(2 * time.Second)
	for arrived < 2 {
		select {
		case <-started:
			arrived++
		case err := <-errs:
			received++
			if err != nil {
				t.Errorf("concurrent first write failed before both writers reached the pause: %v", err)
			}
			goto releaseWriters
		case <-deadline:
			t.Errorf("only %d concurrent writers reached the write pause", arrived)
			goto releaseWriters
		}
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("final skill path is visible before both writes complete: stat err=%v", err)
	}

releaseWriters:
	close(release)
	for received < 2 {
		if err := <-errs; err != nil {
			t.Errorf("concurrent first write failed: %v", err)
		}
		received++
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("concurrent first-write result differs from full content: bytes=%d err=%v", len(got), err)
	}
}

func TestFrozenSkillInterruptionHelper(t *testing.T) {
	if os.Getenv("POSSE_TEST_FROZEN_SKILL_INTERRUPT") != "1" {
		return
	}
	root := os.Getenv("POSSE_TEST_FROZEN_SKILL_ROOT")
	path := os.Getenv("POSSE_TEST_FROZEN_SKILL_PATH")
	ready := os.Getenv("POSSE_TEST_FROZEN_SKILL_READY")
	contents, err := setupassets.Skill("posse")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFrozenSkillWith(root, path, contents, func(file *os.File, contents []byte) error {
		if _, err := file.Write(contents[:8]); err != nil {
			return err
		}
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			return err
		}
		select {}
	}); err != nil {
		t.Fatal(err)
	}
	t.Fatal("interrupted writer unexpectedly returned")
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
