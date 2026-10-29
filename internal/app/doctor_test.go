package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestDoctorReportsPosseOwnedDiskAndArtifactRetention(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "scratch", "shop", "t1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "scratch", "shop", "t1", "fixture"), []byte("1234567"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[retention]\ntask_artifacts = \"48h\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	result, err := service.collectDoctorChecks(&axi.Context{Context: context.Background()})
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range result.Checks {
		if check.Name == "Posse-owned disk usage" {
			if check.Status != "ok" || !strings.Contains(check.Detail, "bytes under "+home) || !strings.Contains(check.Detail, "retention.task_artifacts=48h") {
				t.Fatalf("disk usage check = %#v", check)
			}
			return
		}
	}
	t.Fatalf("doctor omitted Posse-owned disk usage: %#v", result.Checks)
}

func TestDoctorReportsProjectDiskBreakdown(t *testing.T) {
	home := t.TempDir()
	project := store.Project{Name: "shop"}
	for _, relative := range []string{"scratch/shop/t1", "pool/shop/sandbox-1", "projects/shop/tasks/t1"} {
		path := filepath.Join(home, relative, "data")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("1234"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	detail, err := projectDiskUsage(home, project)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"12 bytes total", "scratch=4 bytes", "Pool=4 bytes", "Project data=4 bytes"} {
		if !strings.Contains(detail, expected) {
			t.Fatalf("Project disk usage %q does not contain %q", detail, expected)
		}
	}
}

func TestDoctorWarnsAboutLargeSharedGoCacheWithoutDeleting(t *testing.T) {
	cache := t.TempDir()
	contents := filepath.Join(cache, "large-cache-entry")
	file, err := os.Create(contents)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(largeSharedGoCacheBytes); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOCACHE", cache)
	status, detail, action := sharedGoCacheDiagnostic(context.Background())
	if status != "warn" || !strings.Contains(detail, cache) || !strings.Contains(action, "go clean -cache") {
		t.Fatalf("shared cache diagnostic = %q, %q, %q", status, detail, action)
	}
	info, err := os.Stat(contents)
	if err != nil || info.Size() != largeSharedGoCacheBytes {
		t.Fatalf("doctor removed or changed the shared cache fixture: %v, %v", info, err)
	}
}

func TestDoctorShowsDispatchInheritanceAndDisabledKinds(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\ndispatch_inheritance = \"lead_profile\"\n[kinds.pi]\ndisabled = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := testService(home, nil).collectDoctorChecks(&axi.Context{Context: context.Background()})
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range result.Checks {
		if check.Name == "config" {
			if !strings.Contains(check.Detail, "implicit dispatch=lead_profile") || !strings.Contains(check.Detail, "disabled dispatch kinds=pi") {
				t.Fatalf("config doctor detail = %#v", check)
			}
			return
		}
	}
	t.Fatalf("doctor omitted config check: %#v", result.Checks)
}

func TestExecutionResourceSummarySeparatesActiveNativeAndParkedCounts(t *testing.T) {
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	// The doctor skips Project roots under os.TempDir, so keep TMPDIR off the fixture root.
	fixtureRoot := t.TempDir()
	projectRoot := filepath.Join(fixtureRoot, "shop")
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(fixtureRoot, "tmp"))
	project, err := db.CreateProject(ctx, "shop", projectRoot, "main")
	if err != nil {
		t.Fatal(err)
	}
	createTask := func(name string) (int64, error) {
		id, _, err := db.CreateTaskWithSequence(ctx, project.ID, "shop", store.Task{Type: "change", Title: name, ShortName: name, Branch: "posse/" + name}, func(int) (bool, error) { return false, nil })
		return id, err
	}
	activeID, err := createTask("active")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireSandbox(ctx, project.ID, activeID, filepath.Join(home, "pool", "shop")); err != nil {
		t.Fatal(err)
	}
	parkedID, err := createTask("parked")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, parkedID, store.StateSpawning, store.StateWorking, "cli", "fixture start"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, parkedID, store.StateWorking, store.StateFailed, "worker", "fixture failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireSandbox(ctx, project.ID, parkedID, filepath.Join(home, "pool", "shop")); err != nil {
		t.Fatal(err)
	}
	got, err := executionResourceSummary(ctx, db, project.ID, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"active_workers=1/4", "native_children=0", "parked_resources=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("resource summary %q omitted %q", got, want)
		}
	}
	result, err := testService(home, nil).collectDoctorChecks(&axi.Context{Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range result.Checks {
		if check.Name == "execution resources shop" {
			if !strings.Contains(check.Detail, "active_workers=1/4") || !strings.Contains(check.Detail, "native_children=0") || !strings.Contains(check.Detail, "parked_resources=1") {
				t.Fatalf("doctor execution resource detail = %#v", check)
			}
			return
		}
	}
	t.Fatalf("doctor omitted execution resource counts: %#v", result.Checks)
}

func TestDoctorNamesNoMistakesConfigReplacements(t *testing.T) {
	root := t.TempDir()
	shopRoot := filepath.Join(root, "shop")
	initRepo(t, shopRoot)
	workspaceRoot := filepath.Join(root, "stack")
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProject(context.Background(), "shop", shopRoot, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateWorkspaceProject(context.Background(), "stack", workspaceRoot, []store.ProjectRepo{{
		Name: "api", Path: "api", DefaultBranch: "main", Status: store.RepoActive, OriginHost: "github.com",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	configBefore := map[string][]byte{}
	for project, text := range map[string]string{
		"shop":  "[defaults]\nmerge_mode = \"no-mistakes\"\n",
		"stack": "[defaults]\nmerge_mode = \"pr\"\n[repositories.api]\nreview = \"no-mistakes\"\n",
	} {
		path := filepath.Join(home, "projects", project, "config.toml")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		configBefore[path] = []byte(text)
	}
	service := testService(home, nil)
	output := &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, output
	cli.Run([]string{"doctor", "--json"})
	var result struct {
		Checks []struct{ Check, Status, Detail string }
		Help   []struct{ Check, Action string }
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode doctor: %v %s", err, output.String())
	}
	checks := map[string]struct{ Status, Detail, Action string }{}
	for _, check := range result.Checks {
		checks[check.Check] = struct{ Status, Detail, Action string }{check.Status, check.Detail, ""}
	}
	for _, help := range result.Help {
		check := checks[help.Check]
		check.Action = help.Action
		checks[help.Check] = check
	}
	if _, duplicated := checks["agent configuration"]; duplicated {
		t.Fatalf("doctor reported the removed value twice: %s", output.String())
	}
	shopPath := filepath.Join(home, "projects", "shop", "config.toml")
	shop := checks["config shop"]
	if shop.Status != "fail" || !strings.Contains(shop.Detail, shopPath+": defaults.merge_mode: no-mistakes was removed; use pr") ||
		!strings.Contains(shop.Action, "`posse config set defaults.merge_mode pr --project shop`") || !strings.Contains(shop.Action, "commands.test and commands.lint") {
		t.Fatalf("shop config check = %#v\n%s", shop, output.String())
	}
	stackPath := filepath.Join(home, "projects", "stack", "config.toml")
	stack := checks["config stack"]
	if stack.Status != "fail" || !strings.Contains(stack.Detail, stackPath+": repositories.api.review: no-mistakes was removed; use full-pipeline (quick or none are lighter choices)") ||
		!strings.Contains(stack.Action, "`posse config set repositories.api.review full-pipeline --project stack`") ||
		!strings.Contains(stack.Action, "`posse config set repositories.api.review quick --project stack`") || !strings.Contains(stack.Action, "`posse config set repositories.api.review none --project stack`") {
		t.Fatalf("stack config check = %#v\n%s", stack, output.String())
	}
	if strings.Contains(output.String(), "no_mistakes_migration") {
		t.Fatalf("doctor still prints the removed migration list: %s", output.String())
	}
	for path, before := range configBefore {
		if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
			t.Fatalf("doctor changed %s: %v\n%s", path, err, after)
		}
	}
}

func TestDoctorRecommendsRemovingProjectsWithStaleRoots(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "posse-home")
	poolRoot := filepath.Join(home, "pool", "shop", "sandbox-2")
	tempRoot := filepath.Join(root, "e2erepo")
	missingRoot := filepath.Join(root, "leadrepo")
	for _, dir := range []string{poolRoot, tempRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range []struct {
		name, root string
	}{
		{"sandbox-2", poolRoot},
		{"e2erepo", tempRoot},
		{"leadrepo", missingRoot},
	} {
		if _, err := db.CreateProject(context.Background(), project.name, project.root, "main"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	service := testService(home, nil)
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"doctor", "--json"}); code != 0 {
		t.Fatalf("doctor: %d %s", code, output.String())
	}
	var result struct {
		Checks []map[string]string `json:"checks"`
		Help   []map[string]string `json:"help"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("doctor did not return JSON: %s: %v", output.String(), err)
	}
	wantDetails := map[string]string{
		"sandbox-2": "inside a Pool Sandbox",
		"e2erepo":   "under a temporary directory",
		"leadrepo":  "is missing",
	}
	for name, detail := range wantDetails {
		checkName := "project " + name
		found := false
		for _, check := range result.Checks {
			if check["check"] == checkName {
				found = check["status"] == "warn" && strings.Contains(check["detail"], detail)
			}
			if check["check"] == "forge "+name {
				t.Errorf("doctor reported a forge error for stale Project %s: %#v", name, check)
			}
		}
		if !found {
			t.Errorf("doctor omitted stale-root warning for %s (%s): %s", name, detail, output.String())
		}
		helpFound := false
		for _, help := range result.Help {
			if help["check"] == checkName && strings.Contains(help["action"], "posse project remove "+name) {
				helpFound = true
			}
		}
		if !helpFound {
			t.Errorf("doctor omitted project removal hint for %s: %s", name, output.String())
		}
	}
}

func TestDoctorWarnsOnUnresolvableReviewDispatchRules(t *testing.T) {
	for _, test := range []struct {
		name, config, status, detail string
	}{
		{name: "typed rule", config: "[profiles.deep]\nkind = \"claude\"\n[[dispatch]]\ntype = \"review\"\nuse = \"deep\"\n", status: "ok", detail: "deep"},
		{name: "no rule matches review", config: "[profiles.deep]\nkind = \"claude\"\n[[dispatch]]\ntype = \"change\"\nuse = \"deep\"\n", status: "warn", detail: "no Dispatch Rule matches"},
		{name: "disabled kind", config: "[kinds.pi]\ndisabled = true\n[profiles.deep]\nkind = \"pi\"\n[[dispatch]]\ntype = \"review\"\nuse = \"deep\"\n", status: "warn", detail: "disabled"},
		{name: "one candidate unusable", config: "[kinds.pi]\ndisabled = true\n[profiles.deep]\nkind = \"pi\"\n[profiles.quick]\nkind = \"claude\"\n[[dispatch]]\ntype = \"review\"\nwhen = \"small\"\nuse = \"deep\"\n[[dispatch]]\ntype = \"review\"\nwhen = \"large\"\nuse = \"quick\"\n", status: "warn", detail: "deep"},
		{name: "candidates all usable", config: "[profiles.deep]\nkind = \"claude\"\n[profiles.quick]\nkind = \"codex\"\n[[dispatch]]\ntype = \"review\"\nwhen = \"small\"\nuse = \"deep\"\n[[dispatch]]\ntype = \"review\"\nwhen = \"large\"\nuse = \"quick\"\n", status: "ok", detail: "deep, quick"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(test.config), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(home, "")
			if err != nil {
				t.Fatal(err)
			}
			status, detail, action := testService(home, nil).reviewDispatchCheck(context.Background(), store.Project{Name: "shop"}, cfg)
			if status != test.status || !strings.Contains(detail, test.detail) || (status == "warn") != (action != "") {
				t.Fatalf("review dispatch check = %s %q %q, want %s naming %q", status, detail, action, test.status, test.detail)
			}
		})
	}
}

func TestDoctorWarnsOnGlobalPosseSkillsShadowingRoleSnapshots(t *testing.T) {
	root := t.TempDir()
	dirs := setupDirs{claude: filepath.Join(root, "claude"), agents: filepath.Join(root, "agents", "skills")}
	if _, status, _, _ := globalSkillShadowCheck(dirs); status != "ok" {
		t.Fatalf("empty global skill dirs = %s", status)
	}
	for _, path := range []string{
		filepath.Join(dirs.agents, "posse-tdd", "SKILL.md"),
		filepath.Join(dirs.claude, "skills", "posse-review", "SKILL.md"),
		filepath.Join(dirs.claude, "skills", "my-skill", "SKILL.md"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("---\nname: x\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, status, detail, action := globalSkillShadowCheck(dirs)
	if status != "warn" || action == "" || !strings.Contains(detail, filepath.Join(dirs.agents, "posse-tdd")) || !strings.Contains(detail, filepath.Join(dirs.claude, "skills", "posse-review")) || strings.Contains(detail, "my-skill") {
		t.Fatalf("global skill shadow check = %s %q %q", status, detail, action)
	}
}
