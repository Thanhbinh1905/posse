package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func TestTaskAddMemberCLIRefusesNonRunningTask(t *testing.T) {
	fixture := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"local\"\nreview = \"none\"\nauto_teardown = \"never\"\n", []string{"worker"}, []string{"worker"})
	output := outcomeCLI(t, fixture.service, 1, "task", "add-member", "t1", "e2e-tool")
	if !strings.Contains(output, "task_not_running") {
		t.Fatalf("expected named running-state refusal, got %s", output)
	}
}

func TestTaskAddMemberCLINonChangeRefusal(t *testing.T) {
	fixture := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"local\"\nreview = \"none\"\nauto_teardown = \"never\"\n", []string{"worker"}, []string{"worker"})
	admitRunningWorkspacePlan(t, fixture)
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET type='research' WHERE id=?`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	output := outcomeCLI(t, fixture.service, 1, "task", "add-member", "t1", "e2e-tool")
	if !strings.Contains(output, "task_not_change") {
		t.Fatalf("expected named Change Task refusal, got %s", output)
	}
}

func TestTaskAddMemberCLIRefusesRepositoryProject(t *testing.T) {
	fixture := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"local\"\nreview = \"none\"\nauto_teardown = \"never\"\n", []string{"worker"}, []string{"worker"})
	ctx := context.Background()
	repoRoot := filepath.Join(fixture.workspace, "worker")
	detected, err := detectProject(ctx, repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	project, err := registerDetected(ctx, fixture.db, detected)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:worker:lead"); err != nil {
		t.Fatal(err)
	}
	taskID, _, err := createTaskWithSequence(ctx, fixture.db, project, fixture.home, store.Task{Type: "change", Title: "Repository task", ShortName: "repo-task", MergeMode: "local", AutonomyMerge: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET state='working' WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoRoot)
	output := outcomeCLI(t, fixture.service, 1, "task", "add-member", "t1", "worker")
	if !strings.Contains(output, "task_not_workspace") {
		t.Fatalf("expected named workspace refusal, got %s", output)
	}
}

func TestTaskAddMemberCLIAddsMemberAndNotifiesWorker(t *testing.T) {
	fixture := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"local\"\nreview = \"none\"\nauto_teardown = \"never\"\n[repositories.e2e-tool]\ntarget_branch = \"release\"\n", []string{"worker"}, []string{"worker"})
	gitTest(t, filepath.Join(fixture.workspace, "e2e-tool"), "branch", "release", "main")
	admitRunningWorkspacePlan(t, fixture)
	ctx := context.Background()
	if err := fixture.db.UpdateTaskWorkspace(ctx, fixture.task.ID, "w1", "w1:p2"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.UpdateTaskLaunch(ctx, fixture.task.ID, fixture.task.WorktreePath, "w1", "w1:p2", fixture.task.PaneLabel, "worker"); err != nil {
		t.Fatal(err)
	}
	fake := fixture.service.Herdr.(*herdr.Fake)
	fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{PaneID: "w1:p2", WorkspaceID: "w1", Label: fixture.task.PaneLabel, Agent: "claude", AgentStatus: "idle"})
	output := outcomeCLI(t, fixture.service, 0, "task", "add-member", "t1", "e2e-tool", "--base", "main", "--user-approved", "Add e2e-tool to this Task")
	if !strings.Contains(output, "state: delivered") {
		t.Fatalf("Worker notification was not delivered: %s", output)
	}
	var instruction string
	if err := fixture.db.QueryRowContext(ctx, `SELECT body FROM messages WHERE task_id=? ORDER BY id DESC LIMIT 1`, fixture.task.ID).Scan(&instruction); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(instruction, filepath.Join(fixture.task.WorktreePath, "e2e-tool")) {
		t.Fatalf("Worker was not told the new Sandbox path: %s", instruction)
	}
	planTask, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planTask.AdmittedPlan()
	if err != nil || len(plan.Execution.Members) != 2 || plan.Revision != 2 {
		t.Fatalf("admitted plan was not revised with the Member: %#v, %v", plan, err)
	}
	added := plan.Execution.Members[1]
	if added.Name != "e2e-tool" || added.Target != "release" || added.Base != "refs/heads/main" {
		t.Fatalf("member branches did not honor the base flag and Project target override: %#v", added)
	}
	member, err := fixture.db.TaskRepos(ctx, fixture.task.ID)
	if err != nil || len(member) != 2 {
		t.Fatalf("Task Sandbox Member row missing: %#v, %v", member, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.task.WorktreePath, "e2e-tool", ".git")); err != nil {
		t.Fatalf("new Member checkout missing from Sandbox: %v", err)
	}
}

func TestTaskAddMemberRequiresApproval(t *testing.T) {
	fixture := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"local\"\nreview = \"none\"\nauto_teardown = \"never\"\n", []string{"worker"}, []string{"worker"})
	admitRunningWorkspacePlan(t, fixture)
	output := outcomeCLI(t, fixture.service, 1, "task", "add-member", "t1", "e2e-tool")
	if !strings.Contains(output, "user_approval_required") {
		t.Fatalf("expected named User-approval refusal, got %s", output)
	}
}

func TestTaskAddMemberStoresScopedUserApproval(t *testing.T) {
	fixture := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"local\"\nreview = \"none\"\nauto_teardown = \"never\"\n", []string{"worker"}, []string{"worker"})
	gitTest(t, filepath.Join(fixture.workspace, "e2e-tool"), "branch", "release", "main")
	admitRunningWorkspacePlan(t, fixture)
	ctx := context.Background()
	if err := fixture.db.UpdateTaskWorkspace(ctx, fixture.task.ID, "w1", "w1:p2"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.UpdateTaskLaunch(ctx, fixture.task.ID, fixture.task.WorktreePath, "w1", "w1:p2", fixture.task.PaneLabel, "worker"); err != nil {
		t.Fatal(err)
	}
	fake := fixture.service.Herdr.(*herdr.Fake)
	fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{PaneID: "w1:p2", WorkspaceID: "w1", Label: fixture.task.PaneLabel, Agent: "claude", AgentStatus: "idle"})
	quote := "  Add e2e-tool to this Task only.  "
	outcomeCLI(t, fixture.service, 0, "task", "add-member", "t1", "e2e-tool", "--base", "main", "--user-approved", quote)
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := task.AdmittedPlan()
	if err != nil {
		t.Fatal(err)
	}
	if plan.UserChoice != nil {
		t.Fatalf("Member approval changed Brief preferences: %#v", plan.UserChoice)
	}
	if len(plan.MemberAdmissions) != 1 || plan.MemberAdmissions[0].Repository != "e2e-tool" || plan.MemberAdmissions[0].Scope != "member:e2e-tool" || plan.MemberAdmissions[0].Quote != quote || plan.MemberAdmissions[0].Actor != "lead" || plan.MemberAdmissions[0].Source != "task add-member --user-approved" {
		t.Fatalf("Member approval did not survive admitted-plan decoding: %#v", plan.MemberAdmissions)
	}
	mergeGrantFound := false
	for _, grant := range plan.Grants {
		if grant.Name == "autonomy.merge" {
			mergeGrantFound = grant.Value == "ask"
		}
		if grant.Name == "autonomy.discard" {
			t.Fatalf("Member approval added discard authority: %#v", plan.Grants)
		}
	}
	if !mergeGrantFound {
		t.Fatalf("Member approval changed merge authority: %#v", plan.Grants)
	}
}

func TestTaskAddMemberCLIRefusalCases(t *testing.T) {
	for _, tc := range []struct {
		name string
		repo string
		want string
	}{
		{name: "already selected", repo: "worker", want: "member_already_added"},
		{name: "not a Project Member", repo: "missing", want: "not_project_member"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"local\"\nreview = \"none\"\nauto_teardown = \"never\"\n", []string{"worker"}, []string{"worker"})
			admitRunningWorkspacePlan(t, fixture)
			output := outcomeCLI(t, fixture.service, 1, "task", "add-member", "t1", tc.repo)
			if !strings.Contains(output, tc.want) {
				t.Fatalf("expected %s, got %s", tc.want, output)
			}
		})
	}
}

func admitRunningWorkspacePlan(t *testing.T, fixture *workspaceMergeFixture) {
	t.Helper()
	ctx := context.Background()
	brief := []byte("---\ntype: change\ntitle: Span members\ndone_when: members changed\nrepos: [worker]\n---\nChange the member.\n")
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief:     taskplan.Select("brief", "workspace.md", brief),
		Execution: taskplan.Execution{Scope: "workspace", Members: []taskplan.Member{{Name: "worker", Path: "worker", Base: "refs/heads/main", Target: "main", Delivery: "local", Review: "none", ReviewSource: "defaults.review"}}},
		Launch:    taskplan.Launch{ProfileName: "author", Profile: config.Profile{Kind: "claude"}},
		Review:    "none", Delivery: "local", Autonomy: config.Autonomy{Review: "ask", Merge: "ask"},
		Paths: taskplan.Paths{Scratch: filepath.Join(fixture.home, "scratch", "stack", "t1"), Output: filepath.Join(fixture.home, "projects", "stack", "tasks", "t1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET state='working',plan_json=? WHERE id=?`, encoded, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `INSERT INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,1,?)`, fixture.task.ID, encoded); err != nil {
		t.Fatal(err)
	}
}
