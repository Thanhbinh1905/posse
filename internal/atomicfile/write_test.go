package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteRefusesToReplaceSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	link := filepath.Join(directory, "link")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := Write(link, []byte("updated"), 0o600); err == nil {
		t.Fatal("Write followed by replacing the symlink instead of refusing it")
	}
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Fatalf("symlink = %q, %v; want %q", got, err, target)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "original" {
		t.Fatalf("target = %q, %v; want original bytes", got, err)
	}
}

func TestWriteLeavesIdenticalFileInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.toml")
	if err := Write(path, []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("identical contents replaced the file instead of leaving it in place")
	}
	if err := Write(path, []byte("state"), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o644 || os.SameFile(before, info) {
		t.Fatalf("mode change was not written: info=%v err=%v", info, err)
	}
	if err := Write(path, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "changed" {
		t.Fatalf("contents = %q, %v; want changed", got, err)
	}
}
