package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestBrokenProjectDoesNotBlockHomeRosterOrProjectsRegeneration(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	goodRoot := filepath.Join(root, "good-repo")
	brokenRoot := filepath.Join(root, "broken-repo")
	initRepo(t, goodRoot)
	initRepo(t, brokenRoot)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nlanding_mode = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProject(ctx, "good", goodRoot, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProject(ctx, "broken", brokenRoot, "main"); err != nil {
		t.Fatal(err)
	}
	brokenConfig := filepath.Join(home, "projects", "broken", "config.toml")
	if err := os.MkdirAll(filepath.Dir(brokenConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brokenConfig, []byte("[defaults]\nlanding_mode = \"invalid\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	service := testService(home, nil)
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out = &output
	if code := cli.Run(nil); code != 0 {
		t.Fatalf("home failed on a broken Project: code=%d output=%s", code, output.String())
	}
	if !strings.Contains(output.String(), "good") || !strings.Contains(output.String(), "broken") || !strings.Contains(output.String(), "landing_mode") {
		t.Fatalf("home did not report both Project rows and the broken config: %s", output.String())
	}

	output.Reset()
	if code := cli.Run([]string{"roster", "--all", "--json"}); code != 0 {
		t.Fatalf("roster --all failed on a broken Project: code=%d output=%s", code, output.String())
	}
	var result struct {
		Projects []map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode roster JSON: %v: %s", err, output.String())
	}
	if len(result.Projects) != 2 || result.Projects[0]["name"] != "broken" || result.Projects[0]["error"] == nil || result.Projects[1]["name"] != "good" {
		t.Fatalf("roster rows did not isolate the bad Project: %#v", result.Projects)
	}

	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := service.regenerateProjects(ctx, db); err != nil {
		t.Fatalf("projects.md regeneration stopped at the broken Project: %v", err)
	}
	generated, err := os.ReadFile(filepath.Join(home, "projects.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(generated), "## broken") || !strings.Contains(string(generated), "## good") || !strings.Contains(string(generated), "landing_mode") {
		t.Fatalf("projects.md omitted a Project or its config error: %s", generated)
	}
}

func TestProjectShowSeparatesRepositoryStatusFromLeadLiveness(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p2", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p2", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "working"}}}
	var output bytes.Buffer
	cli := testService(home, fake).CLI()
	cli.Out = &output
	if code := cli.Run([]string{"--json", "project", "show"}); code != 0 {
		t.Fatalf("project show failed: code=%d output=%s", code, output.String())
	}
	var result map[string]any
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode project show: %v: %s", err, output.String())
	}
	if result["status"] != "active" || result["lead"] != "working" {
		t.Fatalf("Project repository status and Lead liveness were conflated: %#v", result)
	}
}
