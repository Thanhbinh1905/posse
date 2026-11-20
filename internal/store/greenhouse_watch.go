package store

import "context"

type GreenhouseTaskWatch struct {
	TaskID          int64
	RunID           string
	Status          string
	Stage           string
	RunObservedAt   int64
	StageObservedAt int64
	RunStartedAt    int64
	StageStartedAt  int64
	StageFinishedAt int64
	UpdatedAt       int64
	PaneID          string
}

func (db *DB) GreenhouseTaskWatch(ctx context.Context, taskID int64) (GreenhouseTaskWatch, error) {
	var watch GreenhouseTaskWatch
	err := db.QueryRowContext(ctx, `SELECT task_id,run_id,status,stage,run_observed_at,stage_observed_at,run_started_at,stage_started_at,stage_finished_at,updated_at,pane_id FROM greenhouse_task_watches WHERE task_id=?`, taskID).
		Scan(&watch.TaskID, &watch.RunID, &watch.Status, &watch.Stage, &watch.RunObservedAt, &watch.StageObservedAt, &watch.RunStartedAt, &watch.StageStartedAt, &watch.StageFinishedAt, &watch.UpdatedAt, &watch.PaneID)
	return watch, err
}

func (db *DB) SaveGreenhouseTaskWatch(ctx context.Context, watch GreenhouseTaskWatch) error {
	_, err := db.ExecContext(ctx, `INSERT INTO greenhouse_task_watches(task_id,run_id,status,stage,run_observed_at,stage_observed_at,run_started_at,stage_started_at,stage_finished_at,updated_at,pane_id) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(task_id) DO UPDATE SET run_id=excluded.run_id,status=excluded.status,stage=excluded.stage,run_observed_at=excluded.run_observed_at,stage_observed_at=excluded.stage_observed_at,run_started_at=excluded.run_started_at,stage_started_at=excluded.stage_started_at,stage_finished_at=excluded.stage_finished_at,updated_at=excluded.updated_at,pane_id=excluded.pane_id`,
		watch.TaskID, watch.RunID, watch.Status, watch.Stage, watch.RunObservedAt, watch.StageObservedAt, watch.RunStartedAt, watch.StageStartedAt, watch.StageFinishedAt, watch.UpdatedAt, watch.PaneID)
	return err
}
