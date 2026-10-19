package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWorkspaceDiscardClosesEveryRecordedMemberPR(t *testing.T) {
	f := newWorkspaceLandFixture(t, localWorkspaceConfig, []string{"backend", "worker"}, []string{"backend", "worker"})
	ctx := context.Background()
	urls := map[string]string{
		"backend": "https://github.com/acme/backend/pull/11",
		"worker":  "https://github.com/acme/worker/pull/12",
	}
	repos, err := f.db.TaskRepos(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	heads := make(map[string]string, len(repos))
	for _, repo := range repos {
		target, err := f.service.projectTarget(ctx, f.db, f.project, repo.Repo)
		if err != nil {
			t.Fatal(err)
		}
		remoteURL := "https://github.com/acme/" + repo.Repo + ".git"
		gitTest(t, target.Root, "remote", "set-url", "origin", remoteURL)
		repo.GatedSHA = strings.TrimSpace(gitTest(t, repo.WorktreePath, "rev-parse", "HEAD"))
		repo.PRURL = urls[repo.Repo]
		repo.LandingMode = "pr"
		repo.State = store.TaskRepoLanding
		heads[repo.Repo] = repo.GatedSHA
		if err := f.db.UpdateTaskRepo(ctx, repo); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(f.root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	closedDir := filepath.Join(f.root, "closed")
	if err := os.MkdirAll(closedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(f.root, "gh.log")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$*" >> "$POSSE_TEST_GH_LOG"
case "$1 $2" in
  "pr view")
    url=$3
    name=${url##*/}
    state=OPEN
    [ ! -e "$POSSE_TEST_CLOSED_DIR/$name" ] || state=CLOSED
    case "$url" in
      *backend/pull/11) head=%q ;;
      *worker/pull/12) head=%q ;;
      *) exit 91 ;;
    esac
    printf '{"url":"%%s","state":"%%s","headRefOid":"%%s"}\n' "$url" "$state" "$head"
    ;;
  "pr close")
    url=$3
    name=${url##*/}
    : > "$POSSE_TEST_CLOSED_DIR/$name"
    ;;
  *) exit 90 ;;
esac
`, heads["backend"], heads["worker"])
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GH_LOG", log)
	t.Setenv("POSSE_TEST_CLOSED_DIR", closedDir)
	cfg := f.config()
	cfg.Defaults.Forge = "github"
	task, err := f.db.TaskByID(ctx, f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := f.service.discardPullRequests(ctx, f.db, f.project, task)
	if err != nil || len(requests) != 2 {
		t.Fatalf("recorded Member PRs = %#v, %v; want both members", requests, err)
	}
	if err := f.service.closeAndVerifyDiscardPullRequests(ctx, f.db, requests, cfg); err != nil {
		t.Fatalf("close all recorded Member PRs: %v", err)
	}
	for _, url := range urls {
		name := filepath.Base(url)
		if _, err := os.Stat(filepath.Join(closedDir, name)); err != nil {
			t.Errorf("recorded Member PR %s was not closed: %v", url, err)
		}
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(calls), "pr close "); got != 2 {
		t.Fatalf("forge close calls = %d, want one per recorded Member PR: %s", got, calls)
	}
}

func TestDiscardClosesRecordedGitLabMergeRequest(t *testing.T) {
	f := newWorkspaceLandFixture(t, localWorkspaceConfig, []string{"backend"}, []string{"backend"})
	ctx := context.Background()
	repos, err := f.db.TaskRepos(ctx, f.task.ID)
	if err != nil || len(repos) != 1 {
		t.Fatalf("Task member = %#v, %v", repos, err)
	}
	repo := repos[0]
	target, err := f.service.projectTarget(ctx, f.db, f.project, repo.Repo)
	if err != nil {
		t.Fatal(err)
	}
	gitTest(t, target.Root, "remote", "set-url", "origin", "https://gitlab.com/acme/backend.git")
	repo.GatedSHA = strings.TrimSpace(gitTest(t, repo.WorktreePath, "rev-parse", "HEAD"))
	repo.PRURL = "https://gitlab.com/acme/backend/-/merge_requests/7"
	repo.LandingMode = "pr"
	repo.State = store.TaskRepoLanding
	if err := f.db.UpdateTaskRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(f.root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	closed := filepath.Join(f.root, "gitlab-closed")
	log := filepath.Join(f.root, "glab.log")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$POSSE_TEST_GLAB_LOG"
case "$1 $2" in
  "api --hostname")
    state=opened
    [ ! -e "$POSSE_TEST_GLAB_CLOSED" ] || state=closed
    printf '{"web_url":"https://gitlab.com/acme/backend/-/merge_requests/7","state":"%s","sha":"%s"}\n' "$state" "$POSSE_TEST_GLAB_HEAD"
    ;;
  "mr close") : > "$POSSE_TEST_GLAB_CLOSED" ;;
  *) exit 90 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "glab"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GLAB_LOG", log)
	t.Setenv("POSSE_TEST_GLAB_CLOSED", closed)
	t.Setenv("POSSE_TEST_GLAB_HEAD", repo.GatedSHA)
	cfg := f.config()
	cfg.Defaults.Forge = "gitlab"
	task, err := f.db.TaskByID(ctx, f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := f.service.discardPullRequests(ctx, f.db, f.project, task)
	if err != nil || len(requests) != 1 {
		t.Fatalf("recorded Member MR = %#v, %v", requests, err)
	}
	if err := f.service.closeAndVerifyDiscardPullRequests(ctx, f.db, requests, cfg); err != nil {
		t.Fatalf("close and verify recorded GitLab MR: %v", err)
	}
	if _, err := os.Stat(closed); err != nil {
		t.Fatalf("GitLab MR close command was not called: %v", err)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "mr close 7 --repo https://gitlab.com/acme/backend") || !strings.Contains(string(calls), "api --hostname gitlab.com projects/acme%2Fbackend/merge_requests/7") {
		t.Fatalf("GitLab adapter used unexpected calls: %s", calls)
	}
}
