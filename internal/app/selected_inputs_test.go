package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSelectedInputIgnoredAcceptsNonGitProjectRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "contract.md"), []byte("contract"), 0o600); err != nil {
		t.Fatal(err)
	}
	ignored, err := selectedInputIgnored(root, "contract.md", false, false)
	if err != nil || ignored {
		t.Fatalf("non-Git Project root ignore status = %v, %v; want not ignored", ignored, err)
	}
}
