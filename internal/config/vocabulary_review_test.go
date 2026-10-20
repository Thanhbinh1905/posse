package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVocabularyMalformedConfigRetainsInvalidError(t *testing.T) {
	for _, reader := range []string{"load", "edit"} {
		t.Run(reader, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "config.toml")
			original := "[defaults\n"
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			if reader == "load" {
				_, err = Load(home, "")
			} else {
				_, _, err = ReadFile(path)
			}
			var invalid *InvalidError
			if !errors.As(err, &invalid) || invalid.File != path || invalid.Cause == nil {
				t.Fatalf("malformed config lost file context: %T %v", err, err)
			}
			if data, readErr := os.ReadFile(path); readErr != nil || string(data) != original {
				t.Fatalf("malformed config changed: %q %v", data, readErr)
			}
		})
	}
}

func TestVocabularyProjectShorthandUpgrade(t *testing.T) {
	for _, mode := range []string{"local", "no-mistakes"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "projects", "p", "config.toml")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			original := "# v0.14 Project shorthand\nlanding_mode = '" + mode + "'\nauto_unsaddle = 'landed'\ngate = ['just test']\n"
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				cfg, err := Load(home, "p")
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Defaults.MergeMode != mode || cfg.Defaults.AutoTeardown != "merged" || len(cfg.Defaults.Gate) != 1 || cfg.Defaults.Gate[0] != "just test" {
					t.Fatalf("Project shorthand changed meaning: %#v", cfg.Defaults)
				}
			}
			if backup, err := os.ReadFile(path + ".v014"); err != nil || string(backup) != original {
				t.Fatalf("original Project config lost: %q %v", backup, err)
			}
			for key, replacement := range map[string]string{"landing_mode": "merge_mode", "auto_unsaddle": "auto_teardown"} {
				if _, err := SetFile(path, key, "local", true); err == nil || !strings.Contains(err.Error(), "use "+replacement) {
					t.Fatalf("legacy shorthand became an input alias: %s: %v", key, err)
				}
			}
		})
	}
}

func TestVocabularyMigrationValidatesBeforeRecordingCompletion(t *testing.T) {
	for _, original := range []string{"not_a_setting = true\n", "landing_mode = 'local'\nnot_a_setting = true\n", "landing_mode = 'local'\nmerge_mode = 'pr'\n", "auto_unsaddle = 'landed'\nauto_teardown = 'never'\n"} {
		t.Run(strings.TrimSpace(original), func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "projects", "p", "config.toml")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(home, "p"); err == nil {
				t.Fatal("invalid config was accepted")
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != original {
				t.Fatalf("invalid config rewritten/marked: %q %v", data, err)
			}
			for _, suffix := range []string{".vocabulary", ".v014"} {
				if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
					t.Fatalf("invalid config recorded completion/backup %s: %v", suffix, err)
				}
			}
			if err := os.WriteFile(path, []byte("landing_mode = 'local'\nauto_unsaddle = 'landed'\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if cfg, err := Load(home, "p"); err != nil || cfg.Defaults.MergeMode != "local" || cfg.Defaults.AutoTeardown != "merged" {
				t.Fatalf("repaired config could not retry migration: %#v %v", cfg.Defaults, err)
			}
		})
	}
}

func TestReviewModeUpgradeFrom015PreservesLayoutAndRejectsLocalAlias(t *testing.T) {
	home := t.TempDir()
	globalPath := ConfigPath(home, "")
	projectPath := ConfigPath(home, "shop")
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o700); err != nil {
		t.Fatal(err)
	}
	globalOriginal := vocabularyMarkerV1 + "[defaults]\nreview = 'local' # global review\nmerge_mode = 'local' # local delivery stays local\n"
	projectOriginal := vocabularyMarkerV1 + "review = 'local' # project shorthand\n[repositories.api]\nreview = 'local' # member review\n"
	if err := os.WriteFile(globalPath, []byte(globalOriginal), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte(projectOriginal), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(home, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.Review != "quick" || cfg.Defaults.MergeMode != "local" || cfg.MemberReview("api") != "quick" {
		t.Fatalf("migration changed review semantics or local delivery: %#v", cfg)
	}
	globalWant := vocabularyMarker + "[defaults]\nreview = 'quick' # global review\nmerge_mode = 'local' # local delivery stays local\n"
	projectWant := vocabularyMarker + "review = 'quick' # project shorthand\n[repositories.api]\nreview = 'quick' # member review\n"
	for path, want := range map[string]string{globalPath: globalWant, projectPath: projectWant} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("migrated %s = %q, %v; want %q", path, got, err, want)
		}
	}
	for path, want := range map[string]string{globalPath + ".v015": globalOriginal, projectPath + ".v015": projectOriginal} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("backup %s = %q, %v; want original bytes %q", path, got, err, want)
		}
	}
	if _, err := SetFile(globalPath, "defaults.review", "local", false); err == nil || !strings.Contains(err.Error(), "use quick") {
		t.Fatalf("post-migration local config remained an alias: %v", err)
	}
	if _, err := SetFile(globalPath, "defaults.review", "full-pipeline", false); err != nil {
		t.Fatalf("full-pipeline could not be selected: %v", err)
	}
	cfg, err = Load(home, "shop")
	if err != nil || cfg.Defaults.Review != "quick" {
		t.Fatalf("Project override or frozen config changed: %#v %v", cfg.Defaults, err)
	}
}

func TestReviewModeUpgradeFailureLeaves015BytesUsableForRepair(t *testing.T) {
	home := t.TempDir()
	path := ConfigPath(home, "shop")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := vocabularyMarkerV1 + "[defaults]\nreview = 'local' # preserve\nunknown = true\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(home, "shop"); err == nil {
		t.Fatal("invalid configuration was accepted")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != original {
		t.Fatalf("failed migration changed the original bytes: %q %v", got, err)
	}
	for _, suffix := range []string{".vocabulary", ".v015"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("failed migration wrote %s: %v", suffix, err)
		}
	}
}

func TestVocabularyReplacementKeysPreserveRepositoryNames(t *testing.T) {
	for _, key := range []string{"repositories.landing_mode", "repositories.auto_unsaddle", "repositories.landing_mode.gate", "repositories.auto_unsaddle.review", "repositories.remuda.gate", "profiles.landing_mode.model"} {
		if replacement := ReplacementKey(key); replacement != "" {
			t.Errorf("repository or Profile name %q rewritten to %q", key, replacement)
		}
	}
	for old, want := range map[string]string{"landing_mode": "merge_mode", "auto_unsaddle": "auto_teardown", "remuda": "pool", "remuda.setup": "pool.setup", "defaults.landing_mode": "defaults.merge_mode", "defaults.auto_unsaddle": "defaults.auto_teardown", "autonomy.land": "autonomy.merge", "repositories.myremuda.landing_mode": "repositories.myremuda.merge_mode"} {
		if replacement := ReplacementKey(old); replacement != want {
			t.Errorf("%q -> %q, want %q", old, replacement, want)
		}
	}
}
