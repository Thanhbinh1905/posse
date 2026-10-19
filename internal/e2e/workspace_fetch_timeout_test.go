//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
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

func countWorkspaceNoticeKind(notices []store.Notice, kind string) int {
	count := 0
	for _, notice := range notices {
		if notice.Kind == kind {
			count++
		}
	}
	return count
}

func TestWorkspaceFetchTimeoutNoticeCoalescesAndResets(t *testing.T) {
	root := newFixtureRoot(t, fixturePrefix("t279-"))
	binDir := filepath.Join(root, "bin")
	workspaceRoot := filepath.Join(root, "stack")
	for _, directory := range []string{binDir, workspaceRoot, filepath.Join(root, "posse")} {
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
	if err := os.WriteFile(filepath.Join(root, "posse", "config.toml"), []byte("[defaults]\nmerge_mode = \"pr\"\npr_poll = \"1ms\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	blackhole, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connectionsMu sync.Mutex
	connections := map[net.Conn]struct{}{}
	var stopOnce sync.Once
	stopBlackhole := func() {
		stopOnce.Do(func() {
			_ = blackhole.Close()
			connectionsMu.Lock()
			for connection := range connections {
				_ = connection.Close()
			}
			connectionsMu.Unlock()
		})
	}
	t.Cleanup(stopBlackhole)
	go func() {
		for {
			connection, acceptErr := blackhole.Accept()
			if acceptErr != nil {
				return
			}
			connectionsMu.Lock()
			connections[connection] = struct{}{}
			connectionsMu.Unlock()
			go func(connection net.Conn) {
				_, _ = io.Copy(io.Discard, connection)
				connectionsMu.Lock()
				delete(connections, connection)
				connectionsMu.Unlock()
				_ = connection.Close()
			}(connection)
		}
	}()

	members := []store.ProjectRepo{
		{Name: "alpha", Path: "alpha", DefaultBranch: "main", Status: store.RepoActive, OriginHost: "fetch-timeout.invalid"},
		{Name: "beta", Path: "beta", DefaultBranch: "main", Status: store.RepoActive, OriginHost: "fetch-timeout.invalid"},
	}
	remotes := map[string]string{}
	for _, member := range members {
		repoRoot := filepath.Join(workspaceRoot, member.Path)
		remote := filepath.Join(root, member.Name+".git")
		remotes[member.Name] = remote
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
		gitTest(t, env, repoRoot, "remote", "set-url", "origin", "https://fetch-timeout.invalid/acme/"+member.Name+".git")
		gitTest(t, env, repoRoot, "config", "url.http://"+blackhole.Addr().String()+"/.insteadOf", "https://fetch-timeout.invalid/")
		gitTest(t, env, repoRoot, "config", "http.lowSpeedLimit", "1")
		gitTest(t, env, repoRoot, "config", "http.lowSpeedTime", "1")
	}

	binary := filepath.Join(binDir, "posse")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir, build.Env = moduleRoot(t), env
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse: %v\n%s", err, output)
	}

	db, err := store.Open(filepath.Join(root, "posse"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close isolated database: %v", err)
		}
	})
	ctx := context.Background()
	project, err := db.CreateWorkspaceProject(ctx, "stack", workspaceRoot, members)
	if err != nil {
		t.Fatal(err)
	}
	client := herdr.NewWithEnv("herdr", env)
	startServer(t, client)
	workspace, err := createWorkspace(client, workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, workspace.Workspace.WorkspaceID, workspace.RootPane.PaneID, "posse:stack:lead"); err != nil {
		t.Fatal(err)
	}
	project.HerdrWorkspaceID = workspace.Workspace.WorkspaceID
	watch, err := createTab(client, project.HerdrWorkspaceID, workspaceRoot, "posse:stack:watch")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(ctx, "pane.rename", map[string]any{"pane_id": watch.RootPane.PaneID, "label": "posse:stack:watch"}); err != nil {
		t.Fatal(err)
	}
	watchPaneID := watch.RootPane.PaneID
	t.Cleanup(func() {
		_, _ = client.Run(context.Background(), "pane", "send-keys", watchPaneID, "ctrl+c")
	})
	if _, err := client.Run(ctx, "pane", "run", watchPaneID, binary+" watch --poll-only"); err != nil {
		t.Fatalf("start isolated Watch: %v", err)
	}

	if !waitForCondition(12*time.Second, func() bool {
		for _, member := range members {
			state, err := db.ProjectRepoWatchState(ctx, project.ID, member.Name)
			if err != nil || state.CheckoutCheckedAt == 0 {
				return false
			}
		}
		return true
	}) {
		t.Fatal("Watch did not attempt to fetch both workspace Members")
	}
	for _, member := range members {
		state, err := db.ProjectRepoWatchState(ctx, project.ID, member.Name)
		if err != nil || state.CheckoutStatus != "fetch_failed" {
			t.Fatalf("timed-out origin fetch for %s was recorded as %+v, err=%v; want fetch_failed", member.Name, state, err)
		}
	}
	notices, err := db.Notices(ctx, project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if countWorkspaceNoticeKind(notices, "root_behind") != 0 {
		t.Fatalf("timed-out origin fetch produced a false root_behind Notice: %#v", notices)
	}

	if !waitForCondition(15*time.Second, func() bool {
		notices, err := db.Notices(ctx, project.ID, true)
		if err != nil || countWorkspaceNoticeKind(notices, "root_fetch_failing") != 1 {
			return false
		}
		for _, member := range members {
			var failures int
			if err := db.QueryRowContext(ctx, `SELECT consecutive_failures FROM repo_fetch_failures WHERE project_id=? AND repo=?`, project.ID, member.Name).Scan(&failures); err != nil || failures < 3 {
				return false
			}
		}
		return true
	}) {
		notices, _ := db.Notices(ctx, project.ID, true)
		t.Fatalf("repeated Member fetch timeouts did not produce one coalesced Notice: %#v", notices)
	}
	notices, err = db.Notices(ctx, project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	var fetchNotice store.Notice
	for _, notice := range notices {
		if notice.Kind == "root_fetch_failing" {
			fetchNotice = notice
		}
	}
	if fetchNotice.ID == 0 || !strings.Contains(fetchNotice.DataJSON, `"alpha"`) || !strings.Contains(fetchNotice.DataJSON, `"beta"`) {
		t.Fatalf("coalesced fetch Notice does not identify both failing Members: %#v", fetchNotice)
	}

	for _, member := range members {
		gitTest(t, env, filepath.Join(workspaceRoot, member.Path), "remote", "set-url", "origin", "file://"+remotes[member.Name])
	}
	if !waitForCondition(10*time.Second, func() bool {
		for _, member := range members {
			state, err := db.ProjectRepoWatchState(ctx, project.ID, member.Name)
			if err != nil || state.CheckoutStatus != "updated" {
				return false
			}
			var failures int
			if err := db.QueryRowContext(ctx, `SELECT consecutive_failures FROM repo_fetch_failures WHERE project_id=? AND repo=?`, project.ID, member.Name).Scan(&failures); err != nil || failures != 0 {
				return false
			}
		}
		return true
	}) {
		t.Fatal("successful origin fetch did not reset both Member failure streaks")
	}
	notices, err = db.Notices(ctx, project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if countWorkspaceNoticeKind(notices, "root_fetch_failing") != 0 {
		t.Fatalf("successful recovery did not clear the active fetch-failure Notice: %#v", notices)
	}

	alphaRoot := filepath.Join(workspaceRoot, "alpha")
	remoteClone := filepath.Join(root, "alpha-remote-work")
	gitTest(t, env, root, "clone", "-q", "file://"+remotes["alpha"], remoteClone)
	for index := 1; index <= 2; index++ {
		if err := os.WriteFile(filepath.Join(remoteClone, fmt.Sprintf("remote-%d.txt", index)), []byte(fmt.Sprintf("remote %d\n", index)), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, env, remoteClone, "add", ".")
		gitTest(t, env, remoteClone, "commit", "-qm", fmt.Sprintf("remote update %d", index))
	}
	gitTest(t, env, remoteClone, "push", "origin", "main")
	if err := os.WriteFile(filepath.Join(alphaRoot, "local-only.txt"), []byte("local commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, env, alphaRoot, "add", "local-only.txt")
	gitTest(t, env, alphaRoot, "commit", "-qm", "local-only update")
	if !waitForCondition(10*time.Second, func() bool {
		state, err := db.ProjectRepoWatchState(ctx, project.ID, "alpha")
		if err != nil || state.CheckoutStatus != "root_behind" {
			return false
		}
		var behind int
		notices, err := db.Notices(ctx, project.ID, true)
		if err != nil {
			return false
		}
		for _, notice := range notices {
			if notice.Kind == "root_behind" && strings.Contains(notice.DataJSON, `"commits_behind":2`) {
				behind = 2
			}
		}
		return behind == 2
	}) {
		t.Fatal("a real divergent checkout did not report the correct two-commit behind count")
	}
	notices, err = db.Notices(ctx, project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "root_behind" && !strings.Contains(notice.DataJSON, `"commits_behind":2`) {
			t.Fatalf("root_behind Notice has the wrong count: %#v", notice)
		}
	}
}
