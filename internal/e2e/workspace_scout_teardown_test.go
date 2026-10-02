//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// TestWorkspaceScoutTeardownPreservesMemberAttachments exercises Report, ack,
// and Teardown against isolated Herdr for Scouts checking one and two Members.
func TestWorkspaceScoutTeardownPreservesMemberAttachments(t *testing.T) {
	root := newFixtureRoot(t, fixturePrefix("workspace-scout-"))
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(binDir, "posse")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = moduleRoot(t)
	build.Env = isolatedE2EEnv(t, root)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse: %v\n%s", err, output)
	}
	remuda := filepath.Join(root, "posse", "remuda")
	leadLog := filepath.Join(root, "lead.log")
	fakeAgent := `#!/bin/sh
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
case "$PWD/" in
  "$POSSE_E2E_REMUDA/"*)
    IFS= read -r prompt || exit 0
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
    printf 'Report for %s\n' "$PWD" > report.md
    for member in */; do
      [ -e "$member/.git" ] || continue
      printf 'committed evidence from %s\n' "$member" > "${member}evidence.txt"
      git -C "$member" add evidence.txt && git -C "$member" commit -qm "evidence from $member" || exit 0
      printf 'untracked evidence from %s\n' "$member" > "${member}untracked.txt"
    done
    tries=0
    until posse holler done 'Scout report ready' --report report.md >/dev/null 2>&1; do
      tries=$((tries + 1)); [ "$tries" -lt 100 ] || break; sleep 0.1
    done
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
    while IFS= read -r line; do :; done
    ;;
  *)
    printf 'lead in %s\n' "$PWD" >> "$POSSE_E2E_LEAD_LOG"
    while IFS= read -r line; do printf '%s\n' "$line" >> "$POSSE_E2E_LEAD_LOG"; done
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(fakeAgent), 0o700); err != nil {
		t.Fatal(err)
	}
	env := isolatedE2EEnv(t, root)
	for key, value := range map[string]string{
		"POSSE_TEST_ROOT": root, "POSSE_E2E_REMUDA": remuda, "POSSE_E2E_LEAD_LOG": leadLog,
		"PATH":            binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GIT_AUTHOR_NAME": "Posse E2E", "GIT_AUTHOR_EMAIL": "posse-e2e@example.test",
		"GIT_COMMITTER_NAME": "Posse E2E", "GIT_COMMITTER_EMAIL": "posse-e2e@example.test",
	} {
		env = setEnv(env, key, value)
	}
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(root, "posse"), filepath.Join(root, "claude", "skills")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config := "[lead]\nkind = \"claude\"\n\n[defaults]\nlanding_mode = \"local\"\nauto_unsaddle = \"finished\"\n\n[profiles.deep]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"deep\"\n"
	if err := os.WriteFile(filepath.Join(root, "posse", "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	workspace := filepath.Join(root, "stack")
	for _, member := range []string{"backend", "worker"} {
		repo := filepath.Join(workspace, member)
		if err := os.MkdirAll(repo, 0o700); err != nil {
			t.Fatal(err)
		}
		gitTest(t, env, repo, "init", "-q", "-b", "main")
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte(member+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, env, repo, "add", "README.md")
		gitTest(t, env, repo, "commit", "-qm", "initial "+member)
	}

	client := herdr.NewWithEnv("herdr", env)
	startServer(t, client)
	if _, err := client.Run(context.Background(), "integration", "install", "claude"); err != nil {
		t.Fatal(err)
	}
	leadPane, err := createWorkspace(client, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "pane", "run", leadPane.RootPane.PaneID, "posse up --yes"); err != nil {
		t.Fatalf("start workspace Lead: %v", err)
	}
	if !waitForCondition(30*time.Second, func() bool {
		log, _ := os.ReadFile(leadLog)
		return strings.Contains(string(log), "lead in "+workspace)
	}) {
		screen, _ := client.Run(context.Background(), "pane", "read", leadPane.RootPane.PaneID, "--lines", "40")
		t.Fatalf("workspace Lead did not start: %s", screen)
	}
	leadEnv := setEnv(env, "HERDR_ENV", "1")
	leadEnv = setEnv(leadEnv, "HERDR_PANE_ID", leadPane.RootPane.PaneID)
	leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", leadPane.Workspace.WorkspaceID)

	db, err := store.Open(filepath.Join(root, "posse"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.ProjectByName(context.Background(), "stack")
	if err != nil || !project.IsWorkspace() {
		t.Fatalf("workspace Project = %#v, %v", project, err)
	}

	for _, test := range []struct {
		taskID, title, name string
		members             []string
	}{
		{taskID: "t1", title: "Inspect backend member", name: "inspect-backend-member", members: []string{"backend"}},
		{taskID: "t2", title: "Inspect both members", name: "inspect-both-members", members: []string{"backend", "worker"}},
	} {
		t.Run(test.taskID, func(t *testing.T) {
			brief := filepath.Join(root, test.taskID+".md")
			if err := os.WriteFile(brief, []byte("---\ntype: scout\ntitle: "+test.title+"\ndone_when: report and member evidence are preserved\nrepos: ["+strings.Join(test.members, ", ")+"]\n---\nInspect the listed member repositories and attach their evidence.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runPosse(t, binary, workspace, leadEnv, "ride", "--brief", brief, "--name", test.name)
			if !waitForCondition(60*time.Second, func() bool {
				task, err := db.Task(context.Background(), project.ID, test.taskID)
				return err == nil && task.State == store.StateReported
			}) {
				task, _ := db.Task(context.Background(), project.ID, test.taskID)
				t.Fatalf("Scout did not report: %#v", task)
			}
			task, err := db.Task(context.Background(), project.ID, test.taskID)
			if err != nil {
				t.Fatal(err)
			}
			notices, err := db.Notices(context.Background(), project.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			var noticeID string
			for _, notice := range notices {
				if notice.TaskID == task.ID && notice.Kind == "task_done" {
					noticeID = fmt.Sprint(notice.ID)
					break
				}
			}
			if noticeID == "" {
				t.Fatalf("Task %s has no task_done Notice: %#v", test.taskID, notices)
			}
			runPosse(t, binary, workspace, leadEnv, "ack", noticeID)
			if !waitForCondition(30*time.Second, func() bool {
				current, err := db.Task(context.Background(), project.ID, test.taskID)
				return err == nil && current.State == store.StateTornDown
			}) {
				current, _ := db.Task(context.Background(), project.ID, test.taskID)
				mounts, _ := db.Mounts(context.Background(), project.ID)
				t.Fatalf("ack did not complete Teardown: task=%#v mounts=%#v", current, mounts)
			}
			mounts, err := db.Mounts(context.Background(), project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(mounts) == 0 || mounts[0].State != "idle" {
				t.Fatalf("Mount was not released: %#v", mounts)
			}
			artifacts := []string{"report.md"}
			for _, member := range test.members {
				artifacts = append(artifacts, filepath.Join(member, "evidence.txt"), filepath.Join(member, "untracked.txt"))
			}
			for _, artifact := range artifacts {
				if contents, err := os.ReadFile(filepath.Join(root, "posse", "projects", "stack", "tasks", test.taskID, artifact)); err != nil || len(contents) == 0 {
					t.Errorf("saved artifact %s = %q, %v", artifact, contents, err)
				}
			}
			show := runPosse(t, binary, workspace, leadEnv, "show", test.taskID)
			for _, artifact := range artifacts[1:] {
				if !strings.Contains(show, filepath.ToSlash(artifact)) {
					t.Errorf("posse show omitted saved attachment %q:\n%s", artifact, show)
				}
			}
		})
	}
}
