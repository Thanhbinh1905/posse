-- +goose Up
ALTER TABLE notices ADD COLUMN claim_token TEXT NOT NULL DEFAULT '';
ALTER TABLE notices ADD COLUMN claimed_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN claim_token TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN claimed_at INTEGER NOT NULL DEFAULT 0;

CREATE INDEX notices_claim_idx ON notices(project_id, claim_token, delivered_at, acked_at);
CREATE INDEX messages_claim_idx ON messages(task_id, status, claimed_at);

-- +goose Down
DROP INDEX messages_claim_idx;
DROP INDEX notices_claim_idx;
ALTER TABLE messages DROP COLUMN claimed_at;
ALTER TABLE messages DROP COLUMN claim_token;
ALTER TABLE notices DROP COLUMN claimed_at;
ALTER TABLE notices DROP COLUMN claim_token;
