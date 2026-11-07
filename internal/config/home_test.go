package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func homeLookup(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func writePointer(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveHomePrecedence(t *testing.T) {
	root := t.TempDir()
	userHome := filepath.Join(root, "user")
	xdg := filepath.Join(root, "xdg")
	data := filepath.Join(root, "data folder ✓")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveHome(homeLookup(map[string]string{"HOME": userHome}))
	if err != nil || got.Path != filepath.Join(userHome, ".posse") || got.Source != HomeSourceDefault {
		t.Fatalf("missing pointer: %#v %v", got, err)
	}
	if got.Pointer != filepath.Join(userHome, ".config", "posse", "home") {
		t.Fatalf("default pointer path = %s", got.Pointer)
	}

	writePointer(t, filepath.Join(xdg, "posse", "home"), data+"\n")
	got, err = ResolveHome(homeLookup(map[string]string{"HOME": userHome, "XDG_CONFIG_HOME": xdg}))
	if err != nil || got.Path != data || got.Source != HomeSourcePointer || got.Pointer != filepath.Join(xdg, "posse", "home") {
		t.Fatalf("pointer: %#v %v", got, err)
	}

	got, err = ResolveHome(homeLookup(map[string]string{"HOME": userHome, "XDG_CONFIG_HOME": xdg, "POSSE_HOME": filepath.Join(root, "explicit")}))
	if err != nil || got.Path != filepath.Join(root, "explicit") || got.Source != HomeSourceEnv {
		t.Fatalf("explicit POSSE_HOME must win: %#v %v", got, err)
	}

	// A relative XDG_CONFIG_HOME is ignored, as the XDG spec requires.
	got, err = ResolveHome(homeLookup(map[string]string{"HOME": userHome, "XDG_CONFIG_HOME": "relative"}))
	if err != nil || got.Pointer != filepath.Join(userHome, ".config", "posse", "home") {
		t.Fatalf("relative XDG_CONFIG_HOME: %#v %v", got, err)
	}
}

func TestResolveHomeRefusesInvalidPointer(t *testing.T) {
	root := t.TempDir()
	xdg := filepath.Join(root, "xdg")
	pointer := filepath.Join(xdg, "posse", "home")
	existing := filepath.Join(root, "existing")
	if err := os.MkdirAll(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"empty":       "",
		"relative":    "data\n",
		"two lines":   existing + "\n" + existing + "\n",
		"control":     existing + "\x1b\n",
		"missing dir": filepath.Join(root, "absent") + "\n",
		"not a dir":   file + "\n",
		"oversized":   "/" + strings.Repeat("a", 5000) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			writePointer(t, pointer, contents)
			got, err := ResolveHome(homeLookup(map[string]string{"HOME": filepath.Join(root, "user"), "XDG_CONFIG_HOME": xdg}))
			if err == nil {
				t.Fatalf("invalid pointer %q resolved to %#v", contents, got)
			}
			if !strings.Contains(err.Error(), pointer) {
				t.Fatalf("error does not name the pointer file: %v", err)
			}
		})
	}
}

func TestSetHomePointerWritesPrivatePointerAndCreatesFolder(t *testing.T) {
	root := t.TempDir()
	userHome := filepath.Join(root, "user")
	xdg := filepath.Join(root, "xdg")
	lookup := homeLookup(map[string]string{"HOME": userHome, "XDG_CONFIG_HOME": xdg})
	target := filepath.Join(root, "Dữ liệu posse")

	got, err := SetHomePointer(lookup, target)
	if err != nil || got.Path != target || got.Source != HomeSourcePointer {
		t.Fatalf("set: %#v %v", got, err)
	}
	pointer := filepath.Join(xdg, "posse", "home")
	data, err := os.ReadFile(pointer)
	if err != nil || string(data) != target+"\n" {
		t.Fatalf("pointer contents %q %v", data, err)
	}
	if info, err := os.Stat(pointer); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("pointer mode: %v %v", info.Mode(), err)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("data folder: %v", err)
	}
	if resolved, err := ResolveHome(lookup); err != nil || resolved.Path != target {
		t.Fatalf("resolve after set: %#v %v", resolved, err)
	}
	if again, err := SetHomePointer(lookup, target); err != nil || again.Path != target {
		t.Fatalf("set again: %#v %v", again, err)
	}
}

func TestSetHomePointerKeepsLegacyDefaultWithoutPointer(t *testing.T) {
	root := t.TempDir()
	userHome := filepath.Join(root, "user")
	lookup := homeLookup(map[string]string{"HOME": userHome})
	got, err := SetHomePointer(lookup, "~/.posse")
	if err != nil || got.Path != filepath.Join(userHome, ".posse") || got.Source != HomeSourceDefault {
		t.Fatalf("default choice: %#v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(userHome, ".config", "posse", "home")); !os.IsNotExist(err) {
		t.Fatalf("choosing the default wrote a pointer: %v", err)
	}
}

func TestSetHomePointerExpandsTilde(t *testing.T) {
	root := t.TempDir()
	userHome := filepath.Join(root, "user")
	lookup := homeLookup(map[string]string{"HOME": userHome})
	got, err := SetHomePointer(lookup, "~/posse data")
	if err != nil || got.Path != filepath.Join(userHome, "posse data") {
		t.Fatalf("tilde: %#v %v", got, err)
	}
}

func TestSetHomePointerRefusesUnsafeChoices(t *testing.T) {
	root := t.TempDir()
	userHome := filepath.Join(root, "user")
	xdg := filepath.Join(root, "xdg")
	lookup := homeLookup(map[string]string{"HOME": userHome, "XDG_CONFIG_HOME": xdg})
	unrelated := filepath.Join(root, "photos")
	if err := os.MkdirAll(unrelated, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelated, "cat.jpg"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"relative":     "data",
		"empty":        "",
		"newline":      filepath.Join(root, "a\nb"),
		"unrelated":    unrelated,
		"regular file": file,
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := SetHomePointer(lookup, target); err == nil {
				t.Fatalf("accepted %q: %#v", target, got)
			}
			if _, err := os.Stat(filepath.Join(xdg, "posse", "home")); !os.IsNotExist(err) {
				t.Fatalf("refused choice wrote a pointer: %v", err)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(unrelated, "cat.jpg")); err != nil {
		t.Fatalf("unrelated data changed: %v", err)
	}
}

func TestSetHomePointerRefusesToLeaveAHomeWithData(t *testing.T) {
	root := t.TempDir()
	userHome := filepath.Join(root, "user")
	xdg := filepath.Join(root, "xdg")
	lookup := homeLookup(map[string]string{"HOME": userHome, "XDG_CONFIG_HOME": xdg})
	legacy := filepath.Join(userHome, ".posse")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "posse.db"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := SetHomePointer(lookup, filepath.Join(root, "new"))
	if err == nil || !strings.Contains(err.Error(), legacy) {
		t.Fatalf("switching away from a home with data: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "new")); !os.IsNotExist(statErr) {
		t.Fatalf("refused switch created the new folder")
	}
	// After the User moves the data themselves, the moved folder is accepted.
	moved := filepath.Join(root, "moved")
	if err := os.Rename(legacy, moved); err != nil {
		t.Fatal(err)
	}
	if got, err := SetHomePointer(lookup, moved); err != nil || got.Path != moved {
		t.Fatalf("moved home: %#v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(moved, "posse.db")); err != nil {
		t.Fatalf("moved data changed: %v", err)
	}
}

func TestSetHomePointerRefusesWhenPOSSEHOMEIsSet(t *testing.T) {
	root := t.TempDir()
	lookup := homeLookup(map[string]string{"HOME": filepath.Join(root, "user"), "XDG_CONFIG_HOME": filepath.Join(root, "xdg"), "POSSE_HOME": filepath.Join(root, "env")})
	if _, err := SetHomePointer(lookup, filepath.Join(root, "other")); err == nil || !strings.Contains(err.Error(), "POSSE_HOME") {
		t.Fatalf("set under POSSE_HOME: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "xdg", "posse", "home")); !os.IsNotExist(err) {
		t.Fatalf("explicit POSSE_HOME let setup write the User pointer")
	}
}
