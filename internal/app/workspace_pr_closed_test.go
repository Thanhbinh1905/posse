package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWorkspaceClosedMemberRaisesDecisionWithoutReopeningTask(t *testing.T) {
	f := newWorkspaceLandFixture(t, "[defaults]\nlanding_mode = \"pr\"\nauto_unsaddle = \"never\"\n", []string{"worker", "e2e-tool"}, []string{"worker", "e2e-tool"})
	bin := filepath.Join(f.root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$1 $2\" in\n  'pr list') echo '[]' ;;\n  'pr create') echo 'https://github.com/acme/worker/pull/7' ;;\n  'pr view') cat \"$POSSE_TEST_GH_VIEW\" ;;\n  *) exit 90 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GH_LOG", filepath.Join(f.root, "gh.log"))
	state := filepath.Join(f.root, "gh-view.json")
	t.Setenv("POSSE_TEST_GH_VIEW", state)
	worker := filepath.Join(f.workspace, "worker")
	gitTest(t, worker, "remote", "set-url", "origin", "https://github.com/acme/worker.git")
	gitTest(t, worker, "config", "remote.origin.pushurl", filepath.Join(f.root, "remotes", "worker.git"))
	head := strings.TrimSpace(gitTest(t, filepath.Join(f.task.WorktreePath, "worker"), "rev-parse", "HEAD"))
	url := "https://github.com/acme/worker/pull/7"
	writeView := func(stateName string) {
		body := `{"url":"` + url + `","state":"` + stateName + `","headRefOid":"` + head + `","mergeable":"MERGEABLE","reviewDecision":"APPROVED","statusCheckRollup":[]}`
		if err := os.WriteFile(state, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeView("OPEN")
	if code := f.run("land", "t1"); code != 0 {
		t.Fatalf("land: %s", f.out.String())
	}
	writeView("CLOSED")
	ctx := context.Background()
	for tick := 0; tick < 2; tick++ {
		if err := f.service.pollWorkspacePullRequests(ctx, f.db, f.project, f.config(), true); err != nil {
			t.Fatal(err)
		}
	}
	if f.state() != store.StateLanding || f.repos()["worker"].State != store.TaskRepoLanding {
		t.Fatalf("closed Member changed Task: %s %#v", f.state(), f.repos())
	}
	decisions, err := f.db.Decisions(ctx, f.project.ID, true)
	closed := 0
	for _, decision := range decisions {
		if decision.Kind == "pr_closed" && decision.Origin == "pr_closed:"+url {
			closed++
		}
	}
	if err != nil || closed != 1 {
		t.Fatalf("closed Member Decision: %#v %v", decisions, err)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil || countNoticeKind(notices, "pr_closed") != 1 || countNoticeKind(notices, "pr_watch_failing") != 0 {
		t.Fatalf("closed Member Notices: %#v %v", notices, err)
	}
}
