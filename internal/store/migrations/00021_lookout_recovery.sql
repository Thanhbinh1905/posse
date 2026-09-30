-- +goose Up
ALTER TABLE project_runtime ADD COLUMN lookout_pane_id TEXT NOT NULL DEFAULT '';
ALTER TABLE project_runtime ADD COLUMN lookout_retry_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE project_runtime ADD COLUMN lookout_failed_starts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE project_runtime ADD COLUMN lookout_notice_raised INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE project_runtime DROP COLUMN lookout_notice_raised;
ALTER TABLE project_runtime DROP COLUMN lookout_failed_starts;
ALTER TABLE project_runtime DROP COLUMN lookout_retry_at;
ALTER TABLE project_runtime DROP COLUMN lookout_pane_id;
