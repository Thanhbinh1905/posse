package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestCallerRecognizesRegisteredNativeChildrenAndUnboundAgents(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "scout", Title: "Native", PaneID: "w2:p1"}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	t.Setenv("HERDR_PANE_ID", "")
	service := testService(home, herdr.NewFake())
	previous := procRoot
	t.Cleanup(func() { procRoot = previous })
	for _, tc := range []struct {
		name string
		env  []string
		role string
	}{
		{"Lead child", []string{"HERDR_PANE_ID=w1:p1"}, "lead"},
		{"native Rider child", []string{"HERDR_PANE_ID=w2:p1"}, "worker"},
		{"unbound native agent", []string{"PI_CODING_AGENT=1"}, "agent"},
		{"User shell", nil, "user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			procRoot = fakeProc(t, repo, tc.env)
			if got := service.configCallerRole(ctx, db); got != tc.role {
				t.Fatalf("role=%s want %s", got, tc.role)
			}
			code, output := runCLI(t, service, "config", "set", "kinds.claude.prepare", "claude-trust", "--project", "shop")
			if tc.role == "user" {
				if code != 0 {
					t.Fatal(output)
				}
				return
			}
			if code != 1 {
				t.Fatalf("agent wrote a protected setting: %s", output)
			}
			if tc.role == "lead" {
				quote := "  Prepare trusted directories for Claude.  "
				code, output = runCLI(t, service, "config", "set", "kinds.claude.prepare", "claude-trust", "--user-approved", quote)
				if code != 0 {
					t.Fatalf("registered Lead child: %s", output)
				}
				var recorded string
				if err := db.QueryRowContext(ctx, "SELECT user_quote FROM config_approvals ORDER BY id DESC LIMIT 1").Scan(&recorded); err != nil || recorded != quote {
					t.Fatalf("recorded=%q err=%v", recorded, err)
				}
			}
		})
	}
	procRoot = fakeProc(t, repo, nil)
	t.Setenv("HERDR_PANE_ID", "foreign:p1")
	code, output := runCLI(t, service, "config", "set", "kinds.claude.prepare", "claude-trust", "--project", "shop", "--user-approved", "claimed consent")
	if code != 1 || !strings.Contains(output, "user_only") {
		t.Fatalf("unbound pane was treated as a User: %s", output)
	}
}

func TestPermissionTemplateConsentCoversSetAndUnsetBeforeWriting(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "lead"); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	service := testService(home, herdr.NewFake())
	for _, key := range []string{"kinds.claude.auto_approve_args", "kinds.claude.model_args", "kinds.claude.effort_args", "kinds.claude.resume_args", "kinds.claude.system_prompt_args", "kinds.claude.lead_args", "kinds.claude.prepare", "profiles.deep.args", "profiles.deep.kind"} {
		for _, args := range [][]string{{"config", "set", key, "[]", "--project", "shop"}, {"config", "unset", key, "--project", "shop"}} {
			code, output := runCLI(t, service, args...)
			if code != 1 || !strings.Contains(output, "user_only") {
				t.Fatalf("%v: code=%d %s", args, code, output)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(home, "projects", "shop", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("refused commands wrote config: %v", err)
	}
	var count int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM config_approvals").Scan(&count); err != nil || count != 0 {
		t.Fatalf("refusal created approvals: %d %v", count, err)
	}
}
