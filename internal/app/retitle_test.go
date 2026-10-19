package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestRetitleValidatesAndPersistsConventionalPRTitle(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	for _, invalid := range []string{"Not conventional", "fix:", "Fix(publish): capitalized type", "fix(publish): line one\nline two"} {
		code, output, stderr := fixture.run("retitle", "t1", invalid)
		if code == 0 || !strings.Contains(output+stderr, "pr_title_invalid") {
			t.Fatalf("invalid title %q was accepted: code=%d output=%s stderr=%s", invalid, code, output, stderr)
		}
	}
	before, err := fixture.db.TaskByID(context.Background(), fixture.project.ID, fixture.task.ID)
	if err != nil || before.PRTitle != "" {
		t.Fatalf("invalid retitle changed the saved PR title: %#v, %v", before, err)
	}

	want := "fix(publish)!: preserve the explicit PR title"
	code, output, stderr := fixture.run("retitle", "t1", want)
	if code != 0 || !strings.Contains(output, want) {
		t.Fatalf("valid retitle: code=%d output=%s stderr=%s", code, output, stderr)
	}
	task, err := fixture.db.TaskByID(context.Background(), fixture.project.ID, fixture.task.ID)
	if err != nil || task.Title != "E2E PR title" || task.PRTitle != want {
		t.Fatalf("retitled Task = %#v, %v", task, err)
	}

	data, err := os.ReadFile(fixture.db.TaskSnapshotPath(fixture.project.Name, task.Seq))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot store.TaskSnapshot
	if _, err := toml.Decode(string(data), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Task.Title != task.Title || snapshot.Task.PRTitle != want {
		t.Fatalf("Task snapshot titles = Brief %q, PR %q; want %q and %q", snapshot.Task.Title, snapshot.Task.PRTitle, task.Title, want)
	}
}

func TestRetitleRefusesNonPRChangeTask(t *testing.T) {
	fixture := newPRMergeFixture(t, "local", store.StateWorking)
	code, output, stderr := fixture.run("retitle", "t1", "fix(publish): update the title")
	if code == 0 || !strings.Contains(output+stderr, "retitle_refused") {
		t.Fatalf("retitle of local Task was not refused: code=%d output=%s stderr=%s", code, output, stderr)
	}
	task, err := fixture.db.TaskByID(context.Background(), fixture.project.ID, fixture.task.ID)
	if err != nil || task.PRTitle != "" {
		t.Fatalf("refused retitle changed the saved PR title: %#v, %v", task, err)
	}
}
