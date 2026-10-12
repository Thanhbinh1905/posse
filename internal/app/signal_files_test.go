package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestCopySignalFileAllowsOnlyWorktreeAndOwnScratchAfterResolvingSymlinks(t *testing.T) {
	home := t.TempDir()
	project := store.Project{Name: "shop"}
	task := store.Task{Seq: 1, WorktreePath: filepath.Join(t.TempDir(), "worktree")}
	if err := os.MkdirAll(task.WorktreePath, 0o700); err != nil {
		t.Fatal(err)
	}
	scratch, err := ensureTaskScratch(home, project, task)
	if err != nil {
		t.Fatal(err)
	}
	otherScratch, err := ensureTaskScratch(home, project, store.Task{Seq: 2})
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()

	for _, root := range []string{task.WorktreePath, scratch} {
		name := filepath.Base(root) + ".md"
		contents := "evidence from " + root
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(root, "alias.md")
		if err := os.Symlink(filepath.Join(root, name), alias); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(t.TempDir(), "copied.md")
		if _, err := copySignalFile(home, project, task, alias, destination); err != nil {
			t.Fatalf("copy from allowed root %s: %v", root, err)
		}
		copied, err := os.ReadFile(destination)
		if err != nil || string(copied) != contents {
			t.Fatalf("copied file from %s = %q, %v", root, copied, err)
		}
	}

	outsideFile := filepath.Join(outside, "outside.md")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(scratch, "escape.md")
	if err := os.Symlink(outsideFile, escape); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(otherScratch, "foreign.md"), outsideFile, escape} {
		destination := filepath.Join(t.TempDir(), "copied.md")
		if _, err := copySignalFile(home, project, task, path, destination); err == nil {
			t.Errorf("accepted Signal file outside this Task's worktree and scratch: %s", path)
		} else if !strings.Contains(err.Error(), task.WorktreePath) || !strings.Contains(err.Error(), scratch) {
			t.Errorf("rejection did not name both allowed locations: %v", err)
		}
		if _, err := os.Stat(destination); !os.IsNotExist(err) {
			t.Errorf("rejected Signal file was copied to %s: %v", destination, err)
		}
	}
}
