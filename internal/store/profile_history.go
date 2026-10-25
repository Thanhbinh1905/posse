package store

import "context"

// TaskHistory summarizes one Task's recorded transitions.
type TaskHistory struct {
	// FirstDoneAt is the first transition into done in Unix milliseconds, or 0.
	FirstDoneAt int64
	// Landed is true when a transition recorded a Merge or an accepted Report.
	Landed bool
}

// TaskHistories reads transition summaries for Tasks created at or after
// sinceMillis. Tasks without transitions are absent from the result.
func (db *DB) TaskHistories(ctx context.Context, projectID, sinceMillis int64) (map[int64]TaskHistory, error) {
	rows, err := db.QueryContext(ctx, `SELECT tr.task_id,
  COALESCE(MIN(CASE WHEN tr.to_state='done' THEN tr.at END),0),
  COALESCE(MAX(CASE WHEN tr.to_state IN ('merged','reported') THEN 1 ELSE 0 END),0)
FROM transitions tr JOIN tasks t ON t.id=tr.task_id
WHERE t.project_id=? AND t.created_at>=?
GROUP BY tr.task_id`, projectID, sinceMillis)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	histories := map[int64]TaskHistory{}
	for rows.Next() {
		var id int64
		var history TaskHistory
		var landed int
		if err := rows.Scan(&id, &history.FirstDoneAt, &landed); err != nil {
			return nil, err
		}
		history.Landed = landed == 1
		histories[id] = history
	}
	return histories, rows.Err()
}
