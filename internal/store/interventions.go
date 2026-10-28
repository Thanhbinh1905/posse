package store

import (
	"context"
	"time"
)

const (
	InterventionSend       = "send"
	InterventionInterrupt  = "interrupt"
	InterventionKeys       = "keys"
	InterventionRelaunch   = "relaunch"
	InterventionLeadLaunch = "lead_launch"
)

type Intervention struct {
	ID        int64
	ProjectID int64
	TaskID    int64
	Kind      string
	Reason    string
	At        int64
}

// RecordIntervention stores one steering action. A zero taskID marks a
// Project-level action such as a Lead launch.
func (db *DB) RecordIntervention(ctx context.Context, projectID, taskID int64, kind, reason string) error {
	var task any
	if taskID != 0 {
		task = taskID
	}
	_, err := db.ExecContext(ctx, `INSERT INTO interventions(project_id,task_id,kind,reason,at) VALUES(?,?,?,?,?)`, projectID, task, kind, reason, time.Now().UnixMilli())
	return err
}

func (db *DB) Interventions(ctx context.Context, projectID int64, kind string) ([]Intervention, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,project_id,COALESCE(task_id,0),kind,reason,at FROM interventions WHERE project_id=? AND kind=? ORDER BY id`, projectID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Intervention
	for rows.Next() {
		var row Intervention
		if err := rows.Scan(&row.ID, &row.ProjectID, &row.TaskID, &row.Kind, &row.Reason, &row.At); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
