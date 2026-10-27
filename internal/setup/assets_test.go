package setup

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"gopkg.in/yaml.v3"
)

func TestLifecycleSkillsAreEmbeddedAndUserInvoked(t *testing.T) {
	for _, name := range []string{"posse-down", "posse-update"} {
		data, err := Skill(name)
		if err != nil {
			t.Fatalf("Skill(%q): %v", name, err)
		}
		content := string(data)
		for _, required := range []string{"name: " + name, "disable-model-invocation: true"} {
			if !strings.Contains(content, required) {
				t.Errorf("%s does not contain %q", name, required)
			}
		}
		if !strings.Contains(content, "Never run the command yourself") && !strings.Contains(content, "never run them yourself") {
			t.Errorf("%s must reserve command execution for the User", name)
		}
		if name == "posse-down" && (!strings.Contains(content, "`posse down`") || strings.Contains(content, "run `posse down` yourself")) {
			t.Errorf("%s must name the exact command and reserve execution for the User", name)
		}
		if name == "posse-update" && (!strings.Contains(content, "`posse update`") || strings.Contains(content, "run `posse update` yourself")) {
			t.Errorf("%s must name the exact command and reserve execution for the User", name)
		}
	}
}

func TestShippedSkillFrontmatter(t *testing.T) {
	directories, err := fs.Glob(files, "skills/*/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(directories) != len(skillNames) {
		t.Errorf("skills directory has %d skills, skillNames lists %d: %q", len(directories), len(skillNames), directories)
	}
	for _, path := range directories {
		directory := strings.Split(path, "/")[1]
		if !IsSkill(directory) {
			t.Errorf("skill %s is not listed in skillNames", directory)
			continue
		}
		data, err := Skill(directory)
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.SplitN(string(data), "---\n", 3)
		if len(parts) != 3 || parts[0] != "" {
			t.Errorf("%s has no frontmatter block", directory)
			continue
		}
		var frontmatter struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
		}
		if err := yaml.Unmarshal([]byte(parts[1]), &frontmatter); err != nil {
			t.Errorf("%s frontmatter does not parse: %v", directory, err)
			continue
		}
		if frontmatter.Name != directory {
			t.Errorf("%s frontmatter name = %q, want the directory name", directory, frontmatter.Name)
		}
		if frontmatter.Description == "" || len(frontmatter.Description) > 1024 {
			t.Errorf("%s description has %d characters, want 1-1024", directory, len(frontmatter.Description))
		}
	}
}

func TestEveryWorkflowSkillIsRecommended(t *testing.T) {
	covered := map[string]bool{}
	for _, name := range PlatformSkillNames() {
		covered[name] = true
	}
	for role, names := range RecommendedSkills() {
		for _, name := range names {
			if !IsSkill(name) {
				t.Errorf("recommended %s skill %s is not embedded", role, name)
			}
			covered[name] = true
		}
	}
	for _, name := range SkillNames() {
		if !covered[name] {
			t.Errorf("embedded skill %s is neither a platform skill nor recommended", name)
		}
	}
}

func TestSkillSummaryIsTheFirstSentenceOfTheDescription(t *testing.T) {
	for _, name := range SkillNames() {
		summary := SkillSummary(name)
		if summary == "" || strings.Contains(summary, ". ") || strings.HasSuffix(summary, ".") {
			t.Errorf("%s summary = %q", name, summary)
		}
	}
	if SkillSummary("not-a-skill") != "" {
		t.Fatal("unknown skill has a summary")
	}
}

func TestReadSkillAssetsListsFilesBesideSkill(t *testing.T) {
	skill := fstest.MapFS{
		"SKILL.md":           {Data: []byte("skill")},
		"assets/board.html":  {Data: []byte("<html>")},
		"assets/js/board.js": {Data: []byte("render()")},
		".cache/state":       {Data: []byte("hidden")},
		"assets/.draft.html": {Data: []byte("hidden")},
		"assets/link.html":   {Data: []byte("board.html"), Mode: fs.ModeSymlink},
	}
	assets, err := ReadSkillAssets(skill)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, asset := range assets {
		got = append(got, asset.Path+"="+string(asset.Data))
	}
	if want := "assets/board.html=<html>,assets/js/board.js=render()"; strings.Join(got, ",") != want {
		t.Fatalf("assets = %q, want %q", strings.Join(got, ","), want)
	}
}

func TestEmbeddedSkillAssetsAreReadable(t *testing.T) {
	for _, name := range SkillNames() {
		if _, err := SkillAssets(name); err != nil {
			t.Errorf("SkillAssets(%q): %v", name, err)
		}
	}
	if _, err := SkillAssets("not-a-skill"); err == nil {
		t.Fatal("SkillAssets accepted an unknown skill")
	}
}
