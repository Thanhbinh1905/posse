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

func TestWorkspaceMemberCommittedAfterFirstPollCannotBeSkippedAsUnchanged(t *testing.T) {
	f := newWorkspaceLandFixture(t, "[defaults]\nlanding_mode = \"pr\"\nauto_unsaddle = \"never\"\n", []string{"worker", "e2e-tool"}, []string{"worker"})
	bin := filepath.Join(f.root, "bin")
	_ = os.MkdirAll(bin, 0o700)
	viewPath := filepath.Join(f.root, "gh-view.json")
	script := "#!/bin/sh\nif [ \"$1 $2\" = 'pr view' ]; then cat \"$POSSE_TEST_GH_VIEW\"; else exit 90; fi\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GH_VIEW", viewPath)
	gitTest(t, filepath.Join(f.workspace, "worker"), "remote", "set-url", "origin", "https://github.com/acme/worker.git")
	head := strings.TrimSpace(gitTest(t, filepath.Join(f.task.WorktreePath, "worker"), "rev-parse", "HEAD"))
	url := "https://github.com/acme/worker/pull/7"
	repo := f.repos()["worker"]
	repo.PRURL = url
	if err := f.db.UpdateTaskRepo(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	writeView := func(state string) {
		view, _ := json.Marshal(map[string]any{"url": url, "state": state, "headRefOid": head, "mergeable": "MERGEABLE", "reviewDecision": "APPROVED", "mergeCommit": map[string]any{"oid": head}, "statusCheckRollup": []any{}})
		if err := os.WriteFile(viewPath, view, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeView("OPEN")
	ctx := context.Background()
	if err := f.service.pollWorkspacePullRequests(ctx, f.db, f.project, f.config(), true); err != nil {
		t.Fatal(err)
	}
	if got := f.repos()["e2e-tool"].State; got != store.TaskRepoOpen {
		t.Fatalf("early poll froze member as %s", got)
	}
	tool := filepath.Join(f.task.WorktreePath, "e2e-tool")
	if err := os.WriteFile(filepath.Join(tool, "later.txt"), []byte("later work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, tool, "add", "later.txt")
	gitTest(t, tool, "commit", "-m", "later work")
	writeView("MERGED")
	if err := f.service.pollWorkspacePullRequests(ctx, f.db, f.project, f.config(), true); err != nil {
		t.Fatal(err)
	}
	if f.state() == store.StateLanded {
		t.Fatal("Task Landed without the later member commit")
	}
	if got := f.repos()["e2e-tool"].State; got != store.TaskRepoOpen {
		t.Fatalf("member commit was skipped: %s", got)
	}
	// A later authorized Teardown must preserve even clean committed work.
	member := f.repos()["e2e-tool"]
	target, err := f.service.projectTarget(ctx, f.db, f.project, member.Repo)
	if err != nil {
		t.Fatal(err)
	}
	memberProject := f.project
	memberProject.Root = target.Root
	memberProject.DefaultBranch = target.DefaultBranch
	memberTask := f.task
	memberTask.WorktreePath = tool
	if err := snapshotUnmergedMemberWork(ctx, f.db, memberProject, memberTask, member.Repo); err != nil {
		t.Fatal(err)
	}
	if got := gitTest(t, target.Root, "show", "refs/heads/posse/span-members-leftover:later.txt"); got != "later work\n" {
		t.Fatalf("committed Leftover lost: %q", got)
	}
	decisions, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil || len(decisions) == 0 {
		t.Fatalf("committed work has no Decision: %#v %v", decisions, err)
	}
}
