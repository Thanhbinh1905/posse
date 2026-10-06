//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLookoutKeepsHealthyMemberWorkWhenOtherForgeQueryStalls(t *testing.T) {
	root := newFixtureRoot(t, herdr.TestRootName())
	binDir := filepath.Join(root, "bin")
	workspace := filepath.Join(root, "stack")
	home := filepath.Join(root, "posse")
	for _, directory := range []string{binDir, workspace, home} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := isolatedE2EEnv(t, root)
	env = setEnv(env, "POSSE_TEST_ROOT", root)
	env = setEnv(env, "PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	env = setEnv(env, "GIT_AUTHOR_NAME", "Posse E2E")
	env = setEnv(env, "GIT_AUTHOR_EMAIL", "posse-e2e@example.test")
	env = setEnv(env, "GIT_COMMITTER_NAME", "Posse E2E")
	env = setEnv(env, "GIT_COMMITTER_EMAIL", "posse-e2e@example.test")
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nlanding_mode = \"pr\"\nauto_unsaddle = \"landed\"\npr_poll = \"1ms\"\nforge = \"github\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	members := []store.ProjectRepo{
		{Name: "offline", Path: "offline", DefaultBranch: "main", Status: store.RepoActive, OriginHost: "vpn-t181.invalid"},
		{Name: "good", Path: "good", DefaultBranch: "main", Status: store.RepoActive, OriginHost: "github.com"},
	}
	for _, member := range members {
		repoRoot := filepath.Join(workspace, member.Path)
		remote := filepath.Join(root, member.Name+".git")
		if err := os.MkdirAll(repoRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		gitTest(t, env, repoRoot, "init", "-q", "-b", "main")
		if err := os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte(member.Name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, env, repoRoot, "add", "README.md")
		gitTest(t, env, repoRoot, "commit", "-qm", "fixture "+member.Name)
		gitTest(t, env, root, "init", "--bare", "-q", "-b", "main", remote)
		gitTest(t, env, repoRoot, "remote", "add", "origin", remote)
		gitTest(t, env, repoRoot, "push", "-q", "-u", "origin", "main")
		hostURL := "https://" + member.OriginHost + "/acme/" + member.Name + ".git"
		gitTest(t, env, repoRoot, "remote", "set-url", "origin", hostURL)
		gitTest(t, env, repoRoot, "config", "url.file://"+remote+".insteadOf", hostURL)
	}

	binary := filepath.Join(binDir, "posse")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir, build.Env = moduleRoot(t), env
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse: %v\n%s", err, output)
	}

	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateWorkspaceProject(context.Background(), "stack", workspace, members)
	if err != nil {
		t.Fatal(err)
	}
	client := herdr.NewWithEnv("herdr", env)
	startServer(t, client)
	lead, err := createWorkspace(client, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, lead.Workspace.WorkspaceID, lead.RootPane.PaneID, "posse:stack:lead"); err != nil {
		t.Fatal(err)
	}
	project.HerdrWorkspaceID = lead.Workspace.WorkspaceID

	createLandingTask := func(seq int, member store.ProjectRepo, prURL string, merge bool) (store.Task, string) {
		t.Helper()
		branch := fmt.Sprintf("posse/watch-%s-%d", member.Name, seq)
		mountRoot := filepath.Join(root, "mounts", fmt.Sprintf("t%d", seq))
		memberWorktree := filepath.Join(mountRoot, member.Name)
		if err := os.MkdirAll(mountRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		gitTest(t, env, filepath.Join(workspace, member.Path), "worktree", "add", "-q", "-b", branch, memberWorktree, "main")
		if err := os.WriteFile(filepath.Join(memberWorktree, "change.txt"), []byte(fmt.Sprintf("%s change %d\n", member.Name, seq)), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, env, memberWorktree, "add", "change.txt")
		gitTest(t, env, memberWorktree, "commit", "-qm", "change "+member.Name)
		head := strings.TrimSpace(gitTest(t, env, memberWorktree, "rev-parse", "HEAD"))
		if merge {
			repoRoot := filepath.Join(workspace, member.Path)
			gitTest(t, env, repoRoot, "merge", "--ff-only", head)
			gitTest(t, env, repoRoot, "push", "origin", "main")
		}
		taskID, err := db.CreateTask(context.Background(), project.ID, store.Task{
			Seq: seq, Type: "ship", Title: "Watch " + member.Name, LandingMode: "pr", Branch: branch, BaseRef: "main", WorktreePath: mountRoot,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, transition := range []struct {
			from, to store.State
			source   string
		}{{store.StateSpawning, store.StateWorking, "cli"}, {store.StateWorking, store.StateDone, "worker"}, {store.StateDone, store.StateLanding, "cli"}} {
			if err := db.Transition(context.Background(), taskID, transition.from, transition.to, transition.source, "E2E fixture"); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.SetTaskGatedSHA(context.Background(), taskID, head); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UnixMilli()
		if _, err := db.ExecContext(context.Background(), `INSERT INTO mounts(project_id,n,path,state,task_id,acquired_at) VALUES(?,?,?,'held',?,?)`, project.ID, seq, mountRoot, taskID, now); err != nil {
			t.Fatal(err)
		}
		var mountID int64
		if err := db.QueryRowContext(context.Background(), `SELECT id FROM mounts WHERE project_id=? AND n=?`, project.ID, seq).Scan(&mountID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(context.Background(), `UPDATE tasks SET mount_id=? WHERE id=?`, mountID, taskID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(context.Background(), `INSERT INTO task_repos(task_id,repo,worktree_path,base_ref,landing_mode,state,gated_sha,pr_url,updated_at) VALUES(?,?,?,?,?,'open',?,?,?)`, taskID, member.Name, memberWorktree, "main", "pr", head, prURL, now); err != nil {
			t.Fatal(err)
		}
		task, err := db.TaskByID(context.Background(), project.ID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		return task, head
	}

	offline, _ := createLandingTask(1, members[0], "https://vpn-t181.invalid/acme/offline/pull/1", false)
	goodURL := "https://github.com/acme/good/pull/2"
	good, goodHead := createLandingTask(2, members[1], goodURL, true)
	preLanded, preLandedHead := createLandingTask(3, members[1], "", true)
	if err := db.Transition(context.Background(), preLanded.ID, store.StateLanding, store.StateLanded, "cli", "fixture already landed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE tasks SET landed_ref=? WHERE id=?`, preLandedHead, preLanded.ID); err != nil {
		t.Fatal(err)
	}
	viewPath := filepath.Join(root, "good-pr-view.json")
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
	ghLog := filepath.Join(root, "gh.log")
	stallStarted := filepath.Join(root, "offline-pr-stalled")
	releaseStall := filepath.Join(root, "release-pr-stall")
	gh := "#!/bin/sh\nprintf '%s %s\\n' \"${HERDR_PANE_ID:-none}\" \"$*\" >> \"" + ghLog + "\"\ncase \"$1 $2\" in\n  \"auth status\") exit 0 ;;\n  \"pr view\")\n    case \"$3\" in\n      *vpn-t181.invalid*) : > \"" + stallStarted + "\"; while [ ! -e \"" + releaseStall + "\" ]; do sleep 0.05; done; echo \"stalled forge released by test\" >&2; exit 1 ;;\n      *github.com*) cat \"" + viewPath + "\" ;;\n    esac ;;\n  *) echo \"unexpected gh command: $*\" >&2; exit 90 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(gh), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "glab"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	lookoutPaneID := ""
	t.Cleanup(func() {
		_ = os.WriteFile(releaseStall, []byte("release\n"), 0o600)
		if lookoutPaneID != "" {
			_, _ = client.Run(context.Background(), "pane", "send-keys", lookoutPaneID, "ctrl+c")
		}
	})
	lookout, err := createTab(client, project.HerdrWorkspaceID, workspace, "posse:stack:lookout")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(context.Background(), "pane.rename", map[string]any{"pane_id": lookout.RootPane.PaneID, "label": "posse:stack:lookout"}); err != nil {
		t.Fatal(err)
	}
	lookoutPaneID = lookout.RootPane.PaneID
	if _, err := client.Run(context.Background(), "pane", "run", lookoutPaneID, binary+" lookout --poll-only"); err != nil {
		t.Fatalf("start isolated Lookout: %v", err)
	}
	if !waitForCondition(5*time.Second, func() bool {
		_, err := os.Stat(stallStarted)
		return err == nil
	}) {
		t.Fatal("offline Member forge query did not enter its deliberate stall")
	}

	if !waitForCondition(7*time.Second, func() bool {
		observation, observationErr := db.LatestMemberPRObservation(context.Background(), good.ID, "good")
		watch, watchErr := db.ProjectRepoWatchState(context.Background(), project.ID, "offline")
		landed, landedErr := db.TaskByID(context.Background(), project.ID, preLanded.ID)
		healthy, healthyErr := db.TaskByID(context.Background(), project.ID, good.ID)
		return observationErr == nil && observation.State == "MERGED" && watchErr == nil && watch.CheckoutCheckedAt > 0 && landedErr == nil && landed.State == store.StateTornDown && healthyErr == nil && healthy.State == store.StateTornDown
	}) {
		observation, observationErr := db.LatestMemberPRObservation(context.Background(), good.ID, "good")
		watch, watchErr := db.ProjectRepoWatchState(context.Background(), project.ID, "offline")
		landed, landedErr := db.TaskByID(context.Background(), project.ID, preLanded.ID)
		healthy, healthyErr := db.TaskByID(context.Background(), project.ID, good.ID)
		t.Fatalf("stalled offline forge blocked independent maintenance: healthy observation=%#v (%v), offline checkout=%#v (%v), already-landed Task=%#v (%v), healthy merged Task=%#v (%v)", observation, observationErr, watch, watchErr, landed, landedErr, healthy, healthyErr)
	}
	if _, err := os.Stat(releaseStall); err == nil {
		t.Fatal("offline forge stall ended before healthy Member and independent maintenance completed")
	}
	observation, err := db.LatestMemberPRObservation(context.Background(), good.ID, "good")
	if err != nil || observation.State != "MERGED" {
		t.Fatalf("healthy Member PR observation = %#v, %v", observation, err)
	}
	currentGood, err := db.TaskByID(context.Background(), project.ID, good.ID)
	if err != nil || currentGood.State != store.StateTornDown {
		t.Fatalf("healthy merged Member Task was not auto-torn down during the offline forge stall: %#v, %v", currentGood, err)
	}
	for _, name := range []string{"offline", "good"} {
		watch, err := db.ProjectRepoWatchState(context.Background(), project.ID, name)
		if err != nil || watch.CheckoutCheckedAt == 0 || watch.CheckoutStatus == "unknown" {
			t.Fatalf("Member %s checkout status = %#v, %v", name, watch, err)
		}
	}
	ghCalls, err := os.ReadFile(ghLog)
	if err != nil || !strings.Contains(string(ghCalls), "pr view https://github.com/acme/good/pull/2") {
		t.Fatalf("healthy Member forge was not queried after offline discovery failed: %s %v", ghCalls, err)
	}
	if err := os.WriteFile(releaseStall, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(5*time.Second, func() bool {
		notices, err := db.Notices(context.Background(), project.ID, false)
		if err != nil {
			return false
		}
		for _, notice := range notices {
			if notice.TaskID == offline.ID && notice.Kind == "pr_watch_failing" && strings.Contains(notice.Summary, "offline") {
				return true
			}
		}
		return false
	}) {
		t.Fatal("released offline forge failure was not recorded against its Member Task")
	}
	if _, err := client.Run(context.Background(), "pane", "send-keys", lookoutPaneID, "ctrl+c"); err != nil {
		t.Errorf("stop isolated Lookout: %v", err)
	}
	t.Logf("while offline PR query remained stalled, healthy Member observation, checkout polling, and landed Task Teardown all completed")
}
