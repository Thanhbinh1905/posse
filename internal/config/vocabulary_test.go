package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoredVocabularyMigratesOnceAndRejectsReintroducedKeys(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	original := "# preserve this for rollback\n[remuda]\nclean = 'warm'\nkeep_idle = 7\n[defaults]\nlanding_mode = 'pr'\nauto_unsaddle = 'landed'\n[[dispatch]]\ntype = 'ship'\nuse = 'builder'\n[profiles.builder]\nkind = 'pi'\n[kinds.claude]\nnotice_delivery = 'lookout'\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pool.KeepIdle != 7 || cfg.Defaults.MergeMode != "pr" || cfg.Defaults.AutoTeardown != "merged" || cfg.Dispatch[0].Type != "change" || cfg.Kinds["claude"].NoticeDelivery != "watch" {
		t.Fatalf("migrated config: %#v", cfg)
	}
	backup, err := os.ReadFile(path + ".v014")
	if err != nil || string(backup) != original {
		t.Fatalf("rollback backup lost original bytes: %q %v", backup, err)
	}
	data, _ := os.ReadFile(path)
	for _, old := range []string{"remuda", "landing_mode", "auto_unsaddle", "lookout", "ship"} {
		if strings.Contains(string(data), old) {
			t.Errorf("persisted config retains %s", old)
		}
	}
	for old, replacement := range map[string]string{"remuda.keep_idle": "pool.keep_idle", "defaults.landing_mode": "defaults.merge_mode", "defaults.auto_unsaddle": "defaults.auto_teardown", "autonomy.land": "autonomy.merge"} {
		if _, err := SetFile(path, old, "never", false); err == nil || !strings.Contains(err.Error(), replacement) {
			t.Errorf("old key %s: %v", old, err)
		}
	}
	if err := os.WriteFile(path, append(data, []byte("\n[remuda]\nkeep_idle = 9\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(home, ""); err == nil || !strings.Contains(err.Error(), "use pool") {
		t.Fatalf("old key became a working alias: %v", err)
	}
}

func TestRepairingLegacyReviewPreservesOriginalVocabularyBackup(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	original := "[defaults]\nreview = 'on_risk'\nlanding_mode = 'pr'\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SetFile(path, "defaults.review", "none", false); err != nil {
		t.Fatalf("repair review: %v", err)
	}
	if _, err := Load(home, ""); err != nil {
		t.Fatalf("load repaired config: %v", err)
	}
	backup, err := os.ReadFile(path + ".v014")
	if err != nil || string(backup) != original {
		t.Fatalf("migration backup = %q err=%v; want original %q", backup, err, original)
	}
}

func TestVocabularyMigrationPreservesCommentsAndLayout(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	original := "# User comment\n[remuda] # Pool settings\nkeep_idle   = 7 # Keep four extra Sandboxes\n\n[defaults]\nlanding_mode = 'pr' # Selected merge mode\nauto_unsaddle = 'landed'\n"
	want := "# posse vocabulary: 1\n# User comment\n[pool] # Pool settings\nkeep_idle   = 7 # Keep four extra Sandboxes\n\n[defaults]\nmerge_mode = 'pr' # Selected merge mode\nauto_teardown = 'merged'\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(home, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("migrated config = %q, err %v; want %q", data, err, want)
	}
}

func TestValidateAllChecksProjectConfigWithoutWriting(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "projects", "shop", "config.toml")
	original := "[defaults]\nreview = 'on_risk'\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAll(home); err == nil || !strings.Contains(err.Error(), "defaults.review") || !strings.Contains(err.Error(), "on_risk") {
		t.Fatalf("ValidateAll did not reject the legacy Project policy: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != original {
		t.Fatalf("read-only validation changed config: %q err=%v", data, err)
	}
	for _, suffix := range []string{".v014", ".vocabulary", ".vocabulary-lock"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("read-only validation created %s: %v", suffix, err)
		}
	}
}

func TestVocabularyMigrationPreservesInlineDispatchComments(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	original := "# Keep ship in this comment\ndispatch = [\n  { type = 'ship', use = 'builder' }, # legacy rule\n]\n"
	want := "# posse vocabulary: 1\n# Keep ship in this comment\ndispatch = [\n  { type = 'change', use = 'builder' }, # legacy rule\n]\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(home, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("migrated config = %q, err %v; want %q", data, err, want)
	}
}

func TestVocabularyUpgradeRejectsConflictingKeysWithoutWriting(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	original := "[defaults]\nlanding_mode = 'pr'\nmerge_mode = 'local'\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(home, ""); err == nil || !strings.Contains(err.Error(), "both defaults.landing_mode") {
		t.Fatalf("conflict: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != original {
		t.Fatal("failed migration changed config")
	}
}
