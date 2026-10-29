package store

import (
	"context"
	"time"
)

// RecordTaskSkillUse counts one invocation of skill by the Worker of a Task.
func (db *DB) RecordTaskSkillUse(ctx context.Context, taskID int64, skill string) error {
	now := time.Now().UnixMilli()
	_, err := db.ExecContext(ctx, `INSERT INTO task_skill_uses(task_id, skill, uses, first_at, last_at) VALUES (?,?,1,?,?)
		ON CONFLICT(task_id, skill) DO UPDATE SET uses=uses+1, last_at=excluded.last_at`, taskID, skill, now, now)
	return err
}

// TaskSkillUses maps each skill the Worker of a Task invoked to its count.
func (db *DB) TaskSkillUses(ctx context.Context, taskID int64) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, `SELECT skill, uses FROM task_skill_uses WHERE task_id=?`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	uses := map[string]int{}
	for rows.Next() {
		var skill string
		var count int
		if err := rows.Scan(&skill, &count); err != nil {
			return nil, err
		}
		uses[skill] = count
	}
	return uses, rows.Err()
}
