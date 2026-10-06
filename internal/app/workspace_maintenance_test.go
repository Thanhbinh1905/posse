package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWorkspaceMaintenanceContinuesAfterMemberForgeDiscoveryFailure(t *testing.T) {
	f := newWorkspaceLandFixture(t, "[defaults]\nlanding_mode = \"pr\"\nauto_unsaddle = \"landed\"\npr_poll = \"1ms\"\nforge = \"auto\"\n", []string{"backend"}, []string{"backend"})
	ctx := context.Background()
	goodURL := "https://github.com/acme/worker/pull/2"
	offlineURL := "https://vpn-t181.invalid/acme/backend/pull/1"

	for name, host := range map[string]string{"backend": "vpn-t181.invalid", "worker": "github.com"} {
		repoRoot := filepath.Join(f.workspace, name)
		remote := filepath.Join(f.root, "remotes", name+".git")
		url := "https://" + host + "/acme/" + name + ".git"
		gitTest(t, repoRoot, "remote", "set-url", "origin", url)
		gitTest(t, repoRoot, "config", "url.file://"+remote+".insteadOf", url)
	}
	detected, err := detectProject(ctx, f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	scannedRepos := make([]store.ProjectRepo, 0, len(detected.Repos))
	for _, repo := range detected.Repos {
		scannedRepos = append(scannedRepos, store.ProjectRepo{
			Name: repo.Name, Path: repo.Path, DefaultBranch: repo.DefaultBranch, Status: store.RepoActive, OriginHost: repo.Remote,
		})
	}
	if err := f.db.SyncProjectRepos(ctx, f.project.ID, scannedRepos); err != nil {
		t.Fatal(err)
	}

	setLandingPR := func(task store.Task, repoName, prURL string) store.TaskRepo {
		t.Helper()
		memberWorktree := filepath.Join(task.WorktreePath, repoName)
		head := strings.TrimSpace(gitTest(t, memberWorktree, "rev-parse", "HEAD"))
		if err := f.db.SetTaskGatedSHA(ctx, task.ID, head); err != nil {
			t.Fatal(err)
		}
		if task.State == store.StateDone {
			if err := f.db.Transition(ctx, task.ID, store.StateDone, store.StateLanding, "cli", "PR published"); err != nil {
				t.Fatal(err)
			}
		}
		repos, err := f.db.TaskRepos(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, repo := range repos {
			if repo.Repo != repoName {
				continue
			}
			repo.PRURL = prURL
			repo.GatedSHA = head
			if err := f.db.UpdateTaskRepo(ctx, repo); err != nil {
				t.Fatal(err)
			}
			return repo
		}
		t.Fatalf("Task %d has no repo %q", task.ID, repoName)
		return store.TaskRepo{}
	}
	setLandingPR(f.task, "backend", offlineURL)

	cfg := f.config()
	members, _, err := f.service.planTaskMembers(ctx, f.db, f.project, cfg, dispatch.Brief{Type: "ship", Repos: []string{"worker"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	goodID, _, err := createTaskWithSequence(ctx, f.db, f.project, f.home, store.Task{Type: "ship", Title: "Good Member merged PR", ShortName: "good-member-pr", LandingMode: "pr"})
	if err != nil {
		t.Fatal(err)
	}
	goodTask, err := f.db.TaskByID(ctx, f.project.ID, goodID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.acquireWorkspaceMount(ctx, f.db, f.project, goodTask, members, f.home, "warm", nil); err != nil {
		t.Fatal(err)
	}
	goodTask, err = f.db.TaskByID(ctx, f.project.ID, goodID)
	if err != nil {
		t.Fatal(err)
	}
	goodWorktree := filepath.Join(goodTask.WorktreePath, "worker")
	if err := os.WriteFile(filepath.Join(goodWorktree, "good-change.txt"), []byte("merged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, goodWorktree, "add", "good-change.txt")
	gitTest(t, goodWorktree, "commit", "-m", "good member change")
	for _, transition := range []struct {
		from, to store.State
		source   string
	}{{store.StateSpawning, store.StateWorking, "cli"}, {store.StateWorking, store.StateDone, "worker"}, {store.StateDone, store.StateLanding, "cli"}} {
		if err := f.db.Transition(ctx, goodID, transition.from, transition.to, transition.source, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	goodRepo := setLandingPR(goodTask, "worker", goodURL)
	goodHead := goodRepo.GatedSHA
	workerRoot := filepath.Join(f.workspace, "worker")
	gitTest(t, workerRoot, "merge", "--ff-only", goodHead)
	gitTest(t, workerRoot, "push", "origin", "main")

	bin := filepath.Join(f.root, "maintenance-bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	viewPath := filepath.Join(f.root, "good-pr-view.json")
	view, err := json.Marshal(map[string]any{
		"url": goodURL, "state": "MERGED", "headRefOid": goodHead, "mergeable": "MERGEABLE",
		"reviewDecision": "APPROVED", "mergeCommit": map[string]string{"oid": goodHead}, "statusCheckRollup": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(viewPath, view, 0o600); err != nil {
		t.Fatal(err)
	}
	gh := "#!/bin/sh\ncase \"$1 $2\" in\n  \"auth status\") case \" $* \" in *vpn-t181.invalid*) exit 1 ;; *) exit 0 ;; esac ;;\n  \"pr view\") cat \"" + viewPath + "\" ;;\n  *) echo \"unexpected gh command: $*\" >&2; exit 90 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o700); err != nil {
		t.Fatal(err)
	}
	glab := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "glab"), []byte(glab), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := f.service.maintainProjectWatch(ctx, f.db, f.project); err != nil {
		t.Fatalf("workspace maintenance aborted: %v", err)
	}
	observation, err := f.db.LatestMemberPRObservation(ctx, goodID, "worker")
	if err != nil || observation.State != "MERGED" {
		t.Fatalf("healthy Member PR was not observed: %#v, %v", observation, err)
	}
	goodTask, err = f.db.TaskByID(ctx, f.project.ID, goodID)
	if err != nil || goodTask.State != store.StateTornDown {
		t.Fatalf("healthy merged Task was not auto-torn down: %#v, %v", goodTask, err)
	}
	for _, name := range []string{"backend", "worker"} {
		watch, err := f.db.ProjectRepoWatchState(ctx, f.project.ID, name)
		if err != nil || watch.CheckoutCheckedAt == 0 || watch.CheckoutStatus == "unknown" {
			t.Fatalf("Member %s checkout was not checked after forge discovery failed: %#v, %v", name, watch, err)
		}
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	foundOfflineFailure := false
	for _, notice := range notices {
		if notice.TaskID == f.task.ID && notice.Kind == "pr_watch_failing" && strings.Contains(notice.Summary, "backend") && strings.Contains(notice.Summary, "vpn-t181.invalid") {
			foundOfflineFailure = true
		}
	}
	if !foundOfflineFailure {
		t.Fatalf("offline Member-specific PR discovery Notice missing: %#v", notices)
	}
}
