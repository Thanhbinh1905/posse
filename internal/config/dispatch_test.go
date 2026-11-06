package config

import (
	"path/filepath"
	"testing"
)

func TestSetDispatchAlternatives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if _, err := SetFile(path, "dispatch", `[{type="change",use=["one","two"]}]`, true); err != nil {
		t.Fatal(err)
	}
	values, _, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, ok := tableEntries(values["dispatch"])
	if !ok || len(entries) != 1 {
		t.Fatalf("%+v", values)
	}
	names := (Rule{Use: entries[0]["use"]}).ProfileNames()
	if len(names) != 2 || names[0] != "one" || names[1] != "two" {
		t.Fatalf("lost declared order: %+v", names)
	}
}

func TestRejectInvalidDispatchAlternatives(t *testing.T) {
	for _, literal := range []string{`[{use=[]}]`, `[{use=[""]}]`, `[{use=[1]}]`, `[{use=true}]`} {
		if _, err := SetFile(filepath.Join(t.TempDir(), "config.toml"), "dispatch", literal, true); err == nil {
			t.Fatalf("accepted %s", literal)
		}
	}
}
