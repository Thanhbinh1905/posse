package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWorkspaceSendRefusesRefreshedMergedMemberPR(t *testing.T) {
	f := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"pr\"\nauto_teardown = \"never\"\n[repositories.worker]\nforge = \"github\"\n", []string{"worker", "e2e-tool"}, []string{"worker"})
	bin := filepath.Join(f.root, "bin")
	_ = os.MkdirAll(bin, 0o700)
	viewPath := filepath.Join(f.root, "gh-view.json")
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nif [ \"$1 $2\" = 'pr view' ]; then cat \"$POSSE_TEST_GH_VIEW\"; else exit 90; fi\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GH_VIEW", viewPath)
	worker := filepath.Join(f.workspace, "worker")
	remote := filepath.Join(f.root, "remotes", "worker.git")
	gitTest(t, worker, "remote", "set-url", "origin", "https://github.com/acme/worker.git")
	gitTest(t, worker, "config", "remote.origin.pushurl", remote)
	gitTest(t, worker, "config", "url.file://"+remote+".insteadOf", "https://github.com/acme/worker.git")
	head := strings.TrimSpace(gitTest(t, filepath.Join(f.task.WorktreePath, "worker"), "rev-parse", "HEAD"))
	url := "https://github.com/acme/worker/pull/7"
	repo := f.repos()["worker"]
	repo.PRURL = url
	if err := f.db.UpdateTaskRepo(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	view, _ := json.Marshal(map[string]any{"url": url, "state": "MERGED", "headRefOid": head, "baseRefName": "main", "mergeable": "MERGEABLE", "reviewDecision": "APPROVED", "mergeCommit": map[string]any{"oid": head}, "statusCheckRollup": []any{}})
	if err := os.WriteFile(viewPath, view, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run("send", "t1", "Add another fix"); code == 0 || !strings.Contains(f.out.String(), "pr_merged") {
		t.Fatalf("send did not refuse a refreshed merged Member PR: code=%d output=%s", code, f.out.String())
	}
	if state := f.state(); state != store.StateMerged && state != store.StateTornDown {
		t.Fatalf("merged Member was not Merged: %s", state)
	}
}
