-- +goose Up
ALTER TABLE decisions ADD COLUMN kind TEXT NOT NULL DEFAULT '';
ALTER TABLE decisions ADD COLUMN task_launches INTEGER NOT NULL DEFAULT 0;
ALTER TABLE decisions ADD COLUMN obsolete_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE decisions ADD COLUMN obsolete_reason TEXT NOT NULL DEFAULT '';
CREATE TABLE decision_notice_cursors (
    project_id INTEGER PRIMARY KEY REFERENCES projects(id),
    last_notice_id INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX decisions_one_pending_recovery_idx ON decisions(task_id)
    WHERE kind='recovery' AND answered_at=0 AND obsolete_at=0;

-- +goose Down
DROP INDEX decisions_one_pending_recovery_idx;
DROP TABLE decision_notice_cursors;
ALTER TABLE decisions DROP COLUMN obsolete_reason;
ALTER TABLE decisions DROP COLUMN obsolete_at;
ALTER TABLE decisions DROP COLUMN task_launches;
ALTER TABLE decisions DROP COLUMN kind;
