package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHomeCommandReportsAndSavesTheDataFolder(t *testing.T) {
	root := t.TempDir()
	userHome := filepath.Join(root, "user")
	xdg := filepath.Join(root, "xdg")
	t.Setenv("HOME", userHome)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("POSSE_HOME", "")
	t.Setenv(workerHomeEnv, "")
	service := New("", nil)

	code, output := runCLI(t, service, "_home")
	want := filepath.Join(userHome, ".posse") + "\ndefault\n" + filepath.Join(xdg, "posse", "home") + "\n"
	if code != 0 || output != want {
		t.Fatalf("_home = %d %q, want %q", code, output, want)
	}

	data := filepath.Join(root, "my data")
	code, output = runCLI(t, service, "_home", "--set", data)
	if code != 0 || !strings.HasPrefix(output, data+"\npointer\n") {
		t.Fatalf("_home --set = %d %q", code, output)
	}
	if contents, err := os.ReadFile(filepath.Join(xdg, "posse", "home")); err != nil || string(contents) != data+"\n" {
		t.Fatalf("pointer = %q %v", contents, err)
	}
	if home, err := service.homePath(); err != nil || home != data {
		t.Fatalf("CLI home after --set = %q %v", home, err)
	}

	if err := os.WriteFile(filepath.Join(data, "posse.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output = runCLI(t, service, "_home", "--set", filepath.Join(root, "elsewhere"))
	if code == 0 || !strings.Contains(output, "posse does not move it") {
		t.Fatalf("switch away from data = %d %q", code, output)
	}
	if strings.Contains(output, "error{") {
		t.Fatalf("_home printed TOON instead of a readable message: %q", output)
	}
}
