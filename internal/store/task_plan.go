package store

import (
	"context"
	"fmt"
	"time"

	"github.com/thanhbinh1905/posse/internal/taskplan"
)

// AdmittedPlan returns nil for a historical Task. Never reconstruct a missing
// plan from today's configuration or infer historical identity.
func (t Task) AdmittedPlan() (*taskplan.Plan, error) {
	if t.PlanJSON == "" {
		return nil, nil
	}
	plan, err := taskplan.Decode(t.PlanJSON)
	if err != nil {
		return nil, err
	}
	return &plan, nil
}

func bindTaskPlan(task Task, sequence int, workflowRunID string) (string, error) {
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil {
		return "", err
	}
	if plan.Revision != 1 || plan.PreviousSHA256 != "" {
		return "", fmt.Errorf("new Task requires execution plan revision 1")
	}
	if plan.Workflow != nil && plan.Workflow.RunID == "new" {
		if workflowRunID == "" {
			return "", fmt.Errorf("new workflow run identity must be assigned during Task admission")
		}
		plan.Workflow.RunID = workflowRunID
	}
	return plan.Bind(fmt.Sprintf("t%d", sequence)).Encode()
}

func (db *DB) TaskPlanRevisions(ctx context.Context, taskID int64) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT plan_json FROM task_plan_revisions WHERE task_id=? ORDER BY revision`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var plans []string
	for rows.Next() {
		var plan string
		if err := rows.Scan(&plan); err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, rows.Err()
}

// ReviseTaskPlan compares the admitted revision before recording an explicit
// launch reconfiguration. Gate evidence is invalidated in the same transaction;
// old revisions and Signals remain history, not evidence for the new revision.
func (db *DB) ReviseTaskPlan(ctx context.Context, task Task, plan taskplan.Plan) error {
	previous, err := task.AdmittedPlan()
	if err != nil {
		return err
	}
	if previous == nil || plan.Revision != previous.Revision+1 || plan.PreviousSHA256 != previous.SHA256() {
		return fmt.Errorf("execution plan revision does not extend the admitted plan")
	}
	data, err := plan.Encode()
	if err != nil {
		return err
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET plan_json=?, profile=?, dispatch_rule=?, gated_sha='', updated_at=? WHERE id=? AND plan_json=? AND launches=? AND state=?`,
		data, plan.Launch.ProfileName, plan.Launch.DispatchRule, time.Now().UnixMilli(), task.ID, task.PlanJSON, task.Launches, task.State)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil {
		return err
	} else if count != 1 {
		return ErrStateRace
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,?,?)`, task.ID, plan.Revision, data); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_repos SET gated_sha='' WHERE task_id=?`, task.ID); err != nil {
		return err
	}
	note := fmt.Sprintf("Execution plan revised from %d to %d; prior revision evidence invalidated", previous.Revision, plan.Revision)
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions(task_id,from_state,to_state,source,note,at) VALUES(?,?,?,?,?,?)`, task.ID, task.State, task.State, "lead", note, time.Now().UnixMilli()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, task.ID)
}
