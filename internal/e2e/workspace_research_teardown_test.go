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
// ack, and Teardown for one- and two-Member Research Tasks on a reused workspace Sandbox.
func TestWorkspaceResearchTeardownPreservesMemberAndRootAttachments(t *testing.T) {
	root := newFixtureRoot(t, fixturePrefix("workspace-research-"))
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
	pool := filepath.Join(root, "posse", "pool")
	leadLog := filepath.Join(root, "lead.log")
	fakeAgent := `#!/bin/sh
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
case "$PWD/" in
  "$POSSE_E2E_POOL/"*)
    IFS= read -r prompt || exit 0
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
    until [ -e "$POSSE_TEST_ROOT/research-ready" ]; do sleep 0.05; done
    rm -f "$POSSE_TEST_ROOT/research-ready"
    mode=$(cat "$POSSE_TEST_ROOT/research-mode" 2>/dev/null || true)
    rm -f "$POSSE_TEST_ROOT/research-mode"
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
    until posse signal done 'Research report ready' --report report.md >/dev/null 2>&1; do
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
		"POSSE_TEST_ROOT": root, "POSSE_E2E_POOL": pool, "POSSE_E2E_LEAD_LOG": leadLog,
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
	config := "[lead]\nkind = \"claude\"\n\n[defaults]\nmerge_mode = \"local\"\nauto_teardown = \"finished\"\n\n[pool]\nkeep_idle = 10\n\n[profiles.deep]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"deep\"\n"
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
	runPosse(t, binary, workspace, leadEnv, "config", "set", "profiles.deep.model", "model-author")

	parentTest := t
	var firstSandboxID int64
	var firstReportDigest string
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
				if err := os.WriteFile(filepath.Join(root, "research-mode"), []byte(test.scenario), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			brief := filepath.Join(root, test.taskID+".md")
			reportKind := ""
			if test.taskID == "t1" {
				reportKind = "report_kind: spec\n"
			}
			if err := os.WriteFile(brief, []byte("---\ntype: research\ntitle: "+test.title+"\ndone_when: report and member evidence are preserved\n"+reportKind+"repos: ["+strings.Join(test.members, ", ")+"]\n---\nInspect the listed member repositories and attach their evidence.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runPosse(t, binary, workspace, leadEnv, "start", "--brief", brief, "--name", test.name)
			current, err := db.Task(context.Background(), project.ID, test.taskID)
			if err != nil {
				t.Fatal(err)
			}
			if err := prepareLegacyWorkspaceResearch(t, db, project, workspace, env, root, current, test.unrequestedMember); err != nil {
				t.Fatal(err)
			}
			if !waitForCondition(60*time.Second, func() bool {
				task, err := db.Task(context.Background(), project.ID, test.taskID)
				return err == nil && task.State == store.StateReported
			}) {
				task, _ := db.Task(context.Background(), project.ID, test.taskID)
				t.Fatalf("Research did not report: %#v", task)
			}
			task, err := db.Task(context.Background(), project.ID, test.taskID)
			if err != nil {
				t.Fatal(err)
			}
			if test.taskID == "t1" {
				firstSandboxID = task.SandboxID
			} else if task.SandboxID != firstSandboxID {
				t.Fatalf("Research did not reuse Sandbox %d: got Sandbox %d", firstSandboxID, task.SandboxID)
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
				if _, err := os.ReadFile(baselinePath); err != nil {
					t.Fatalf("read acquisition baseline: %v", err)
				}
				if err := os.Remove(baselinePath); err != nil {
					t.Fatalf("remove acquisition baseline: %v", err)
				}
				command := exec.Command(binary, "teardown", test.taskID)
				command.Dir = workspace
				command.Env = setEnv(leadEnv, "POSSE_INTENT_CRASH_AT", "teardown:before:report.attachments")
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
				runPosse(t, binary, workspace, leadEnv, "list")

				command = exec.Command(binary, "teardown", test.taskID)
				command.Dir, command.Env = workspace, leadEnv
				output, teardownErr := command.CombinedOutput()
				if teardownErr == nil || !strings.Contains(string(output), "teardown_incomplete") || !strings.Contains(string(output), "acquisition baseline") {
					t.Fatalf("recovered Teardown without baseline = %v, want preservation refusal; output:\n%s", teardownErr, output)
				}
				current, err := db.Task(context.Background(), project.ID, test.taskID)
				if err != nil || current.State != store.StateReported {
					t.Fatalf("Task after missing-baseline refusal = %#v, %v", current, err)
				}
				sandbox, err := db.SandboxByTask(context.Background(), task.ID)
				if err != nil || sandbox.ID != task.SandboxID || sandbox.TaskID != task.ID || sandbox.State != "held" {
					t.Fatalf("Sandbox after missing-baseline refusal = %#v, %v", sandbox, err)
				}
				if _, err := os.Stat(task.WorktreePath); err != nil {
					t.Fatalf("Sandbox path after missing-baseline refusal: %v", err)
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
					if notice.TaskID == task.ID && notice.Kind == "teardown_incomplete" && strings.Contains(notice.Summary, "acquisition baseline") {
						foundFailure = true
						break
					}
				}
				if !foundFailure {
					t.Fatalf("missing-baseline refusal did not record an teardown_incomplete Notice: %#v", failureNotices)
				}
				incompleteBeforeApply := 0
				for _, notice := range failureNotices {
					if notice.TaskID == task.ID && notice.Kind == "teardown_incomplete" {
						incompleteBeforeApply++
					}
				}
				decisions, err := db.Decisions(context.Background(), project.ID, true)
				if err != nil {
					t.Fatal(err)
				}
				var repairDiscardID int64
				for _, decision := range decisions {
					if decision.TaskID == task.ID && decision.Kind == "orphan" && strings.Join(decision.Options, ",") == "repair,discard" {
						repairDiscardID = decision.ID
						break
					}
				}
				if repairDiscardID == 0 {
					t.Fatalf("missing-baseline refusal did not raise repair/discard Decision: %#v", decisions)
				}
				answer := exec.Command(binary, "decide", fmt.Sprint(repairDiscardID), "discard", "--user-approved", "User approved discarding the unsaved Report attachments")
				answer.Dir, answer.Env = workspace, leadEnv
				if output, err := answer.CombinedOutput(); err != nil {
					t.Fatalf("answer orphan discard: %v\n%s", err, output)
				}
				crashAt := "teardown:after:approval.record"
				apply := exec.Command(binary, "apply", fmt.Sprint(repairDiscardID))
				apply.Dir, apply.Env = workspace, setEnv(leadEnv, "POSSE_INTENT_CRASH_AT", crashAt)
				output, err = apply.CombinedOutput()
				if exit, ok := err.(*exec.ExitError); !ok {
					t.Fatalf("approved Report discard did not interrupt after approval.record: %v\n%s", err, output)
				} else {
					assertCrashExit(t, exit.ExitCode(), string(output), crashAt)
				}
				// The background Watch can claim the crashed intent first. Retry
				// only its documented transient conflict, not a discard refusal.
				settled := waitForCondition(30*time.Second, func() bool {
					apply = exec.Command(binary, "apply", fmt.Sprint(repairDiscardID))
					apply.Dir, apply.Env = workspace, leadEnv
					output, err = apply.CombinedOutput()
					return err == nil || !strings.Contains(string(output), `"intent_active"`)
				})
				if !settled || err != nil || !strings.Contains(string(output), "torn-down") {
					t.Fatalf("retry apply after interrupted Report discard: %v\n%s", err, output)
				}
				current, err = db.Task(context.Background(), project.ID, test.taskID)
				if err != nil || current.State != store.StateTornDown {
					t.Fatalf("discarded Task state = %#v, %v", current, err)
				}
				sandboxes, err := db.Sandboxes(context.Background(), project.ID)
				if err != nil {
					t.Fatal(err)
				}
				sandboxIdle := false
				for _, sandbox := range sandboxes {
					if sandbox.ID == task.SandboxID && sandbox.State == "idle" {
						sandboxIdle = true
					}
				}
				if !sandboxIdle {
					t.Fatalf("discard did not release Sandbox %d: %#v", task.SandboxID, sandboxes)
				}
				var approvals int
				var quote, branchSHA string
				if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*),MAX(user_quote),MAX(branch_sha) FROM approvals WHERE task_id=? AND action='discard'`, task.ID).Scan(&approvals, &quote, &branchSHA); err != nil {
					t.Fatal(err)
				}
				if approvals != 1 || quote != "User approved discarding the unsaved Report attachments" || branchSHA == "" {
					t.Fatalf("discard approval = count %d, quote %q, branch %q", approvals, quote, branchSHA)
				}
				bundlePaths, err := filepath.Glob(filepath.Join(root, "posse", "projects", "stack", "tasks", test.taskID, "discard", "*.bundle"))
				if err != nil || len(bundlePaths) == 0 {
					t.Fatalf("approved discard bundle paths = %v, %v", bundlePaths, err)
				}
				appliedDecision, err := db.Decision(context.Background(), project.ID, repairDiscardID)
				if err != nil || appliedDecision.ObsoleteAt == 0 {
					t.Fatalf("applied orphan Decision is not obsolete: %#v %v", appliedDecision, err)
				}
				afterNotices, err := db.Notices(context.Background(), project.ID, false)
				if err != nil {
					t.Fatal(err)
				}
				incompleteAfterApply := 0
				for _, notice := range afterNotices {
					if notice.TaskID == task.ID && notice.Kind == "teardown_incomplete" {
						incompleteAfterApply++
					}
				}
				if incompleteAfterApply != incompleteBeforeApply {
					t.Fatalf("discard created %d new teardown_incomplete Notices", incompleteAfterApply-incompleteBeforeApply)
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
				sandboxes, _ := db.Sandboxes(context.Background(), project.ID)
				t.Fatalf("ack did not complete Teardown: ack=%s task=%#v sandboxes=%#v", ackOutput, current, sandboxes)
			}
			sandboxes, err := db.Sandboxes(context.Background(), project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(sandboxes) == 0 || sandboxes[0].State != "idle" {
				t.Fatalf("Sandbox was not released: %#v", sandboxes)
			}
			artifacts := []string{"report.md"}
			if !test.missingBaseline {
				artifacts = append(artifacts, "root-evidence.txt", "workspace-note.txt")
			}
			if !test.missingBaseline {
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
			}
			if _, err := os.Stat(filepath.Join(root, "posse", "scratch", "stack", test.taskID)); !os.IsNotExist(err) {
				t.Fatalf("Research Teardown kept Task scratch: %v", err)
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
			for artifact, want := range wantFiles {
				if contents, err := os.ReadFile(filepath.Join(savedDir, artifact)); err != nil || string(contents) != want {
					t.Errorf("saved root artifact %s = %q, want %q: %v", artifact, contents, want, err)
				}
			}
			if test.taskID == "t1" {
				sealedTask, err := db.Task(context.Background(), project.ID, "t1")
				if err != nil {
					t.Fatal(err)
				}
				firstReportDigest = sealedTask.ReportDigest
				assertE2EReportBundle(t, savedDir, sealedTask)
			}
			if test.missingBaseline {
				if _, err := os.Stat(filepath.Join(savedDir, "new-root-evidence.txt")); !os.IsNotExist(err) {
					t.Errorf("approved discard preserved an attachment the User chose to discard: %v", err)
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
			t.Fatal("stop dependent Sandbox-reuse scenarios after a failed subtest")
		}
	}

	if !t.Run("scratch cleanup retry cannot reach another Worker's Sandbox", func(t *testing.T) {
		brief := filepath.Join(root, "t7.md")
		contents := "---\ntype: research\ntitle: scratch-failure\ndone_when: report and member evidence are preserved\nrepos: [backend]\n---\nInspect the backend member and attach evidence.\n"
		if err := os.WriteFile(brief, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		runPosse(t, binary, workspace, leadEnv, "start", "--brief", brief, "--name", "scratch-failure")
		current, err := db.Task(context.Background(), project.ID, "t7")
		if err != nil {
			t.Fatal(err)
		}
		if err := prepareLegacyWorkspaceResearch(t, db, project, workspace, env, root, current, false); err != nil {
			t.Fatal(err)
		}
		if !waitForCondition(60*time.Second, func() bool {
			current, err := db.Task(context.Background(), project.ID, "t7")
			return err == nil && current.State == store.StateReported
		}) {
			current, _ := db.Task(context.Background(), project.ID, "t7")
			t.Fatalf("Research t7 did not report: %#v", current)
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
		command := exec.Command(binary, "teardown", "t7")
		command.Dir, command.Env = workspace, leadEnv
		failure, commandErr := command.CombinedOutput()
		if commandErr == nil || !strings.Contains(string(failure), "teardown_incomplete") {
			t.Fatalf("teardown did not expose the injected scratch cleanup failure: err=%v output=%s", commandErr, failure)
		}
		sandbox, err := db.SandboxByTask(context.Background(), first.ID)
		if err != nil || sandbox.ID != first.SandboxID || sandbox.TaskID != first.ID || sandbox.State != "held" {
			t.Fatalf("Sandbox became reusable before scratch cleanup completed: sandbox=%#v err=%v", sandbox, err)
		}

		brief = filepath.Join(root, "t8.md")
		contents = "---\ntype: research\ntitle: scratch-successor\ndone_when: report and member evidence are preserved\nrepos: [backend]\n---\nInspect the backend member and attach evidence.\n"
		if err := os.WriteFile(brief, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		runPosse(t, binary, workspace, leadEnv, "start", "--brief", brief, "--name", "scratch-successor")
		current, err = db.Task(context.Background(), project.ID, "t8")
		if err != nil {
			t.Fatal(err)
		}
		if err := prepareLegacyWorkspaceResearch(t, db, project, workspace, env, root, current, false); err != nil {
			t.Fatal(err)
		}
		if !waitForCondition(60*time.Second, func() bool {
			current, err := db.Task(context.Background(), project.ID, "t8")
			return err == nil && current.State == store.StateReported
		}) {
			current, _ := db.Task(context.Background(), project.ID, "t8")
			t.Fatalf("successor Research t8 did not report: %#v", current)
		}
		second, err := db.Task(context.Background(), project.ID, "t8")
		if err != nil {
			t.Fatal(err)
		}
		if second.SandboxID == first.SandboxID {
			t.Fatalf("successor Worker reused t7's Sandbox before its cleanup completed: t7=%#v t8=%#v", first, second)
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
				t.Error("successor Worker process did not exit during cleanup")
			}
		})

		if err := os.Remove(firstScratch); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(firstScratch, 0o700); err != nil {
			t.Fatal(err)
		}
		runPosse(t, binary, workspace, leadEnv, "teardown", "t7")
		select {
		case err := <-processExited:
			processWaited = true
			t.Fatalf("retrying t7 stopped the process owned by t8: %v", err)
		case <-time.After(150 * time.Millisecond):
		}
		if _, err := os.Stat(foreignFile); err != nil {
			t.Fatalf("retrying t7 changed the successor Worker's file: %v", err)
		}
		firstSaved := filepath.Join(root, "posse", "projects", "stack", "tasks", "t7")
		if _, err := os.Stat(filepath.Join(firstSaved, "t8-only.txt")); !os.IsNotExist(err) {
			t.Fatalf("retrying t7 captured t8's unmerged file: %v", err)
		}
		teardown := exec.Command(binary, "teardown", "t8")
		teardown.Dir, teardown.Env = workspace, leadEnv
		output, err := teardown.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "unverified process") {
			t.Fatalf("Teardown did not refuse the unproved t8 process: err=%v output=%s", err, output)
		}
		if err := process.Process.Kill(); err != nil {
			t.Fatalf("stop the E2E-owned t8 process after fail-closed refusal: %v", err)
		}
		select {
		case <-processExited:
			processWaited = true
		case <-time.After(5 * time.Second):
			t.Fatal("E2E-owned t8 process did not exit after explicit cleanup")
		}
		if _, err := os.Stat(foreignFile); err != nil {
			t.Fatalf("refused t8 Teardown changed its work: %v", err)
		}
		runPosse(t, binary, workspace, leadEnv, "teardown", "t8")
	}) {
		return
	}

	if !t.Run("restore acquisition baseline and retry Report Teardown", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "research-mode"), []byte("missing-baseline"), 0o600); err != nil {
			t.Fatal(err)
		}
		brief := filepath.Join(root, "t9.md")
		contents := "---\ntype: research\ntitle: repair-baseline\ndone_when: report and member evidence are preserved\nrepos: [backend]\n---\nInspect the backend member and attach evidence.\n"
		if err := os.WriteFile(brief, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		runPosse(t, binary, workspace, leadEnv, "start", "--brief", brief, "--name", "repair-baseline")
		task, err := db.Task(context.Background(), project.ID, "t9")
		if err != nil {
			t.Fatal(err)
		}
		if err := prepareLegacyWorkspaceResearch(t, db, project, workspace, env, root, task, false); err != nil {
			t.Fatal(err)
		}
		if !waitForCondition(60*time.Second, func() bool {
			current, err := db.Task(context.Background(), project.ID, "t9")
			return err == nil && current.State == store.StateReported
		}) {
			current, _ := db.Task(context.Background(), project.ID, "t9")
			t.Fatalf("repair-baseline Research did not report: %#v", current)
		}
		task, err = db.Task(context.Background(), project.ID, "t9")
		if err != nil {
			t.Fatal(err)
		}
		baselinePath := filepath.Join(root, "posse", "projects", "stack", ".workspace-root-baselines", "t9.json")
		baseline, err := os.ReadFile(baselinePath)
		if err != nil {
			t.Fatalf("read acquisition baseline: %v", err)
		}
		if err := os.Remove(baselinePath); err != nil {
			t.Fatal(err)
		}
		command := exec.Command(binary, "teardown", "t9")
		command.Dir, command.Env = workspace, leadEnv
		output, err := command.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "acquisition baseline") {
			t.Fatalf("missing-baseline Teardown = %v, want preservation refusal; output:\n%s", err, output)
		}
		if current, err := db.Task(context.Background(), project.ID, "t9"); err != nil || current.State != store.StateReported {
			t.Fatalf("Task after missing-baseline refusal = %#v, %v", current, err)
		}
		if err := os.WriteFile(baselinePath, baseline, 0o600); err != nil {
			t.Fatalf("restore acquisition baseline for retry: %v", err)
		}
		notices, err := db.Notices(context.Background(), project.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		noticeID := ""
		for _, notice := range notices {
			if notice.TaskID == task.ID && notice.Kind == "task_done" {
				noticeID = fmt.Sprint(notice.ID)
				break
			}
		}
		if noticeID == "" {
			t.Fatalf("repair-baseline Task has no task_done Notice: %#v", notices)
		}
		ackOutput := ""
		// A concurrent Notice delivery can temporarily own the acknowledgement.
		// Reissue it after delivery settles rather than only waiting for teardown.
		if !waitForCondition(30*time.Second, func() bool {
			command := exec.Command(binary, "ack", noticeID)
			command.Dir, command.Env = workspace, leadEnv
			output, commandErr := command.CombinedOutput()
			ackOutput = string(output)
			if commandErr != nil && !strings.Contains(ackOutput, `"intent_active"`) {
				t.Fatalf("ack repaired Report: %v\n%s", commandErr, output)
			}
			current, err := db.Task(context.Background(), project.ID, "t9")
			return err == nil && current.State == store.StateTornDown
		}) {
			current, taskErr := db.Task(context.Background(), project.ID, "t9")
			sandbox, sandboxErr := db.SandboxByTask(context.Background(), task.ID)
			transitions, transitionErr := db.TaskTransitions(context.Background(), task.ID, 20)
			allNotices, noticeErr := db.Notices(context.Background(), project.ID, true)
			intents, intentErr := db.Intents(context.Background(), project.ID)
			t.Fatalf("ack or an already-committed acknowledgement did not repair Report Teardown: %s; task=%#v err=%v sandbox=%#v err=%v transitions=%#v err=%v notices=%#v err=%v intents=%#v err=%v", ackOutput, current, taskErr, sandbox, sandboxErr, transitions, transitionErr, allNotices, noticeErr, intents, intentErr)
		}
		if !waitForCondition(30*time.Second, func() bool {
			current, err := db.Task(context.Background(), project.ID, "t9")
			return err == nil && current.State == store.StateTornDown
		}) {
			current, _ := db.Task(context.Background(), project.ID, "t9")
			t.Fatalf("restored baseline did not allow Teardown retry: %#v", current)
		}
		savedDir := filepath.Join(root, "posse", "projects", "stack", "tasks", "t9")
		for path, want := range map[string]string{"report.md": "Report for " + task.WorktreePath + "\nSee new-root-evidence.txt\n", "new-root-evidence.txt": "new root evidence\n"} {
			if contents, err := os.ReadFile(filepath.Join(savedDir, path)); err != nil || string(contents) != want {
				t.Errorf("repaired Teardown attachment %s = %q, want %q: %v", path, contents, want, err)
			}
		}
		decisions, err := db.Decisions(context.Background(), project.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, decision := range decisions {
			if decision.TaskID == task.ID {
				t.Fatalf("repaired Teardown left a pending Decision: %#v", decision)
			}
		}
	}) {
		return
	}

	if firstReportDigest == "" {
		t.Fatal("publication-bound Research Report has no sealed digest")
	}
	unreviewed := exec.Command(binary, "accept", "t1")
	unreviewed.Dir, unreviewed.Env = workspace, env
	output, err := unreviewed.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "report_review_required") {
		t.Fatalf("publication-bound Report accepted without independent review: err=%v output=%s", err, output)
	}
	reportTask, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	reviewerID, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 10, Type: "review", ReviewsTaskID: reportTask.ID, Title: "Independent Report review", Profile: "report-reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(context.Background(), reviewerID, "report-reviewer", "model-review", true); err != nil {
		t.Fatal(err)
	}
	// This synthetic reviewer has no live pane. Seed its final evidence state
	// atomically so the background Watch cannot reconcile an unbound working Task.
	if _, err := db.ExecContext(context.Background(), `UPDATE tasks SET state='reported' WHERE id=?`, reviewerID); err != nil {
		t.Fatal(err)
	}
	if err := db.PersistTask(context.Background(), reviewerID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordReportReview(context.Background(), reportTask.ID, reviewerID, firstReportDigest, "pass"); err != nil {
		t.Fatalf("record independent Report review: %v", err)
	}
	accept := exec.Command(binary, "accept", "t1", "--user-approved", "User explicitly accepts this exact spec")
	accept.Dir, accept.Env = workspace, leadEnv
	output, err = accept.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "provenance: lead-quote") || !strings.Contains(string(output), "User explicitly accepts this exact spec") {
		t.Fatalf("Lead acceptance with quoted User approval: err=%v output=%s", err, output)
	}
	if _, err := db.RebuildFromSnapshots(context.Background(), filepath.Join(root, "posse")); err != nil {
		t.Fatalf("rebuild Report and acceptance from snapshots: %v", err)
	}
	rebuiltReport, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil || rebuiltReport.ReportDigest != firstReportDigest || rebuiltReport.ReportKind != "spec" {
		t.Fatalf("rebuilt Report Task = %#v, err=%v", rebuiltReport, err)
	}
	rebuiltAcceptance, err := db.ReportAcceptanceForTask(context.Background(), reportTask.ID)
	if err != nil || rebuiltAcceptance == nil || rebuiltAcceptance.ReportDigest != firstReportDigest || rebuiltAcceptance.UserQuote != "User explicitly accepts this exact spec" {
		t.Fatalf("rebuilt Report acceptance = %#v, err=%v", rebuiltAcceptance, err)
	}
	assertE2EReportBundle(t, filepath.Join(root, "posse", "projects", "stack", "tasks", "t1"), rebuiltReport)

	sandboxes, err := db.Sandboxes(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sandboxes) == 0 {
		t.Fatal("workspace Project has no idle Sandbox for stale registration coverage")
	}
	staleSandbox := sandboxes[0]
	staleMemberWorktree := filepath.Join(staleSandbox.Path, "backend")
	if err := os.RemoveAll(staleMemberWorktree); err != nil {
		t.Fatal(err)
	}
	dryRun := runPosse(t, binary, workspace, leadEnv, "pool", "prune")
	if !strings.Contains(dryRun, "worktree-registration") || !strings.Contains(dryRun, "dry_run: true") {
		t.Fatalf("workspace prune dry run omitted its member registration: %s", dryRun)
	}
	applied := runPosse(t, binary, workspace, leadEnv, "pool", "prune", "--yes")
	if !strings.Contains(applied, "dry_run: false") {
		t.Fatalf("workspace prune --yes did not apply its dry-run plan: %s", applied)
	}
	listing := gitTest(t, env, filepath.Join(workspace, "backend"), "worktree", "list", "--porcelain")
	if strings.Contains(listing, staleMemberWorktree) {
		t.Fatalf("workspace prune left the stale Member worktree registration: %s", listing)
	}
}
