-- +goose Up
ALTER TABLE verdicts ADD COLUMN scope_revision INTEGER NOT NULL DEFAULT 0;

CREATE VIEW current_verdicts AS
SELECT v.* FROM verdicts v JOIN tasks t ON t.id = v.task_id
WHERE v.provider <> 'greenhouse'
   OR v.scope_revision = COALESCE(json_extract(NULLIF(t.plan_json, ''), '$.scope_amendments[#-1].revision'), 0);

-- +goose Down
DROP VIEW current_verdicts;
ALTER TABLE verdicts DROP COLUMN scope_revision;
