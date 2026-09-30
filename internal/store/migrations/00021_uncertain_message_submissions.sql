-- +goose Up
-- Claims created before the submission marker cannot be distinguished from
-- prompts accepted just before a crash, so preserve them instead of retrying.
UPDATE messages SET status='submitting' WHERE status='claimed';

-- +goose Down
-- Keep the uncertain outcome. Converting it back to a retryable claim could
-- submit the same instruction twice.
SELECT 1;
