//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func Test014LegacyReviewBlocksUpdatePreflightUntilRepaired(t *testing.T) {
	f := newPRLifecycleFixtureWithManualWatchOwner(t)
	defer f.db.Close()

	configPath := filepath.Join(f.home, "projects", f.project.Name, "config.toml")
	for _, suffix := range []string{".v014", ".vocabulary", ".vocabulary-lock"} {
		if err := os.Remove(configPath + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	original := "# 0.14 Project config\n[defaults]\nreview = \"on_risk\"\nlanding_mode = 'pr' # Keep this comment\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	preflight := func() ([]byte, error) {
		command := exec.Command(f.binary, "_update-preflight", "--json")
		command.Dir, command.Env = f.repo, f.env
		return command.CombinedOutput()
	}
	output, err := preflight()
	if err == nil || !strings.Contains(string(output), `"code":"config_invalid"`) || !strings.Contains(string(output), "legacy review policy") || !strings.Contains(string(output), "on_risk") {
		t.Fatalf("preflight did not refuse the legacy policy: err=%v output=%s", err, output)
	}
	if data, err := os.ReadFile(configPath); err != nil || string(data) != original {
		t.Fatalf("preflight changed Project config: %q err=%v", data, err)
	}
	for _, suffix := range []string{".v014", ".vocabulary", ".vocabulary-lock"} {
		if _, err := os.Stat(configPath + suffix); !os.IsNotExist(err) {
			t.Fatalf("preflight created %s: %v", suffix, err)
		}
	}

	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", "none", "--project", f.project.Name)
	if output, err = preflight(); err != nil {
		t.Fatalf("preflight rejected repaired 0.14 config: %v %s", err, output)
	}
	migrated, err := os.ReadFile(configPath)
	if err != nil || !strings.Contains(string(migrated), "# 0.14 Project config") || !strings.Contains(string(migrated), "merge_mode = 'pr' # Keep this comment") {
		t.Fatalf("config repair lost the migration layout: %q err=%v", migrated, err)
	}
}
