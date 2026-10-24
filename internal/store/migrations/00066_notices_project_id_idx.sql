-- +goose Up
-- Project snapshots keep a bounded tail of recent Notices. Without an index
-- ordered by id within a Project, that tail sorted every Notice of the Project.
CREATE INDEX notices_project_id_idx ON notices(project_id, id);
-- Every reconciliation asks each held Sandbox for its latest missing or
-- recovered Notice. Notices are otherwise unindexed by Task.
CREATE INDEX notices_sandbox_episode_idx ON notices(task_id, id) WHERE kind IN ('sandbox_missing','sandbox_recovered');

-- +goose Down
DROP INDEX notices_sandbox_episode_idx;
DROP INDEX notices_project_id_idx;
