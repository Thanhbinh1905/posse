package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestReportAttachmentGitArgsOmitEmptyBaseRef(t *testing.T) {
	got := reportAttachmentGitArgs("")
	want := [][]string{
		{"diff", "--name-only", "-z", "HEAD"},
		{"ls-files", "--others", "--exclude-standard", "-z"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Git argv = %#v, want %#v", got, want)
	}
	for _, command := range got {
		for _, arg := range command {
			if arg == "" {
				t.Fatalf("Git argv contains an empty argument: %#v", command)
			}
		}
	}
}

func TestUnsaddleIncompleteHelpNamesFailedStep(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", root, "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "scout", Title: "Scout report"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	err = service.unsaddleIncomplete(ctx, db, project, task, "Report attachment preservation", errors.New("fixture failure"))
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "unsaddle_incomplete" {
		t.Fatalf("Teardown error = %#v", err)
	}
	if len(failure.Help) != 1 || !strings.Contains(failure.Help[0], "Report attachment preservation") || strings.Contains(failure.Help[0], "remaining pane or Mount process") {
		t.Fatalf("Teardown help = %#v", failure.Help)
	}
}
