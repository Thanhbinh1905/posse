package config

import (
	"errors"
	"os"
	"path/filepath"
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

func TestVocabularyReplacementKeysPreserveRepositoryNames(t *testing.T) {
	for _, key := range []string{"repositories.landing_mode", "repositories.auto_unsaddle", "repositories.landing_mode.gate", "repositories.auto_unsaddle.review", "repositories.remuda.gate", "profiles.landing_mode.model"} {
		if replacement := ReplacementKey(key); replacement != "" {
			t.Errorf("repository or Profile name %q rewritten to %q", key, replacement)
		}
	}
	for old, want := range map[string]string{"remuda": "pool", "remuda.setup": "pool.setup", "defaults.landing_mode": "defaults.merge_mode", "defaults.auto_unsaddle": "defaults.auto_teardown", "autonomy.land": "autonomy.merge", "repositories.myremuda.landing_mode": "repositories.myremuda.merge_mode"} {
		if replacement := ReplacementKey(old); replacement != want {
			t.Errorf("%q -> %q, want %q", old, replacement, want)
		}
	}
}
