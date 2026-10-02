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
	"github.com/thanhbinh1905/posse/internal/dispatch"
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

func TestWorkspaceRootReportAttachmentsIncludeOnlyChangedSharedFiles(t *testing.T) {
	projectRoot, mountRoot := filepath.Join(t.TempDir(), "project"), filepath.Join(t.TempDir(), "mount")
	for _, root := range []string{projectRoot, mountRoot} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, contents := range map[string]string{
		filepath.Join(projectRoot, "unchanged.txt"):         "same\n",
		filepath.Join(projectRoot, "changed.txt"):           "original\n",
		filepath.Join(mountRoot, "unchanged.txt"):           "same\n",
		filepath.Join(mountRoot, "changed.txt"):             "edited\n",
		filepath.Join(mountRoot, "new.txt"):                 "new evidence\n",
		filepath.Join(mountRoot, "report.md"):               "canonical report\n",
		filepath.Join(mountRoot, ".git", "hidden"):          "metadata\n",
		filepath.Join(mountRoot, "backend", "evidence.txt"): "member evidence\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	attachments, err := workspaceRootReportAttachments(projectRoot, mountRoot, map[string]bool{"backend": true})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]reportAttachmentSource{
		"changed.txt": {root: mountRoot, name: "changed.txt"},
		"new.txt":     {root: mountRoot, name: "new.txt"},
	}
	if !reflect.DeepEqual(attachments, want) {
		t.Fatalf("workspace-root attachments = %#v, want %#v", attachments, want)
	}
}

func TestWorkspaceScoutProtocolDescribesRootAttachments(t *testing.T) {
	project := store.Project{Name: "stack", Kind: store.ProjectKindWorkspace}
	task := store.Task{Seq: 1, Type: "scout"}
	protocol := workerProtocol(project, task, dispatch.Brief{}, "/tmp/launch.md") + workspaceProtocol(project, nil)
	for _, phrase := range []string{
		"new or edited shared-root files",
		"but not unchanged Project copies",
		"are saved as Report attachments at Teardown",
	} {
		if !strings.Contains(protocol, phrase) {
			t.Errorf("workspace Scout protocol omits %q:\n%s", phrase, protocol)
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
