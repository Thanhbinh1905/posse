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

func TestTaskScratchIsPosseOwnedIsolatedAndRemovable(t *testing.T) {
	home := t.TempDir()
	project := store.Project{Name: "shop"}
	task := store.Task{Seq: 12}
	path, err := ensureTaskScratch(home, project, task)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "scratch", "shop", "t12")
	if path != want {
		t.Fatalf("Task scratch path = %q, want %q", path, want)
	}
	if err := os.MkdirAll(filepath.Join(path, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "nested", "fixture"), []byte("temporary"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(path, "nested"), 0o500); err != nil {
		t.Fatal(err)
	}
	if err := removeTaskScratch(home, project, task); err != nil {
		t.Fatalf("remove Task scratch: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Task scratch remains after removal: %v", err)
	}
	if err := removeTaskScratch(home, project, task); err != nil {
		t.Fatalf("repeat Task scratch removal: %v", err)
	}
}

func TestTaskScratchRefusesSymlinkedOwnedPath(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "scratch"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "scratch", "shop")); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureTaskScratch(home, store.Project{Name: "shop"}, store.Task{Seq: 1}); err == nil {
		t.Fatal("scratch creation followed a symlink outside POSSE_HOME")
	}
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeTaskScratch(home, store.Project{Name: "shop"}, store.Task{Seq: 1}); err == nil {
		t.Fatal("scratch removal accepted a symlinked Project directory")
	}
	if contents, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(contents) != "safe" {
		t.Fatalf("outside file changed: %q %v", contents, err)
	}
}

func TestExportTaskScratchSetsTemporaryVariablesInOwnedPane(t *testing.T) {
	fake := herdr.NewFake()
	service := testService(t.TempDir(), fake)
	scratch := "/tmp/posse scratch/it's-owned"
	if err := exportTaskScratch(context.Background(), service, "w1:p2", scratch); err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls) != 1 || fake.Calls[0].Method != "pane.send_input" || fake.Calls[0].Params["pane_id"] != "w1:p2" {
		t.Fatalf("scratch environment was not sent to the Rider pane: %#v", fake.Calls)
	}
	command, ok := fake.Calls[0].Params["text"].(string)
	if !ok {
		t.Fatalf("environment command type = %T", fake.Calls[0].Params["text"])
	}
	if !strings.Contains(command, `it'"'"'s-owned`) {
		t.Fatalf("scratch path was not shell-quoted safely: %s", command)
	}
	for _, variable := range []string{"TMPDIR", "GOTMPDIR", "TMP", "TEMP"} {
		if !strings.Contains(command, variable+"='/tmp/posse scratch/it") {
			t.Errorf("environment command omitted quoted %s: %s", variable, command)
		}
	}
}
