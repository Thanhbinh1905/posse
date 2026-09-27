package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

// Exercises the same CLI commands as a workspace Worker: publish one changed
// PR member, leave another changed member local, signal done, then let the Lead
// gate both. The Project root is not a Git repository.
func TestWorkspaceWorkerPublishesOnlyItsPRMemberAndLeadGates(t *testing.T) {
	f := newWorkspaceLandFixture(t, "[defaults]\nlanding_mode = \"pr\"\nauto_unsaddle = \"never\"\n", []string{"worker", "e2e-tool"}, []string{"worker", "e2e-tool"})
	// No live Herdr pane is launched by this fixture; avoid reconciling its
	// simulated Worker away before the CLI can publish.
	f.service.Herdr = nil
	briefPath := filepath.Join(f.home, "projects", "stack", "tasks", "t1", "brief.md")
	brief := "---\ntype: ship\ntitle: Span members\ndone_when: members changed\nrepos: [worker, e2e-tool]\nissues: [worker#12, e2e-tool#13]\nrefs: [worker#14]\n---\nChange the members.\n"
	if err := writeFile(briefPath, []byte(brief)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := f.db.Transition(ctx, f.task.ID, store.StateDone, store.StateWorking, "lead", "publish test"); err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(f.workspace, "worker")
	gitTest(t, origin, "remote", "set-url", "origin", "https://github.com/acme/worker.git")
	gitTest(t, origin, "config", "url.file://"+filepath.Join(f.root, "remotes", "worker.git")+".insteadOf", "https://github.com/acme/worker.git")
	bin := filepath.Join(f.root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(f.root, "gh.log")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$POSSE_TEST_GH_LOG"
case "$1 $2" in
 "pr list") printf '[]\n' ;;
 "pr create") printf 'https://github.com/acme/worker/pull/7\n' ;;
 "pr view") printf '{"url":"https://github.com/acme/worker/pull/7","state":"OPEN","headRefOid":"%s","headRefName":"posse/span-members","baseRefName":"main","headRepository":{"nameWithOwner":"%s"}}\n' "$(git -C "$POSSE_TEST_WORKTREE" rev-parse HEAD)" "${POSSE_TEST_GH_SOURCE:-acme/worker}" ;;
 *) exit 90 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GH_LOG", log)
	t.Setenv("POSSE_TEST_WORKTREE", filepath.Join(f.task.WorktreePath, "worker"))
	t.Setenv("POSSE_WORKER_HOME", f.home)
	t.Chdir(f.task.WorktreePath)
	url := "https://github.com/acme/worker/pull/7"
	if code := f.run("publish", "--repo", "e2e-tool", "Worker summary"); code == 0 {
		t.Fatalf("local member published: %s", f.out.String())
	}
	if code := f.run("holler", "done", "Worker summary"); code == 0 {
		t.Fatalf("unpublished PR member accepted: %s", f.out.String())
	}
	if code := f.run("publish", "--repo", "worker", "Worker summary"); code != 0 || !strings.Contains(f.out.String(), url) {
		t.Fatalf("publish: %d %s task=%#v state=%s", code, f.out.String(), f.task, f.state())
	}
	if f.repos()["worker"].PRURL != url {
		t.Fatalf("PR not recorded for the member: %#v", f.repos())
	}
	t.Setenv("POSSE_TEST_GH_SOURCE", "another/worker")
	if code := f.run("holler", "done", "Worker summary"); code == 0 || !strings.Contains(f.out.String(), "pr_head_mismatch") {
		t.Fatalf("accepted a fork PR for the member: %d %s", code, f.out.String())
	}
	t.Setenv("POSSE_TEST_GH_SOURCE", "acme/worker")
	if code := f.run("holler", "done", "Worker summary"); code != 0 {
		t.Fatalf("done: %d %s", code, f.out.String())
	}
	t.Setenv("POSSE_WORKER_HOME", "")
	t.Chdir(f.workspace)
	t.Setenv("POSSE_TEST_GH_SOURCE", "another/worker")
	if code := f.run("land", "t1"); code == 0 || !strings.Contains(f.out.String(), "pr_head_mismatch") {
		t.Fatalf("Land accepted a fork PR: %d %s", code, f.out.String())
	}
	if f.state() != store.StateDone {
		t.Fatalf("invalid PR passed Gate: %s", f.state())
	}
	t.Setenv("POSSE_TEST_GH_SOURCE", "acme/worker")
	if code := f.run("land", "t1"); code != 0 {
		t.Fatalf("land: %d %s", code, f.out.String())
	}
	repos := f.repos()
	if repos["worker"].State != store.TaskRepoLanding || repos["worker"].PRURL != url || repos["e2e-tool"].State != store.TaskRepoGated || f.state() != store.StateLanding {
		t.Fatalf("Land changed the member states: %#v / %s", repos, f.state())
	}
	calls, err := os.ReadFile(log)
	if err != nil || strings.Count(string(calls), "pr create") != 1 {
		t.Fatalf("Lead created another PR: %s (%v)", calls, err)
	}
	for _, expected := range []string{"Closes #12", "Refs #14"} {
		if !strings.Contains(string(calls), expected) {
			t.Fatalf("worker member PR omitted %q: %s", expected, calls)
		}
	}
	if strings.Contains(string(calls), "Closes #13") {
		t.Fatalf("worker member PR contained another member's issue: %s", calls)
	}
	if code := f.run("land", "t1", "--merge"); code == 0 || !strings.Contains(f.out.String(), "land_approval_required") {
		t.Fatalf("merged without User approval: %d %s", code, f.out.String())
	}
}
