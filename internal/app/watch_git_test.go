package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchShutdownStopsInFlightGitFetch(t *testing.T) {
	f := newPRMergeFixture(t, "pr", "working")
	t.Setenv("POSSE_TEST_HERDR", "1")
	t.Setenv("POSSE_TEST_DISABLE_SYSTEMD_WATCH", "1")
	t.Setenv("POSSE_TEST_EXTERNAL_COMMAND_TIMEOUT", "60s")
	binary := filepath.Join(f.root, "posse-cli")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	marker := filepath.Join(f.root, "fetch-started")
	uploadPack := filepath.Join(f.root, "upload-pack")
	script := "#!/bin/sh\nprintf started > '" + marker + "'\nsleep 30\nexec git-upload-pack \"$@\"\n"
	if err := os.WriteFile(uploadPack, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.repo, "config", "remote.origin.uploadpack", uploadPack)
	previous := watchExecutableVerifier
	watchExecutableVerifier = func(string) bool { return true }
	t.Cleanup(func() { watchExecutableVerifier = previous })
	t.Cleanup(func() { cleanupTestWatchRuntime(t, f.home, f.project.ID, "") })
	if err := f.service.startWatchRuntime(context.Background(), f.db, f.home, f.project, binary); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("Watch did not start the delayed fetch")
	}
	cleanupTestWatchRuntime(t, f.home, f.project.ID, "")
	if processes := repositoryGitProcesses(f.root); len(processes) != 0 {
		t.Errorf("Watch shutdown left git children: %v", processes)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "pr_watch_failing" {
			t.Errorf("Watch shutdown created a failure Notice: %s", notice.Summary)
		}
	}
}
