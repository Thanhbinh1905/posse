package app

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGitStatusOutputDisablesOptionalLocks(t *testing.T) {
	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "git-args")
	shim := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$GIT_ARGS_CAPTURE\"\nprintf ' M file\\n'\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_ARGS_CAPTURE", capture)

	root := filepath.Join(t.TempDir(), "repo with spaces")
	got, err := gitStatusOutput(context.Background(), root, "--porcelain", "--untracked-files=all")
	if err != nil {
		t.Fatal(err)
	}
	if got != "M file" {
		t.Fatalf("gitStatusOutput() = %q, want trimmed status", got)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	gotArgs := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	wantArgs := []string{"-C", root, "--no-optional-locks", "status", "--porcelain", "--untracked-files=all"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("git args = %q, want %q", gotArgs, wantArgs)
	}
}
