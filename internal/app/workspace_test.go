package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// workspaceFixture builds a folder shaped like a multi-repository stack: shared
// context files at the root, two members with an origin, one without, a hidden
// folder and a linked worktree that must not become members.
func workspaceFixture(t *testing.T, root string) string {
	t.Helper()
	workspace := filepath.Join(root, "stack")
	for _, name := range []string{"backend", "worker", "e2e-tool"} {
		initRepo(t, filepath.Join(workspace, name))
	}
	for _, name := range []string{"backend", "worker"} {
		remote := filepath.Join(root, "remotes", name+".git")
		if err := os.MkdirAll(remote, 0o700); err != nil {
			t.Fatal(err)
		}
		gitTest(t, remote, "init", "--bare", "-b", "main")
		gitTest(t, filepath.Join(workspace, name), "remote", "add", "origin", remote)
		gitTest(t, filepath.Join(workspace, name), "push", "-u", "origin", "main")
		gitTest(t, filepath.Join(workspace, name), "remote", "set-head", "origin", "main")
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".worktrees"), 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, filepath.Join(workspace, "backend"), "worktree", "add", "-b", "side", filepath.Join(workspace, "backend-side"))
	initRepo(t, filepath.Join(workspace, ".hidden"))
	if err := os.MkdirAll(filepath.Join(workspace, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{"CLAUDE.md": "how the repos fit together\n", "docs/arch.md": "arch\n", ".env": "TOKEN=1\n"} {
		if err := os.WriteFile(filepath.Join(workspace, path), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestDetectProjectFindsWorkspaceMembersOnly(t *testing.T) {
	workspace := workspaceFixture(t, t.TempDir())
	detected, err := detectProject(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if detected.Kind != store.ProjectKindWorkspace || detected.Root != workspace || detected.Name != "stack" {
		t.Fatalf("detected = %#v", detected)
	}
	var names []string
	for _, repo := range detected.Repos {
		names = append(names, repo.Name+":"+repo.DefaultBranch)
		if repo.Name == "e2e-tool" && repo.Remote != "" {
			t.Fatalf("e2e-tool has no origin but remote = %q", repo.Remote)
		}
		if repo.Name == "worker" && repo.Remote == "" {
			t.Fatal("worker origin was not detected")
		}
	}
	if got := strings.Join(names, ","); got != "backend:main,e2e-tool:main,worker:main" {
		t.Fatalf("members = %s", got)
	}
}

func TestDetectProjectInsideMemberSuggestsWorkspace(t *testing.T) {
	workspace := workspaceFixture(t, t.TempDir())
	detected, err := detectProject(context.Background(), filepath.Join(workspace, "worker"))
	if err != nil {
		t.Fatal(err)
	}
	if detected.Kind != store.ProjectKindRepo || detected.Name != "worker" || !strings.Contains(detected.Hint, workspace) {
		t.Fatalf("detected = %#v", detected)
	}
}

func TestDetectProjectRefusesFolderWithoutRepositories(t *testing.T) {
	_, err := detectProject(context.Background(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no Git repositories directly inside it") {
		t.Fatalf("err = %v", err)
	}
}

type upHarness struct {
	service *Service
	fake    *herdr.Fake
	home    string
	out     bytes.Buffer
}

func newUpHarness(t *testing.T, root, cwd string) *upHarness {
	t.Helper()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	t.Chdir(cwd)
	harness := &upHarness{fake: herdr.NewFake(), home: filepath.Join(root, "posse")}
	harness.fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Focused: true}}}
	harness.service = testService(harness.home, harness.fake)
	return harness
}

func (h *upHarness) run(args ...string) int {
	h.out.Reset()
	cli := h.service.CLI()
	cli.Out, cli.ErrOut = &h.out, &h.out
	return cli.Run(args)
}

func (h *upHarness) db(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(h.home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestTerminalConfirmRefusesPipedInput(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	previous := os.Stdin
	os.Stdin = reader
	defer func() { os.Stdin = previous }()
	var output bytes.Buffer
	yes, interactive, err := terminalConfirm(&output, "Register this workspace?")
	if err != nil || yes || interactive || output.Len() != 0 {
		t.Fatalf("piped confirmation = yes:%t interactive:%t output:%q err:%v", yes, interactive, output.String(), err)
	}
}

func TestUpRegistersWorkspaceAfterOneConfirmation(t *testing.T) {
	root := t.TempDir()
	workspace := workspaceFixture(t, root)
	harness := newUpHarness(t, root, workspace)
	var questions []string
	harness.service.confirm = func(out io.Writer, question string) (bool, bool, error) {
		questions = append(questions, question)
		return true, true, nil
	}
	if code := harness.run("up", "--claude"); code != 0 {
		t.Fatalf("up: %d\n%s", code, harness.out.String())
	}
	if len(questions) != 1 || !strings.Contains(questions[0], workspace) || !strings.Contains(questions[0], "workspace") {
		t.Fatalf("questions = %q", questions)
	}
	for _, want := range []string{"kind: workspace", "backend,main", "e2e-tool,main,none,local", "worker,main,local,pr", "lead: starting"} {
		if !strings.Contains(harness.out.String(), want) {
			t.Fatalf("up output lacks %q:\n%s", want, harness.out.String())
		}
	}
	db := harness.db(t)
	project, err := db.ProjectByRoot(context.Background(), workspace)
	if err != nil || !project.IsWorkspace() || project.Name != "stack" {
		t.Fatalf("project = %#v, %v", project, err)
	}
	repos, err := db.ProjectRepos(context.Background(), project.ID)
	if err != nil || len(repos) != 3 {
		t.Fatalf("repos = %#v, %v", repos, err)
	}
	if harness.service.pendingLead == nil || harness.service.pendingLead.ProjectID != project.ID {
		t.Fatalf("Lead was not scheduled: %#v", harness.service.pendingLead)
	}

	// A second `posse up` from inside a member resolves the same Project and asks nothing.
	t.Chdir(filepath.Join(workspace, "worker"))
	harness.fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Focused: true}}}
	if code := harness.run("project", "show"); code != 0 || !strings.Contains(harness.out.String(), "project: stack") {
		t.Fatalf("project show from a member: %d\n%s", code, harness.out.String())
	}
	if len(questions) != 1 {
		t.Fatalf("registered Project asked again: %q", questions)
	}
}

func TestUpRegistersSingleRepositoryAfterConfirmation(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "shop")
	initRepo(t, repo)
	harness := newUpHarness(t, root, repo)
	asked := 0
	harness.service.confirm = func(io.Writer, string) (bool, bool, error) { asked++; return true, true, nil }
	if code := harness.run("up", "--claude"); code != 0 {
		t.Fatalf("up: %d\n%s", code, harness.out.String())
	}
	if asked != 1 || !strings.Contains(harness.out.String(), "kind: repo") {
		t.Fatalf("asked=%d output:\n%s", asked, harness.out.String())
	}
	resolved, _ := filepath.EvalSymlinks(repo)
	project, err := harness.db(t).ProjectByRoot(context.Background(), resolved)
	if err != nil || project.IsWorkspace() || project.DefaultBranch != "main" {
		t.Fatalf("project = %#v, %v", project, err)
	}
}

func TestUpRefusesToRegisterWithoutConfirmation(t *testing.T) {
	for _, test := range []struct {
		name        string
		yes         bool
		interactive bool
		want        string
	}{
		{name: "declined", yes: false, interactive: true, want: "registration_declined"},
		{name: "no terminal", yes: false, interactive: false, want: "posse up --yes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			workspace := workspaceFixture(t, root)
			harness := newUpHarness(t, root, workspace)
			harness.service.confirm = func(io.Writer, string) (bool, bool, error) { return test.yes, test.interactive, nil }
			if code := harness.run("up", "--claude"); code == 0 || !strings.Contains(harness.out.String(), test.want) {
				t.Fatalf("up: %d\n%s", code, harness.out.String())
			}
			projects, err := harness.db(t).Projects(context.Background())
			if err != nil || len(projects) != 0 {
				t.Fatalf("projects = %#v, %v", projects, err)
			}
		})
	}
}

func TestUpYesRegistersWithoutAsking(t *testing.T) {
	root := t.TempDir()
	workspace := workspaceFixture(t, root)
	harness := newUpHarness(t, root, workspace)
	harness.service.confirm = func(io.Writer, string) (bool, bool, error) {
		t.Fatal("--yes must not ask")
		return false, false, nil
	}
	if code := harness.run("up", "--claude", "--yes"); code != 0 {
		t.Fatalf("up --yes: %d\n%s", code, harness.out.String())
	}
}

func TestProjectAddAndScanTrackWorkspaceMembers(t *testing.T) {
	root := t.TempDir()
	workspace := workspaceFixture(t, root)
	harness := newUpHarness(t, root, workspace)
	t.Setenv("HERDR_PANE_ID", "")
	if code := harness.run("project", "add"); code != 0 || !strings.Contains(harness.out.String(), "registered: true") {
		t.Fatalf("project add: %d\n%s", code, harness.out.String())
	}
	if err := os.RemoveAll(filepath.Join(workspace, "e2e-tool")); err != nil {
		t.Fatal(err)
	}
	initRepo(t, filepath.Join(workspace, "guest-image"))
	if code := harness.run("project", "scan"); code != 0 {
		t.Fatalf("project scan: %d\n%s", code, harness.out.String())
	}
	for _, want := range []string{"added[1]: guest-image", "missing[1]: e2e-tool", "e2e-tool,main,none,local,missing", "guest-image,main,none,local,active"} {
		if !strings.Contains(harness.out.String(), want) {
			t.Fatalf("scan output lacks %q:\n%s", want, harness.out.String())
		}
	}
	project, err := harness.db(t).ProjectByName(context.Background(), "stack")
	if err != nil {
		t.Fatal(err)
	}
	targets, err := harness.service.projectTargets(context.Background(), harness.db(t), project)
	if err != nil || len(targets) != 3 {
		t.Fatalf("targets = %#v, %v", targets, err)
	}
}
