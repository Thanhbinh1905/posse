package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

var (
	workflowRunArgument = regexp.MustCompile(`^w[1-9][0-9]*$`)
	taskIDArgument      = regexp.MustCompile(`^t[1-9][0-9]*$`)
)

func (s *Service) workflowStatus(ctx *axi.Context, args []string) error {
	if len(args) > 1 {
		return axi.Usage("workflow status takes at most one run ID")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if err := s.requireLeadOrUser(ctx.Context, db, project); err != nil {
		return err
	}
	var runs []store.WorkflowRun
	if len(args) == 1 {
		run, err := db.WorkflowRun(ctx.Context, project.ID, args[0])
		if err != nil {
			if errors.Is(err, store.ErrWorkflowRunUnknown) {
				return workflowRunUnknownFailure(args[0])
			}
			return err
		}
		runs = []store.WorkflowRun{run}
	} else {
		runs, err = db.WorkflowRuns(ctx.Context, project.ID)
		if err != nil {
			return err
		}
	}
	rows := make([]any, 0, len(runs))
	for _, run := range runs {
		rows = append(rows, workflowRunOutput(ctx.Context, db, project, run, workflowProfileConfigStatus(home, project, run)))
	}
	return ctx.Print(axi.Object{{Key: "runs", Value: rows}, {Key: "help", Value: []any{"Start an approved stage with `workflow_run: <run>` and `stage: <name>` in its Task Brief", "Use `posse workflow advance <run> --to <next-stage> --user-approved \"<User's words>\"` to record the next User decision"}}})
}

func (s *Service) workflowAdvance(ctx *axi.Context, args []string) error {
	runID, toStage, quote, abandonSeqs, err := parseWorkflowAdvanceArgs(args)
	if err != nil {
		return err
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if err := s.requireLead(ctx.Context, db, project); err != nil {
		return err
	}
	if strings.TrimSpace(quote) == "" {
		return axi.Failure("user_only", "workflow stage advancement requires the User's exact approval quote", false)
	}
	run, err := db.WorkflowRun(ctx.Context, project.ID, runID)
	if err != nil {
		if errors.Is(err, store.ErrWorkflowRunUnknown) {
			return workflowRunUnknownFailure(runID)
		}
		return err
	}
	fromIndex := run.CurrentStageIndex
	toIndex := fromIndex + 1
	if toIndex >= len(run.Profile.Profile.Stages) {
		return axi.Failure("workflow_no_next_stage", fmt.Sprintf("workflow run %s is already at its final stage %q", runID, run.CurrentStage), false)
	}
	next := run.Profile.Profile.Stages[toIndex]
	if toStage != next.Name {
		return axi.Failure("workflow_stage_order", fmt.Sprintf("workflow run %s must advance from %q to %q; skipping and reordering are not supported", runID, run.CurrentStage, next.Name), false)
	}
	from := run.Profile.Profile.Stages[fromIndex]
	stageTasks := workflowTasksForStage(run.Tasks, runID, fromIndex)
	results, anchorTaskID, err := s.workflowStageResults(ctx.Context, db, project, run, from, stageTasks, abandonSeqs, quote)
	if err != nil {
		return err
	}
	digest, err := workflowResultsDigest(results)
	if err != nil {
		return err
	}
	advance := store.WorkflowAdvance{RunID: runID, FromStage: from.Name, FromIndex: fromIndex, ToStage: next.Name, ToIndex: toIndex, Results: results, ResultSHA256: digest, UserQuote: quote}
	decision, err := db.RecordWorkflowAdvance(ctx.Context, project.ID, anchorTaskID, advance, quote)
	if err != nil {
		return workflowAdvanceFailure(err)
	}
	return ctx.Print(axi.Object{
		{Key: "decision", Value: decisionOutput(ctx.Context, db, project.ID, decision)},
		{Key: "run", Value: runID},
		{Key: "from_stage", Value: from.Name},
		{Key: "to_stage", Value: next.Name},
		{Key: "result_sha256", Value: digest},
		{Key: "dispatched", Value: false},
		{Key: "help", Value: []any{"The User's stage decision is recorded. Start the next stage explicitly with `workflow_run: " + runID + "` and `stage: " + next.Name + "` in a Task Brief"}},
	})
}

func parseWorkflowAdvanceArgs(args []string) (string, string, string, map[int]bool, error) {
	var positionals []string
	to, quote := "", ""
	abandon := map[int]bool{}
	for i := 0; i < len(args); i++ {
		key, value, hasValue := strings.Cut(args[i], "=")
		if !hasValue {
			key = args[i]
			if key == "--to" || key == "--user-approved" || key == "--abandon" {
				if i+1 == len(args) || strings.HasPrefix(args[i+1], "--") {
					return "", "", "", nil, axi.Usage(key + " requires a value")
				}
				i++
				value = args[i]
			} else if strings.HasPrefix(key, "--") {
				return "", "", "", nil, axi.Usage("unknown workflow advance flag " + key)
			} else {
				positionals = append(positionals, key)
				continue
			}
		}
		switch key {
		case "--to":
			if to != "" {
				return "", "", "", nil, axi.Usage("--to may be supplied once")
			}
			to = value
		case "--user-approved":
			if quote != "" {
				return "", "", "", nil, axi.Usage("--user-approved may be supplied once")
			}
			quote = value
		case "--abandon":
			seq, err := strconv.Atoi(strings.TrimPrefix(value, "t"))
			if err != nil || seq < 1 || !taskIDArgument.MatchString(value) {
				return "", "", "", nil, axi.Usage("--abandon requires a Task ID such as t3")
			}
			if abandon[seq] {
				return "", "", "", nil, axi.Usage("--abandon Task IDs must be distinct")
			}
			abandon[seq] = true
		}
	}
	if len(positionals) != 1 || to == "" || quote == "" {
		return "", "", "", nil, axi.Usage("workflow advance requires <run> --to <next-stage> --user-approved \"<User's words>\"")
	}
	if !workflowRunArgument.MatchString(positionals[0]) {
		return "", "", "", nil, axi.Usage("workflow run ID must look like w1")
	}
	return positionals[0], to, quote, abandon, nil
}

func workflowTasksForStage(tasks []store.Task, runID string, stageIndex int) []store.Task {
	var selected []store.Task
	for _, task := range tasks {
		plan, err := task.AdmittedPlan()
		if err == nil && plan != nil && plan.Workflow != nil && plan.Workflow.RunID == runID && plan.Workflow.StageIndex == stageIndex {
			selected = append(selected, task)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Seq < selected[j].Seq })
	return selected
}

func (s *Service) workflowStageResults(ctx context.Context, db *store.DB, project store.Project, run store.WorkflowRun, stage config.WorkflowStage, stageTasks []store.Task, abandonSeqs map[int]bool, quote string) ([]store.WorkflowStageResult, int64, error) {
	results := make([]store.WorkflowStageResult, 0, len(stageTasks))
	if stage.TaskType == "none" {
		if len(abandonSeqs) > 0 {
			return nil, 0, axi.Failure("workflow_abandon_refused", "none stages have no Worker Task to abandon", false)
		}
		if !workflowNoneStageComplete(run.Tasks) {
			return nil, 0, axi.Failure("workflow_stage_incomplete", fmt.Sprintf("workflow run %s stage %q is not complete; every Change Task must be merged or torn down", run.ID, stage.Name), false)
		}
		for _, task := range run.Tasks {
			if task.Type != "change" {
				continue
			}
			identity, err := workflowNoneTaskResultIdentity(ctx, db, project, task)
			if err != nil {
				return nil, 0, err
			}
			results = append(results, store.WorkflowStageResult{TaskID: task.ID, Identity: identity})
		}
		if len(run.Tasks) == 0 {
			return nil, 0, axi.Failure("workflow_stage_empty", "workflow run has no Task result to anchor this Decision", false)
		}
		return results, run.Tasks[0].ID, nil
	}
	if len(stageTasks) == 0 {
		return nil, 0, axi.Failure("workflow_stage_empty", fmt.Sprintf("workflow run %s stage %q has no Tasks or results", run.ID, stage.Name), false)
	}
	for _, task := range stageTasks {
		identity, err := workflowTaskResultIdentity(ctx, db, project, task)
		if err != nil {
			return nil, 0, err
		}
		abandoned := abandonSeqs[task.Seq]
		if identity == "" && !abandoned {
			return nil, 0, axi.Failure("workflow_stage_incomplete", fmt.Sprintf("Task t%d has no completed result for stage %q", task.Seq, stage.Name), false, "Complete the Task or explicitly abandon it with `--abandon t"+strconv.Itoa(task.Seq)+"` and a User quote that names the Task and its abandonment")
		}
		if identity != "" && abandoned {
			return nil, 0, axi.Failure("workflow_abandon_refused", fmt.Sprintf("Task t%d has a result and cannot be recorded as abandoned", task.Seq), false)
		}
		if abandoned {
			quoted := strings.ToLower(quote)
			taskMention := regexp.MustCompile(`\b` + regexp.QuoteMeta("t"+strconv.Itoa(task.Seq)) + `\b`)
			if !strings.Contains(quoted, "abandon") || !taskMention.MatchString(quoted) {
				return nil, 0, axi.Failure("workflow_abandon_quote_required", fmt.Sprintf("User quote must explicitly name abandonment of Task t%d", task.Seq), false)
			}
			identity = "abandoned"
		}
		results = append(results, store.WorkflowStageResult{TaskID: task.ID, Identity: identity, Abandoned: abandoned})
	}
	for seq := range abandonSeqs {
		found := false
		for _, task := range stageTasks {
			found = found || task.Seq == seq
		}
		if !found {
			return nil, 0, axi.Failure("workflow_abandon_refused", fmt.Sprintf("Task t%d is not an incomplete Task in current stage %q", seq, stage.Name), false)
		}
	}
	return results, stageTasks[0].ID, nil
}

func workflowTaskResultIdentity(ctx context.Context, db *store.DB, project store.Project, task store.Task) (string, error) {
	allowedState := task.State == store.StateDone || task.State == store.StateReported || task.State == store.StateMerging || task.State == store.StateMerged || task.State == store.StateTornDown
	if !allowedState {
		return "", nil
	}
	signals, err := db.TaskSignals(ctx, task.ID, 10000)
	if err != nil {
		return "", err
	}
	if len(signals) == 0 || signals[0].Verb != "done" {
		return "", nil
	}
	if task.Type == "change" {
		parts := []string{}
		if project.IsWorkspace() {
			repos, err := db.TaskRepos(ctx, task.ID)
			if err != nil {
				return "", err
			}
			candidates, err := db.ValidationCandidates(ctx, task.ID)
			if err != nil {
				return "", err
			}
			for _, repo := range repos {
				sha := repo.GatedSHA
				if sha == "" {
					sha = activeCandidateSHA(candidates, repo.Repo)
				}
				if sha == "" && repo.State == store.TaskRepoUnchanged {
					continue
				}
				if sha == "" {
					return "", nil
				}
				parts = append(parts, repo.Repo+"="+sha)
			}
			if len(parts) == 0 {
				return "", nil
			}
		} else {
			sha := task.GatedSHA
			if sha == "" {
				candidates, err := db.ValidationCandidates(ctx, task.ID)
				if err != nil {
					return "", err
				}
				sha = activeCandidateSHA(candidates, "")
			}
			if sha == "" {
				return "", nil
			}
			parts = append(parts, sha)
		}
		sort.Strings(parts)
		return "change:" + strings.Join(parts, ","), nil
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(signals[0].DataJSON), &data); err != nil {
		return "", err
	}
	digest, _ := data["workflow_report_sha256"].(string)
	if len(digest) != 64 {
		return "", nil
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", nil
	}
	return "report:" + digest, nil
}

func activeCandidateSHA(candidates []store.ValidationCandidate, repo string) string {
	for i := len(candidates) - 1; i >= 0; i-- {
		candidate := candidates[i]
		if candidate.Repo == repo && (candidate.State == store.ValidationCandidateActive || candidate.State == store.ValidationCandidatePromoting || candidate.State == store.ValidationCandidatePromoted) {
			return candidate.CandidateSHA
		}
	}
	return ""
}

func workflowNoneTaskResultIdentity(ctx context.Context, db *store.DB, project store.Project, task store.Task) (string, error) {
	if task.State == store.StateTornDown {
		return "torn-down", nil
	}
	if task.State != store.StateMerged {
		return "", fmt.Errorf("change Task t%d is not merged or torn down", task.Seq)
	}
	if project.IsWorkspace() {
		repos, err := db.TaskRepos(ctx, task.ID)
		if err != nil {
			return "", err
		}
		if len(repos) == 0 {
			return "", fmt.Errorf("merged workspace Change Task t%d has no Member results", task.Seq)
		}
		parts := make([]string, 0, len(repos))
		for _, repo := range repos {
			switch repo.State {
			case store.TaskRepoMerged:
				parts = append(parts, repo.Repo+"=merged:"+repo.MergedRef+":head:"+repo.GatedSHA)
			case store.TaskRepoUnchanged:
				parts = append(parts, repo.Repo+"=unchanged")
			default:
				return "", fmt.Errorf("workspace Change Task t%d Member %s is not merged", task.Seq, repo.Repo)
			}
		}
		sort.Strings(parts)
		return "merged:" + strings.Join(parts, ","), nil
	}
	identity := "merged:" + task.MergedRef
	if task.GatedSHA != "" {
		identity += ":head:" + task.GatedSHA
	}
	return identity, nil
}

func workflowNoneStageComplete(tasks []store.Task) bool {
	for _, task := range tasks {
		if task.Type == "change" && task.State != store.StateMerged && task.State != store.StateTornDown {
			return false
		}
	}
	return true
}

func workflowResultsDigest(results []store.WorkflowStageResult) (string, error) {
	sort.Slice(results, func(i, j int) bool { return results[i].TaskID < results[j].TaskID })
	data, err := json.Marshal(results)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func workflowAdvanceFailure(err error) error {
	switch {
	case err == store.ErrWorkflowStageNotCurrent:
		return axi.Failure("workflow_stage_blocked", err.Error(), false)
	case err == store.ErrWorkflowRunUnknown:
		return axi.Failure("workflow_run_unknown", err.Error(), false)
	case err == store.ErrWorkflowAdvanceInvalid:
		return axi.Failure("workflow_advance_refused", err.Error(), false)
	default:
		return err
	}
}

func workflowProfileConfigStatus(home string, project store.Project, run store.WorkflowRun) map[string]any {
	result := map[string]any{"frozen_sha256": run.Profile.SHA256}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		result["status"] = "unavailable"
		result["note"] = "Current config could not be resolved; this run continues with its frozen profile."
		result["error"] = err.Error()
		return result
	}
	current, found, err := cfg.ResolveWorkflow(run.Profile.Name)
	if err != nil {
		result["status"] = "unavailable"
		result["note"] = "Current profile could not be resolved; this run continues with its frozen profile."
		result["error"] = err.Error()
		return result
	}
	if !found {
		result["status"] = "missing"
		result["note"] = "Profile is missing from current config; this run continues with its frozen profile."
		return result
	}
	result["current_sha256"] = current.SHA256
	if current.SHA256 != run.Profile.SHA256 {
		result["status"] = "changed"
		result["note"] = "Current config digest differs from the frozen run profile; this run continues with its frozen profile."
		return result
	}
	result["status"] = "matches"
	return result
}

func workflowRunOutput(ctx context.Context, db *store.DB, project store.Project, run store.WorkflowRun, currentConfig map[string]any) map[string]any {
	type stageResultKey struct {
		taskID     int64
		stageIndex int
	}
	stageResults := map[stageResultKey]store.WorkflowStageResult{}
	for _, advance := range run.Advances {
		if advance.FromIndex < 0 || advance.FromIndex >= len(run.Profile.Profile.Stages) || run.Profile.Profile.Stages[advance.FromIndex].TaskType == "none" {
			continue
		}
		for _, result := range advance.Results {
			stageResults[stageResultKey{taskID: result.TaskID, stageIndex: advance.FromIndex}] = result
		}
	}
	stages := make([]any, 0, len(run.Profile.Profile.Stages))
	for index, stage := range run.Profile.Profile.Stages {
		stageTasks := workflowTasksForStage(run.Tasks, run.ID, index)
		items := make([]any, 0, len(stageTasks))
		for _, task := range stageTasks {
			identity, err := workflowTaskResultIdentity(ctx, db, project, task)
			plan, _ := task.AdmittedPlan()
			stageIndex := index
			if plan != nil && plan.Workflow != nil {
				stageIndex = plan.Workflow.StageIndex
			}
			recorded, recordedResult := stageResults[stageResultKey{taskID: task.ID, stageIndex: stageIndex}]
			if recordedResult {
				identity = recorded.Identity
			}
			item := map[string]any{"task": taskIDString(task.Seq), "title": task.Title, "type": task.Type, "state": task.State, "result": identity, "abandoned": recorded.Abandoned}
			if err != nil {
				item["result_error"] = err.Error()
			}
			items = append(items, item)
		}
		stageComplete := index < run.CurrentStageIndex || index == run.CurrentStageIndex && stage.TaskType == "none" && workflowNoneStageComplete(run.Tasks)
		stages = append(stages, map[string]any{"name": stage.Name, "index": index, "frame": stage.Frame, "task_type": stage.TaskType, "workers": stage.Workers, "current": index == run.CurrentStageIndex, "complete": stageComplete, "tasks": items})
	}
	advances := make([]any, 0, len(run.Advances))
	for _, advance := range run.Advances {
		advances = append(advances, advance)
	}
	return map[string]any{"run": run.ID, "profile": run.Profile.Name, "profile_sha256": run.Profile.SHA256, "source": run.Profile.Source, "current_config": currentConfig,
		"current_stage": run.CurrentStage, "current_stage_index": run.CurrentStageIndex, "status": run.Status, "stages": stages, "advance_history": advances}
}
