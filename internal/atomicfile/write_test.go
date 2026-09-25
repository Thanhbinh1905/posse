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
