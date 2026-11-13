package config

import (
	"path/filepath"
	"testing"
)

func TestReviewProviderDefaultsAndProjectOverride(t *testing.T) {
	spec, ok := Lookup("review_provider")
	if !ok || spec.Default != "builtin" || !spec.UserOnly || len(spec.Allowed) != 2 || spec.Allowed[0] != "builtin" || spec.Allowed[1] != "greenhouse" {
		t.Fatalf("review_provider schema = %+v, %v", spec, ok)
	}
	home := t.TempDir()
	got, err := Load(home, "shop")
	if err != nil || got.EffectiveReviewProvider("") != "builtin" {
		t.Fatalf("default provider = %q, %v; want builtin", got.ReviewProvider, err)
	}
	if _, err := SetFile(filepath.Join(home, "config.toml"), "review_provider", "greenhouse", false); err != nil {
		t.Fatal(err)
	}
	got, err = Load(home, "shop")
	if err != nil || got.EffectiveReviewProvider("") != "greenhouse" {
		t.Fatalf("global provider = %q, %v; want greenhouse", got.ReviewProvider, err)
	}
	if _, err := SetFile(ConfigPath(home, "shop"), "review_provider", "builtin", true); err != nil {
		t.Fatal(err)
	}
	got, err = Load(home, "shop")
	if err != nil || got.EffectiveReviewProvider("") != "builtin" {
		t.Fatalf("Project provider = %q, %v; want builtin", got.ReviewProvider, err)
	}
	if _, err := SetFile(ConfigPath(home, "shop"), "review_provider", "greenhouse", true); err != nil {
		t.Fatal(err)
	}
	if _, err := SetFile(ConfigPath(home, "shop"), "repositories.api.review_provider", "builtin", true); err != nil {
		t.Fatal(err)
	}
	got, err = Load(home, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if got.EffectiveReviewProvider("api") != "builtin" {
		t.Fatalf("Member provider = %q; want builtin", got.EffectiveReviewProvider("api"))
	}
	if got.EffectiveReviewProvider("other") != "greenhouse" {
		t.Fatalf("Project provider = %q; want greenhouse", got.EffectiveReviewProvider("other"))
	}
}
