package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEffectiveSourcesFollowAliasesOverridesAndDerivedGrants(t *testing.T) {
	home := t.TempDir()
	global := ConfigPath(home, "")
	project := ConfigPath(home, "shop")
	if err := os.MkdirAll(filepath.Dir(project), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte("[defaults]\nreview='always'\nlanding_mode='local'\n[profiles.claude]\nkind='claude'\n[[dispatch]]\ntype='ship'\nuse='claude'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte("review='never'\n[autonomy]\nyolo=true\n[[dispatch]]\ntype='scout'\nuse='claude'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(home, "shop")
	if err != nil {
		t.Fatal(err)
	}
	sources, err := cfg.EffectiveSources()
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string][]string{
		"defaults.review": {project}, "defaults.landing_mode": {global}, "defaults.max_workers": {"built-in default"},
		"autonomy.review": {project}, "autonomy.land": {project}, "dispatch": {project, global},
	} {
		if !reflect.DeepEqual(sources[key], want) {
			t.Errorf("%s sources=%v want %v", key, sources[key], want)
		}
	}
	if cfg.Defaults.Review != "never" || cfg.Autonomy.Land != "auto" {
		t.Fatalf("effective settings disagree: %#v", cfg)
	}
}
