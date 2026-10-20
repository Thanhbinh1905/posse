//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

type workflowStatusEnvelope struct {
	Runs []struct {
		CurrentStage  string `json:"current_stage"`
		Status        string `json:"status"`
		ProfileSHA256 string `json:"profile_sha256"`
		CurrentConfig struct {
			Status        string `json:"status"`
			FrozenSHA256  string `json:"frozen_sha256"`
			CurrentSHA256 string `json:"current_sha256"`
			Note          string `json:"note"`
		} `json:"current_config"`
		Stages []struct {
			Name     string `json:"name"`
			Complete bool   `json:"complete"`
			Tasks    []struct {
				Result    string `json:"result"`
				State     string `json:"state"`
				Abandoned bool   `json:"abandoned"`
			} `json:"tasks"`
		} `json:"stages"`
		AdvanceHistory []struct {
			UserQuote    string                      `json:"user_quote"`
			Results      []store.WorkflowStageResult `json:"results"`
			ResultSHA256 string                      `json:"result_sha256"`
		} `json:"advance_history"`
	} `json:"runs"`
}

func TestWorkflowRunsRequireUserApprovedProgressionAndRecoverPosition(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	baseConfig := `[lead]
kind = "claude"

[defaults]
max_workers = 8
merge_mode = "local"
auto_teardown = "never"
pr_poll = "1ms"

[ci_loop]
max_fix_rounds = 0
rerun_cancelled = 0
`
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(baseConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	projectConfigPath := filepath.Join(fixture.home, "projects", fixture.project.Name, "config.toml")
	originalWorkflow := `[workflows.feature]
stages = [
  { name = "Discover", frame = "plan", gate = "user", task_type = "research", workers = "2..4" },
  { name = "Build", frame = "implement", gate = "user", task_type = "change", workers = 1 },
  { name = "Deliver", frame = "ship", gate = "user", task_type = "none", workers = 0 },
]
`
	if err := os.MkdirAll(filepath.Dir(projectConfigPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfigPath, []byte(originalWorkflow), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	start := func(name, brief string) string {
		t.Helper()
		path := filepath.Join(fixture.root, name+".md")
		if err := os.WriteFile(path, []byte(brief), 0o600); err != nil {
			t.Fatal(err)
		}
		before, err := fixture.db.Tasks(ctx, fixture.project.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", path, "--name", name)
		after, err := fixture.db.Tasks(ctx, fixture.project.ID, true)
		if err != nil || len(after) != len(before)+1 {
			t.Fatalf("start %s Task count: before=%d after=%d err=%v", name, len(before), len(after), err)
		}
		return "t" + strconv.Itoa(after[len(after)-1].Seq)
	}
	researchBrief := func(title string, continuation string) string {
		return "---\ntype: research\ntitle: " + title + "\ndone_when: record the findings\n" + continuation + "---\nRecord the findings in the Report.\n"
	}
	firstID := start("workflow-first", researchBrief("First workflow discovery", "workflow: feature\n"))
	secondRunID := start("workflow-second", researchBrief("Concurrent workflow discovery", "workflow: feature\n"))
	secondTaskID := start("first-second-task", researchBrief("Second discovery Task", "workflow_run: w1\nstage: Discover\n"))
	firstTask, err := fixture.db.Task(ctx, fixture.project.ID, firstID)
	if err != nil {
		t.Fatal(err)
	}
	secondRunTask, err := fixture.db.Task(ctx, fixture.project.ID, secondRunID)
	if err != nil {
		t.Fatal(err)
	}
	secondStageTask, err := fixture.db.Task(ctx, fixture.project.ID, secondTaskID)
	if err != nil {
		t.Fatal(err)
	}
	abandonedTaskID := start("workflow-abandon", researchBrief("Abandoned workflow discovery", "workflow: feature\n"))
	abandonedTask, err := fixture.db.Task(ctx, fixture.project.ID, abandonedTaskID)
	if err != nil {
		t.Fatal(err)
	}
	abandonedPlan, err := abandonedTask.AdmittedPlan()
	if err != nil || abandonedPlan == nil || abandonedPlan.Workflow.RunID != "w3" {
		t.Fatalf("third workflow run identity = %#v, %v", abandonedPlan, err)
	}
	firstPlan, err := firstTask.AdmittedPlan()
	if err != nil || firstPlan == nil || firstPlan.Workflow.RunID != "w1" || firstPlan.Workflow.StageIndex != 0 {
		t.Fatalf("first run identity = %#v, %v", firstPlan, err)
	}
	secondPlan, err := secondRunTask.AdmittedPlan()
	if err != nil || secondPlan == nil || secondPlan.Workflow.RunID != "w2" {
		t.Fatalf("concurrent run identity = %#v, %v", secondPlan, err)
	}
	secondStagePlan, err := secondStageTask.AdmittedPlan()
	if err != nil || secondStagePlan == nil || secondStagePlan.Workflow.RunID != "w1" || secondStagePlan.Workflow.StageIndex != 0 {
		t.Fatalf("multi-Task stage identity = %#v, %v", secondStagePlan, err)
	}

	assertRefused := func(want string, args ...string) string {
		t.Helper()
		command := exec.Command(fixture.binary, args...)
		command.Dir, command.Env = fixture.repo, fixture.leadEnv
		output, err := command.CombinedOutput()
		if err == nil || !strings.Contains(string(output), want) {
			t.Fatalf("posse %s was not refused with %q: err=%v\n%s", strings.Join(args, " "), want, err, output)
		}
		return string(output)
	}
	assertRefused("workflow advance requires", "workflow", "advance", "w1", "--to", "Build")
	assertRefused("no completed result", "workflow", "advance", "w1", "--to", "Build", "--user-approved", "User approves the Build stage")
	assertRefused("must advance from", "workflow", "advance", "w1", "--to", "Deliver", "--user-approved", "User approves the next stage")
	outOfOrderBrief := filepath.Join(fixture.root, "out-of-order.md")
	if err := os.WriteFile(outOfOrderBrief, []byte("---\ntype: change\ntitle: Out of order Task\ndone_when: checked\nworkflow_run: w1\nstage: Build\n---\nThis must not dispatch.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertRefused("workflow_stage_blocked", "start", "--brief", outOfOrderBrief, "--name", "out-of-order")
	tasksBeforeAdvance, err := fixture.db.Tasks(ctx, fixture.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasksBeforeAdvance) != 4 {
		t.Fatalf("refused stage admission allocated a Task: %#v", tasksBeforeAdvance)
	}

	completeResearch := func(task store.Task) {
		t.Helper()
		taskScratch := filepath.Join(fixture.home, "scratch", fixture.project.Name, "t"+strconv.Itoa(task.Seq))
		if err := os.MkdirAll(taskScratch, 0o700); err != nil {
			t.Fatal(err)
		}
		report := filepath.Join(taskScratch, "report.md")
		if err := os.WriteFile(report, []byte("# Findings\n\nRecorded evidence for t"+strconv.Itoa(task.Seq)+".\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		token, err := fixture.db.TaskTurnToken(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		workerEnv := setEnv(fixture.leadEnv, "HERDR_ENV", "1")
		workerEnv = setEnv(workerEnv, "HERDR_PANE_ID", task.PaneID)
		workerEnv = setEnv(workerEnv, "HERDR_WORKSPACE_ID", task.HerdrWorkspaceID)
		workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", fixture.home)
		runPosse(t, fixture.binary, task.WorktreePath, workerEnv, "signal", "done", "Research Report recorded", "--report", report, "--turn", token)
		updated, err := fixture.db.TaskByID(ctx, fixture.project.ID, task.ID)
		if err != nil || updated.State != store.StateReported {
			t.Fatalf("Research Task t%d completion = %#v, %v", task.Seq, updated, err)
		}
	}
	completeResearch(firstTask)
	completeResearch(secondStageTask)
	assertRefused("workflow_abandon_quote_required", "workflow", "advance", "w3", "--to", "Build", "--user-approved", "User approves the Build stage", "--abandon", abandonedTaskID)
	abandonOutput := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "workflow", "advance", "w3", "--to", "Build", "--user-approved", "User explicitly abandons "+abandonedTaskID+" and approves Build", "--abandon", abandonedTaskID, "--json")
	var abandonedAdvance workflowStatusEnvelope
	if err := json.Unmarshal([]byte(abandonOutput), &abandonedAdvance); err != nil {
		t.Fatalf("decode abandonment Decision: %v\\n%s", err, abandonOutput)
	}
	abandonStatus := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "workflow", "status", "w3", "--json")
	var abandonedRun workflowStatusEnvelope
	if err := json.Unmarshal([]byte(abandonStatus), &abandonedRun); err != nil || len(abandonedRun.Runs) != 1 || len(abandonedRun.Runs[0].AdvanceHistory) != 1 || len(abandonedRun.Runs[0].AdvanceHistory[0].Results) != 1 || abandonedRun.Runs[0].AdvanceHistory[0].Results[0].TaskID != abandonedTask.ID || abandonedRun.Runs[0].AdvanceHistory[0].Results[0].Identity != "abandoned" || !abandonedRun.Runs[0].AdvanceHistory[0].Results[0].Abandoned {
		t.Fatalf("explicit abandonment was not bound to its Decision: %v\\n%s", err, abandonStatus)
	}

	editedWorkflow := strings.ReplaceAll(strings.ReplaceAll(originalWorkflow, "Discover", "Explore"), "Build", "Implement")
	if err := os.WriteFile(projectConfigPath, []byte(editedWorkflow), 0o600); err != nil {
		t.Fatal(err)
	}
	changedStatus := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "workflow", "status", "w1", "--json")
	var changedRun workflowStatusEnvelope
	if err := json.Unmarshal([]byte(changedStatus), &changedRun); err != nil || len(changedRun.Runs) != 1 || changedRun.Runs[0].CurrentStage != "Discover" || changedRun.Runs[0].ProfileSHA256 != firstPlan.Workflow.SHA256 || changedRun.Runs[0].CurrentConfig.Status != "changed" || !strings.Contains(changedRun.Runs[0].CurrentConfig.Note, "differs from the frozen run profile") || len(changedRun.Runs[0].Stages) != 3 || changedRun.Runs[0].Stages[0].Name != "Discover" || changedRun.Runs[0].Stages[1].Name != "Build" {
		t.Fatalf("profile edit changed a running workflow or was not reported: %v\\n%s", err, changedStatus)
	}

	advanceOutput := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "workflow", "advance", "w1", "--to", "Build", "--user-approved", "User approves the Build stage", "--json")
	var advance struct {
		Decision struct {
			ID int64 `json:"id"`
		} `json:"decision"`
		Run        string `json:"run"`
		FromStage  string `json:"from_stage"`
		ToStage    string `json:"to_stage"`
		Dispatched bool   `json:"dispatched"`
	}
	if err := json.Unmarshal([]byte(advanceOutput), &advance); err != nil {
		t.Fatalf("decode workflow advance: %v\n%s", err, advanceOutput)
	}
	if advance.Run != "w1" || advance.FromStage != "Discover" || advance.ToStage != "Build" || advance.Decision.ID == 0 || advance.Dispatched {
		t.Fatalf("workflow advance output = %#v", advance)
	}
	afterDecision, err := fixture.db.Tasks(ctx, fixture.project.ID, true)
	if err != nil || len(afterDecision) != len(tasksBeforeAdvance) {
		t.Fatalf("User stage Decision dispatched a next-stage Task: count=%d err=%v", len(afterDecision), err)
	}
	status := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "workflow", "status", "w1", "--json")
	var statusData workflowStatusEnvelope
	if err := json.Unmarshal([]byte(status), &statusData); err != nil {
		t.Fatalf("decode workflow status: %v\n%s", err, status)
	}
	if len(statusData.Runs) != 1 || statusData.Runs[0].CurrentStage != "Build" || len(statusData.Runs[0].AdvanceHistory) != 1 || len(statusData.Runs[0].AdvanceHistory[0].Results) != 2 || len(statusData.Runs[0].AdvanceHistory[0].ResultSHA256) != 64 {
		t.Fatalf("current stage or bound results after User approval = %#v\n%s", statusData.Runs, status)
	}
	for _, result := range statusData.Runs[0].AdvanceHistory[0].Results {
		if !strings.HasPrefix(result.Identity, "report:") || result.Abandoned {
			t.Fatalf("Research Report result was not recorded: %#v", result)
		}
	}
	otherRun := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "workflow", "status", "w2", "--json")
	var otherStatus workflowStatusEnvelope
	if err := json.Unmarshal([]byte(otherRun), &otherStatus); err != nil || len(otherStatus.Runs) != 1 || otherStatus.Runs[0].CurrentStage != "Discover" {
		t.Fatalf("advancing w1 changed concurrent w2: %v\n%s", err, otherRun)
	}

	newRunID := start("workflow-new-config", researchBrief("New profile run", "workflow: feature\n"))
	newRunTask, err := fixture.db.Task(ctx, fixture.project.ID, newRunID)
	if err != nil {
		t.Fatal(err)
	}
	newRunPlan, err := newRunTask.AdmittedPlan()
	if err != nil || newRunPlan == nil || newRunPlan.Workflow.RunID != "w4" || newRunPlan.Workflow.StageName != "Explore" || newRunPlan.Workflow.SHA256 == firstPlan.Workflow.SHA256 || newRunPlan.Workflow.Profile.Stages[1].Name != "Implement" {
		t.Fatalf("new workflow run did not freeze the edited profile: %#v, %v", newRunPlan, err)
	}
	newRunStatus := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "workflow", "status", "w4", "--json")
	var newRunEnvelope workflowStatusEnvelope
	if err := json.Unmarshal([]byte(newRunStatus), &newRunEnvelope); err != nil || len(newRunEnvelope.Runs) != 1 || newRunEnvelope.Runs[0].CurrentStage != "Explore" || newRunEnvelope.Runs[0].CurrentConfig.Status != "matches" {
		t.Fatalf("new run did not use the current profile: %v\\n%s", err, newRunStatus)
	}
	if err := os.Remove(projectConfigPath); err != nil {
		t.Fatal(err)
	}
	deletedSourceStatus := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "workflow", "status", "w1", "--json")
	var deletedSourceRun workflowStatusEnvelope
	if err := json.Unmarshal([]byte(deletedSourceStatus), &deletedSourceRun); err != nil || len(deletedSourceRun.Runs) != 1 || deletedSourceRun.Runs[0].ProfileSHA256 != firstPlan.Workflow.SHA256 || deletedSourceRun.Runs[0].CurrentConfig.Status != "missing" || !strings.Contains(deletedSourceRun.Runs[0].CurrentConfig.Note, "continues with its frozen profile") {
		t.Fatalf("deleting the profile source broke or altered the old run: %v\\n%s", err, deletedSourceStatus)
	}

	buildID := start("workflow-build", "---\ntype: change\ntitle: Build workflow result\ndone_when: gated head recorded\nworkflow_run: w1\nstage: Build\n---\nImplement the selected change.\n")
	buildTask, err := fixture.db.Task(ctx, fixture.project.ID, buildID)
	if err != nil {
		t.Fatal(err)
	}
	branch := "refs/heads/" + buildTask.Branch
	if !waitForCondition(10*time.Second, func() bool {
		_, err := gitCommand(fixture.env, buildTask.WorktreePath, "rev-parse", "--verify", branch)
		return err == nil
	}) {
		t.Fatalf("Worker did not create its Change commit for %s", buildID)
	}
	workerEnv := setEnv(fixture.leadEnv, "HERDR_ENV", "1")
	workerEnv = setEnv(workerEnv, "HERDR_PANE_ID", buildTask.PaneID)
	workerEnv = setEnv(workerEnv, "HERDR_WORKSPACE_ID", buildTask.HerdrWorkspaceID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", fixture.home)
	turn, err := fixture.db.TaskTurnToken(ctx, buildTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, buildTask.WorktreePath, workerEnv, "signal", "done", "Build change is ready", "--turn", turn)
	gateOutput := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", buildID)
	if !strings.Contains(gateOutput, "state: merging") {
		t.Fatalf("local gate did not leave Merge authority pending: %s", gateOutput)
	}
	buildTask, err = fixture.db.TaskByID(ctx, fixture.project.ID, buildTask.ID)
	if err != nil || buildTask.State != store.StateMerging || buildTask.GatedSHA == "" {
		t.Fatalf("gated Build Task = %#v, %v", buildTask, err)
	}
	shipAdvance := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "workflow", "advance", "w1", "--to", "Deliver", "--user-approved", "User approves the Deliver stage", "--json")
	var shipResult struct {
		Dispatched bool `json:"dispatched"`
	}
	if err := json.Unmarshal([]byte(shipAdvance), &shipResult); err != nil || shipResult.Dispatched {
		t.Fatalf("none stage advance dispatched work: %v\n%s", err, shipAdvance)
	}
	buildTask, err = fixture.db.TaskByID(ctx, fixture.project.ID, buildTask.ID)
	if err != nil || buildTask.State != store.StateMerging {
		t.Fatalf("stage approval changed independent Merge authority: %#v, %v", buildTask, err)
	}
	mergeOutput := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "merge", buildID, "--execute", "--user-approved", "User separately approves the local merge")
	if !strings.Contains(mergeOutput, "state: merged") {
		t.Fatalf("separate Merge approval did not merge the Change Task: %s", mergeOutput)
	}
	completedStatus := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "workflow", "status", "w1", "--json")
	var completed workflowStatusEnvelope
	if err := json.Unmarshal([]byte(completedStatus), &completed); err != nil || len(completed.Runs) != 1 || completed.Runs[0].Status != "complete" || completed.Runs[0].CurrentStage != "Deliver" || len(completed.Runs[0].Stages) != 3 || !completed.Runs[0].Stages[2].Complete {
		t.Fatalf("final none stage did not complete mechanically after Merge: %v\n%s", err, completedStatus)
	}

	// A cold database reconstruction exercises the same frozen plans and
	// answered Decision snapshot used to recover the run after a Lead restart.
	if _, err := fixture.db.RebuildFromSnapshots(ctx, fixture.home); err != nil {
		t.Fatalf("rebuild workflow state from snapshots: %v", err)
	}
	recovered := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "workflow", "status", "w1", "--json")
	var recoveredStatus workflowStatusEnvelope
	if err := json.Unmarshal([]byte(recovered), &recoveredStatus); err != nil || len(recoveredStatus.Runs) != 1 || recoveredStatus.Runs[0].CurrentStage != "Deliver" || recoveredStatus.Runs[0].Status != "complete" || recoveredStatus.Runs[0].CurrentConfig.Status != "missing" || len(recoveredStatus.Runs[0].Stages) != 3 || len(recoveredStatus.Runs[0].AdvanceHistory) != 2 || recoveredStatus.Runs[0].AdvanceHistory[1].UserQuote != "User approves the Deliver stage" || len(recoveredStatus.Runs[0].AdvanceHistory[1].Results) != 1 || recoveredStatus.Runs[0].AdvanceHistory[1].Results[0].Identity != "change:"+buildTask.GatedSHA || len(recoveredStatus.Runs[0].Stages[0].Tasks) != 2 || !strings.HasPrefix(recoveredStatus.Runs[0].Stages[0].Tasks[0].Result, "report:") || len(recoveredStatus.Runs[0].Stages[1].Tasks) != 1 || recoveredStatus.Runs[0].Stages[1].Tasks[0].State != string(store.StateMerged) || !recoveredStatus.Runs[0].Stages[2].Complete {
		t.Fatalf("workflow position or Decision was not recovered: %v\n%s", err, recovered)
	}
	otherRun = runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "workflow", "status", "w2", "--json")
	if err := json.Unmarshal([]byte(otherRun), &otherStatus); err != nil || len(otherStatus.Runs) != 1 || otherStatus.Runs[0].CurrentStage != "Discover" {
		t.Fatalf("snapshot recovery lost concurrent workflow position: %v\n%s", err, otherRun)
	}
	otherRun = runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "workflow", "status", "w3", "--json")
	if err := json.Unmarshal([]byte(otherRun), &abandonedRun); err != nil || len(abandonedRun.Runs) != 1 || abandonedRun.Runs[0].CurrentStage != "Build" || len(abandonedRun.Runs[0].AdvanceHistory) != 1 || len(abandonedRun.Runs[0].AdvanceHistory[0].Results) != 1 || !abandonedRun.Runs[0].AdvanceHistory[0].Results[0].Abandoned || len(abandonedRun.Runs[0].Stages) != 3 || len(abandonedRun.Runs[0].Stages[0].Tasks) != 1 || !abandonedRun.Runs[0].Stages[0].Tasks[0].Abandoned {
		t.Fatalf("snapshot recovery lost explicit abandonment or its stage position: %v\n%s", err, otherRun)
	}
	otherRun = runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "workflow", "status", "w4", "--json")
	if err := json.Unmarshal([]byte(otherRun), &newRunEnvelope); err != nil || len(newRunEnvelope.Runs) != 1 || newRunEnvelope.Runs[0].CurrentStage != "Explore" || newRunEnvelope.Runs[0].ProfileSHA256 != newRunPlan.Workflow.SHA256 || newRunEnvelope.Runs[0].CurrentConfig.Status != "missing" || len(newRunEnvelope.Runs[0].Stages) != 3 || newRunEnvelope.Runs[0].Stages[1].Name != "Implement" {
		t.Fatalf("snapshot recovery did not preserve the new run's frozen profile: %v\n%s", err, otherRun)
	}
}
