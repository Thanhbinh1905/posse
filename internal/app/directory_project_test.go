package app

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func plainFolder(t *testing.T, parent string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(parent, "notes")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, contents := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func assertNoGit(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("%s gained Git metadata: %v", dir, err)
	}
}

func TestProjectAddRegistersPlainFolderAsDirectoryProject(t *testing.T) {
	parent := t.TempDir()
	dir := plainFolder(t, parent, map[string]string{"ideas.md": "plan\n"})
	harness := newUpHarness(t, parent, dir)
	if code := harness.run("project", "add"); code != 0 {
		t.Fatalf("project add: %d\n%s", code, harness.out.String())
	}
	for _, want := range []string{"kind: directory", "execution: none", "registered: true"} {
		if !strings.Contains(harness.out.String(), want) {
			t.Fatalf("project add output lacks %q:\n%s", want, harness.out.String())
		}
	}
	project, err := harness.db(t).ProjectByRoot(context.Background(), dir)
	if err != nil || !project.IsDirectory() || project.Name != "notes" {
		t.Fatalf("project = %#v, %v", project, err)
	}
	assertNoGit(t, dir)
}

func TestUpStartsDirectoryLeadInEmptyFolderWithoutGit(t *testing.T) {
	parent := t.TempDir()
	dir := plainFolder(t, parent, nil)
	harness := newUpHarness(t, parent, dir)
	var questions []string
	harness.service.confirm = func(_ io.Writer, question string) (bool, bool, error) {
		questions = append(questions, question)
		return true, true, nil
	}
	if code := harness.run("up", "--claude"); code != 0 {
		t.Fatalf("up: %d\n%s", code, harness.out.String())
	}
	if len(questions) != 1 || strings.Contains(questions[0], "Initialize") || !strings.Contains(questions[0], "directory") {
		t.Fatalf("questions = %q", questions)
	}
	for _, want := range []string{"kind: directory", "execution: none", "lead: starting"} {
		if !strings.Contains(harness.out.String(), want) {
			t.Fatalf("up output lacks %q:\n%s", want, harness.out.String())
		}
	}
	assertNoGit(t, dir)
	project, err := harness.db(t).ProjectByRoot(context.Background(), dir)
	if err != nil || !project.IsDirectory() {
		t.Fatalf("project = %#v, %v", project, err)
	}
	if harness.service.pendingLead == nil || harness.service.pendingLead.ProjectID != project.ID {
		t.Fatalf("Lead was not scheduled: %#v", harness.service.pendingLead)
	}
}

func startDirectoryLead(t *testing.T, files map[string]string) (*upHarness, string) {
	t.Helper()
	parent := t.TempDir()
	dir := plainFolder(t, parent, files)
	harness := newUpHarness(t, parent, dir)
	if code := harness.run("up", "--claude", "--yes"); code != 0 {
		t.Fatalf("up: %d\n%s", code, harness.out.String())
	}
	return harness, dir
}

func TestDirectoryProjectRefusesExecutionWithoutCreatingTasks(t *testing.T) {
	harness, dir := startDirectoryLead(t, map[string]string{"ideas.md": "plan\n"})
	brief := filepath.Join(t.TempDir(), "change.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Edit ideas\ndone_when: ideas.md is edited\n---\nEdit it.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inPlace := filepath.Join(t.TempDir(), "in-place.md")
	if err := os.WriteFile(inPlace, []byte("---\ntype: change\ntitle: Edit ideas\ndone_when: ideas.md is edited\nexecution: in-place\npaths: [ideas.md]\n---\nEdit it.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"start", "--brief", brief, "--name", "edit-ideas"},
		{"start", "--brief", brief, "--name", "edit-ideas", "--queue"},
		{"dispatch", "--brief", brief, "--name", "edit-ideas"},
		{"sync"},
	} {
		if code := harness.run(args...); code == 0 || !strings.Contains(harness.out.String(), "execution_unavailable") || !strings.Contains(harness.out.String(), "project scan") {
			t.Fatalf("%v was not refused as execution_unavailable: %d\n%s", args, code, harness.out.String())
		}
	}
	if code := harness.run("start", "--brief", inPlace, "--name", "edit-ideas", "--user-approved", "edit it in place"); code == 0 || !strings.Contains(harness.out.String(), "in-place execution was removed") {
		t.Fatalf("in-place start was not refused: %d\n%s", code, harness.out.String())
	}

	results := make(chan string, 4)
	for range 4 {
		go func() {
			service := testService(harness.home, harness.fake)
			var out strings.Builder
			cli := service.CLI()
			cli.Out, cli.ErrOut = &out, &out
			cli.Run([]string{"start", "--brief", brief, "--name", "edit-ideas"})
			results <- out.String()
		}()
	}
	for range 4 {
		if output := <-results; !strings.Contains(output, "execution_unavailable") {
			t.Fatalf("a concurrent start was not refused: %s", output)
		}
	}

	db := harness.db(t)
	project, err := db.ProjectByRoot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if tasks, err := db.Tasks(context.Background(), project.ID, true); err != nil || len(tasks) != 0 {
		t.Fatalf("refused execution left Tasks %#v, %v", tasks, err)
	}
	if sandboxes, err := db.Sandboxes(context.Background(), project.ID); err != nil || len(sandboxes) != 0 {
		t.Fatalf("refused execution left Sandboxes %#v, %v", sandboxes, err)
	}
	if leases, err := db.ResourceLeases(context.Background(), project.ID); err != nil || len(leases) != 0 {
		t.Fatalf("refused execution left resource leases %#v, %v", leases, err)
	}
	assertNoGit(t, dir)
}

func TestDirectoryProjectRefusesLegacyWorkerPublication(t *testing.T) {
	fixture := newWorkerFixture(t)
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE projects SET kind='directory' WHERE id=?`, fixture.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workerHomeEnv, fixture.home)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w2:p1")
	t.Chdir(fixture.sandbox)
	service := testService(fixture.home, nil)
	if code, output := runCLI(t, service, "publish", "Legacy publication"); code == 0 || !strings.Contains(output, "execution_unavailable") {
		t.Fatalf("Directory Worker publication: code=%d output=%s", code, output)
	}
}

func TestDirectoryProjectShowListsBoundedNestedRepositories(t *testing.T) {
	parent := t.TempDir()
	dir := plainFolder(t, parent, map[string]string{
		".gitignore":                "build/\n",
		"clients/vendor/.gitignore": "cache/\n",
		".env":                      "TOKEN=secret-value\n",
	})
	initRepo(t, filepath.Join(dir, "clients", "acme"))
	initRepo(t, filepath.Join(dir, "clients", "acme", "nested"))
	initRepo(t, filepath.Join(dir, "a", "b", "deep"))
	initRepo(t, filepath.Join(dir, "a", "b", "c", "too-deep"))
	initRepo(t, filepath.Join(dir, "build", "out", "ignored"))
	initRepo(t, filepath.Join(dir, "clients", "vendor", "cache"))
	initRepo(t, filepath.Join(dir, ".hidden", "x", "repo"))
	outside := filepath.Join(parent, "outside")
	initRepo(t, filepath.Join(outside, "linked"))
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(dir, "locked")
	initRepo(t, filepath.Join(locked, "x", "repo"))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	harness := newUpHarness(t, parent, dir)
	if code := harness.run("project", "add"); code != 0 {
		t.Fatalf("project add: %d\n%s", code, harness.out.String())
	}
	if code := harness.run("project", "show"); code != 0 {
		t.Fatalf("project show: %d\n%s", code, harness.out.String())
	}
	output := harness.out.String()
	for _, want := range []string{"execution: none", "depth_limit: 3", "a/b/deep", "clients/acme", "truncated: false", "skipped_unreadable: 1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("project show lacks %q:\n%s", want, output)
		}
	}
	for _, unwanted := range []string{"too-deep", "acme/nested", "build/out", "vendor/cache", ".hidden", "linked", "locked/x", "secret-value"} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("project show includes %q:\n%s", unwanted, output)
		}
	}
	assertNoGit(t, dir)
}

func TestDirectoryProjectShowStopsWhenIgnoreMatchingFails(t *testing.T) {
	for _, command := range []string{"init", "check-ignore"} {
		t.Run(command, func(t *testing.T) {
			parent := t.TempDir()
			dir := plainFolder(t, parent, map[string]string{".gitignore": "excluded/\n"})
			if err := os.MkdirAll(filepath.Join(dir, "excluded", "private-repo", ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			harness := newUpHarness(t, parent, dir)
			if code := harness.run("project", "add"); code != 0 {
				t.Fatalf("project add: %d\n%s", code, harness.out.String())
			}
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("POSSE_TEST_REAL_GIT", realGit)
			t.Setenv("POSSE_TEST_FAIL_GIT_COMMAND", command)
			wrapper := "#!/bin/sh\nfor arg do\n  if [ \"$arg\" = \"$POSSE_TEST_FAIL_GIT_COMMAND\" ]; then\n    exit 128\n  fi\ndone\nexec \"$POSSE_TEST_REAL_GIT\" \"$@\"\n"
			if err := os.WriteFile(filepath.Join(parent, "bin", "git"), []byte(wrapper), 0o700); err != nil {
				t.Fatal(err)
			}
			if code := harness.run("project", "show"); code != 0 {
				t.Fatalf("project show: %d\n%s", code, harness.out.String())
			}
			output := harness.out.String()
			for _, want := range []string{"repositories: []", "ignore_rules: unavailable", "truncated: true"} {
				if !strings.Contains(output, want) {
					t.Fatalf("discovery continued after %s failed; lacks %q:\n%s", command, want, output)
				}
			}
			if strings.Contains(output, "private-repo") {
				t.Fatalf("discovery exposed an excluded repository:\n%s", output)
			}
			assertNoGit(t, dir)
		})
	}
}

func TestDirectoryProjectShowRespectsIgnorePathBytes(t *testing.T) {
	for _, name := range []string{"ignored日本語", "ignored space ", "ignored\ttab", "ignored\nline", "ignored\"quote", " ignored-leading"} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir := plainFolder(t, parent, map[string]string{".gitignore": "*ignored*/\n"})
			for _, path := range []string{filepath.Join(name, "hidden-repo", ".git"), filepath.Join("kept\nline", "visible-repo", ".git")} {
				if err := os.MkdirAll(filepath.Join(dir, path), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			harness := newUpHarness(t, parent, dir)
			if code := harness.run("project", "add"); code != 0 {
				t.Fatalf("project add: %d\n%s", code, harness.out.String())
			}
			if code := harness.run("project", "show"); code != 0 {
				t.Fatalf("project show: %d\n%s", code, harness.out.String())
			}
			if output := harness.out.String(); strings.Contains(output, "hidden-repo") || !strings.Contains(output, "visible-repo") {
				t.Fatalf("discovery corrupted ignore path bytes:\n%s", output)
			}
			found := discoverNestedRepositories(context.Background(), dir)
			if len(found.Repositories) != 1 || found.Repositories[0] != "kept\nline/visible-repo" {
				t.Fatalf("discovery paths = %q", found.Repositories)
			}
			assertNoGit(t, dir)
		})
	}
}

func TestDirectoryProjectShowStopsAtGlobalEntryBudget(t *testing.T) {
	parent := t.TempDir()
	dir := plainFolder(t, parent, nil)
	wide := filepath.Join(dir, "a-wide")
	if err := os.Mkdir(wide, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := range maxDiscoveryEntries + 1 {
		if err := os.WriteFile(filepath.Join(wide, strconv.Itoa(index)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	locked := filepath.Join(dir, "z-locked")
	if err := os.Mkdir(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	harness := newUpHarness(t, parent, dir)
	if code := harness.run("project", "add"); code != 0 {
		t.Fatalf("project add: %d\n%s", code, harness.out.String())
	}
	if code := harness.run("project", "show"); code != 0 {
		t.Fatalf("project show: %d\n%s", code, harness.out.String())
	}
	for _, want := range []string{"truncated: true", "skipped_unreadable: 0"} {
		if !strings.Contains(harness.out.String(), want) {
			t.Fatalf("discovery visited a parent after exhausting its budget; lacks %q:\n%s", want, harness.out.String())
		}
	}
}

func TestNestedRepositoryDiscoveryReportsTruncation(t *testing.T) {
	dir := plainFolder(t, t.TempDir(), nil)
	for index := range maxNestedRepositories + 1 {
		if err := os.MkdirAll(filepath.Join(dir, "group", "repo-"+strconv.Itoa(index), ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	found := discoverNestedRepositories(context.Background(), dir)
	if !found.Truncated || len(found.Repositories) != maxNestedRepositories {
		t.Fatalf("discovery = truncated %t with %d repositories", found.Truncated, len(found.Repositories))
	}
}

func TestDirectoryLeadRestartKeepsIdentityAndStatesCapabilities(t *testing.T) {
	harness, dir := startDirectoryLead(t, map[string]string{"ideas.md": "plan\n"})
	db := harness.db(t)
	first, err := db.ProjectByRoot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	harness.service.confirm = func(io.Writer, string) (bool, bool, error) {
		t.Fatal("a registered Directory Project asked to register again")
		return false, false, nil
	}
	if code := harness.run("up", "--claude"); code != 0 || !strings.Contains(harness.out.String(), "execution: none") {
		t.Fatalf("restart up: %d\n%s", code, harness.out.String())
	}
	t.Chdir(filepath.Join(dir))
	if code := harness.run("lead"); code != 0 {
		t.Fatalf("lead: %d\n%s", code, harness.out.String())
	}
	for _, want := range []string{"execution: none", "Directory guidance", "admit no Tasks", "never print secret files", "Posse does not enforce on the Lead shell", "posse project scan", "exploration and planning only"} {
		if !strings.Contains(harness.out.String(), want) {
			t.Fatalf("lead output lacks %q:\n%s", want, harness.out.String())
		}
	}
	if strings.Contains(harness.out.String(), "use `posse start`") {
		t.Fatalf("Directory Lead help points at posse start:\n%s", harness.out.String())
	}
	projects, err := db.Projects(context.Background())
	if err != nil || len(projects) != 1 || projects[0].ID != first.ID || projects[0].UUID != first.UUID || !projects[0].IsDirectory() {
		t.Fatalf("projects after restart = %#v, %v", projects, err)
	}
	assertNoGit(t, dir)
}

func TestProjectScanConvertsDirectoryProjectInPlace(t *testing.T) {
	for _, shape := range []string{"repo", "workspace"} {
		t.Run(shape, func(t *testing.T) {
			harness, dir := startDirectoryLead(t, map[string]string{"ideas.md": "plan\n"})
			db := harness.db(t)
			before, err := db.ProjectByRoot(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			if code := harness.run("project", "scan"); code != 0 || !strings.Contains(harness.out.String(), "kind: directory") || !strings.Contains(harness.out.String(), "converted: false") {
				t.Fatalf("scan of an unchanged folder: %d\n%s", code, harness.out.String())
			}
			if shape == "repo" {
				gitTest(t, dir, "init", "-b", "main")
			} else {
				initRepo(t, filepath.Join(dir, "api"))
				initRepo(t, filepath.Join(dir, "web"))
			}
			if code := harness.run("project", "scan"); code != 0 || !strings.Contains(harness.out.String(), "kind: "+shape) || !strings.Contains(harness.out.String(), "converted: true") || !strings.Contains(harness.out.String(), "execution: git-sandbox") {
				t.Fatalf("scan after the folder gained Git: %d\n%s", code, harness.out.String())
			}
			after, err := db.ProjectByRoot(context.Background(), dir)
			if err != nil || after.ID != before.ID || after.UUID != before.UUID || after.Name != before.Name || after.Kind != shape {
				t.Fatalf("converted project = %#v (before %#v), %v", after, before, err)
			}
			if shape == "repo" && after.DefaultBranch != "main" {
				t.Fatalf("converted repo default branch = %q", after.DefaultBranch)
			}
			if shape == "workspace" {
				repos, err := db.ProjectRepos(context.Background(), after.ID)
				if err != nil || len(repos) != 2 {
					t.Fatalf("converted workspace members = %#v, %v", repos, err)
				}
			}
			if projects, err := db.Projects(context.Background()); err != nil || len(projects) != 1 {
				t.Fatalf("conversion registered another Project: %#v, %v", projects, err)
			}
		})
	}
}

func TestProjectScanKeepsDirectoryProjectWithUnfinishedLegacyTasks(t *testing.T) {
	harness, dir := startDirectoryLead(t, map[string]string{"ideas.md": "plan\n"})
	db := harness.db(t)
	project, err := db.ProjectByRoot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 1, Type: "change", Title: "legacy in-place edit", State: store.StateSpawning, WorktreePath: dir}); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "init", "-b", "main")
	if code := harness.run("project", "scan"); code == 0 || !strings.Contains(harness.out.String(), "project_scan_blocked") || !strings.Contains(harness.out.String(), "t1") {
		t.Fatalf("scan with an unfinished legacy Task: %d\n%s", code, harness.out.String())
	}
	if after, err := db.ProjectByID(context.Background(), project.ID); err != nil || !after.IsDirectory() {
		t.Fatalf("project after a refused conversion = %#v, %v", after, err)
	}
}

func TestMixedFolderStartsOneWorkspaceLeadWithNestedContext(t *testing.T) {
	parent := t.TempDir()
	dir := plainFolder(t, parent, map[string]string{"notes/plan.md": "plan\n"})
	initRepo(t, filepath.Join(dir, "api"))
	initRepo(t, filepath.Join(dir, "web"))
	initRepo(t, filepath.Join(dir, "tools", "lint"))
	harness := newUpHarness(t, parent, dir)
	if code := harness.run("up", "--claude", "--yes"); code != 0 || !strings.Contains(harness.out.String(), "kind: workspace") || !strings.Contains(harness.out.String(), "execution: git-sandbox") {
		t.Fatalf("up in a mixed folder: %d\n%s", code, harness.out.String())
	}
	db := harness.db(t)
	project, err := db.ProjectByRoot(context.Background(), dir)
	if err != nil || !project.IsWorkspace() {
		t.Fatalf("project = %#v, %v", project, err)
	}
	repos, err := db.ProjectRepos(context.Background(), project.ID)
	if err != nil || len(repos) != 2 || repos[0].Name != "api" || repos[1].Name != "web" {
		t.Fatalf("members = %#v, %v", repos, err)
	}
	if code := harness.run("project", "show"); code != 0 || !strings.Contains(harness.out.String(), "repositories[1]: tools/lint") {
		t.Fatalf("project show lacks the nested repository context: %d\n%s", code, harness.out.String())
	}
	t.Chdir(filepath.Join(dir, "tools", "lint"))
	if code := harness.run("up", "--claude"); code != 0 {
		t.Fatalf("up inside a nested repository: %d\n%s", code, harness.out.String())
	}
	if projects, err := db.Projects(context.Background()); err != nil || len(projects) != 1 {
		t.Fatalf("up inside a nested repository registered another Project: %#v, %v", projects, err)
	}
}

func TestRecoverRebuildKeepsConvertedAndDirectoryProjects(t *testing.T) {
	parent := t.TempDir()
	dir := plainFolder(t, parent, nil)
	converted := filepath.Join(parent, "later")
	if err := os.MkdirAll(converted, 0o700); err != nil {
		t.Fatal(err)
	}
	harness := newUpHarness(t, parent, dir)
	if code := harness.run("project", "add"); code != 0 {
		t.Fatalf("project add: %d\n%s", code, harness.out.String())
	}
	t.Chdir(converted)
	if code := harness.run("project", "add"); code != 0 {
		t.Fatalf("project add: %d\n%s", code, harness.out.String())
	}
	gitTest(t, converted, "init", "-b", "main")
	if code := harness.run("project", "scan"); code != 0 {
		t.Fatalf("project scan: %d\n%s", code, harness.out.String())
	}
	db, err := store.Open(harness.home)
	if err != nil {
		t.Fatal(err)
	}
	before, err := db.Projects(context.Background())
	if closeErr := db.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	t.Setenv("HERDR_PANE_ID", "")
	if code := harness.run("recover", "--rebuild"); code != 0 {
		t.Fatalf("recover --rebuild: %d\n%s", code, harness.out.String())
	}
	after, err := harness.db(t).Projects(context.Background())
	if err != nil || len(after) != len(before) {
		t.Fatalf("rebuilt projects = %#v, %v", after, err)
	}
	for index := range before {
		if after[index].ID != before[index].ID || after[index].UUID != before[index].UUID || after[index].Kind != before[index].Kind || after[index].DefaultBranch != before[index].DefaultBranch {
			t.Fatalf("rebuilt project %#v, want %#v", after[index], before[index])
		}
	}
	kinds := map[string]string{}
	for _, project := range after {
		kinds[project.Name] = project.Kind
	}
	if kinds["notes"] != store.ProjectKindDirectory || kinds["later"] != store.ProjectKindRepo {
		t.Fatalf("rebuilt kinds = %v", kinds)
	}
}
