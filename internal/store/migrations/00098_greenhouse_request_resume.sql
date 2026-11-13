-- +goose Up
CREATE UNIQUE INDEX IF NOT EXISTS greenhouse_review_requests_grant_idx ON greenhouse_review_requests(task_id, repo, grant_id) WHERE grant_id IS NOT NULL;
DROP TRIGGER greenhouse_review_requests_immutable;
CREATE TRIGGER greenhouse_review_requests_immutable BEFORE UPDATE ON greenhouse_review_requests
WHEN NOT (
    OLD.outcome = 'failed' AND OLD.grant_id IS NOT NULL AND
    NEW.id = OLD.id AND NEW.project_id = OLD.project_id AND NEW.task_id = OLD.task_id AND
    NEW.repo = OLD.repo AND NEW.requested_head = OLD.requested_head AND NEW.provider = OLD.provider AND
    NEW.requested_at = OLD.requested_at AND NEW.grant_id = OLD.grant_id AND
    NEW.settled_at >= OLD.settled_at AND NEW.outcome IN ('failed', 'verdict', 'refused', 'head_mismatch')
)
BEGIN SELECT RAISE(ABORT, 'Greenhouse review request is immutable'); END;

-- +goose Down
DROP TRIGGER greenhouse_review_requests_immutable;
CREATE TRIGGER greenhouse_review_requests_immutable BEFORE UPDATE ON greenhouse_review_requests
BEGIN SELECT RAISE(ABORT, 'Greenhouse review request is immutable'); END;
DROP INDEX greenhouse_review_requests_grant_idx;
