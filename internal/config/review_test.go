package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewDefaultsOverridesAndUserControl(t *testing.T) {
	home := t.TempDir()
	cfg, err := Load(home, "shop")
	if err != nil || cfg.Defaults.Review != "none" || cfg.MemberReview("api") != "none" {
		t.Fatalf("defaults: %#v %v", cfg.Defaults, err)
	}
	path := ConfigPath(home, "shop")
	if _, err := SetFile(path, "defaults.review", "local", true); err != nil {
		t.Fatal(err)
	}
	if _, err := SetFile(path, "repositories.api.review", "none", true); err != nil {
		t.Fatal(err)
	}
	if _, err := SetFile(path, "repositories.worker.review", "no-mistakes", true); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(home, "shop")
	if err != nil || cfg.MemberReview("api") != "none" || cfg.MemberReview("worker") != "no-mistakes" || cfg.MemberReview("tool") != "local" {
		t.Fatalf("overrides: %#v %v", cfg, err)
	}
	for _, key := range []string{"defaults.review", "repositories.api.review"} {
		if !IsUserOnly(key) {
			t.Errorf("%s is not User-only", key)
		}
	}
}

func TestExplicitLegacyReviewRefusesEveryConfigLayer(t *testing.T) {
	for _, value := range []string{"always", "on_risk", "never"} {
		for _, scope := range []string{"global", "project", "alias", "member"} {
			t.Run(value+"/"+scope, func(t *testing.T) {
				home := t.TempDir()
				path, text := ConfigPath(home, ""), "[defaults]\nreview='"+value+"'\n"
				switch scope {
				case "project":
					path = ConfigPath(home, "shop")
				case "alias":
					path, text = ConfigPath(home, "shop"), "review='"+value+"'\n"
				case "member":
					path, text = ConfigPath(home, "shop"), "[repositories.api]\nreview='"+value+"'\n"
				}
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
				// A newer layer must not hide an explicit legacy global value.
				if scope == "global" {
					if _, err := SetFile(ConfigPath(home, "shop"), "defaults.review", "none", true); err != nil {
						t.Fatal(err)
					}
				}
				_, err := Load(home, "shop")
				if err == nil || !strings.Contains(err.Error(), "legacy review policy") || !strings.Contains(err.Error(), "none, local or no-mistakes") || !strings.Contains(err.Error(), path) {
					t.Fatalf("legacy loaded: %v", err)
				}
			})
		}
	}
}
