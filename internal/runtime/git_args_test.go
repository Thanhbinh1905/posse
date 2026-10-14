package runtime

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRepositoryFingerprintDisablesOptionalGitLocks(t *testing.T) {
	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "git-args")
	shim := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$GIT_ARGS_CAPTURE\"\ncase \"$4\" in\n  rev-parse) printf 'head\\n' ;;\n  status) printf ' M file\\n' ;;\n  *) exit 2 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_ARGS_CAPTURE", capture)

	root := filepath.Join(t.TempDir(), "repo with spaces")
	if _, err := repositoryFingerprint(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	gotArgs := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	wantArgs := []string{
		"--no-optional-locks", "-C", root, "rev-parse", "HEAD",
		"--no-optional-locks", "-C", root, "status", "--porcelain",
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("git args = %q, want %q", gotArgs, wantArgs)
	}
}
