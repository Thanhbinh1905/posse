package app

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
)

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
