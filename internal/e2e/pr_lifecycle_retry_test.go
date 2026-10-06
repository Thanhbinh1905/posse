//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestRideRetriesRetryableStoreBusyBeforeTaskCreation(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "posse"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "retried")
	binary := filepath.Join(root, "posse-fake")
	script := "#!/bin/sh\nset -eu\nif [ ! -e \"$POSSE_RETRY_MARKER\" ]; then\n  : > \"$POSSE_RETRY_MARKER\"\n  printf '%s\\n' 'error{code,message,retryable,help}: \"store_busy\",\"database contention\",true,[\"Retry the command after the store is available\"]'\n  exit 1\nfi\n: > \"$POSSE_RETRY_MARKER.second\"\nprintf 'task: t1\\n'\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := &prLifecycleFixture{
		repo: repo, binary: binary, leadEnv: []string{"POSSE_RETRY_MARKER=" + marker},
		db: db, project: project,
	}
	output := fixture.runRideWithStoreBusyRetry(t, filepath.Join(root, "brief.md"), "retryable-ride")
	if !strings.Contains(output, "task: t1") {
		t.Fatalf("successful retry output = %s", output)
	}
	if _, err := os.Stat(marker + ".second"); err != nil {
		t.Fatalf("ride was not retried after retryable store_busy: %v", err)
	}
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM tasks WHERE project_id=? AND short_name=?`, project.ID, "retryable-ride").Scan(&count); err != nil || count != 0 {
		t.Fatalf("helper created %d Tasks while retrying a pre-create failure, err=%v", count, err)
	}
}
