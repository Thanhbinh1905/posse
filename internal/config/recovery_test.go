package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryPolicyDefaultsAndProjectOverrides(t *testing.T) {
	home := t.TempDir()
	cfg, err := Load(home, "")
	if err != nil || cfg.Defaults.RecoveryAttempts != 3 || cfg.Defaults.RecoveryBackoff != "5s" {
		t.Fatalf("default recovery policy=%#v err=%v", cfg.Defaults, err)
	}
	dir := filepath.Join(home, "projects", "shop")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[defaults]\nrecovery_attempts = 2\nrecovery_backoff = \"20ms\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(home, "shop")
	if err != nil || cfg.Defaults.RecoveryAttempts != 2 || cfg.Defaults.RecoveryBackoff != "20ms" {
		t.Fatalf("Project recovery policy=%#v err=%v", cfg.Defaults, err)
	}
	for _, setting := range []struct {
		key   string
		value any
	}{
		{"defaults.recovery_attempts", 0}, {"defaults.recovery_attempts", -1}, {"defaults.recovery_attempts", "3"},
		{"defaults.recovery_backoff", "0s"}, {"defaults.recovery_backoff", "-1s"}, {"defaults.recovery_backoff", "invalid"},
	} {
		if err := ValidateSetting(setting.key, setting.value, true); err == nil {
			t.Errorf("accepted invalid %s=%v", setting.key, setting.value)
		}
	}
}
