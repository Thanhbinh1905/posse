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
	mountRoot := filepath.Join(t.TempDir(), "mount")
	for path, contents := range map[string]string{
		filepath.Join(mountRoot, "unchanged.txt"):           "same\n",
		filepath.Join(mountRoot, "changed.txt"):             "original\n",
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
	members := map[string]bool{"backend": true}
	baselineFiles, err := workspaceRootFileStates(mountRoot, members)
	if err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		filepath.Join(mountRoot, "changed.txt"): "edited\n",
		filepath.Join(mountRoot, "new.txt"):     "new evidence\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	attachments, err := workspaceRootReportAttachments(context.Background(), mountRoot, members, workspaceRootBaseline{Version: workspaceRootBaselineVersion, Files: baselineFiles})
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

func TestWorkspaceRootReportAttachmentsIncludeNonIgnoredNestedGitFiles(t *testing.T) {
	mountRoot := t.TempDir()
	repository := filepath.Join(mountRoot, "git-evidence")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repository, "init", "-q", "-b", "main")
	gitTest(t, repository, "config", "user.name", "Posse Test")
	gitTest(t, repository, "config", "user.email", "posse@example.test")
	if err := os.WriteFile(filepath.Join(repository, ".git", "info", "exclude"), []byte("ignored.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deeper := filepath.Join(repository, "deeper")
	if err := os.MkdirAll(deeper, 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, deeper, "init", "-q", "-b", "main")
	gitTest(t, deeper, "config", "user.name", "Posse Test")
	gitTest(t, deeper, "config", "user.email", "posse@example.test")
	for name, contents := range map[string]string{"tracked.txt": "deep committed proof\n", "untracked.txt": "deep untracked evidence\n"} {
		if err := os.WriteFile(filepath.Join(deeper, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, deeper, "add", "tracked.txt")
	gitTest(t, deeper, "commit", "-m", "deep evidence")
	for name, contents := range map[string]string{"proof.txt": "committed proof\n", "notes.txt": "untracked evidence\n", "ignored.txt": "ignored data\n"} {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, repository, "add", "proof.txt")
	gitTest(t, repository, "commit", "-m", "evidence")
	attachments, err := workspaceRootReportAttachments(context.Background(), mountRoot, map[string]bool{}, workspaceRootBaseline{Version: workspaceRootBaselineVersion, Files: map[string]workspaceRootFingerprint{}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]reportAttachmentSource{
		"git-evidence/proof.txt":            {root: repository, name: "proof.txt"},
		"git-evidence/notes.txt":            {root: repository, name: "notes.txt"},
		"git-evidence/deeper/tracked.txt":   {root: deeper, name: "tracked.txt"},
		"git-evidence/deeper/untracked.txt": {root: deeper, name: "untracked.txt"},
	}
	if !reflect.DeepEqual(attachments, want) {
		t.Fatalf("nested Git attachments = %#v, want %#v", attachments, want)
	}
}

func TestWorkspaceRootBaselineUsesMountContentsAtAcquireAndRemainsHidden(t *testing.T) {
	home := t.TempDir()
	project := store.Project{Name: "stack", Kind: store.ProjectKindWorkspace, Root: filepath.Join(t.TempDir(), "project")}
	task := store.Task{Seq: 1, WorktreePath: filepath.Join(t.TempDir(), "mount")}
	for _, root := range []string{project.Root, task.WorktreePath, filepath.Join(task.WorktreePath, "backend")} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	backend := filepath.Join(task.WorktreePath, "backend")
	gitTest(t, backend, "init", "-q", "-b", "main")
	gitTest(t, backend, "config", "user.name", "Posse Test")
	gitTest(t, backend, "config", "user.email", "posse@example.test")
	for _, root := range []string{project.Root, task.WorktreePath} {
		if err := os.WriteFile(filepath.Join(root, "workspace-note.txt"), []byte("acquired copy\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(backend, "member.txt"), []byte("member\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, backend, "add", "member.txt")
	gitTest(t, backend, "commit", "-m", "member baseline")
	memberBaseline := strings.TrimSpace(gitTest(t, backend, "rev-parse", "HEAD"))
	if err := writeWorkspaceRootBaseline(context.Background(), home, project, task, task.WorktreePath, []taskMember{{repoTarget: repoTarget{Name: "backend"}, Path: "backend"}}); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{project.Root, task.WorktreePath} {
		if err := os.WriteFile(filepath.Join(root, "workspace-note.txt"), []byte("same later update\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(project.Root, "root-evidence.txt"), []byte("same new evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task.WorktreePath, "root-evidence.txt"), []byte("same new evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task.WorktreePath, ".workspace-root-baseline.json"), []byte("evidence with an internal-looking name\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, err := readWorkspaceRootBaseline(home, project, task)
	if err != nil {
		t.Fatal(err)
	}
	if got := baseline.Members["backend"]; got != memberBaseline {
		t.Fatalf("Member acquisition baseline = %q, want %q", got, memberBaseline)
	}
	attachments, err := workspaceRootReportAttachments(context.Background(), task.WorktreePath, map[string]bool{"backend": true}, baseline)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]reportAttachmentSource{
		".workspace-root-baseline.json": {root: task.WorktreePath, name: ".workspace-root-baseline.json"},
		"root-evidence.txt":             {root: task.WorktreePath, name: "root-evidence.txt"},
		"workspace-note.txt":            {root: task.WorktreePath, name: "workspace-note.txt"},
	}
	if !reflect.DeepEqual(attachments, want) {
		t.Fatalf("attachments after live Project update = %#v, want %#v", attachments, want)
	}
	listed, err := reportAttachments(home, project, task)
	if err != nil || len(listed) != 0 {
		t.Fatalf("internal baseline appeared as a Report attachment: %#v, %v", listed, err)
	}
}

func TestWorkspaceScoutProtocolDescribesRootAttachments(t *testing.T) {
	project := store.Project{Name: "stack", Kind: store.ProjectKindWorkspace}
	task := store.Task{Seq: 1, Type: "scout"}
	protocol := workerProtocol(project, task, dispatch.Brief{}, "/tmp/launch.md") + workspaceProtocol(project, nil)
	for _, phrase := range []string{
		"new or edited shared-root files",
		"Mount-acquisition snapshot",
		"each unrequested Member's commits",
		"Later edits to the live Project do not change the shared-root baseline",
		"Teardown keeps the Mount",
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
