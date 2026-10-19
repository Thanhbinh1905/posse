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
	if err := db.Transition(ctx, parkedID, store.StateWorking, store.StateDone, "worker", "fixture done"); err != nil {
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
