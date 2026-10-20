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
	if _, err := SetFile(path, "defaults.review", "quick", true); err != nil {
		t.Fatal(err)
	}
	if _, err := SetFile(path, "repositories.api.review", "none", true); err != nil {
		t.Fatal(err)
	}
	if _, err := SetFile(path, "repositories.worker.review", "no-mistakes", true); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(home, "shop")
	if err != nil || cfg.MemberReview("api") != "none" || cfg.MemberReview("worker") != "no-mistakes" || cfg.MemberReview("tool") != "quick" {
		t.Fatalf("overrides: %#v %v", cfg, err)
	}
	for _, key := range []string{"defaults.review", "repositories.api.review"} {
		if !IsUserOnly(key) {
			t.Errorf("%s is not User-only", key)
		}
	}
}

func TestFullPipelineReviewCanBeSelectedWithoutImplicitDefault(t *testing.T) {
	home := t.TempDir()
	if _, err := SetFile(ConfigPath(home, "shop"), "defaults.review", "full-pipeline", true); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(home, "shop")
	if err != nil || cfg.Defaults.Review != "full-pipeline" {
		t.Fatalf("full-pipeline selection = %q, err=%v", cfg.Defaults.Review, err)
	}
	global, err := Load(t.TempDir(), "")
	if err != nil || global.Defaults.Review != "none" {
		t.Fatalf("full-pipeline changed the default: %q, err=%v", global.Defaults.Review, err)
	}
}

func TestProjectReviewShorthandCanBeRepaired(t *testing.T) {
	for _, action := range []string{"set", "unset"} {
		t.Run(action, func(t *testing.T) {
			home := t.TempDir()
			path := ConfigPath(home, "shop")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("review = 'on_risk'\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			if action == "set" {
				_, err = SetFile(path, "defaults.review", "none", true)
			} else {
				err = UnsetFile(path, "defaults.review", true)
			}
			if err != nil {
				t.Fatalf("repair Project review with %s: %v", action, err)
			}
			if _, err := Load(home, "shop"); err != nil {
				t.Fatalf("Project review still invalid after %s: %v", action, err)
			}
		})
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
				if err == nil || !strings.Contains(err.Error(), "legacy review policy") || !strings.Contains(err.Error(), "none, quick, full-pipeline") || !strings.Contains(err.Error(), path) {
					t.Fatalf("legacy loaded: %v", err)
				}
			})
		}
	}
}
