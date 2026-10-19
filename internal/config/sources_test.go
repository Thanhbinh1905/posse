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
	if err := os.WriteFile(global, []byte("[defaults]\nreview='local'\nmerge_mode='local'\n[profiles.claude]\nkind='claude'\n[[dispatch]]\ntype='change'\nuse='claude'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte("review='none'\n[autonomy]\nyolo=true\n[[dispatch]]\ntype='research'\nuse='claude'\n"), 0o600); err != nil {
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
		"defaults.review": {project}, "defaults.merge_mode": {global}, "defaults.max_workers": {"built-in default"},
		"defaults.dispatch_inheritance": {"built-in default"}, "kinds.claude.disabled": {"built-in default"},
		"autonomy.review": {project}, "autonomy.merge": {project}, "dispatch": {project, global},
	} {
		if !reflect.DeepEqual(sources[key], want) {
			t.Errorf("%s sources=%v want %v", key, sources[key], want)
		}
	}
	if cfg.Defaults.Review != "none" || cfg.Autonomy.Merge != "auto" {
		t.Fatalf("effective settings disagree: %#v", cfg)
	}
}
