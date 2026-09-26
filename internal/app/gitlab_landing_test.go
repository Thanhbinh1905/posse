package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func gitlabFixture(t *testing.T, states ...store.State) *prLandingFixture {
	t.Helper()
	state := store.StateDone
	if len(states) > 0 {
		state = states[0]
	}
	f := newPRLandingFixture(t, "pr", state)
	origin := "https://git.example.com/group/sub/shop.git"
	gitTest(t, f.repo, "remote", "set-url", "origin", origin)
	gitTest(t, f.repo, "config", "url.file://"+f.remote+".insteadOf", origin)
	if err := os.WriteFile(filepath.Join(f.home, "projects", "shop", "config.toml"), []byte("[defaults]\nforge = \"gitlab\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$POSSE_TEST_GH_LOG"
case "$*" in
 *"merge_requests?state=opened"*) if [ -n "${POSSE_TEST_GLAB_LIST:-}" ]; then cat "$POSSE_TEST_GLAB_LIST"; else printf '[]\n'; fi ;;
 *"/approvals"*) cat "$POSSE_TEST_GLAB_APPROVALS" ;;
 *"/merge_requests/17"*) cat "$POSSE_TEST_GH_STATE" ;;
 "mr create "*)
   while [ "$#" -gt 0 ]; do
     if [ "$1" = "--description" ]; then printf '%s' "$2" > "$POSSE_TEST_GLAB_DESCRIPTION"; break; fi
     shift
   done
   printf 'Creating merge request...\n%s\n' 'https://git.example.com/group/sub/shop/-/merge_requests/17' ;;
 "mr merge "*) printf 'Merged\n' ;;
 "auth status "*) printf 'authenticated\n' ;;
 *) printf 'unexpected glab invocation: %s\n' "$*" >&2; exit 90 ;;
esac
`
	if err := os.WriteFile(filepath.Join(f.bin, "glab"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GLAB_DESCRIPTION", filepath.Join(f.root, "mr-description"))
	f.setGitLabApprovals(t, true, 0)
	f.setGitLabState(t, "opened", "mergeable", "running", f.headSHA)
	return f
}

func (f *prLandingFixture) setGitLabApprovals(t *testing.T, approved bool, remaining int) {
	t.Helper()
	path := filepath.Join(f.root, "glab-approvals.json")
	encoded, err := json.Marshal(map[string]any{"approved": approved, "approvals_left": remaining})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GLAB_APPROVALS", path)
}

func (f *prLandingFixture) setGitLabState(t *testing.T, state, mergeable, pipeline, sha string) {
	t.Helper()
	mr := map[string]any{"web_url": "https://git.example.com/group/sub/shop/-/merge_requests/17", "state": state, "sha": sha, "source_branch": "posse/t1", "target_branch": "main", "source_project_id": 7, "target_project_id": 7, "detailed_merge_status": mergeable, "merge_commit_sha": "", "head_pipeline": map[string]any{"status": pipeline, "web_url": "https://git.example.com/group/sub/shop/-/pipelines/3"}}
	if state == "merged" {
		mr["merge_commit_sha"] = strings.Repeat("c", 40)
	}
	encoded, err := json.Marshal(mr)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.ghState, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGitLabWorkerPublishesAndLeadLands(t *testing.T) {
	f := gitlabFixture(t, store.StateWorking)
	url := "https://git.example.com/group/sub/shop/-/merge_requests/17"
	if code, out, e := f.run("publish", "Worker completion summary"); code != 0 || !strings.Contains(out, url) {
		t.Fatalf("publish MR: %d %s %s", code, out, e)
	}
	if code, out, e := f.run("holler", "done", "Worker completion summary", "--pr", url); code != 0 {
		t.Fatalf("signal MR: %d %s %s", code, out, e)
	}
	if code, out, e := f.run("land", "t1"); code != 0 {
		t.Fatalf("land MR: %d %s %s", code, out, e)
	}
	task, err := f.db.Task(context.Background(), f.project.ID, "t1")
	if err != nil || task.State != store.StateLanding || task.GatedSHA != f.headSHA {
		t.Fatalf("MR not watched: %+v %v", task, err)
	}
	log, _ := os.ReadFile(f.ghLog)
	if strings.Count(string(log), "mr create ") != 1 {
		t.Fatalf("Lead recreated MR: %s", log)
	}
}

func TestGitLabWorkerRejectsForkAndRebaseBeforePublishing(t *testing.T) {
	f := gitlabFixture(t, store.StateWorking)
	if err := os.WriteFile(filepath.Join(f.home, "projects", "shop", "config.toml"), []byte("[defaults]\nforge = \"gitlab\"\nmerge_method = \"rebase\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := f.run("publish", "Worker summary"); code != 1 || !strings.Contains(out, "pr_merge_method_unsupported") {
		t.Fatalf("rebase publish: %d %s", code, out)
	}
	if got := strings.TrimSpace(gitTest(t, f.remote, "for-each-ref", "--format=%(refname)", "refs/heads/posse/")); got != "" {
		t.Fatalf("published despite rebase: %s", got)
	}
	if err := os.WriteFile(filepath.Join(f.home, "projects", "shop", "config.toml"), []byte("[defaults]\nforge = \"gitlab\"\nmerge_method = \"squash\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.setGitLabState(t, "opened", "mergeable", "running", f.headSHA)
	if err := os.WriteFile(f.ghState, []byte(`{"web_url":"https://git.example.com/group/sub/shop/-/merge_requests/17","state":"opened","sha":"`+f.headSHA+`","source_branch":"posse/t1","target_branch":"main","source_project_id":8,"target_project_id":7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := f.run("publish", "Worker summary"); code != 1 || !strings.Contains(out, "pr_head_mismatch") {
		t.Fatalf("fork publish: %d %s", code, out)
	}
	if code, out, _ := f.run("holler", "done", "Worker summary", "--pr", "https://git.example.com/group/sub/shop/-/merge_requests/17"); code != 1 || !strings.Contains(out, "pr_head_mismatch") {
		t.Fatalf("fork signal: %d %s", code, out)
	}
}

func TestGitLabWatchRecognizesMergeDuringFollowUp(t *testing.T) {
	f := gitlabFixture(t, store.StateWorking)
	ctx := context.Background()
	url := "https://git.example.com/group/sub/shop/-/merge_requests/17"
	if code, out, stderr := f.run("publish", "Worker summary"); code != 0 {
		t.Fatalf("publish: %d %s %s", code, out, stderr)
	}
	if err := f.db.UpdateTaskLanding(ctx, f.task.ID, url, ""); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.worktree, "commit", "--allow-empty", "-m", "local follow-up")
	f.setGitLabState(t, "merged", "mergeable", "success", f.headSHA)
	for i := 0; i < 2; i++ {
		if err := f.pollGitLab(t); err != nil {
			t.Fatal(err)
		}
	}
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil || task.State != store.StateWorking || task.LandedRef != "" {
		t.Fatalf("active Rider was ended by merged MR: %#v %v", task, err)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil || countNoticeKind(notices, "pr_merged") != 1 {
		t.Fatalf("merge Notices: %#v %v", notices, err)
	}
}

func TestGitLabMRDescriptionMatchesGitHubPRBody(t *testing.T) {
	f := gitlabFixture(t)
	title, body, err := prDetails(context.Background(), f.db, f.project, f.task, f.service.homePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body, "## Rider summary\n") {
		t.Fatalf("PR description has the wrong visible role label: %q", body)
	}
	if code, output, stderr := f.run("land", "t1"); code != 0 {
		t.Fatalf("land: %d %s %s", code, output, stderr)
	}
	written, err := os.ReadFile(os.Getenv("POSSE_TEST_GLAB_DESCRIPTION"))
	if err != nil || string(written) != body {
		t.Fatalf("MR body differs from GitHub PR body: %q want %q: %v", written, body, err)
	}
	log, err := os.ReadFile(f.ghLog)
	if err != nil || !strings.Contains(string(log), "--title "+title) {
		t.Fatalf("MR title differs from Brief title: %q: %v", log, err)
	}
}

func TestGitLabLandingAndWatchLifecycle(t *testing.T) {
	f := gitlabFixture(t)
	if code, output, stderr := f.run("land", "t1"); code != 0 {
		t.Fatalf("land: %d %s %s", code, output, stderr)
	}
	task, err := f.db.Task(context.Background(), f.project.ID, "t1")
	if err != nil || task.PRURL != "https://git.example.com/group/sub/shop/-/merge_requests/17" || task.State != store.StateLanding {
		t.Fatalf("task: %+v %v", task, err)
	}
	if got := strings.TrimSpace(gitTest(t, f.remote, "rev-parse", "refs/heads/posse/t1")); got != task.GatedSHA {
		t.Fatalf("pushed %q want %q", got, task.GatedSHA)
	}
	f.setGitLabState(t, "opened", "conflict", "failed", task.GatedSHA)
	if err := f.pollGitLab(t); err != nil {
		t.Fatal(err)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"pr_opened", "pr_checks_failed", "pr_conflict"} {
		if countNoticeKind(notices, kind) != 1 {
			t.Fatalf("missing %s: %+v", kind, notices)
		}
	}
	f.setGitLabState(t, "opened", "mergeable", "success", task.GatedSHA)
	if err := f.pollGitLab(t); err != nil {
		t.Fatal(err)
	}
	notices, err = f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil || countNoticeKind(notices, "land_ready") != 1 {
		t.Fatalf("land ready: %+v %v", notices, err)
	}
	if code, output, stderr := f.run("land", "t1", "--merge", "--user-approved", "User approved MR"); code != 0 {
		t.Fatalf("merge: %d %s %s", code, output, stderr)
	}
	log, err := os.ReadFile(f.ghLog)
	if err != nil || !strings.Contains(string(log), "--sha "+task.GatedSHA) || !strings.Contains(string(log), "--squash") || !strings.Contains(string(log), "--auto-merge=false") {
		t.Fatalf("unprotected merge: %q %v", log, err)
	}
	f.setGitLabState(t, "merged", "mergeable", "success", task.GatedSHA)
	if err := f.pollGitLab(t); err != nil {
		t.Fatal(err)
	}
	task, err = f.db.Task(context.Background(), f.project.ID, "t1")
	if err != nil || task.State != store.StateLanded {
		t.Fatalf("merged task: %+v %v", task, err)
	}
}

func (f *prLandingFixture) pollGitLab(t *testing.T) error {
	t.Helper()
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		return err
	}
	return f.service.pollProjectPullRequests(context.Background(), f.db, f.project, cfg, true)
}

func TestGitLabReviewChangesAndApprovals(t *testing.T) {
	f := gitlabFixture(t)
	if code, output, stderr := f.run("land", "t1"); code != 0 {
		t.Fatalf("land: %d %s %s", code, output, stderr)
	}
	f.setGitLabApprovals(t, false, 1)
	f.setGitLabState(t, "opened", "not_approved", "success", f.headSHA)
	if err := f.pollGitLab(t); err != nil {
		t.Fatal(err)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil || countNoticeKind(notices, "land_ready") != 0 {
		t.Fatalf("premature ready: %+v %v", notices, err)
	}
	f.setGitLabState(t, "opened", "discussions_not_resolved", "success", f.headSHA)
	if err := f.pollGitLab(t); err != nil {
		t.Fatal(err)
	}
	notices, err = f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil || countNoticeKind(notices, "pr_changes_requested") != 1 {
		t.Fatalf("changes notice: %+v %v", notices, err)
	}
	f.setGitLabApprovals(t, true, 0)
	f.setGitLabState(t, "opened", "mergeable", "success", f.headSHA)
	if err := f.pollGitLab(t); err != nil {
		t.Fatal(err)
	}
	notices, err = f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil || countNoticeKind(notices, "land_ready") != 1 {
		t.Fatalf("ready after approval: %+v %v", notices, err)
	}
}

func TestGitLabClosedAndChangedHead(t *testing.T) {
	f := gitlabFixture(t)
	if code, output, stderr := f.run("land", "t1"); code != 0 {
		t.Fatalf("land: %d %s %s", code, output, stderr)
	}
	f.setGitLabState(t, "opened", "mergeable", "success", strings.Repeat("f", 40))
	if code, output, _ := f.run("land", "t1", "--merge", "--user-approved", "Approved"); code != 1 || !strings.Contains(output, "branch_moved") {
		t.Fatalf("moved branch: %d %s", code, output)
	}
	log, err := os.ReadFile(f.ghLog)
	if err != nil || strings.Contains(string(log), "mr merge") {
		t.Fatalf("unexpected merge: %q %v", log, err)
	}
	// Retry after the head is restored and the task is gated again.
	f.setGitLabState(t, "opened", "mergeable", "success", f.headSHA)
	if code, output, stderr := f.run("land", "t1"); code != 0 {
		t.Fatalf("reland: %d %s %s", code, output, stderr)
	}
	f.setGitLabState(t, "closed", "mergeable", "success", f.headSHA)
	if err := f.pollGitLab(t); err != nil {
		t.Fatal(err)
	}
	task, err := f.db.Task(context.Background(), f.project.ID, "t1")
	if err != nil || task.State != store.StateDone {
		t.Fatalf("closed MR: %+v %v", task, err)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil || countNoticeKind(notices, "pr_closed") != 1 {
		t.Fatalf("closed notice: %+v %v", notices, err)
	}
}

func TestGitLabDoctorChecksHostAuthentication(t *testing.T) {
	f := gitlabFixture(t)
	code, output, stderr := f.run("doctor")
	if code != 0 || !strings.Contains(output, "glab auth git.example.com") || !strings.Contains(output, "authenticated") {
		t.Fatalf("doctor: %d %s %s", code, output, stderr)
	}
}

func TestGitLabMRLookupIgnoresForkBranch(t *testing.T) {
	f := gitlabFixture(t)
	list := filepath.Join(f.root, "mr-list.json")
	if err := os.WriteFile(list, []byte(`[{"web_url":"https://git.example.com/group/sub/shop/-/merge_requests/16","sha":"`+f.headSHA+`","source_branch":"posse/t1","source_project_id":18,"target_project_id":7},{"web_url":"https://git.example.com/group/sub/shop/-/merge_requests/18","sha":"`+f.headSHA+`","source_branch":"posse/t1","target_project_id":7}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GLAB_LIST", list)
	if code, output, stderr := f.run("land", "t1"); code != 0 {
		t.Fatalf("land: %d %s %s", code, output, stderr)
	}
	task, err := f.db.Task(context.Background(), f.project.ID, "t1")
	if err != nil || !strings.HasSuffix(task.PRURL, "/17") {
		t.Fatalf("used fork MR: %+v %v", task, err)
	}
	log, err := os.ReadFile(f.ghLog)
	if err != nil || !strings.Contains(string(log), "mr create ") {
		t.Fatalf("did not create Worker MR: %q %v", log, err)
	}
}

func TestGitLabAutoDetectsAuthenticatedEnterpriseHost(t *testing.T) {
	f := gitlabFixture(t)
	cfg, err := config.Load(f.home, "")
	if err != nil {
		t.Fatal(err)
	}
	forge, err := forgeForRepository(context.Background(), f.repo, cfg, "")
	if err != nil || forge.Kind != "gitlab" || forge.Host != "git.example.com" {
		t.Fatalf("auto forge: %+v %v", forge, err)
	}
	log, err := os.ReadFile(f.ghLog)
	if err != nil || !strings.Contains(string(log), "auth status --hostname git.example.com") {
		t.Fatalf("missing authentication check: %q %v", log, err)
	}
}

func TestEnterpriseForgeRequiresUnambiguousAuthentication(t *testing.T) {
	f := gitlabFixture(t)
	if err := os.WriteFile(filepath.Join(f.bin, "gh"), []byte("#!/bin/sh\ncase \"$*\" in\n  'auth status --hostname git.example.com') exit 0 ;;\nesac\nexit 90\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.home, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := forgeForRepository(context.Background(), f.repo, cfg, ""); err == nil || !strings.Contains(err.Error(), "authenticated with both gh and glab") {
		t.Fatalf("accepted ambiguous forge: %v", err)
	}
}

func TestGitLabMergeMethodsAndFastForwardObservation(t *testing.T) {
	for _, method := range []string{"squash", "merge"} {
		t.Run(method, func(t *testing.T) {
			f := gitlabFixture(t)
			configText := "[defaults]\nforge = \"gitlab\"\nmerge_method = \"" + method + "\"\n"
			if err := os.WriteFile(filepath.Join(f.home, "projects", "shop", "config.toml"), []byte(configText), 0o600); err != nil {
				t.Fatal(err)
			}
			if code, output, stderr := f.run("land", "t1"); code != 0 {
				t.Fatalf("land: %d %s %s", code, output, stderr)
			}
			f.setGitLabState(t, "opened", "mergeable", "success", f.headSHA)
			if err := f.pollGitLab(t); err != nil {
				t.Fatal(err)
			}
			if code, output, stderr := f.run("land", "t1", "--merge", "--user-approved", "Approved"); code != 0 {
				t.Fatalf("merge: %d %s %s", code, output, stderr)
			}
			log, err := os.ReadFile(f.ghLog)
			if err != nil {
				t.Fatal(err)
			}
			mergeCall := ""
			for _, line := range strings.Split(string(log), "\n") {
				if strings.HasPrefix(line, "mr merge ") {
					mergeCall = line
				}
			}
			if !strings.Contains(mergeCall, "--sha "+f.headSHA) || !strings.Contains(mergeCall, "--auto-merge=false") || strings.Contains(mergeCall, "--squash") != (method == "squash") || strings.Contains(mergeCall, "--rebase") {
				t.Fatalf("wrong merge strategy: %q", mergeCall)
			}
			if method == "merge" {
				f.setGitLabState(t, "merged", "mergeable", "success", f.headSHA)
				// Fast-forward merges can omit both merge_commit_sha and squash_commit_sha.
				if err := os.WriteFile(f.ghState, []byte(`{"web_url":"https://git.example.com/group/sub/shop/-/merge_requests/17","state":"merged","sha":"`+f.headSHA+`"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := f.pollGitLab(t); err != nil {
					t.Fatal(err)
				}
				task, err := f.db.Task(context.Background(), f.project.ID, "t1")
				if err != nil || task.State != store.StateLanded || task.LandedRef != f.headSHA {
					t.Fatalf("fast-forward MR: %+v %v", task, err)
				}
			}
		})
	}
}

func TestGitLabRefusesRebaseWithoutChangingPushedBranch(t *testing.T) {
	f := gitlabFixture(t)
	configPath := filepath.Join(f.home, "projects", "shop", "config.toml")
	setMergeMethod := func(method string) {
		t.Helper()
		if err := os.WriteFile(configPath, []byte("[defaults]\nforge = \"gitlab\"\nmerge_method = \""+method+"\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setMergeMethod("rebase")
	code, output, stderr := f.run("land", "t1")
	if code != 1 || !strings.Contains(output, "pr_merge_method_unsupported") || !strings.Contains(output, "squash or merge") {
		t.Fatalf("landing with rebase: %d %s %s", code, output, stderr)
	}
	if _, err := os.Stat(filepath.Join(f.remote, "refs", "heads", "posse", "t1")); !os.IsNotExist(err) {
		t.Fatalf("rebase config pushed branch: %v", err)
	}
	setMergeMethod("squash")
	if code, output, stderr := f.run("land", "t1"); code != 0 {
		t.Fatalf("open MR: %d %s %s", code, output, stderr)
	}
	setMergeMethod("rebase")
	code, output, stderr = f.run("land", "t1", "--merge", "--user-approved", "Approved")
	if code != 1 || !strings.Contains(output, "pr_merge_method_unsupported") || !strings.Contains(output, "squash or merge") {
		t.Fatalf("merge with rebase: %d %s %s", code, output, stderr)
	}
	log, err := os.ReadFile(f.ghLog)
	if err != nil || strings.Contains(string(log), "mr merge ") {
		t.Fatalf("attempted server rebase: %q %v", log, err)
	}
	if task, err := f.db.Task(context.Background(), f.project.ID, "t1"); err != nil || task.State != store.StateLanding {
		t.Fatalf("rebase refusal changed landing state: %+v %v", task, err)
	}
}

func TestGitLabPendingMergeabilityCannotRaiseReady(t *testing.T) {
	f := gitlabFixture(t)
	if code, output, stderr := f.run("land", "t1"); code != 0 {
		t.Fatalf("land: %d %s %s", code, output, stderr)
	}
	f.setGitLabState(t, "opened", "checking", "success", f.headSHA)
	if err := f.pollGitLab(t); err != nil {
		t.Fatal(err)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil || countNoticeKind(notices, "land_ready") != 0 {
		t.Fatalf("premature ready: %+v %v", notices, err)
	}
}

func TestGitLabForgeOriginAndOverrides(t *testing.T) {
	f := gitlabFixture(t)
	path := filepath.Join(f.home, "projects", "shop", "config.toml")
	if err := os.WriteFile(path, []byte("[defaults]\nforge = \"gitlab\"\n[repositories.second]\nforge = \"github\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	forge, err := forgeForRepository(context.Background(), f.repo, cfg, "")
	if err != nil || forge.Kind != "gitlab" || forge.Path != "group/sub/shop" {
		t.Fatalf("forge: %+v %v", forge, err)
	}
	for _, address := range []string{"https://git.example.com/group/sub/shop-evil/-/merge_requests/17", "https://git.example.com:8443/group/sub/shop/-/merge_requests/17"} {
		if _, err := forgeReference(address, forge); err == nil {
			t.Fatalf("accepted mismatched repository: %s", address)
		}
	}
	member, err := forgeForRepository(context.Background(), f.repo, cfg, "second")
	if err != nil || member.Kind != "github" {
		t.Fatalf("member override: %+v %v", member, err)
	}
}
