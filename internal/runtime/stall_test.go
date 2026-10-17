package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDirectoryProjectFingerprintTracksSourceAndExcludesGitMetadata(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "src", "main.go")
	if err := os.WriteFile(source, []byte("package src\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	progress := SystemProgress{}
	before, err := progress.WorktreeFingerprint(context.Background(), root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("package src\n\nfunc Changed() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := progress.WorktreeFingerprint(context.Background(), root, true)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("source edit did not change the directory fingerprint")
	}
	empty := filepath.Join(root, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	withEmptyDirectory, err := progress.WorktreeFingerprint(context.Background(), root, true)
	if err != nil || withEmptyDirectory == after {
		t.Fatalf("empty directory change was not fingerprinted: %v", err)
	}
	if err := os.Remove(empty); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("metadata\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withGitMetadata, err := progress.WorktreeFingerprint(context.Background(), root, true)
	if err != nil {
		t.Fatal(err)
	}
	if withGitMetadata != after {
		t.Fatal("Git metadata changed the directory fingerprint")
	}
}

func TestDirectoryProjectFingerprintRejectsRootSymlink(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "project")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := (SystemProgress{}).WorktreeFingerprint(context.Background(), link, true); err == nil {
		t.Fatal("directory fingerprint followed a root symlink")
	}
}
