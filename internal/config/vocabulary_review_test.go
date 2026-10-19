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
