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
	"github.com/thanhbinh1905/posse/internal/store"
)

func writePlaybook(t *testing.T, home, relative, content string) {
	t.Helper()
	path := filepath.Join(home, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPlaybookSourcesLayerUserBeforeWorkspaceProjectAndMissingFilesAreEmpty(t *testing.T) {
	home := t.TempDir()
	writePlaybook(t, home, "playbook/rider.md", "User rider rules.\n")
	writePlaybook(t, home, "projects/stack/playbook/rider.md", "Workspace rider rules.\n")
	project := store.Project{Name: "stack", Kind: store.ProjectKindWorkspace}

	sources, err := playbookSources(home, project, "rider")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0].Layer != "User" || sources[1].Layer != "Project stack" {
		t.Fatalf("Playbook source order = %#v", sources)
	}
	effective := renderedPlaybook(sources)
	userIndex := strings.Index(effective, "User rider rules")
	projectIndex := strings.Index(effective, "Workspace rider rules")
	if userIndex < 0 || projectIndex <= userIndex || !strings.Contains(effective, sources[0].Path) || !strings.Contains(effective, sources[1].Path) {
		t.Fatalf("effective Playbook lost source order or headers: %s", effective)
	}

	missing, err := playbookSources(home, project, "lead")
	if err != nil {
		t.Fatal(err)
	}
	if renderedPlaybook(missing) != "" || len(missing) != 2 || missing[0].Exists || missing[1].Exists {
		t.Fatalf("missing Playbooks were not empty: sources=%#v effective=%q", missing, renderedPlaybook(missing))
	}
}

func TestPrepareLeadLaunchUsesOnlyLayeredLeadPlaybooks(t *testing.T) {
	home := t.TempDir()
	writePlaybook(t, home, "playbook/lead.md", "User lead instruction marker.\n")
	writePlaybook(t, home, "projects/shop/playbook/lead.md", "Project lead instruction marker.\n")
	writePlaybook(t, home, "playbook/rider.md", "Rider-only instruction marker.\n")
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	launch, err := service.prepareLeadLaunch(home, store.Project{Name: "shop"}, cfg, "codex")
	if err != nil {
		t.Fatal(err)
	}
	var instructions string
	for _, arg := range launch.Args {
		if value, ok := strings.CutPrefix(arg, "developer_instructions="); ok {
			instructions = value
			break
		}
	}
	userIndex := strings.Index(instructions, "User lead instruction marker")
	projectIndex := strings.Index(instructions, "Project lead instruction marker")
	if userIndex < 0 || projectIndex <= userIndex || strings.Contains(instructions, "Rider-only instruction marker") {
		t.Fatalf("Lead launch did not preserve role separation and layer order: %s", instructions)
	}
	if !strings.Contains(instructions, "Never edit the Project repository") || !strings.Contains(instructions, "Only the User answers Decisions") || !strings.Contains(instructions, "User-only settings") {
		t.Fatalf("Playbook overrode or omitted non-overridable Lead boundaries: %s", instructions)
	}
}

func TestPlaybookShowAndPathExposeEffectiveSources(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse-home")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProject(context.Background(), "shop", repo, "main"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writePlaybook(t, home, "playbook/rider.md", "User source marker.\n")
	writePlaybook(t, home, "projects/shop/playbook/rider.md", "Project source marker.\n")
	t.Chdir(repo)
	service := testService(home, nil)

	var output bytes.Buffer
	cli := service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"playbook", "show", "--json"}); code != 0 {
		t.Fatalf("playbook show failed: %s", output.String())
	}
	text := output.String()
	userIndex := strings.Index(text, "User source marker")
	projectIndex := strings.Index(text, "Project source marker")
	if userIndex < 0 || projectIndex <= userIndex || !strings.Contains(text, filepath.Join(home, "playbook", "rider.md")) || !strings.Contains(text, filepath.Join(home, "projects", "shop", "playbook", "rider.md")) {
		t.Fatalf("playbook show omitted effective content or sources: %s", text)
	}

	output.Reset()
	cli = service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"playbook", "path", "rider", "--json"}); code != 0 {
		t.Fatalf("playbook path failed: %s", output.String())
	}
	if !strings.Contains(output.String(), filepath.Join(home, "playbook", "rider.md")) || !strings.Contains(output.String(), filepath.Join(home, "projects", "shop", "playbook", "rider.md")) {
		t.Fatalf("playbook path omitted one of its sources: %s", output.String())
	}
}

func TestDoctorWarnsWhenEffectivePlaybookExceedsAdvisorySize(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse-home")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProject(context.Background(), "shop", repo, "main"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writePlaybook(t, home, "playbook/rider.md", strings.Repeat("u", 3000))
	writePlaybook(t, home, "projects/shop/playbook/rider.md", strings.Repeat("p", 1200))
	service := testService(home, nil)
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"doctor", "--json"}); code != 0 {
		t.Fatalf("doctor failed: %s", output.String())
	}
	var result struct {
		Checks []map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode doctor result %s: %v", output.String(), err)
	}
	for _, check := range result.Checks {
		if check["check"] == "playbook shop rider" {
			if check["status"] != "warn" || !strings.Contains(check["detail"], "4200 bytes") {
				t.Fatalf("Playbook doctor warning = %#v", check)
			}
			return
		}
	}
	t.Fatalf("doctor omitted the large effective Playbook warning: %s", output.String())
}

func TestPlaybookContentHasNoSeparateLeadCap(t *testing.T) {
	home := t.TempDir()
	writePlaybook(t, home, "playbook/lead.md", strings.Repeat("x", playbookWarningBytes+100))
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	if _, err := service.prepareLeadLaunch(home, store.Project{Name: "shop"}, cfg, "pi"); err != nil {
		t.Fatalf("Playbook content below the existing Lead startup budget was refused: %v", err)
	}
	writePlaybook(t, home, "playbook/lead.md", strings.Repeat("x", leadInstructionMaxBytes))
	if _, err := service.prepareLeadLaunch(home, store.Project{Name: "shop"}, cfg, "pi"); err == nil || !strings.Contains(err.Error(), "lead_instructions_too_large") && !strings.Contains(err.Error(), "Lead instructions exceed") {
		t.Fatalf("Playbook content above the existing Lead startup budget was not refused: %v", err)
	}
}
