package store

import (
	"context"
	"time"
)

// IdleSlotTasks lists the Change Tasks that hold a Pool slot with nothing
// pending (done or merging, no unacked Notice, open Decision or undelivered
// message) and have shown no activity for idleFor, and the Tasks waiting for a
// slot. It returns no idle Tasks while nothing waits.
func (db *DB) IdleSlotTasks(ctx context.Context, projectID int64, idleFor time.Duration, now time.Time) (idle, waiting []Task, err error) {
	waiting, err = db.TasksInState(ctx, projectID, StateQueued)
	if err != nil || len(waiting) == 0 {
		return nil, nil, err
	}
	rows, err := db.QueryContext(ctx, `
SELECT tasks.id FROM tasks
WHERE tasks.project_id=? AND tasks.type='change' AND tasks.state IN ('done','merging')
AND NOT EXISTS (SELECT 1 FROM notices WHERE notices.task_id=tasks.id AND notices.acked_at IS NULL)
AND NOT EXISTS (SELECT 1 FROM decisions WHERE decisions.task_id=tasks.id AND decisions.answered_at=0)
AND NOT EXISTS (SELECT 1 FROM messages WHERE messages.task_id=tasks.id AND messages.status IN ('queued','claimed','interrupting','submitting'))
AND MAX(
  COALESCE((SELECT MAX(at) FROM transitions WHERE task_id=tasks.id),0),
  COALESCE((SELECT MAX(at) FROM signals WHERE task_id=tasks.id),0),
  COALESCE((SELECT MAX(created_at) FROM messages WHERE task_id=tasks.id),0),
  COALESCE((SELECT MAX(created_at) FROM notices WHERE task_id=tasks.id),0),
  tasks.created_at
) <= ?
ORDER BY tasks.seq`, projectID, now.Add(-idleFor).UnixMilli())
	if err != nil {
		return nil, nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for _, id := range ids {
		task, err := db.TaskByID(ctx, projectID, id)
		if err != nil {
			return nil, nil, err
		}
		idle = append(idle, task)
	}
	return idle, waiting, nil
}
