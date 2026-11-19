package store

import (
	"context"
	"fmt"
)

const planInputMigrationBatchSize = 25

func (db *DB) taskPlanInputMigrationComplete(ctx context.Context) (bool, error) {
	var completed int
	if err := db.QueryRowContext(ctx, `SELECT completed FROM task_plan_input_migration WHERE id=1`).Scan(&completed); err != nil {
		return false, err
	}
	return completed == 1, nil
}

func (db *DB) migrateTaskPlanInputs(ctx context.Context) error {
	var completed int
	if err := db.QueryRowContext(ctx, `SELECT completed FROM task_plan_input_migration WHERE id=1`).Scan(&completed); err != nil {
		return err
	}
	if completed != 0 {
		return nil
	}
	if err := db.migrateTaskPlanRows(ctx); err != nil {
		return err
	}
	if err := db.migrateTaskPlanRevisionRows(ctx); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET workflow_run_id=COALESCE(json_extract(plan_json,'$.workflow.run_id'),''),plan_revision=COALESCE(json_extract(plan_json,'$.revision'),0) WHERE plan_json<>''`); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `UPDATE task_plan_input_migration SET completed=1 WHERE id=1`)
	return err
}

func (db *DB) migrateTaskPlanRows(ctx context.Context) error {
	var cursor int64
	for {
		rows, err := db.QueryContext(ctx, `SELECT id,plan_json FROM tasks WHERE id>? AND plan_json<>'' ORDER BY id LIMIT ?`, cursor, planInputMigrationBatchSize)
		if err != nil {
			return err
		}
		type row struct {
			id   int64
			data string
			next string
		}
		batch := make([]row, 0, planInputMigrationBatchSize)
		for rows.Next() {
			var item row
			if err := rows.Scan(&item.id, &item.data); err != nil {
				_ = rows.Close()
				return err
			}
			batch = append(batch, item)
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil {
			return rowsErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(batch) == 0 {
			return nil
		}
		tx, err := db.beginTxWithRetry(ctx)
		if err != nil {
			return err
		}
		for i := range batch {
			item := &batch[i]
			item.next, err = db.externalizePlanJSON(item.data)
			if err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("externalize Task %d plan inputs: %w", item.id, err)
			}
			if item.next != item.data {
				if _, err := tx.ExecContext(ctx, `UPDATE tasks SET plan_json=? WHERE id=? AND plan_json=?`, item.next, item.id, item.data); err != nil {
					_ = tx.Rollback()
					return err
				}
			}
			cursor = item.id
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
}

func (db *DB) migrateTaskPlanRevisionRows(ctx context.Context) error {
	var taskCursor int64
	var revisionCursor int
	for {
		rows, err := db.QueryContext(ctx, `SELECT task_id,revision,plan_json FROM task_plan_revisions WHERE task_id>? OR (task_id=? AND revision>?) ORDER BY task_id,revision LIMIT ?`, taskCursor, taskCursor, revisionCursor, planInputMigrationBatchSize)
		if err != nil {
			return err
		}
		type row struct {
			taskID   int64
			revision int
			data     string
			next     string
		}
		batch := make([]row, 0, planInputMigrationBatchSize)
		for rows.Next() {
			var item row
			if err := rows.Scan(&item.taskID, &item.revision, &item.data); err != nil {
				_ = rows.Close()
				return err
			}
			batch = append(batch, item)
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil {
			return rowsErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(batch) == 0 {
			return nil
		}
		tx, err := db.beginTxWithRetry(ctx)
		if err != nil {
			return err
		}
		for i := range batch {
			item := &batch[i]
			item.next, err = db.externalizePlanJSON(item.data)
			if err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("externalize Task %d plan revision %d inputs: %w", item.taskID, item.revision, err)
			}
			if item.next != item.data {
				if _, err := tx.ExecContext(ctx, `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=? AND revision=? AND plan_json=?`, item.next, item.taskID, item.revision, item.data); err != nil {
					_ = tx.Rollback()
					return err
				}
			}
			taskCursor, revisionCursor = item.taskID, item.revision
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
}
