package store

import (
	"context"
	"time"
)

// CreateUpdateNoticeOnce deduplicates by version across concurrent posse calls.
func (db *DB) CreateUpdateNoticeOnce(ctx context.Context, projectID int64, version, summary string) error {
	data := `{"version":"` + version + `"}` // Version is validated as vX.Y.Z by the release client.
	result, err := db.ExecContext(ctx, `INSERT INTO notices(project_id,kind,summary,data_json,created_at)
 SELECT ?, 'update_available', ?, ?, ? WHERE NOT EXISTS
 (SELECT 1 FROM notices WHERE project_id=? AND kind='update_available' AND data_json=?)`, projectID, summary, data, time.Now().UnixMilli(), projectID, data)
	if err != nil {
		return err
	}
	created, err := result.RowsAffected()
	if err != nil || created == 0 {
		return err
	}
	return db.PersistProject(ctx, projectID)
}
