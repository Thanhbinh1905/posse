//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
)

type skillsFixture struct {
	binary string
	home   string
	env    []string
}

func newSkillsFixture(t *testing.T) skillsFixture {
	t.Helper()
	root := newFixtureRoot(t, herdr.TestRootName())
	binary := filepath.Join(root, "bin", "posse")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = moduleRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse: %v\n%s", err, output)
	}
	return skillsFixture{binary: binary, home: filepath.Join(root, "posse"), env: isolatedE2EEnv(t, root)}
}

func (f skillsFixture) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	command := exec.Command(f.binary, args...)
	command.Dir, command.Env = f.home, f.env
	output, err := command.CombinedOutput()
	return string(output), err
}

func (f skillsFixture) configured(t *testing.T) map[string][]string {
	t.Helper()
	configured, err := config.ConfiguredSkills(filepath.Join(f.home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return configured
}

func TestSkillsAddNonInteractive(t *testing.T) {
	f := newSkillsFixture(t)
	if err := os.MkdirAll(f.home, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(f.home, "config.toml")
	if err := os.WriteFile(configPath, []byte("# keep me\n[defaults]\nreview = \"none\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := f.run(t, "skills", "add"); err == nil || !strings.Contains(output, "needs --all or skill names") {
		t.Fatalf("add without a terminal or names: err=%v output=%s", err, output)
	}
	if output, err := f.run(t, "skills", "add", "posse-nope"); err == nil || !strings.Contains(output, "not a recommended skill") {
		t.Fatalf("unknown name: err=%v output=%s", err, output)
	}
	output, err := f.run(t, "skills", "add", "posse-tdd", "posse-review")
	if err != nil {
		t.Fatalf("add names: %v\n%s", err, output)
	}
	for _, want := range []string{"role: change", "role: review", "skills[1]: posse-tdd", "skills[2]: posse-review,posse-tdd"} {
		if !strings.Contains(output, want) {
			t.Fatalf("result lacks %q:\n%s", want, output)
		}
	}
	if got := f.configured(t); strings.Join(got["change"], ",") != "posse-tdd" || strings.Join(got["review"], ",") != "posse-review,posse-tdd" || len(got["lead"]) != 0 {
		t.Fatalf("configured after names = %#v", got)
	}
	if output, err := f.run(t, "skills", "add", "--all"); err != nil || !strings.Contains(output, "role: lead") {
		t.Fatalf("add --all: %v\n%s", err, output)
	}
	got := f.configured(t)
	if strings.Join(got["change"], ",") != "posse-tdd,posse-deslop,posse-no-comments,posse-technical-writing,posse-unslop" || len(got["lead"]) != 15 || len(got["research"]) != 3 {
		t.Fatalf("configured after --all = %#v", got)
	}
	contents, err := os.ReadFile(configPath)
	if err != nil || !strings.HasPrefix(string(contents), "# keep me\n[defaults]\nreview = \"none\"\n") {
		t.Fatalf("comments and settings were not kept: %v\n%s", err, contents)
	}
	if output, err := f.run(t, "skills"); err != nil || !strings.Contains(output, "recommended_missing: []") {
		t.Fatalf("skills after --all: %v\n%s", err, output)
	}
}
