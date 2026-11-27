package store

import (
	"context"
	"encoding/json"
	"time"
)

const codexInterruptRejectedNotice = "codex_interrupt_rejected"

func (db *DB) CodexInterruptRejectedNoticeIdleSince(ctx context.Context, taskID, messageID int64) (int64, error) {
	var idleSince int64
	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(json_extract(data_json,'$.idle_since')),0) FROM notices WHERE task_id=? AND kind=? AND json_extract(data_json,'$.message_id')=?`, taskID, codexInterruptRejectedNotice, messageID).Scan(&idleSince)
	return idleSince, err
}

func (db *DB) CreateCodexInterruptRejectedNotice(ctx context.Context, task Task, messageID, idleSince int64, turnID, summary string, now time.Time) (*Notice, error) {
	data, err := json.Marshal(struct {
		Launch    int    `json:"launch"`
		IdleSince int64  `json:"idle_since"`
		MessageID int64  `json:"message_id"`
		TurnID    string `json:"turn_id"`
	}{task.Launches, idleSince, messageID, turnID})
	if err != nil {
		return nil, err
	}
	notice := Notice{ProjectID: task.ProjectID, TaskID: task.ID, Kind: codexInterruptRejectedNotice, Summary: summary, DataJSON: string(data), CreatedAt: now.UnixMilli()}
	err = db.QueryRowContext(ctx, `INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at)
SELECT t.project_id,t.id,?,?,?,?
FROM tasks t JOIN task_turn_tokens tok ON tok.task_id=t.id
WHERE t.id=? AND t.project_id=? AND t.launches=? AND t.pane_id=? AND t.idle_since=? AND t.state='working' AND tok.token='message:' || ?
AND NOT EXISTS (SELECT 1 FROM notices n WHERE n.task_id=t.id AND n.kind=? AND json_extract(n.data_json,'$.message_id')=?)
RETURNING id`, notice.Kind, notice.Summary, notice.DataJSON, notice.CreatedAt, task.ID, task.ProjectID, task.Launches, task.PaneID, idleSince, messageID, notice.Kind, messageID).Scan(&notice.ID)
	if IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &notice, db.PersistProject(ctx, task.ProjectID)
}
