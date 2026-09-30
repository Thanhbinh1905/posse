package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLeadOnlyCommandsRequireRecordedLeadPaneAndProjectCheckout(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	mountPath := filepath.Join(root, "mount")
	initRepo(t, repo)
	gitTest(t, repo, "worktree", "add", "-b", "posse/mount-test", mountPath, "main")
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByName(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state) VALUES(?,1,?,'held')`, project.ID, mountPath); err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	t.Setenv("HERDR_ENV", "1")
	t.Chdir(repo)
	for _, paneID := range []string{"", "w1:p9"} {
		t.Setenv("HERDR_PANE_ID", paneID)
		var failure *axi.Error
		if err := service.requireLead(ctx, db, project); !errors.As(err, &failure) || failure.Code != "lead_only" {
			t.Errorf("pane %q bypassed the Lead restriction: %v", paneID, err)
		}
	}
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	if err := service.requireLead(ctx, db, project); err != nil {
		t.Fatalf("recorded Lead pane was refused in the Project checkout: %v", err)
	}
	t.Chdir(mountPath)
	var failure *axi.Error
	if err := service.requireLead(ctx, db, project); !errors.As(err, &failure) || failure.Code != "lead_only" {
		t.Fatalf("recorded Lead pane bypassed the Mount cwd restriction: %v", err)
	}
}

func TestLeadInstructionsCheckRoleAndConfigSetKeepsShellSettingsUserOnly(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	t.Chdir(repo)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p9")
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out = &output
	if code := cli.Run([]string{"lead"}); code != 1 || !strings.Contains(output.String(), "lead_only") {
		t.Fatalf("Lead instructions bypassed pane check: code=%d output=%s", code, output.String())
	}
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	for _, setting := range []struct{ key, value string }{{"defaults.gate", "[]"}, {"remuda.setup", "[]"}, {"kinds.claude.lead_auto_approve", "false"}} {
		output.Reset()
		if code := cli.Run([]string{"config", "set", setting.key, setting.value, "--project", "shop"}); code != 1 || !strings.Contains(output.String(), "user_only") {
			t.Errorf("config set %s was not User-only: code=%d output=%s", setting.key, code, output.String())
		}
	}
}

func TestLeadOnlyRejectsMountSymlinksAndAnyHerdrEnvironment(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	worktree := filepath.Join(root, "mount")
	alias := filepath.Join(root, "mount-alias")
	initRepo(t, repo)
	gitTest(t, repo, "worktree", "add", "--detach", worktree, "main")
	if err := os.Symlink(worktree, alias); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state) VALUES(?,1,?,'held')`, project.ID, worktree); err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(key, "HERDR_") {
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Chdir(alias)
	var failure *axi.Error
	if err := service.requireLead(ctx, db, project); !errors.As(err, &failure) || failure.Code != "lead_only" {
		t.Fatalf("symlinked Mount cwd bypassed Lead restriction without Herdr environment: %v", err)
	}
	t.Chdir(repo)
	t.Setenv("HERDR_CUSTOM_CONTEXT", "1")
	failure = nil
	if err := service.requireLead(ctx, db, project); !errors.As(err, &failure) || failure.Code != "lead_only" {
		t.Fatalf("arbitrary Herdr environment variable bypassed pane ownership check: %v", err)
	}
}

func TestHerdrAncestorDetectionUsesProcessParents(t *testing.T) {
	procRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(procRoot, "100"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(procRoot, "200"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procRoot, "100", "stat"), []byte("100 (posse) S 200 1 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/opt/herdr/bin/herdr", filepath.Join(procRoot, "200", "exe")); err != nil {
		t.Fatal(err)
	}
	if !hasHerdrAncestorAt(procRoot, 100) {
		t.Fatal("Herdr parent process was not recognized")
	}
}
