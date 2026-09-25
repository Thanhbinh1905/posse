package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type workspaceLandFixture struct {
	t         *testing.T
	root      string
	workspace string
	home      string
	db        *store.DB
	service   *Service
	project   store.Project
	task      store.Task
	out       bytes.Buffer
}

// newWorkspaceLandFixture registers the workspace fixture, gives Task t1 a real
// workspace Mount for the named members, commits in the changed ones and moves
// the Task to done.
func newWorkspaceLandFixture(t *testing.T, config string, repos []string, changed []string) *workspaceLandFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &workspaceLandFixture{t: t, root: root, workspace: workspaceFixture(t, root), home: filepath.Join(root, "posse")}
	if err := os.MkdirAll(fixture.home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fixture.db = db
	ctx := context.Background()
	detected, err := detectProject(ctx, fixture.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.project, err = registerDetected(ctx, db, detected); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, fixture.project.ID, "w1", "w1:p1", "posse:stack:lead"); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:stack:lead", Agent: "claude", AgentStatus: "idle"}}}
	fixture.service = testService(fixture.home, fake)
	cfg := fixture.config()
	members, _, err := fixture.service.planTaskMembers(ctx, db, fixture.project, cfg, dispatch.Brief{Type: "ship", Repos: repos}, nil)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _, err := createTaskWithSequence(ctx, db, fixture.project, fixture.home, store.Task{Type: "ship", Title: "Span members", ShortName: "span-members", LandingMode: "local", AutonomyLand: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, fixture.project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.acquireWorkspaceMount(ctx, db, fixture.project, task, members, fixture.home, "warm", nil); err != nil {
		t.Fatal(err)
	}
	if fixture.task, err = db.TaskByID(ctx, fixture.project.ID, taskID); err != nil {
		t.Fatal(err)
	}
	for _, name := range changed {
		worktree := filepath.Join(fixture.task.WorktreePath, name)
		if err := os.WriteFile(filepath.Join(worktree, "change.txt"), []byte(name+" change\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, worktree, "add", "change.txt")
		gitTest(t, worktree, "commit", "-m", "change "+name)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkspaceShipSignal(ctx, db, fixture.task); err != nil {
		t.Fatalf("done Signal refused: %v", err)
	}
	if _, err := db.RecordWorkerSignal(ctx, fixture.task, "done", "changed "+strings.Join(changed, " and "), map[string]string{}, "task_done"); err != nil {
		t.Fatal(err)
	}
	briefPath := filepath.Join(fixture.home, "projects", "stack", "tasks", "t1", "brief.md")
	if err := writeFile(briefPath, []byte("---\ntype: ship\ntitle: Span members\ndone_when: members changed\nrepos: ["+strings.Join(repos, ", ")+"]\n---\nChange the members.\n")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(fixture.workspace)
	return fixture
}

func (f *workspaceLandFixture) config() config.Config {
	cfg, err := config.Load(f.home, "stack")
	if err != nil {
		f.t.Fatal(err)
	}
	return cfg
}

func (f *workspaceLandFixture) run(args ...string) int {
	f.out.Reset()
	cli := f.service.CLI()
	cli.Out, cli.ErrOut = &f.out, &f.out
	return cli.Run(args)
}

func (f *workspaceLandFixture) repos() map[string]store.TaskRepo {
	repos, err := f.db.TaskRepos(context.Background(), f.task.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	byName := map[string]store.TaskRepo{}
	for _, repo := range repos {
		byName[repo.Repo] = repo
	}
	return byName
}

func (f *workspaceLandFixture) state() store.State {
	task, err := f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return task.State
}

const localWorkspaceConfig = "[defaults]\nlanding_mode = \"local\"\nauto_unsaddle = \"never\"\n"

func TestWorkspaceMountMirrorsSharedFilesAndRequestedMembers(t *testing.T) {
	f := newWorkspaceLandFixture(t, localWorkspaceConfig, []string{"backend", "worker"}, []string{"worker"})
	mount := f.task.WorktreePath
	for _, path := range []string{"CLAUDE.md", "docs/arch.md", ".env"} {
		if _, err := os.Stat(filepath.Join(mount, path)); err != nil {
			t.Fatalf("shared file %s was not copied: %v", path, err)
		}
	}
	for _, path := range []string{"e2e-tool", "backend-side", ".hidden"} {
		if _, err := os.Stat(filepath.Join(mount, path)); !os.IsNotExist(err) {
			t.Fatalf("%s must not be in the Mount: %v", path, err)
		}
	}
	for _, member := range []string{"backend", "worker"} {
		if branch := strings.TrimSpace(gitTest(t, filepath.Join(mount, member), "branch", "--show-current")); branch != "posse/span-members" {
			t.Fatalf("%s is on %q", member, branch)
		}
	}
	// A Worker's edit to a shared copy never reaches the workspace root.
	if err := os.WriteFile(filepath.Join(mount, "CLAUDE.md"), []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if contents, _ := os.ReadFile(filepath.Join(f.workspace, "CLAUDE.md")); string(contents) != "how the repos fit together\n" {
		t.Fatalf("workspace CLAUDE.md changed: %q", contents)
	}
}

func TestWorkspaceShipSignalRefusesChangesOutsideRequestedMembers(t *testing.T) {
	f := newWorkspaceLandFixture(t, localWorkspaceConfig, []string{"worker"}, []string{"worker"})
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(f.task.WorktreePath, "worker", "stray.txt"), []byte("stray\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkspaceShipSignal(ctx, f.db, f.task); err == nil || !strings.Contains(err.Error(), "worker: worktree has uncommitted changes") {
		t.Fatalf("err = %v", err)
	}
}

func TestWorkspaceLocalLandingMergesEveryChangedMember(t *testing.T) {
	f := newWorkspaceLandFixture(t, localWorkspaceConfig, []string{"backend", "worker", "e2e-tool"}, []string{"backend", "e2e-tool"})
	if code := f.run("land", "t1"); code != 0 {
		t.Fatalf("land: %d\n%s", code, f.out.String())
	}
	repos := f.repos()
	if repos["backend"].State != store.TaskRepoGated || repos["e2e-tool"].State != store.TaskRepoGated || repos["worker"].State != store.TaskRepoUnchanged {
		t.Fatalf("members after land = %#v", repos)
	}
	if f.state() != store.StateLanding {
		t.Fatalf("state = %s", f.state())
	}
	if code := f.run("land", "t1", "--merge"); code == 0 || !strings.Contains(f.out.String(), "land_approval_required") {
		t.Fatalf("merge without approval: %d\n%s", code, f.out.String())
	}
	if code := f.run("land", "t1", "--merge", "--user-approved", "merge both"); code != 0 {
		t.Fatalf("land --merge: %d\n%s", code, f.out.String())
	}
	if f.state() != store.StateLanded {
		t.Fatalf("state = %s\n%s", f.state(), f.out.String())
	}
	for _, member := range []string{"backend", "e2e-tool"} {
		log := gitTest(t, filepath.Join(f.workspace, member), "log", "--format=%s", "-1", "refs/heads/main")
		if strings.TrimSpace(log) != "change "+member {
			t.Fatalf("%s main = %q", member, log)
		}
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, notice := range notices {
		found = found || (notice.Kind == "land_ready" && strings.Contains(notice.Summary, "backend, e2e-tool"))
	}
	if !found {
		t.Fatalf("land_ready does not name the members: %#v", notices)
	}
	if code := f.run("unsaddle", "t1"); code != 0 || !strings.Contains(f.out.String(), "branch_removed: true") {
		t.Fatalf("unsaddle: %d\n%s", code, f.out.String())
	}
	for _, member := range []string{"backend", "worker", "e2e-tool"} {
		if branches := gitTest(t, filepath.Join(f.workspace, member), "branch", "--list", "posse/t1"); strings.TrimSpace(branches) != "" {
			t.Fatalf("%s kept posse/t1", member)
		}
	}
	if _, err := os.Stat(filepath.Join(f.task.WorktreePath, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatalf("released Mount kept shared copies: %v", err)
	}
}

func TestWorkspaceMergeRefusesMovedMemberBranch(t *testing.T) {
	f := newWorkspaceLandFixture(t, localWorkspaceConfig, []string{"backend", "worker"}, []string{"backend", "worker"})
	if code := f.run("land", "t1"); code != 0 {
		t.Fatalf("land: %d\n%s", code, f.out.String())
	}
	worker := filepath.Join(f.task.WorktreePath, "worker")
	if err := os.WriteFile(filepath.Join(worker, "late.txt"), []byte("late\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, worker, "add", "late.txt")
	gitTest(t, worker, "commit", "-m", "late")
	if code := f.run("land", "t1", "--merge", "--user-approved", "go"); code == 0 || !strings.Contains(f.out.String(), "worker: Task branch moved") {
		t.Fatalf("merge after move: %d\n%s", code, f.out.String())
	}
	if f.state() != store.StateDone || f.repos()["worker"].State != store.TaskRepoOpen {
		t.Fatalf("state = %s repos = %#v", f.state(), f.repos())
	}
}

func TestWorkspacePullRequestMemberLandsWhenMerged(t *testing.T) {
	f := newWorkspaceLandFixture(t, "[defaults]\nlanding_mode = \"pr\"\nauto_unsaddle = \"never\"\n", []string{"worker", "e2e-tool"}, []string{"worker", "e2e-tool"})
	bin := filepath.Join(f.root, "bin")
	state := filepath.Join(f.root, "gh-view.json")
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$POSSE_TEST_GH_LOG\"\ncase \"$1 $2\" in\n" +
		"  \"pr list\") printf '[]\\n' ;;\n" +
		"  \"pr create\") printf 'https://github.com/acme/worker/pull/7\\n' ;;\n" +
		"  \"pr view\") cat \"$POSSE_TEST_GH_VIEW\" ;;\n" +
		"  \"pr merge\") printf 'Merged\\n' ;;\n" +
		"  *) exit 90 ;;\nesac\n"
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("POSSE_TEST_GH_LOG", filepath.Join(f.root, "gh.log"))
	t.Setenv("POSSE_TEST_GH_VIEW", state)
	worker := filepath.Join(f.workspace, "worker")
	gitTest(t, worker, "remote", "set-url", "origin", "https://github.com/acme/worker.git")
	gitTest(t, worker, "config", "remote.origin.pushurl", filepath.Join(f.root, "remotes", "worker.git"))
	head := strings.TrimSpace(gitTest(t, filepath.Join(f.task.WorktreePath, "worker"), "rev-parse", "HEAD"))
	writeView := func(prState, mergeCommit string) {
		view := map[string]any{"url": "https://github.com/acme/worker/pull/7", "state": prState, "headRefOid": head, "mergeable": "MERGEABLE", "reviewDecision": "APPROVED", "statusCheckRollup": []any{map[string]any{"__typename": "CheckRun", "name": "unit", "status": "COMPLETED", "conclusion": "SUCCESS"}}}
		if mergeCommit != "" {
			view["mergeCommit"] = map[string]any{"oid": mergeCommit}
		}
		encoded, _ := json.Marshal(view)
		if err := os.WriteFile(state, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeView("OPEN", "")
	if code := f.run("land", "t1"); code != 0 {
		t.Fatalf("land: %d\n%s", code, f.out.String())
	}
	repos := f.repos()
	if repos["worker"].State != store.TaskRepoLanding || repos["worker"].PRURL != "https://github.com/acme/worker/pull/7" || repos["e2e-tool"].State != store.TaskRepoGated {
		t.Fatalf("members = %#v\n%s", repos, f.out.String())
	}
	if code := f.run("land", "t1", "--merge", "--user-approved", "ship it"); code != 0 {
		t.Fatalf("land --merge: %d\n%s", code, f.out.String())
	}
	if f.state() != store.StateLanding || f.repos()["e2e-tool"].State != store.TaskRepoLanded {
		t.Fatalf("after merge state = %s repos = %#v", f.state(), f.repos())
	}
	writeView("MERGED", head)
	ctx := context.Background()
	if err := f.service.pollWorkspacePullRequests(ctx, f.db, f.project, f.config(), true); err != nil {
		t.Fatal(err)
	}
	if f.state() != store.StateLanded || f.repos()["worker"].LandedRef != head {
		t.Fatalf("after PR merge state = %s repos = %#v", f.state(), f.repos())
	}
	notices, err := f.db.Notices(ctx, f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, notice := range notices {
		if strings.HasPrefix(notice.Kind, "pr_") {
			kinds = append(kinds, notice.Kind+":"+notice.Summary)
		}
	}
	joined := strings.Join(kinds, "|")
	if !strings.Contains(joined, "pr_opened:worker: Span members") || !strings.Contains(joined, "pr_merged:worker: Span members") {
		t.Fatalf("PR Notices do not name the member: %s", joined)
	}
}

func TestWorkspaceBriefsNameKnownMembersAndReviewsInheritThem(t *testing.T) {
	f := newWorkspaceLandFixture(t, localWorkspaceConfig, []string{"backend", "worker"}, []string{"worker"})
	ctx := context.Background()
	cfg := f.config()
	if _, _, err := f.service.planTaskMembers(ctx, f.db, f.project, cfg, dispatch.Brief{Type: "ship"}, nil); err == nil || !strings.Contains(err.Error(), "must list the members") {
		t.Fatalf("ship without repos: %v", err)
	}
	if _, _, err := f.service.planTaskMembers(ctx, f.db, f.project, cfg, dispatch.Brief{Type: "ship", Repos: []string{"nova"}}, nil); err == nil || !strings.Contains(err.Error(), "nova is not a member") {
		t.Fatalf("unknown member: %v", err)
	}
	if _, _, err := f.service.planTaskMembers(ctx, f.db, f.project, cfg, dispatch.Brief{Type: "ship", Repos: []string{"e2e-tool"}, LandingMode: "pr"}, nil); err == nil || !strings.Contains(err.Error(), "has no origin") {
		t.Fatalf("pr on a member without origin: %v", err)
	}
	members, _, err := f.service.planTaskMembers(ctx, f.db, f.project, cfg, dispatch.Brief{Type: "review"}, &f.task)
	if err != nil || len(members) != 2 || members[0].BaseRef != "posse/span-members" {
		t.Fatalf("review members = %#v, %v", members, err)
	}
	if _, _, err := f.service.planTaskMembers(ctx, f.db, f.project, cfg, dispatch.Brief{Type: "review", Repos: []string{"e2e-tool"}}, &f.task); err == nil {
		t.Fatal("a review may not name a member the reviewed Task does not touch")
	}
}
