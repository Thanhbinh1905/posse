package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	setupassets "github.com/thanhbinh1905/posse/internal/setup"
)

func TestSkillsResolveBundledNamesAndSkillDirectories(t *testing.T) {
	home := t.TempDir()
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	writeConfig(t, filepath.Join(userHome, "my-skills", "go-style", "SKILL.md"), "---\nname: go-style\ndescription: Go style.\n---\n")
	absolute := filepath.Join(userHome, "other")
	writeConfig(t, filepath.Join(absolute, "SKILL.md"), "---\ndescription: Named by its directory.\n---\n")
	writeConfig(t, filepath.Join(home, "config.toml"), `[skills]
lead = ["posse-spec"]
change = ["posse-tdd", "~/my-skills/go-style", "`+absolute+`", "posse-tdd"]
`)
	cfg, err := Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	change, err := cfg.RoleSkills("change")
	if err != nil {
		t.Fatal(err)
	}
	want := []SkillSource{{Name: "posse-tdd"}, {Name: "go-style", Path: filepath.Join(userHome, "my-skills", "go-style")}, {Name: "other", Path: absolute}}
	if !reflect.DeepEqual(change, want) {
		t.Fatalf("change skills = %#v, want %#v", change, want)
	}
	if review, err := cfg.RoleSkills("review"); err != nil || len(review) != 0 {
		t.Fatalf("review skills without a list = %#v, %v", review, err)
	}
}

func TestSkillsProjectListReplacesGlobalListForThatRole(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "config.toml"), "[skills]\nlead = [\"posse-spec\"]\nchange = [\"posse-tdd\", \"posse-deslop\"]\n")
	writeConfig(t, filepath.Join(home, "projects", "shop", "config.toml"), "[skills]\nchange = [\"posse-unslop\"]\n")
	cfg, err := Load(home, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Skills.Change, []string{"posse-unslop"}) || !reflect.DeepEqual(cfg.Skills.Lead, []string{"posse-spec"}) {
		t.Fatalf("merged skills = %#v", cfg.Skills)
	}
}

func TestSkillsBadEntryFailsLoadNamingIt(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	if err := os.MkdirAll(filepath.Join(userHome, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, filepath.Join(userHome, "clash", "SKILL.md"), "---\nname: posse-tdd\ndescription: Shadows a bundled skill.\n---\n")
	for _, test := range []struct {
		name, list, key, entry string
	}{
		{name: "unknown bundled name", list: `change = ["posse-nope"]`, key: "skills.change", entry: "posse-nope"},
		{name: "relative path", list: `review = ["my-skills/go-style"]`, key: "skills.review", entry: "my-skills/go-style"},
		{name: "missing directory", list: `lead = ["~/missing"]`, key: "skills.lead", entry: "~/missing"},
		{name: "directory without SKILL.md", list: `research = ["~/empty"]`, key: "skills.research", entry: "~/empty"},
		{name: "path shadowing a bundled skill", list: `change = ["~/clash"]`, key: "skills.change", entry: "~/clash"},
		{name: "not a list of strings", list: `change = "posse-tdd"`, key: "skills.change", entry: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			writeConfig(t, filepath.Join(home, "config.toml"), "[skills]\n"+test.list+"\n")
			_, err := Load(home, "")
			if err == nil || !strings.Contains(err.Error(), test.key) || !strings.Contains(err.Error(), test.entry) {
				t.Fatalf("Load error = %v, want one naming %s and %q", err, test.key, test.entry)
			}
		})
	}
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "config.toml"), "[skills]\nworker = [\"posse-tdd\"]\n")
	if _, err := Load(home, ""); err == nil || !strings.Contains(err.Error(), "skills.worker") {
		t.Fatalf("unknown role error = %v", err)
	}
}

func TestWriteRecommendedSkillsAppendsTheBlockOnce(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	original := "# keep this comment\n[defaults]\nmax_workers = 3\n"
	writeConfig(t, path, original)
	written, err := WriteRecommendedSkills(path)
	if err != nil || !written {
		t.Fatalf("WriteRecommendedSkills = %v, %v", written, err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(contents), original) {
		t.Fatalf("config lost its contents: %s err=%v", contents, err)
	}
	cfg, err := Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	recommended := setupassets.RecommendedSkills()
	for _, role := range SkillRoles {
		if !reflect.DeepEqual(cfg.Skills.Role(role), recommended[role]) {
			t.Fatalf("skills.%s = %v, want %v", role, cfg.Skills.Role(role), recommended[role])
		}
	}
	writeConfig(t, path, "[skills]\nchange = []\n")
	if written, err := WriteRecommendedSkills(path); err != nil || written {
		t.Fatalf("WriteRecommendedSkills over an existing [skills] block = %v, %v", written, err)
	}
}

func TestAddSkillsKeepsCommentsAndExistingEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeConfig(t, path, "# my config\n[defaults]\nreview = \"none\" # keep\n\n[skills]\n# mine\nchange = [\"~/x\", \"posse-tdd\"]\n")
	added, err := AddSkills(path, map[string][]string{"change": {"posse-tdd", "posse-deslop"}, "review": {"posse-review"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{"change": {"posse-deslop"}, "review": {"posse-review"}}; !reflect.DeepEqual(added, want) {
		t.Fatalf("added = %#v, want %#v", added, want)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# my config\n", "review = \"none\" # keep\n", "# mine\n", "\"~/x\",\n  \"posse-tdd\",\n  \"posse-deslop\",\n"} {
		if !strings.Contains(string(contents), want) {
			t.Fatalf("config lost %q:\n%s", want, contents)
		}
	}
	configured, err := ConfiguredSkills(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(configured["review"], []string{"posse-review"}) || len(configured["lead"]) != 0 {
		t.Fatalf("configured = %#v", configured)
	}
	if again, err := AddSkills(path, map[string][]string{"change": {"posse-deslop"}}); err != nil || len(again) != 0 {
		t.Fatalf("repeat add = %#v, %v", again, err)
	}
}

func TestAddSkillsCreatesTheSkillsBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if _, err := AddSkills(path, map[string][]string{"lead": {"posse-spec"}}); err != nil {
		t.Fatal(err)
	}
	cfg, err := ConfiguredSkills(path)
	if err != nil || !reflect.DeepEqual(cfg["lead"], []string{"posse-spec"}) {
		t.Fatalf("configured = %#v, %v", cfg, err)
	}
}
