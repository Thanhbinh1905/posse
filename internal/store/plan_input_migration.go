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

// MigrateTaskPlanInputsBatch externalizes a bounded number of admitted plans.
// Inline plans remain readable until their row is converted.
func (db *DB) MigrateTaskPlanInputsBatch(ctx context.Context) error {
	complete, err := db.taskPlanInputMigrationComplete(ctx)
	if err != nil || complete {
		return err
	}
	lock, err := awaitMigrationLock(ctx, db.Path)
	if err != nil {
		return err
	}
	defer lock.Close()

	var taskDone, revisionDone int
	var taskCursor, revisionTaskCursor int64
	var revisionCursor int
	if err := db.QueryRowContext(ctx, `SELECT tasks_done,revisions_done,task_cursor,revision_task_cursor,revision_cursor FROM task_plan_input_migration WHERE id=1`).Scan(&taskDone, &revisionDone, &taskCursor, &revisionTaskCursor, &revisionCursor); err != nil {
		return err
	}
	if taskDone == 0 {
		if err := db.migrateTaskPlanRowsBatch(ctx, taskCursor); err != nil {
			return err
		}
	}
	if revisionDone == 0 {
		if err := db.migrateTaskPlanRevisionRowsBatch(ctx, revisionTaskCursor, revisionCursor); err != nil {
			return err
		}
	}
	_, err = db.ExecContext(ctx, `UPDATE task_plan_input_migration SET completed=1 WHERE id=1 AND tasks_done=1 AND revisions_done=1`)
	return err
}

func (db *DB) migrateTaskPlanInputs(ctx context.Context) error {
	for {
		complete, err := db.taskPlanInputMigrationComplete(ctx)
		if err != nil || complete {
			return err
		}
		if err := db.MigrateTaskPlanInputsBatch(ctx); err != nil {
			return err
		}
	}
}

func (db *DB) migrateTaskPlanRowsBatch(ctx context.Context, cursor int64) error {
	rows, err := db.QueryContext(ctx, `SELECT id,plan_json FROM tasks WHERE id>? AND plan_json<>'' ORDER BY id LIMIT ?`, cursor, planInputMigrationBatchSize)
	if err != nil {
		return err
	}
	type row struct {
		id   int64
		data string
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
	rowsErr, closeErr := rows.Err(), rows.Close()
	if rowsErr != nil {
		return rowsErr
	}
	if closeErr != nil {
		return closeErr
	}
	type convertedRow struct {
		row
		next string
	}
	converted := make([]convertedRow, 0, len(batch))
	for _, item := range batch {
		next, err := db.externalizePlanJSON(item.data)
		if err != nil {
			return fmt.Errorf("externalize Task %d plan inputs: %w", item.id, err)
		}
		converted = append(converted, convertedRow{row: item, next: next})
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, item := range converted {
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET plan_json=?,workflow_run_id=?,plan_revision=? WHERE id=? AND plan_json=?`, item.next, storedWorkflowRunID(item.next), storedPlanRevision(item.next), item.id, item.data); err != nil {
			return err
		}
		cursor = item.id
	}
	if len(batch) < planInputMigrationBatchSize {
		if _, err := tx.ExecContext(ctx, `UPDATE task_plan_input_migration SET tasks_done=1,task_cursor=? WHERE id=1`, cursor); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE task_plan_input_migration SET task_cursor=? WHERE id=1`, cursor); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) migrateTaskPlanRevisionRowsBatch(ctx context.Context, taskCursor int64, revisionCursor int) error {
	rows, err := db.QueryContext(ctx, `SELECT task_id,revision,plan_json FROM task_plan_revisions WHERE task_id>? OR (task_id=? AND revision>?) ORDER BY task_id,revision LIMIT ?`, taskCursor, taskCursor, revisionCursor, planInputMigrationBatchSize)
	if err != nil {
		return err
	}
	type row struct {
		taskID   int64
		revision int
		data     string
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
	rowsErr, closeErr := rows.Err(), rows.Close()
	if rowsErr != nil {
		return rowsErr
	}
	if closeErr != nil {
		return closeErr
	}
	type convertedRow struct {
		row
		next string
	}
	converted := make([]convertedRow, 0, len(batch))
	for _, item := range batch {
		next, err := db.externalizePlanJSON(item.data)
		if err != nil {
			return fmt.Errorf("externalize Task %d plan revision %d inputs: %w", item.taskID, item.revision, err)
		}
		converted = append(converted, convertedRow{row: item, next: next})
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, item := range converted {
		if _, err := tx.ExecContext(ctx, `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=? AND revision=? AND plan_json=?`, item.next, item.taskID, item.revision, item.data); err != nil {
			return err
		}
		taskCursor, revisionCursor = item.taskID, item.revision
	}
	if len(batch) < planInputMigrationBatchSize {
		if _, err := tx.ExecContext(ctx, `UPDATE task_plan_input_migration SET revisions_done=1,revision_task_cursor=?,revision_cursor=? WHERE id=1`, taskCursor, revisionCursor); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE task_plan_input_migration SET revision_task_cursor=?,revision_cursor=? WHERE id=1`, taskCursor, revisionCursor); err != nil {
		return err
	}
	return tx.Commit()
}
