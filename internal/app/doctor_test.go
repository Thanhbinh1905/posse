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

func TestDoctorRecommendsRemovingProjectsWithStaleRoots(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "posse-home")
	remudaRoot := filepath.Join(home, "remuda", "shop", "mount-2")
	tempRoot := filepath.Join(root, "e2erepo")
	missingRoot := filepath.Join(root, "leadrepo")
	for _, dir := range []string{remudaRoot, tempRoot} {
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
		{"mount-2", remudaRoot},
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
		"mount-2":  "inside a Remuda Mount",
		"e2erepo":  "under a temporary directory",
		"leadrepo": "is missing",
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
