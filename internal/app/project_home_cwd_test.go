package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestProjectForCWDResolvesFromPosseHomeFolder(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	repo := filepath.Join(root, "shop")
	for _, dir := range []string{repo, filepath.Join(home, "projects", "shop", "briefs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	service := testService(home, herdr.NewFake())

	for _, dir := range []string{
		filepath.Join(home, "projects", "shop"),
		filepath.Join(home, "projects", "shop", "briefs"),
	} {
		t.Chdir(dir)
		got, err := service.projectForCWD(context.Background(), db)
		if err != nil || got.ID != project.ID {
			t.Fatalf("projectForCWD in %s = %+v, %v", dir, got, err)
		}
	}

	stray := filepath.Join(home, "projects", "ghost")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(stray)
	_, err = service.projectForCWD(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "not registered as a Project") {
		t.Fatalf("unregistered home folder error = %v", err)
	}
}
