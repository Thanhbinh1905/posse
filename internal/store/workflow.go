package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

var (
	ErrWorkflowRunUnknown        = errors.New("workflow run does not exist")
	ErrWorkflowStageNotCurrent   = errors.New("workflow stage is not the run's current approved stage")
	ErrWorkflowProfileChanged    = errors.New("workflow run profile does not match its frozen profile")
	ErrWorkflowStageTypeMismatch = errors.New("workflow Task type does not match its frozen stage")
	ErrWorkflowStageNoTask       = errors.New("workflow stage has task_type none and cannot admit a Worker Task")
	ErrWorkflowRunComplete       = errors.New("workflow run is complete")
	ErrWorkflowAdvanceInvalid    = errors.New("workflow advance is invalid")
)

type WorkflowStageResult struct {
	TaskID    int64  `json:"task_id"`
	Identity  string `json:"identity"`
	Abandoned bool   `json:"abandoned,omitempty"`
}

type WorkflowAdvance struct {
	RunID        string                `json:"run_id"`
	FromStage    string                `json:"from_stage"`
	FromIndex    int                   `json:"from_index"`
	ToStage      string                `json:"to_stage"`
	ToIndex      int                   `json:"to_index"`
	Results      []WorkflowStageResult `json:"results"`
	ResultSHA256 string                `json:"result_sha256"`
	UserQuote    string                `json:"user_quote"`
}

type WorkflowRun struct {
	ID                string                   `json:"id"`
	Profile           config.WorkflowSelection `json:"profile"`
	CurrentStage      string                   `json:"current_stage"`
	CurrentStageIndex int                      `json:"current_stage_index"`
	Status            string                   `json:"status"`
	Tasks             []Task                   `json:"tasks"`
	Advances          []WorkflowAdvanceRecord  `json:"advances"`
}

type WorkflowAdvanceRecord struct {
	DecisionID   int64                 `json:"decision_id"`
	FromStage    string                `json:"from_stage"`
	FromIndex    int                   `json:"from_index"`
	ToStage      string                `json:"to_stage"`
	ToIndex      int                   `json:"to_index"`
	Results      []WorkflowStageResult `json:"results"`
	ResultSHA256 string                `json:"result_sha256"`
	UserQuote    string                `json:"user_quote"`
	AnsweredAt   int64                 `json:"answered_at"`
}

func WorkflowDecisionOrigin(advance WorkflowAdvance) (string, error) {
	if advance.RunID == "" || advance.FromStage == "" || advance.ToStage == "" || advance.FromIndex < 0 || advance.ToIndex != advance.FromIndex+1 || len(advance.ResultSHA256) != 64 || strings.TrimSpace(advance.UserQuote) == "" {
		return "", ErrWorkflowAdvanceInvalid
	}
	results := append([]WorkflowStageResult{}, advance.Results...)
	slices.SortFunc(results, func(a, b WorkflowStageResult) int {
		if a.TaskID < b.TaskID {
			return -1
		}
		if a.TaskID > b.TaskID {
			return 1
		}
		return 0
	})
	seen := map[int64]bool{}
	for _, result := range results {
		if result.TaskID < 1 || result.Identity == "" || seen[result.TaskID] || result.Abandoned != (result.Identity == "abandoned") {
			return "", ErrWorkflowAdvanceInvalid
		}
		seen[result.TaskID] = true
	}
	resultJSON, err := json.Marshal(results)
	if err != nil {
		return "", err
	}
	resultHash := sha256.Sum256(resultJSON)
	if hex.EncodeToString(resultHash[:]) != advance.ResultSHA256 {
		return "", ErrWorkflowAdvanceInvalid
	}
	advance.Results = results
	encoded, err := json.Marshal(advance)
	if err != nil {
		return "", err
	}
	return "workflow-stage:" + base64.RawURLEncoding.EncodeToString(encoded), nil
}

func ParseWorkflowDecisionOrigin(origin string) (WorkflowAdvance, error) {
	const prefix = "workflow-stage:"
	encoded, ok := strings.CutPrefix(origin, prefix)
	if !ok {
		return WorkflowAdvance{}, ErrWorkflowAdvanceInvalid
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return WorkflowAdvance{}, ErrWorkflowAdvanceInvalid
	}
	var advance WorkflowAdvance
	if err := json.Unmarshal(data, &advance); err != nil {
		return WorkflowAdvance{}, ErrWorkflowAdvanceInvalid
	}
	if _, err := WorkflowDecisionOrigin(advance); err != nil {
		return WorkflowAdvance{}, err
	}
	return advance, nil
}

func (db *DB) WorkflowRun(ctx context.Context, projectID int64, runID string) (WorkflowRun, error) {
	tasks, err := db.Tasks(ctx, projectID, true)
	if err != nil {
		return WorkflowRun{}, err
	}
	decisions, err := db.Decisions(ctx, projectID, false)
	if err != nil {
		return WorkflowRun{}, err
	}
	return workflowRunFromRecords(projectID, runID, tasks, decisions)
}

func (db *DB) WorkflowRuns(ctx context.Context, projectID int64) ([]WorkflowRun, error) {
	tasks, err := db.Tasks(ctx, projectID, true)
	if err != nil {
		return nil, err
	}
	decisions, err := db.Decisions(ctx, projectID, false)
	if err != nil {
		return nil, err
	}
	runIDs := map[string]bool{}
	for _, task := range tasks {
		plan, err := task.AdmittedPlan()
		if err != nil || plan == nil || plan.Workflow == nil || plan.Workflow.RunID == "" || plan.Workflow.RunID == "new" {
			continue
		}
		runIDs[plan.Workflow.RunID] = true
	}
	ids := make([]string, 0, len(runIDs))
	for id := range runIDs {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int {
		aNumber, _ := strconv.Atoi(strings.TrimPrefix(a, "w"))
		bNumber, _ := strconv.Atoi(strings.TrimPrefix(b, "w"))
		return aNumber - bNumber
	})
	runs := make([]WorkflowRun, 0, len(ids))
	for _, id := range ids {
		run, err := workflowRunFromRecords(projectID, id, tasks, decisions)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}

func workflowRunFromRecords(projectID int64, runID string, allTasks []Task, decisions []Decision) (WorkflowRun, error) {
	run := WorkflowRun{ID: runID, Status: "active", Tasks: []Task{}, Advances: []WorkflowAdvanceRecord{}}
	for _, task := range allTasks {
		if task.ProjectID != projectID {
			continue
		}
		plan, err := task.AdmittedPlan()
		if err != nil {
			return WorkflowRun{}, err
		}
		if plan == nil || plan.Workflow == nil || plan.Workflow.RunID != runID {
			continue
		}
		if run.Profile.Name == "" {
			run.Profile = *plan.Workflow
		} else if run.Profile.Name != plan.Workflow.Name || run.Profile.SHA256 != plan.Workflow.SHA256 || !sameWorkflowStages(run.Profile.Profile, plan.Workflow.Profile) {
			return WorkflowRun{}, fmt.Errorf("workflow run %s contains conflicting frozen profiles", runID)
		}
		run.Tasks = append(run.Tasks, task)
	}
	if len(run.Tasks) == 0 {
		return WorkflowRun{}, ErrWorkflowRunUnknown
	}
	for _, decision := range decisions {
		if decision.ProjectID != projectID || decision.Kind != "workflow_stage" || decision.Answer != "advance" || decision.ObsoleteAt != 0 {
			continue
		}
		advance, err := ParseWorkflowDecisionOrigin(decision.Origin)
		if err != nil {
			return WorkflowRun{}, fmt.Errorf("invalid workflow Decision %d: %w", decision.ID, err)
		}
		if advance.UserQuote != decision.UserQuote {
			return WorkflowRun{}, fmt.Errorf("workflow Decision %d quote does not match its origin: %w", decision.ID, ErrWorkflowAdvanceInvalid)
		}
		if advance.RunID != runID {
			continue
		}
		run.Advances = append(run.Advances, WorkflowAdvanceRecord{DecisionID: decision.ID, FromStage: advance.FromStage, FromIndex: advance.FromIndex,
			ToStage: advance.ToStage, ToIndex: advance.ToIndex, Results: advance.Results, ResultSHA256: advance.ResultSHA256,
			UserQuote: decision.UserQuote, AnsweredAt: decision.AnsweredAt})
	}
	current := 0
	for _, advance := range run.Advances {
		if advance.FromIndex != current || advance.ToIndex != current+1 || advance.FromIndex >= len(run.Profile.Profile.Stages) || advance.ToIndex >= len(run.Profile.Profile.Stages) ||
			advance.FromStage != run.Profile.Profile.Stages[advance.FromIndex].Name || advance.ToStage != run.Profile.Profile.Stages[advance.ToIndex].Name {
			return WorkflowRun{}, fmt.Errorf("workflow run %s has a non-sequential stage Decision", runID)
		}
		current = advance.ToIndex
	}
	if current >= len(run.Profile.Profile.Stages) {
		return WorkflowRun{}, fmt.Errorf("workflow run %s has an invalid current stage", runID)
	}
	run.CurrentStageIndex = current
	run.CurrentStage = run.Profile.Profile.Stages[current].Name
	if current == len(run.Profile.Profile.Stages)-1 && run.Profile.Profile.Stages[current].TaskType == "none" && workflowNoneStageComplete(run.Tasks) {
		run.Status = "complete"
	}
	return run, nil
}

func workflowNoneStageComplete(tasks []Task) bool {
	for _, task := range tasks {
		if task.Type == "change" && task.State != StateMerged && task.State != StateTornDown {
			return false
		}
	}
	return true
}

func sameWorkflowStages(a, b config.WorkflowProfile) bool {
	left, err := config.WorkflowDigest(a)
	if err != nil {
		return false
	}
	right, err := config.WorkflowDigest(b)
	return err == nil && left == right
}

func workflowCurrentStageTx(ctx context.Context, tx *sql.Tx, projectID int64, runID string, profile config.WorkflowSelection) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT origin,answered_at,answer,obsolete_at FROM decisions WHERE project_id=? AND kind='workflow_stage' ORDER BY id`, projectID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	current := 0
	for rows.Next() {
		var origin, answer string
		var answeredAt, obsoleteAt int64
		if err := rows.Scan(&origin, &answeredAt, &answer, &obsoleteAt); err != nil {
			return 0, err
		}
		if answeredAt == 0 || answer != "advance" || obsoleteAt != 0 {
			continue
		}
		advance, err := ParseWorkflowDecisionOrigin(origin)
		if err != nil {
			return 0, err
		}
		if advance.RunID != runID {
			continue
		}
		if advance.FromIndex != current || advance.ToIndex != current+1 || advance.FromIndex >= len(profile.Profile.Stages) || advance.ToIndex >= len(profile.Profile.Stages) ||
			advance.FromStage != profile.Profile.Stages[advance.FromIndex].Name || advance.ToStage != profile.Profile.Stages[advance.ToIndex].Name {
			return 0, fmt.Errorf("workflow run %s has a non-sequential stage Decision", runID)
		}
		current = advance.ToIndex
	}
	return current, rows.Err()
}

func validateWorkflowTaskAdmissionTx(ctx context.Context, tx *sql.Tx, projectID int64, taskType string, selection *config.WorkflowSelection) error {
	if selection == nil || selection.RunID == "" || selection.RunID == "new" {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT plan_json FROM tasks WHERE project_id=? AND plan_json<>'' ORDER BY seq`, projectID)
	if err != nil {
		return err
	}
	var frozen *config.WorkflowSelection
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			rows.Close()
			return err
		}
		plan, err := taskplan.Decode(data)
		if err != nil {
			rows.Close()
			return err
		}
		if plan.Workflow == nil || plan.Workflow.RunID != selection.RunID {
			continue
		}
		if frozen == nil {
			copy := *plan.Workflow
			frozen = &copy
		} else if frozen.Name != plan.Workflow.Name || frozen.SHA256 != plan.Workflow.SHA256 || !sameWorkflowStages(frozen.Profile, plan.Workflow.Profile) {
			rows.Close()
			return fmt.Errorf("workflow run %s contains conflicting frozen profiles", selection.RunID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if frozen == nil {
		return ErrWorkflowRunUnknown
	}
	if frozen.Name != selection.Name || frozen.SHA256 != selection.SHA256 || !sameWorkflowStages(frozen.Profile, selection.Profile) {
		return ErrWorkflowProfileChanged
	}
	current, err := workflowCurrentStageTx(ctx, tx, projectID, selection.RunID, *frozen)
	if err != nil {
		return err
	}
	if current >= len(frozen.Profile.Stages) {
		return ErrWorkflowRunComplete
	}
	stage := frozen.Profile.Stages[current]
	if stage.TaskType == "none" {
		return ErrWorkflowStageNoTask
	}
	if selection.StageIndex != current || selection.StageName != stage.Name {
		return ErrWorkflowStageNotCurrent
	}
	if stage.TaskType != taskType {
		return ErrWorkflowStageTypeMismatch
	}
	return nil
}

func nextWorkflowRunIDTx(ctx context.Context, tx *sql.Tx, projectID int64) (string, error) {
	var next int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(CASE WHEN json_valid(plan_json) AND json_extract(plan_json,'$.workflow.run_id') GLOB 'w[0-9]*'
		THEN CAST(substr(json_extract(plan_json,'$.workflow.run_id'),2) AS INTEGER) ELSE 0 END),0)+1
		FROM tasks WHERE project_id=?`, projectID).Scan(&next)
	if err != nil {
		return "", err
	}
	return "w" + strconv.FormatInt(next, 10), nil
}

func validateWorkflowResultCoverageTx(ctx context.Context, tx *sql.Tx, projectID int64, advance WorkflowAdvance, stage config.WorkflowStage) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,type,plan_json FROM tasks WHERE project_id=? AND plan_json<>'' ORDER BY seq`, projectID)
	if err != nil {
		return err
	}
	var expected []int64
	for rows.Next() {
		var taskID int64
		var taskType, data string
		if err := rows.Scan(&taskID, &taskType, &data); err != nil {
			rows.Close()
			return err
		}
		plan, err := taskplan.Decode(data)
		if err != nil {
			rows.Close()
			return err
		}
		if plan.Workflow == nil || plan.Workflow.RunID != advance.RunID {
			continue
		}
		if stage.TaskType == "none" {
			if taskType == "change" {
				expected = append(expected, taskID)
			}
		} else if plan.Workflow.StageIndex == advance.FromIndex {
			expected = append(expected, taskID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	slices.Sort(expected)
	if len(expected) != len(advance.Results) {
		return ErrWorkflowAdvanceInvalid
	}
	for index, result := range advance.Results {
		if expected[index] != result.TaskID {
			return ErrWorkflowAdvanceInvalid
		}
	}
	return nil
}

func (db *DB) RecordWorkflowAdvance(ctx context.Context, projectID, taskID int64, advance WorkflowAdvance, userQuote string) (Decision, error) {
	if strings.TrimSpace(userQuote) == "" || advance.UserQuote != userQuote {
		return Decision{}, fmt.Errorf("User quote is required and must match the workflow stage Decision")
	}
	origin, err := WorkflowDecisionOrigin(advance)
	if err != nil {
		return Decision{}, err
	}
	optionsJSON, _ := json.Marshal([]string{"advance", "stay"})
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback()
	var profileJSON string
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT plan_json,state FROM tasks WHERE id=? AND project_id=?`, taskID, projectID).Scan(&profileJSON, &state); err != nil {
		return Decision{}, err
	}
	anchorPlan, err := taskplan.Decode(profileJSON)
	if err != nil || anchorPlan.Workflow == nil || anchorPlan.Workflow.RunID != advance.RunID {
		return Decision{}, ErrWorkflowRunUnknown
	}
	if advance.FromIndex >= len(anchorPlan.Workflow.Profile.Stages) || advance.ToIndex >= len(anchorPlan.Workflow.Profile.Stages) ||
		advance.FromStage != anchorPlan.Workflow.Profile.Stages[advance.FromIndex].Name || advance.ToStage != anchorPlan.Workflow.Profile.Stages[advance.ToIndex].Name {
		return Decision{}, ErrWorkflowAdvanceInvalid
	}
	current, err := workflowCurrentStageTx(ctx, tx.Tx, projectID, advance.RunID, *anchorPlan.Workflow)
	if err != nil {
		return Decision{}, err
	}
	if existing, existingErr := decisionRow(tx.QueryRowContext(ctx, decisionSelect+` WHERE project_id=? AND origin=?`, projectID, origin)); existingErr == nil {
		if existing.Kind != "workflow_stage" || existing.Answer != "advance" {
			return Decision{}, ErrWorkflowAdvanceInvalid
		}
		if err := tx.Commit(); err != nil {
			return Decision{}, err
		}
		if err := db.PersistProject(ctx, projectID); err != nil {
			return Decision{}, err
		}
		return existing, nil
	} else if !errors.Is(existingErr, ErrNotFound) {
		return Decision{}, existingErr
	}
	if current != advance.FromIndex || advance.ToIndex != current+1 {
		return Decision{}, ErrWorkflowStageNotCurrent
	}
	if advance.FromIndex >= len(anchorPlan.Workflow.Profile.Stages)-1 || advance.ToIndex >= len(anchorPlan.Workflow.Profile.Stages) {
		return Decision{}, ErrWorkflowAdvanceInvalid
	}
	anchorTaskPlan := anchorPlan.Workflow
	if advance.FromStage != anchorTaskPlan.Profile.Stages[advance.FromIndex].Name || advance.ToStage != anchorTaskPlan.Profile.Stages[advance.ToIndex].Name {
		return Decision{}, ErrWorkflowAdvanceInvalid
	}
	fromStage := anchorTaskPlan.Profile.Stages[advance.FromIndex]
	if fromStage.TaskType == "none" {
		if anchorTaskPlan.StageIndex >= advance.FromIndex {
			return Decision{}, ErrWorkflowAdvanceInvalid
		}
	} else if anchorTaskPlan.StageIndex != advance.FromIndex {
		return Decision{}, ErrWorkflowAdvanceInvalid
	}
	if err := validateWorkflowResultCoverageTx(ctx, tx.Tx, projectID, advance, fromStage); err != nil {
		return Decision{}, err
	}
	var taskSeq int
	if err := tx.QueryRowContext(ctx, `SELECT seq FROM tasks WHERE id=?`, taskID).Scan(&taskSeq); err != nil {
		return Decision{}, err
	}
	question := fmt.Sprintf("User approved workflow run %s to advance from %s to %s using result digest %s", advance.RunID, advance.FromStage, advance.ToStage, advance.ResultSHA256)
	at := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `INSERT INTO decisions(project_id,task_id,origin,question,options_json,answer,user_quote,created_at,answered_at,kind,task_launches)
		SELECT ?,?,?,?,?, 'advance',?,?,?, 'workflow_stage', launches FROM tasks WHERE id=? AND project_id=?`, projectID, taskID, origin, question, string(optionsJSON), userQuote, at, at, taskID, projectID)
	if err != nil {
		return Decision{}, err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return Decision{}, err
		}
		return Decision{}, ErrNotFound
	}
	decision, err := decisionRow(tx.QueryRowContext(ctx, decisionSelect+` WHERE project_id=? AND origin=?`, projectID, origin))
	if err != nil {
		return Decision{}, err
	}
	data, err := json.Marshal(map[string]any{"decision_id": decision.ID, "option": decision.Answer, "user_quote": userQuote})
	if err != nil {
		return Decision{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at) VALUES(?,?,'decision_answered',?,?,?)`, projectID, taskID, question, string(data), at); err != nil {
		return Decision{}, err
	}
	if err := tx.Commit(); err != nil {
		return Decision{}, err
	}
	if err := db.PersistProject(ctx, projectID); err != nil {
		return Decision{}, err
	}
	return decision, nil
}
