package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
 *"--method PUT"*)
   field_mode=
   for argument in "$@"; do
     case "$argument" in
       --field) field_mode=typed ;;
       --raw-field) field_mode=raw ;;
       description=\[*|title=\[*)
         if [ "$field_mode" = typed ]; then
           printf 'Error parsing typed field value: %s\n' "$argument" >&2
           exit 1
         fi
         if [ "${argument#description=}" != "$argument" ]; then printf '%s' "${argument#description=}" > "$POSSE_TEST_GLAB_DESCRIPTION"; fi
         if [ "${argument#title=}" != "$argument" ]; then printf '%s' "${argument#title=}" > "$POSSE_TEST_GLAB_TITLE"; fi
         field_mode= ;;
       description=*) printf '%s' "${argument#description=}" > "$POSSE_TEST_GLAB_DESCRIPTION"; field_mode= ;;
       title=*) printf '%s' "${argument#title=}" > "$POSSE_TEST_GLAB_TITLE"; field_mode= ;;
     esac
   done
   printf 'updated\n' ;;
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
	t.Setenv("POSSE_TEST_GLAB_TITLE", filepath.Join(f.root, "mr-title"))
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
	briefPath := filepath.Join(f.home, "projects", "shop", "tasks", "t1", "brief.md")
	brief := "---\ntype: ship\ntitle: E2E Brief title\ndone_when: commit exists\nissues: [12]\nrefs: [14]\n---\nE2E intent\n"
	if err := os.WriteFile(briefPath, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
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
	description, err := os.ReadFile(os.Getenv("POSSE_TEST_GLAB_DESCRIPTION"))
	if err != nil || !strings.Contains(string(description), "Closes #12") || !strings.Contains(string(description), "Refs #14") {
		t.Fatalf("GitLab MR description omitted issue links: %q %v", description, err)
	}
}

func TestGitLabPublishAddsIssueLinksToAnExistingMergeRequest(t *testing.T) {
	f := gitlabFixture(t, store.StateWorking)
	briefPath := filepath.Join(f.home, "projects", "shop", "tasks", "t1", "brief.md")
	brief := "---\ntype: ship\ntitle: E2E Brief title\ndone_when: commit exists\nissues: [12]\nrefs: [14]\n---\nE2E intent\n"
	if err := os.WriteFile(briefPath, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{
		"web_url": "https://git.example.com/group/sub/shop/-/merge_requests/17",
		"state":   "opened", "sha": f.headSHA, "source_branch": "posse/t1",
		"source_project_id": 7, "target_project_id": 7,
	}
	encoded, err := json.Marshal([]any{request})
	if err != nil {
		t.Fatal(err)
	}
	listPath := filepath.Join(f.root, "existing-mrs.json")
	if err := os.WriteFile(listPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GLAB_LIST", listPath)
	if code, output, stderr := f.run("publish", "Worker completion summary"); code != 0 {
		t.Fatalf("publish: %d %s %s", code, output, stderr)
	}
	description, err := os.ReadFile(os.Getenv("POSSE_TEST_GLAB_DESCRIPTION"))
	if err != nil || !strings.Contains(string(description), "Closes #12") || !strings.Contains(string(description), "Refs #14") {
		t.Fatalf("existing GitLab MR description omitted issue links: %q %v", description, err)
	}
}

func TestGitLabRepublishRefreshesCurrentMetadata(t *testing.T) {
	f := gitlabFixture(t, store.StateWorking)
	briefPath := filepath.Join(f.home, "projects", "shop", "tasks", "t1", "brief.md")
	brief := "---\ntype: ship\ntitle: E2E Brief title\ndone_when: commit exists\nticket: 12\nrefs: [14]\n---\nE2E intent\n"
	if err := os.WriteFile(briefPath, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{
		"web_url": "https://git.example.com/group/sub/shop/-/merge_requests/17",
		"state":   "opened", "sha": f.headSHA, "source_branch": "posse/t1", "target_branch": "main",
		"source_project_id": 7, "target_project_id": 7,
	}
	encoded, err := json.Marshal([]any{request})
	if err != nil {
		t.Fatal(err)
	}
	listPath := filepath.Join(f.root, "existing-mrs.json")
	if err := os.WriteFile(listPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GLAB_LIST", listPath)
	first := []string{"First summary", "--verify", "go test ./... -> pass", "--proof", "first proof", "--risk", "Risk: first"}
	if code, output, stderr := f.run(append([]string{"publish"}, first...)...); code != 0 {
		t.Fatalf("first publish: %d %s %s", code, output, stderr)
	}
	firstBody, err := os.ReadFile(os.Getenv("POSSE_TEST_GLAB_DESCRIPTION"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(firstBody), publishBodyStartPrefix) {
		t.Fatalf("first GitLab publish did not write tokenized ownership markers: %s", firstBody)
	}
	state, err := os.ReadFile(f.ghState)
	if err != nil {
		t.Fatal(err)
	}
	var current map[string]any
	if err := json.Unmarshal(state, &current); err != nil {
		t.Fatal(err)
	}
	humanPrefixedBody := "[Maintainer notes]\n\n" + string(firstBody)
	current["title"], current["description"] = "E2E Brief title", humanPrefixedBody
	state, err = json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.ghState, state, 0o600); err != nil {
		t.Fatal(err)
	}
	updatedBrief := strings.Replace(brief, "title: E2E Brief title", "title: \"[Release] Updated MR title\"", 1)
	if err := os.WriteFile(briefPath, []byte(updatedBrief), 0o600); err != nil {
		t.Fatal(err)
	}
	second := []string{"Second summary", "--verify", "go test ./... -> second pass", "--proof", "second proof", "--risk", "Risk: second"}
	if code, output, stderr := f.run(append([]string{"publish"}, second...)...); code != 0 {
		t.Fatalf("second publish: %d %s %s", code, output, stderr)
	}
	body, err := os.ReadFile(os.Getenv("POSSE_TEST_GLAB_DESCRIPTION"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"[Maintainer notes]", "Second summary", "go test ./... -> second pass", "second proof", "Risk: second", "Closes #12", "Refs #14"} {
		if !strings.Contains(string(body), expected) {
			t.Errorf("refreshed GitLab description omitted %q: %s", expected, body)
		}
	}
	for _, stale := range []string{"First summary", "go test ./... -> pass", "first proof", "Risk: first"} {
		if strings.Contains(string(body), stale) {
			t.Errorf("refreshed GitLab description retained stale %q: %s", stale, body)
		}
	}
	if title, err := os.ReadFile(os.Getenv("POSSE_TEST_GLAB_TITLE")); err != nil || string(title) != "[Release] Updated MR title" {
		t.Errorf("refreshed GitLab title=%q want %q: %v", title, "[Release] Updated MR title", err)
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
	if err != nil || task.State != store.StateLanded || task.LandedRef == "" {
		t.Fatalf("merged MR did not Land the active Task: %#v %v", task, err)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil || countNoticeKind(notices, "pr_merged") != 1 {
		t.Fatalf("merge Notices: %#v %v", notices, err)
	}
}

func TestGitLabMRDescriptionMatchesGitHubPRBody(t *testing.T) {
	f := gitlabFixture(t, store.StateWorking)
	summary := "Worker completion summary"
	verification := "go test ./internal/app -> pass"
	proof := "```text\ngo test ./internal/app\nPASS\n```"
	risk := "Risk: low\nRollback: revert the merge commit"
	title, body, err := prDetails(context.Background(), f.db, f.project, f.task, f.service.homePath, "", summary, verification, proof, risk)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body, "## Summary\n\n"+summary+"\n\n## Issue Link\n\n## Changes\n") {
		t.Fatalf("PR description does not use the seven-section template: %q", body)
	}
	if code, output, stderr := f.run("publish", summary, "--verify", verification, "--proof", proof, "--risk", risk); code != 0 {
		t.Fatalf("publish: %d %s %s", code, output, stderr)
	}
	written, err := os.ReadFile(os.Getenv("POSSE_TEST_GLAB_DESCRIPTION"))
	marker, markerErr := f.db.GetPRBodyMarker(context.Background(), f.task.ID, "")
	if err != nil || markerErr != nil || string(written) != managedPublishBody(body, marker.Token) {
		t.Fatalf("MR body differs from GitHub PR body: %q want tokenized body: %v %v", written, err, markerErr)
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
	if err != nil || task.State != store.StateLanding {
		t.Fatalf("closed MR: %+v %v", task, err)
	}
	decisions, err := f.db.Decisions(context.Background(), f.project.ID, true)
	if err != nil || len(decisions) != 1 || decisions[0].Kind != "pr_closed" {
		t.Fatalf("closed MR Decision: %#v %v", decisions, err)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil || countNoticeKind(notices, "pr_closed") != 1 {
		t.Fatalf("closed notice: %+v %v", notices, err)
	}
}

func TestGitLabDoctorChecksHostAuthentication(t *testing.T) {
	f := gitlabFixture(t)
	// Keep this fixture outside the configured temp directory so doctor treats
	// the Project as a live checkout and reaches the forge authentication check.
	tempDir := filepath.Join(f.root, "other-temp")
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tempDir)
	code, output, stderr := f.run("doctor")
	if code != 0 || !strings.Contains(output, "glab auth git.example.com") || !strings.Contains(output, "authenticated") {
		t.Fatalf("doctor: %d %s %s", code, output, stderr)
	}

	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repositoryForgeReadiness(context.Background(), f.repo, cfg, ""); err != nil {
		t.Fatalf("populate the readiness cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.bin, "glab"), []byte("#!/bin/sh\nprintf 'authentication missing\\n' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, output, stderr = f.run("doctor")
	if code != 0 || !strings.Contains(output, "glab auth git.example.com,warn") || !strings.Contains(output, "authentication missing") {
		t.Fatalf("doctor trusted cached authentication instead of checking current state: %d %s %s", code, output, stderr)
	}
	if err := os.WriteFile(filepath.Join(f.bin, "glab"), []byte("#!/bin/sh\n/bin/sleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	code, output, stderr = f.run("doctor")
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("doctor waited %s for a hanging forge CLI: %d %s %s", elapsed, code, output, stderr)
	}
	if code != 0 || !strings.Contains(output, "glab auth git.example.com,warn") || !strings.Contains(output, "timed out after 500ms") {
		t.Fatalf("doctor did not report the forge CLI timeout: %d %s %s", code, output, stderr)
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
