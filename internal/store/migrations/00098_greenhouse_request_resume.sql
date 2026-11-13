-- +goose Up
DROP TRIGGER greenhouse_review_requests_immutable;
DROP TRIGGER greenhouse_review_requests_no_delete;
UPDATE greenhouse_review_requests AS keeper
SET settled_at = (SELECT settled_at FROM greenhouse_review_requests latest WHERE latest.task_id=keeper.task_id AND latest.repo=keeper.repo AND latest.grant_id=keeper.grant_id ORDER BY latest.id DESC LIMIT 1),
    outcome = (SELECT outcome FROM greenhouse_review_requests latest WHERE latest.task_id=keeper.task_id AND latest.repo=keeper.repo AND latest.grant_id=keeper.grant_id ORDER BY latest.id DESC LIMIT 1),
    verdict_id = (SELECT verdict_id FROM greenhouse_review_requests latest WHERE latest.task_id=keeper.task_id AND latest.repo=keeper.repo AND latest.grant_id=keeper.grant_id ORDER BY latest.id DESC LIMIT 1)
WHERE keeper.grant_id IS NOT NULL AND keeper.id=(SELECT MIN(first.id) FROM greenhouse_review_requests first WHERE first.task_id=keeper.task_id AND first.repo=keeper.repo AND first.grant_id=keeper.grant_id)
  AND EXISTS (SELECT 1 FROM greenhouse_review_requests duplicate WHERE duplicate.task_id=keeper.task_id AND duplicate.repo=keeper.repo AND duplicate.grant_id=keeper.grant_id GROUP BY duplicate.task_id,duplicate.repo,duplicate.grant_id HAVING COUNT(*) > 1);
DELETE FROM greenhouse_review_requests
WHERE grant_id IS NOT NULL AND id NOT IN (SELECT MIN(id) FROM greenhouse_review_requests WHERE grant_id IS NOT NULL GROUP BY task_id,repo,grant_id);
CREATE UNIQUE INDEX IF NOT EXISTS greenhouse_review_requests_grant_idx ON greenhouse_review_requests(task_id, repo, grant_id) WHERE grant_id IS NOT NULL;
CREATE TRIGGER greenhouse_review_requests_no_delete BEFORE DELETE ON greenhouse_review_requests
BEGIN SELECT RAISE(ABORT, 'Greenhouse review request is insert-only'); END;
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
DROP TRIGGER greenhouse_review_requests_no_delete;
CREATE TRIGGER greenhouse_review_requests_immutable BEFORE UPDATE ON greenhouse_review_requests
BEGIN SELECT RAISE(ABORT, 'Greenhouse review request is immutable'); END;
CREATE TRIGGER greenhouse_review_requests_no_delete BEFORE DELETE ON greenhouse_review_requests
BEGIN SELECT RAISE(ABORT, 'Greenhouse review request is insert-only'); END;
DROP INDEX greenhouse_review_requests_grant_idx;
