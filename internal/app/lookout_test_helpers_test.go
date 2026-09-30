package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func newLookoutTestProject(t *testing.T) (*store.DB, store.Project, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "posse")
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	project.HerdrWorkspaceID = "w1"
	return db, project, home
}
