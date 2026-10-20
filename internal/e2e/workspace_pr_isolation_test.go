//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWatchKeepsHealthyMemberWorkWhenOtherForgeAndFetchStall(t *testing.T) {
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
	// Keep the blackholed fetch pending beyond the 30-second maintenance window.
	env = setEnv(env, "POSSE_TEST_EXTERNAL_COMMAND_TIMEOUT", "60s")
	env = setEnv(env, "PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	env = setEnv(env, "GIT_AUTHOR_NAME", "Posse E2E")
	env = setEnv(env, "GIT_AUTHOR_EMAIL", "posse-e2e@example.test")
	env = setEnv(env, "GIT_COMMITTER_NAME", "Posse E2E")
	env = setEnv(env, "GIT_COMMITTER_EMAIL", "posse-e2e@example.test")
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nmerge_mode = \"pr\"\nauto_teardown = \"merged\"\npr_poll = \"1ms\"\nforge = \"github\"\n"), 0o600); err != nil {
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

	createMergeTask := func(seq int, member store.ProjectRepo, prURL string, merge bool) (store.Task, string) {
		t.Helper()
		branch := fmt.Sprintf("posse/watch-%s-%d", member.Name, seq)
		sandboxRoot := filepath.Join(root, "sandboxes", fmt.Sprintf("t%d", seq))
		memberWorktree := filepath.Join(sandboxRoot, member.Name)
		if err := os.MkdirAll(sandboxRoot, 0o700); err != nil {
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
			Seq: seq, Type: "change", Title: "Watch " + member.Name, MergeMode: "pr", Branch: branch, BaseRef: "main", WorktreePath: sandboxRoot,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, transition := range []struct {
			from, to store.State
			source   string
		}{{store.StateSpawning, store.StateWorking, "cli"}, {store.StateWorking, store.StateDone, "worker"}, {store.StateDone, store.StateMerging, "cli"}} {
			if err := db.Transition(context.Background(), taskID, transition.from, transition.to, transition.source, "E2E fixture"); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.SetTaskGatedSHA(context.Background(), taskID, head); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UnixMilli()
		if _, err := db.ExecContext(context.Background(), `INSERT INTO mounts(project_id,n,path,state,task_id,acquired_at) VALUES(?,?,?,'held',?,?)`, project.ID, seq, sandboxRoot, taskID, now); err != nil {
			t.Fatal(err)
		}
		var sandboxID int64
		if err := db.QueryRowContext(context.Background(), `SELECT id FROM mounts WHERE project_id=? AND n=?`, project.ID, seq).Scan(&sandboxID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(context.Background(), `UPDATE tasks SET mount_id=? WHERE id=?`, sandboxID, taskID); err != nil {
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

	preMerged, preMergedHead := createMergeTask(3, members[1], "", true)
	goodURL := "https://github.com/acme/good/pull/2"
	good, goodHead := createMergeTask(2, members[1], goodURL, true)
	offline, _ := createMergeTask(1, members[0], "https://vpn-t181.invalid/acme/offline/pull/1", false)
	if err := db.Transition(context.Background(), preMerged.ID, store.StateMerging, store.StateMerged, "cli", "fixture already merged"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE tasks SET landed_ref=? WHERE id=?`, preMergedHead, preMerged.ID); err != nil {
		t.Fatal(err)
	}
	viewPath := filepath.Join(root, "good-pr-view.json")
	view, err := json.Marshal(map[string]any{
		"url": goodURL, "state": "OPEN", "headRefOid": goodHead, "baseRefName": "main", "mergeable": "MERGEABLE",
		"reviewDecision": "APPROVED", "statusCheckRollup": []any{},
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

	blackhole, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fetchStarted := make(chan struct{}, 1)
	releaseFetch := make(chan struct{})
	var releaseFetchOnce sync.Once
	releaseBlackhole := func() {
		releaseFetchOnce.Do(func() {
			close(releaseFetch)
			_ = blackhole.Close()
		})
	}
	go func() {
		for {
			connection, acceptErr := blackhole.Accept()
			if acceptErr != nil {
				return
			}
			select {
			case fetchStarted <- struct{}{}:
			default:
			}
			go func(connection net.Conn) {
				<-releaseFetch
				_ = connection.Close()
			}(connection)
		}
	}()
	offlineRoot := filepath.Join(workspace, "offline")
	// Hold only the offline checkout-sync lock while Watch performs its
	// target freshness fetch and reaches the deliberate Forge stall. This lets
	// the test install the blackhole route before checkout sync can fetch.
	syncLockOwner := newE2ELockOwner("offline-sync-lock", filepath.Join(offlineRoot, ".git", "posse-sync.lock"), env)
	lockReady, err := syncLockOwner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := syncLockOwner.Start(); err != nil {
		t.Fatal(err)
	}
	releaseSyncLock := func() {
		if syncLockOwner.ProcessState != nil {
			return
		}
		if syncLockOwner.Process != nil {
			_ = syncLockOwner.Process.Kill()
			_ = syncLockOwner.Wait()
		}
	}
	defer releaseSyncLock()
	lockResult := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(lockReady)
		if scanner.Scan() {
			lockResult <- scanner.Text()
			return
		}
		lockResult <- ""
	}()
	select {
	case line := <-lockResult:
		if line != "locked" {
			t.Fatalf("offline checkout sync lock owner did not acquire the lock: %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("offline checkout sync lock acquisition timed out")
	}

	t.Cleanup(func() {
		releaseBlackhole()
		_ = os.WriteFile(releaseStall, []byte("release\n"), 0o600)
	})
	if output := runPosse(t, binary, workspace, env, "watch", "--repair"); !strings.Contains(output, "state: running") {
		t.Fatalf("start supervised hidden Watch runtime: %s", output)
	}
	if !waitForCondition(5*time.Second, func() bool {
		_, err := os.Stat(stallStarted)
		return err == nil
	}) {
		calls, _ := os.ReadFile(ghLog)
		watchLog, _ := os.ReadFile(filepath.Join(home, "projects", project.Name, "watch.log"))
		state, _ := db.WatchRuntime(context.Background(), project.ID)
		_, procErr := os.Stat(fmt.Sprintf("/proc/%d", state.PID))
		t.Fatalf("offline Member forge query did not enter its deliberate stall: gh calls=%s Watch log=%s Watch=%#v procErr=%v pids=%v", calls, watchLog, state, procErr, watchPIDs(root))
	}
	watchBeforeUpdate, err := db.WatchRuntime(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	binaryBytes, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	replacement := binary + ".replacement"
	if err := os.WriteFile(replacement, binaryBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, binary); err != nil {
		t.Fatal(err)
	}
	time.Sleep(7 * time.Second)
	watchDuringStall, err := db.WatchRuntime(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if watchDuringStall.PID != watchBeforeUpdate.PID || watchDuringStall.ExecutableInode != watchBeforeUpdate.ExecutableInode {
		t.Fatalf("Watch re-executed before its in-flight Member poll ended: before=%#v during=%#v", watchBeforeUpdate, watchDuringStall)
	}
	if _, err := os.Stat(releaseStall); err == nil {
		t.Fatal("offline forge stall ended before the update-boundary assertion")
	}
	gitTest(t, env, offlineRoot, "config", "--unset-all", "url.file://"+filepath.Join(root, "offline.git")+".insteadOf")
	gitTest(t, env, offlineRoot, "config", "url.http://"+blackhole.Addr().String()+"/.insteadOf", "https://vpn-t181.invalid/")
	releaseSyncLock()
	select {
	case <-fetchStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("offline Member git fetch did not connect to the deliberate blackhole")
	}

	if !waitForCondition(7*time.Second, func() bool {
		observation, observationErr := db.LatestMemberPRObservation(context.Background(), good.ID, "good")
		return observationErr == nil && observation.State == "OPEN"
	}) {
		observation, observationErr := db.LatestMemberPRObservation(context.Background(), good.ID, "good")
		t.Fatalf("healthy Member's initial OPEN observation was not recorded during offline stalls: %#v, %v", observation, observationErr)
	}
	if task, err := db.TaskByID(context.Background(), project.ID, good.ID); err != nil || task.State != store.StateMerging {
		t.Fatalf("healthy Task changed state before its PR merged: %#v, %v", task, err)
	}
	writer, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin isolated SQLite writer contention: %v", err)
	}
	if _, err := writer.ExecContext(context.Background(), `UPDATE projects SET name=name WHERE id=?`, project.ID); err != nil {
		_ = writer.Rollback()
		t.Fatalf("hold isolated SQLite writer contention: %v", err)
	}
	view, err = json.Marshal(map[string]any{
		"url": goodURL, "state": "MERGED", "headRefOid": goodHead, "baseRefName": "main", "mergeable": "MERGEABLE",
		"reviewDecision": "APPROVED", "mergeCommit": map[string]string{"oid": goodHead}, "statusCheckRollup": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(viewPath, view, 0o600); err != nil {
		_ = writer.Rollback()
		t.Fatal(err)
	}
	time.Sleep(450 * time.Millisecond)
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(30*time.Second, func() bool {
		observation, observationErr := db.LatestMemberPRObservation(context.Background(), good.ID, "good")
		watch, watchErr := db.ProjectRepoWatchState(context.Background(), project.ID, "offline")
		merged, mergedErr := db.TaskByID(context.Background(), project.ID, preMerged.ID)
		healthy, healthyErr := db.TaskByID(context.Background(), project.ID, good.ID)
		return observationErr == nil && observation.State == "MERGED" && watchErr == nil && watch.CheckoutCheckedAt == 0 && mergedErr == nil && merged.State == store.StateTornDown && healthyErr == nil && healthy.State == store.StateTornDown
	}) {
		observation, observationErr := db.LatestMemberPRObservation(context.Background(), good.ID, "good")
		watch, watchErr := db.ProjectRepoWatchState(context.Background(), project.ID, "offline")
		merged, mergedErr := db.TaskByID(context.Background(), project.ID, preMerged.ID)
		healthy, healthyErr := db.TaskByID(context.Background(), project.ID, good.ID)
		intent, intentErr := db.IntentByTask(context.Background(), good.ID)
		sandbox, sandboxErr := db.SandboxByTask(context.Background(), good.ID)
		t.Fatalf("stalled offline forge blocked independent maintenance: healthy observation=%#v (%v), offline checkout=%#v (%v), already-merged Task=%#v (%v), healthy merged Task=%#v (%v), intent=%#v (%v), Sandbox=%#v (%v)", observation, observationErr, watch, watchErr, merged, mergedErr, healthy, healthyErr, intent, intentErr, sandbox, sandboxErr)
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
	goodWatch, err := db.ProjectRepoWatchState(context.Background(), project.ID, "good")
	if err != nil || goodWatch.CheckoutCheckedAt == 0 || goodWatch.CheckoutStatus == "unknown" {
		t.Fatalf("healthy Member checkout status = %#v, %v", goodWatch, err)
	}
	if offlineWatch, err := db.ProjectRepoWatchState(context.Background(), project.ID, "offline"); err != nil || offlineWatch.CheckoutCheckedAt != 0 {
		t.Fatalf("blackholed Member checkout unexpectedly completed during Teardown: %#v, %v", offlineWatch, err)
	}
	ghCalls, err := os.ReadFile(ghLog)
	if err != nil || strings.Count(string(ghCalls), "pr view https://github.com/acme/good/pull/2") < 2 {
		t.Fatalf("healthy Member was not queried again while offline work remained stalled: %s %v", ghCalls, err)
	}
	if err := os.WriteFile(releaseStall, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	releaseBlackhole()
	if !waitForCondition(15*time.Second, func() bool {
		notices, err := db.Notices(context.Background(), project.ID, false)
		if err != nil {
			return false
		}
		foundFailure := false
		for _, notice := range notices {
			if notice.TaskID == offline.ID && notice.Kind == "pr_watch_failing" && strings.Contains(notice.Summary, "offline") && strings.Contains(notice.DataJSON, `"consecutive_failures":3`) {
				foundFailure = true
			}
		}
		watch, watchErr := db.ProjectRepoWatchState(context.Background(), project.ID, "offline")
		return foundFailure && watchErr == nil && watch.CheckoutCheckedAt > 0 && watch.CheckoutStatus == "fetch_failed"
	}) {
		notices, _ := db.Notices(context.Background(), project.ID, false)
		watch, watchErr := db.ProjectRepoWatchState(context.Background(), project.ID, "offline")
		runtime, runtimeErr := db.WatchRuntime(context.Background(), project.ID)
		logData, _ := os.ReadFile(filepath.Join(home, "projects", project.Name, "watch.log"))
		t.Fatalf("released forge failure and blackholed checkout failure were not recorded against the offline Member: notices=%#v watch=%#v watchErr=%v runtime=%#v runtimeErr=%v pids=%v log=%s", notices, watch, watchErr, runtime, runtimeErr, watchPIDs(root), logData)
	}
	notices, err := db.Notices(context.Background(), project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "root_behind" && strings.Contains(notice.Summary, "offline") {
			t.Fatalf("offline Member fetch failure produced a false root_behind Notice: %#v", notice)
		}
	}
	if !waitForCondition(10*time.Second, func() bool {
		updated, err := db.WatchRuntime(context.Background(), project.ID)
		return err == nil && updated.ExecutableInode != watchBeforeUpdate.ExecutableInode
	}) {
		updated, _ := db.WatchRuntime(context.Background(), project.ID)
		t.Fatalf("Watch did not re-exec after its in-flight maintenance ended: before=%#v after=%#v", watchBeforeUpdate, updated)
	}
	memberErrors, err := db.MemberWatchErrors(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	failedPhases := map[string]map[string]bool{}
	for _, failure := range memberErrors {
		if failedPhases[failure.Repo] == nil {
			failedPhases[failure.Repo] = map[string]bool{}
		}
		failedPhases[failure.Repo][failure.Phase] = true
	}
	if !failedPhases["offline"]["pr_poll"] || !failedPhases["offline"]["checkout_sync"] {
		t.Fatalf("offline Member maintenance errors were not retained by phase: %#v", memberErrors)
	}
	show := runPosse(t, binary, workspace, env, "project", "show", "stack")
	for _, expected := range []string{"offline", "maintenance_errors", "pr_poll", "checkout_sync"} {
		if !strings.Contains(show, expected) {
			t.Fatalf("Project show omitted Member maintenance error %q: %s", expected, show)
		}
	}
	stopLiveWatchRuntime(t, root)
	var activeClaims int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM member_pr_poll_state WHERE project_id=? AND (claim_until>0 OR claim_token<>'')`, project.ID).Scan(&activeClaims); err != nil {
		t.Fatal(err)
	}
	if activeClaims != 0 {
		rows, queryErr := db.QueryContext(context.Background(), `SELECT repo,claim_until,claim_token,pr_polled_at FROM member_pr_poll_state WHERE project_id=? AND (claim_until>0 OR claim_token<>'')`, project.ID)
		if queryErr != nil {
			t.Fatalf("stopped Watch left %d Member poll claims active; read claim rows: %v", activeClaims, queryErr)
		}
		defer rows.Close()
		var active []string
		for rows.Next() {
			var repo, token string
			var claimUntil, polledAt int64
			if err := rows.Scan(&repo, &claimUntil, &token, &polledAt); err != nil {
				t.Fatal(err)
			}
			active = append(active, fmt.Sprintf("%s until=%d token=%s polled_at=%d", repo, claimUntil, token, polledAt))
		}
		logData, _ := os.ReadFile(filepath.Join(home, "projects", project.Name, "watch.log"))
		t.Fatalf("stopped Watch left %d Member poll claims active: %v; Watch log=%s", activeClaims, active, logData)
	}
	t.Logf("while offline PR query and git fetch remained stalled and SQLite writes were contended, the healthy Member observation and merged-Task Teardown completed; offline failure phases remained visible and claims were released")
}
