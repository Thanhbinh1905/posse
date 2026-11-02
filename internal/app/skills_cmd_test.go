package app

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
)

func TestEffectiveConfiguredSkillsUsesProjectOverrides(t *testing.T) {
	global := map[string][]string{"change": {"posse-tdd"}, "review": {"posse-review"}}
	project := map[string][]string{"change": {"posse-unslop"}, "review": {}}
	effective := effectiveConfiguredSkills(global, project)
	if !reflect.DeepEqual(effective["change"], project["change"]) || effective["review"] == nil || len(effective["review"]) != 0 || !reflect.DeepEqual(effective["lead"], global["lead"]) {
		t.Fatalf("effective configured skills = %#v", effective)
	}
}

func TestSkillsAddProjectPreservesExplicitRoleOverrides(t *testing.T) {
	for _, test := range []struct {
		name, projectConfig string
		want                []string
	}{
		{name: "override", projectConfig: "[skills]\nchange = [\"posse-unslop\"]\n", want: []string{"posse-unslop", "posse-no-comments"}},
		{name: "explicit empty override", projectConfig: "[skills]\nchange = []\n", want: []string{"posse-no-comments"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.WriteFile(config.ConfigPath(home, ""), []byte("[skills]\nchange = [\"posse-tdd\", \"posse-deslop\"]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			service := testService(home, nil)
			db, _, err := service.openDB()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.CreateProject(context.Background(), "shop", t.TempDir(), "main"); err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(config.ConfigPath(home, "shop"), []byte(test.projectConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			code, output := runCLI(t, service, "skills", "add", "--project", "shop", "posse-no-comments")
			if code != 0 {
				t.Fatalf("posse skills add failed: %s", output)
			}
			got, err := config.ConfiguredSkills(config.ConfigPath(home, "shop"))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got["change"], test.want) {
				t.Fatalf("Project change skills = %v, want %v; output=%s", got["change"], test.want, output)
			}
		})
	}
}

func TestSkillsAddProjectStartsWithGlobalRoleSkills(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(config.ConfigPath(home, ""), []byte("[skills]\nchange = [\"posse-tdd\", \"posse-deslop\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	db, _, err := service.openDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProject(context.Background(), "shop", t.TempDir(), "main"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	code, output := runCLI(t, service, "skills", "add", "--project", "shop", "posse-no-comments")
	if code != 0 {
		t.Fatalf("posse skills add failed: %s", output)
	}
	got, err := config.ConfiguredSkills(config.ConfigPath(home, "shop"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"posse-tdd", "posse-deslop", "posse-no-comments"}
	if !reflect.DeepEqual(got["change"], want) {
		t.Fatalf("Project change skills = %v, want global skills plus the selection %v; output=%s", got["change"], want, output)
	}
}
