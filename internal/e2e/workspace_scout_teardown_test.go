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

// TestWorkspaceScoutTeardownPreservesMemberAndRootAttachments exercises Report,
// ack, and Teardown for one- and two-Member Scouts on a reused workspace Mount.
func TestWorkspaceScoutTeardownPreservesMemberAndRootAttachments(t *testing.T) {
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
    mode=$(cat "$POSSE_TEST_ROOT/scout-mode" 2>/dev/null || true)
    rm -f "$POSSE_TEST_ROOT/scout-mode"
    printf 'Report for %s\n' "$PWD" > report.md
    if [ "$mode" = missing-baseline ]; then
      printf 'new root evidence\n' > new-root-evidence.txt
      printf 'See new-root-evidence.txt\n' >> report.md
    else
      if [ "$mode" = deep-untracked-git ]; then
        printf 'deep root evidence\n' > root-evidence.txt
        printf 'deep workspace context\n' > workspace-note.txt
      else
        printf 'root evidence\n' > root-evidence.txt
        printf 'updated workspace context\n' > workspace-note.txt
      fi
      printf 'See root-evidence.txt and workspace-note.txt\n' >> report.md
    fi
    if [ "$mode" = nested-git ] || [ "$mode" = deep-untracked-git ]; then
      mkdir -p git-evidence
      git -C git-evidence init -q -b main
      printf 'ignored.txt\n' > git-evidence/.git/info/exclude
      printf 'nested Git proof\n' > git-evidence/proof.txt
      printf 'untracked nested evidence\n' > git-evidence/notes.txt
      printf 'ignored nested file\n' > git-evidence/ignored.txt
      git -C git-evidence add proof.txt && git -C git-evidence commit -qm 'nested evidence' || exit 0
      printf 'See git-evidence/proof.txt\n' >> report.md
      if [ "$mode" = deep-untracked-git ]; then
        mkdir -p git-evidence/deeper
        git -C git-evidence/deeper init -q -b main
        printf 'deep tracked evidence\n' > git-evidence/deeper/tracked.txt
        git -C git-evidence/deeper add tracked.txt && git -C git-evidence/deeper commit -qm 'deep evidence' || exit 0
        printf 'deep untracked evidence\n' > git-evidence/deeper/untracked.txt
        printf 'See git-evidence/deeper/tracked.txt\n' >> report.md
      fi
    fi
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
	config := "[lead]\nkind = \"claude\"\n\n[defaults]\nlanding_mode = \"local\"\nauto_unsaddle = \"finished\"\n\n[remuda]\nkeep_idle = 10\n\n[profiles.deep]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"deep\"\n"
	if err := os.WriteFile(filepath.Join(root, "posse", "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	workspace := filepath.Join(root, "stack")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		"workspace-note.txt":    "initial workspace context\n",
		"unchanged-context.txt": "unchanged shared context\n",
	} {
		if err := os.WriteFile(filepath.Join(workspace, path), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
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
	serverProcess := startServer(t, client)
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

	parentTest := t
	var firstMountID int64
	for _, test := range []struct {
		taskID, title, name string
		members             []string
		scenario            string
		nestedGit           bool
		deepUntracked       bool
		unrequestedMember   bool
		updateProject       bool
		missingBaseline     bool
	}{
		{taskID: "t1", title: "Inspect backend member", name: "inspect-backend-member", members: []string{"backend"}},
		{taskID: "t2", title: "Inspect both members", name: "inspect-both-members", members: []string{"backend", "worker"}},
		{taskID: "t3", title: "Preserve unrequested Member commit", name: "preserve-unrequested", members: []string{"backend"}, scenario: "unrequested-member", unrequestedMember: true},
		{taskID: "t4", title: "Preserve root evidence", name: "preserve-root-evidence", members: []string{"backend"}, updateProject: true},
		{taskID: "t5", title: "Refuse missing baseline", name: "refuse-missing-baseline", members: []string{"backend"}, scenario: "missing-baseline", missingBaseline: true},
		{taskID: "t6", title: "Preserve deeper untracked Git repository", name: "preserve-deeper", members: []string{"backend"}, scenario: "deep-untracked-git", nestedGit: true, deepUntracked: true},
	} {
		if !t.Run(test.taskID, func(t *testing.T) {
			if test.scenario != "" {
				if err := os.WriteFile(filepath.Join(root, "scout-mode"), []byte(test.scenario), 0o600); err != nil {
					t.Fatal(err)
				}
			}
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
			if test.taskID == "t1" {
				firstMountID = task.MountID
			} else if task.MountID != firstMountID {
				t.Fatalf("Scout did not reuse Mount %d: got Mount %d", firstMountID, task.MountID)
			}
			if test.updateProject {
				for path, contents := range map[string]string{"root-evidence.txt": "root evidence\n", "workspace-note.txt": "updated workspace context\n"} {
					if err := os.WriteFile(filepath.Join(workspace, path), []byte(contents), 0o600); err != nil {
						t.Fatal(err)
					}
				}
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
			if test.missingBaseline {
				memberPath := filepath.Join(task.WorktreePath, "backend")
				memberBranch := strings.TrimSpace(gitTest(t, env, memberPath, "branch", "--show-current"))
				memberHead := strings.TrimSpace(gitTest(t, env, memberPath, "rev-parse", "HEAD"))
				memberFiles := map[string]string{
					"evidence.txt":  "committed evidence from backend/\n",
					"untracked.txt": "untracked evidence from backend/\n",
				}
				for name, want := range memberFiles {
					if got, err := os.ReadFile(filepath.Join(memberPath, name)); err != nil || string(got) != want {
						t.Fatalf("Member source %s before Teardown = %q, want %q: %v", name, got, want, err)
					}
				}
				baselinePath := filepath.Join(root, "posse", "projects", "stack", ".workspace-root-baselines", fmt.Sprintf("t%d.json", task.Seq))
				baseline, err := os.ReadFile(baselinePath)
				if err != nil {
					t.Fatalf("read acquisition baseline: %v", err)
				}
				if err := os.Remove(baselinePath); err != nil {
					t.Fatalf("remove acquisition baseline: %v", err)
				}
				command := exec.Command(binary, "unsaddle", test.taskID)
				command.Dir = workspace
				command.Env = setEnv(leadEnv, "POSSE_INTENT_CRASH_AT", "unsaddle:before:report.attachments")
				output, interruptErr := command.CombinedOutput()
				exit, ok := interruptErr.(*exec.ExitError)
				if !ok || exit.ExitCode() != 86 {
					t.Fatalf("Teardown interruption before Report attachment preservation = %v, want exit 86; output:\n%s", interruptErr, output)
				}

				stopServer(t, client, serverProcess)
				serverProcess = startServer(parentTest, client)
				leadPane, err = createWorkspace(client, workspace)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := client.Run(context.Background(), "pane", "run", leadPane.RootPane.PaneID, "posse up --yes"); err != nil {
					t.Fatalf("restart workspace Lead: %v", err)
				}
				if !waitForCondition(30*time.Second, func() bool {
					currentProject, err := db.ProjectByName(context.Background(), "stack")
					return err == nil && currentProject.LeadPaneID == leadPane.RootPane.PaneID
				}) {
					t.Fatal("restarted private Lead did not register")
				}
				leadEnv = setEnv(leadEnv, "HERDR_PANE_ID", leadPane.RootPane.PaneID)
				leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", leadPane.Workspace.WorkspaceID)
				runPosse(t, binary, workspace, leadEnv, "roster")

				command = exec.Command(binary, "unsaddle", test.taskID)
				command.Dir, command.Env = workspace, leadEnv
				output, teardownErr := command.CombinedOutput()
				if teardownErr == nil || !strings.Contains(string(output), "unsaddle_incomplete") || !strings.Contains(string(output), "acquisition baseline") {
					t.Fatalf("recovered Teardown without baseline = %v, want preservation refusal; output:\n%s", teardownErr, output)
				}
				current, err := db.Task(context.Background(), project.ID, test.taskID)
				if err != nil || current.State != store.StateReported {
					t.Fatalf("Task after missing-baseline refusal = %#v, %v", current, err)
				}
				mount, err := db.MountByTask(context.Background(), task.ID)
				if err != nil || mount.ID != task.MountID || mount.TaskID != task.ID || mount.State != "held" {
					t.Fatalf("Mount after missing-baseline refusal = %#v, %v", mount, err)
				}
				if _, err := os.Stat(task.WorktreePath); err != nil {
					t.Fatalf("Mount path after missing-baseline refusal: %v", err)
				}
				if contents, err := os.ReadFile(filepath.Join(task.WorktreePath, "new-root-evidence.txt")); err != nil || string(contents) != "new root evidence\n" {
					t.Fatalf("new root evidence after missing-baseline refusal = %q, %v", contents, err)
				}
				if got := strings.TrimSpace(gitTest(t, env, memberPath, "branch", "--show-current")); got != memberBranch {
					t.Fatalf("Member branch after recovery = %q, want %q", got, memberBranch)
				}
				if got := strings.TrimSpace(gitTest(t, env, memberPath, "rev-parse", "HEAD")); got != memberHead {
					t.Fatalf("Member HEAD after recovery = %q, want %q", got, memberHead)
				}
				for name, want := range memberFiles {
					if got, err := os.ReadFile(filepath.Join(memberPath, name)); err != nil || string(got) != want {
						t.Fatalf("Member source %s after recovery = %q, want %q: %v", name, got, want, err)
					}
				}
				failureNotices, err := db.Notices(context.Background(), project.ID, false)
				if err != nil {
					t.Fatal(err)
				}
				foundFailure := false
				for _, notice := range failureNotices {
					if notice.TaskID == task.ID && notice.Kind == "unsaddle_incomplete" && strings.Contains(notice.Summary, "acquisition baseline") {
						foundFailure = true
						break
					}
				}
				if !foundFailure {
					t.Fatalf("missing-baseline refusal did not record an unsaddle_incomplete Notice: %#v", failureNotices)
				}
				decisions, err := db.Decisions(context.Background(), project.ID, true)
				if err != nil {
					t.Fatal(err)
				}
				foundRepairDiscard := false
				for _, decision := range decisions {
					if decision.TaskID == task.ID && decision.Kind == "leftover" && strings.Join(decision.Options, ",") == "repair,discard" {
						foundRepairDiscard = true
						break
					}
				}
				if !foundRepairDiscard {
					t.Fatalf("missing-baseline refusal did not raise repair/discard Decision: %#v", decisions)
				}
				if err := os.WriteFile(baselinePath, baseline, 0o600); err != nil {
					t.Fatalf("restore acquisition baseline for retry: %v", err)
				}
			}
			// A receipt claim can temporarily block ack, so retry the direct ack
			// until it succeeds.
			ackOutput := ""
			if !waitForCondition(15*time.Second, func() bool {
				ackOutput = runPosse(t, binary, workspace, leadEnv, "ack", noticeID)
				return strings.Contains(ackOutput, "acknowledged: 1")
			}) {
				t.Fatalf("task_done Notice %s was not acknowledged: %s", noticeID, ackOutput)
			}
			if !waitForCondition(30*time.Second, func() bool {
				current, err := db.Task(context.Background(), project.ID, test.taskID)
				return err == nil && current.State == store.StateTornDown
			}) {
				current, _ := db.Task(context.Background(), project.ID, test.taskID)
				mounts, _ := db.Mounts(context.Background(), project.ID)
				t.Fatalf("ack did not complete Teardown: ack=%s task=%#v mounts=%#v", ackOutput, current, mounts)
			}
			mounts, err := db.Mounts(context.Background(), project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(mounts) == 0 || mounts[0].State != "idle" {
				t.Fatalf("Mount was not released: %#v", mounts)
			}
			artifacts := []string{"report.md"}
			if test.missingBaseline {
				artifacts = append(artifacts, "new-root-evidence.txt")
			} else {
				artifacts = append(artifacts, "root-evidence.txt", "workspace-note.txt")
			}
			if test.nestedGit {
				artifacts = append(artifacts, "git-evidence/proof.txt", "git-evidence/notes.txt")
			}
			if test.deepUntracked {
				artifacts = append(artifacts, "git-evidence/deeper/tracked.txt", "git-evidence/deeper/untracked.txt")
			}
			if test.unrequestedMember {
				artifacts = append(artifacts, "worker/evidence.txt", "worker/untracked.txt")
			}
			for _, member := range test.members {
				artifacts = append(artifacts, filepath.Join(member, "evidence.txt"), filepath.Join(member, "untracked.txt"))
			}
			if _, err := os.Stat(filepath.Join(root, "posse", "scratch", "stack", test.taskID)); !os.IsNotExist(err) {
				t.Fatalf("Scout Teardown kept Task scratch: %v", err)
			}
			savedDir := filepath.Join(root, "posse", "projects", "stack", "tasks", test.taskID)
			for _, artifact := range artifacts {
				if contents, err := os.ReadFile(filepath.Join(savedDir, artifact)); err != nil || len(contents) == 0 {
					t.Errorf("saved artifact %s = %q, %v", artifact, contents, err)
				}
			}
			wantReport := "Report for " + task.WorktreePath + "\n"
			if test.missingBaseline {
				wantReport += "See new-root-evidence.txt\n"
			} else {
				wantReport += "See root-evidence.txt and workspace-note.txt\n"
				if test.nestedGit {
					wantReport += "See git-evidence/proof.txt\n"
				}
				if test.deepUntracked {
					wantReport += "See git-evidence/deeper/tracked.txt\n"
				}
			}
			wantFiles := map[string]string{"report.md": wantReport}
			if !test.missingBaseline {
				wantFiles["root-evidence.txt"] = "root evidence\n"
				wantFiles["workspace-note.txt"] = "updated workspace context\n"
				if test.deepUntracked {
					wantFiles["root-evidence.txt"] = "deep root evidence\n"
					wantFiles["workspace-note.txt"] = "deep workspace context\n"
				}
			}
			if test.nestedGit {
				wantFiles["git-evidence/proof.txt"] = "nested Git proof\n"
				wantFiles["git-evidence/notes.txt"] = "untracked nested evidence\n"
			}
			if test.deepUntracked {
				wantFiles["git-evidence/deeper/tracked.txt"] = "deep tracked evidence\n"
				wantFiles["git-evidence/deeper/untracked.txt"] = "deep untracked evidence\n"
			}
			if test.unrequestedMember {
				wantFiles["worker/evidence.txt"] = "committed evidence from worker/\n"
				wantFiles["worker/untracked.txt"] = "untracked evidence from worker/\n"
			}
			if test.missingBaseline {
				wantFiles["new-root-evidence.txt"] = "new root evidence\n"
			}
			for artifact, want := range wantFiles {
				if contents, err := os.ReadFile(filepath.Join(savedDir, artifact)); err != nil || string(contents) != want {
					t.Errorf("saved root artifact %s = %q, want %q: %v", artifact, contents, want, err)
				}
			}
			if _, err := os.Stat(filepath.Join(savedDir, "unchanged-context.txt")); !os.IsNotExist(err) {
				t.Errorf("unchanged shared context was saved as an attachment: %v", err)
			}
			if test.nestedGit {
				if _, err := os.Stat(filepath.Join(savedDir, "git-evidence/ignored.txt")); !os.IsNotExist(err) {
					t.Errorf("Git-ignored nested file was saved as an attachment: %v", err)
				}
			}
			if test.updateProject {
				for path, want := range map[string]string{"root-evidence.txt": "root evidence\n", "workspace-note.txt": "updated workspace context\n"} {
					if got, err := os.ReadFile(filepath.Join(workspace, path)); err != nil || string(got) != want {
						t.Errorf("live Project update for %s = %q, want %q: %v", path, got, want, err)
					}
				}
			}
			show := runPosse(t, binary, workspace, leadEnv, "show", test.taskID)
			for _, artifact := range artifacts[1:] {
				if !strings.Contains(show, filepath.ToSlash(artifact)) {
					t.Errorf("posse show omitted saved attachment %q:\n%s", artifact, show)
				}
			}
		}) {
			t.Fatal("stop dependent Mount-reuse scenarios after a failed subtest")
		}
	}

	if !t.Run("scratch cleanup retry cannot reach another Rider's Mount", func(t *testing.T) {
		brief := filepath.Join(root, "t7.md")
		contents := "---\ntype: scout\ntitle: scratch-failure\ndone_when: report and member evidence are preserved\nrepos: [backend]\n---\nInspect the backend member and attach evidence.\n"
		if err := os.WriteFile(brief, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		runPosse(t, binary, workspace, leadEnv, "ride", "--brief", brief, "--name", "scratch-failure")
		if !waitForCondition(60*time.Second, func() bool {
			current, err := db.Task(context.Background(), project.ID, "t7")
			return err == nil && current.State == store.StateReported
		}) {
			current, _ := db.Task(context.Background(), project.ID, "t7")
			t.Fatalf("Scout t7 did not report: %#v", current)
		}

		first, err := db.Task(context.Background(), project.ID, "t7")
		if err != nil {
			t.Fatal(err)
		}
		firstScratch := filepath.Join(root, "posse", "scratch", "stack", "t7")
		if err := os.RemoveAll(firstScratch); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(firstScratch, []byte("force scratch cleanup failure"), 0o600); err != nil {
			t.Fatal(err)
		}
		command := exec.Command(binary, "unsaddle", "t7")
		command.Dir, command.Env = workspace, leadEnv
		failure, commandErr := command.CombinedOutput()
		if commandErr == nil || !strings.Contains(string(failure), "unsaddle_incomplete") {
			t.Fatalf("unsaddle did not expose the injected scratch cleanup failure: err=%v output=%s", commandErr, failure)
		}
		mount, err := db.MountByTask(context.Background(), first.ID)
		if err != nil || mount.ID != first.MountID || mount.TaskID != first.ID || mount.State != "held" {
			t.Fatalf("Mount became reusable before scratch cleanup completed: mount=%#v err=%v", mount, err)
		}

		brief = filepath.Join(root, "t8.md")
		contents = "---\ntype: scout\ntitle: scratch-successor\ndone_when: report and member evidence are preserved\nrepos: [backend]\n---\nInspect the backend member and attach evidence.\n"
		if err := os.WriteFile(brief, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		runPosse(t, binary, workspace, leadEnv, "ride", "--brief", brief, "--name", "scratch-successor")
		if !waitForCondition(60*time.Second, func() bool {
			current, err := db.Task(context.Background(), project.ID, "t8")
			return err == nil && current.State == store.StateReported
		}) {
			current, _ := db.Task(context.Background(), project.ID, "t8")
			t.Fatalf("successor Scout t8 did not report: %#v", current)
		}
		second, err := db.Task(context.Background(), project.ID, "t8")
		if err != nil {
			t.Fatal(err)
		}
		if second.MountID == first.MountID {
			t.Fatalf("successor Rider reused t7's Mount before its cleanup completed: t7=%#v t8=%#v", first, second)
		}
		foreignFile := filepath.Join(second.WorktreePath, "t8-only.txt")
		if err := os.WriteFile(foreignFile, []byte("belongs only to t8\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		process := exec.Command("sleep", "300")
		process.Dir = second.WorktreePath
		if err := process.Start(); err != nil {
			t.Fatal(err)
		}
		processExited := make(chan error, 1)
		go func() { processExited <- process.Wait() }()
		processWaited := false
		t.Cleanup(func() {
			if processWaited {
				return
			}
			_ = process.Process.Kill()
			select {
			case <-processExited:
			case <-time.After(5 * time.Second):
				t.Error("successor Rider process did not exit during cleanup")
			}
		})

		if err := os.Remove(firstScratch); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(firstScratch, 0o700); err != nil {
			t.Fatal(err)
		}
		runPosse(t, binary, workspace, leadEnv, "unsaddle", "t7")
		select {
		case err := <-processExited:
			processWaited = true
			t.Fatalf("retrying t7 stopped the process owned by t8: %v", err)
		case <-time.After(150 * time.Millisecond):
		}
		if _, err := os.Stat(foreignFile); err != nil {
			t.Fatalf("retrying t7 changed the successor Rider's file: %v", err)
		}
		firstSaved := filepath.Join(root, "posse", "projects", "stack", "tasks", "t7")
		if _, err := os.Stat(filepath.Join(firstSaved, "t8-only.txt")); !os.IsNotExist(err) {
			t.Fatalf("retrying t7 captured t8's unlanded file: %v", err)
		}
		runPosse(t, binary, workspace, leadEnv, "unsaddle", "t8")
	}) {
		return
	}

	mounts, err := db.Mounts(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) == 0 {
		t.Fatal("workspace Project has no idle Mount for stale registration coverage")
	}
	staleMount := mounts[0]
	staleMemberWorktree := filepath.Join(staleMount.Path, "backend")
	if err := os.RemoveAll(staleMemberWorktree); err != nil {
		t.Fatal(err)
	}
	dryRun := runPosse(t, binary, workspace, leadEnv, "remuda", "prune")
	if !strings.Contains(dryRun, "worktree-registration") || !strings.Contains(dryRun, "dry_run: true") {
		t.Fatalf("workspace prune dry run omitted its member registration: %s", dryRun)
	}
	applied := runPosse(t, binary, workspace, leadEnv, "remuda", "prune", "--yes")
	if !strings.Contains(applied, "dry_run: false") {
		t.Fatalf("workspace prune --yes did not apply its dry-run plan: %s", applied)
	}
	listing := gitTest(t, env, filepath.Join(workspace, "backend"), "worktree", "list", "--porcelain")
	if strings.Contains(listing, staleMemberWorktree) {
		t.Fatalf("workspace prune left the stale Member worktree registration: %s", listing)
	}
}
