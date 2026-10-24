-- +goose Up
-- Decision reconciliation looks up merge_ready, review_cap_reached and
-- worker_exited Notices of live Tasks. Without an index by Task, each pass
-- walked every Notice of the Project.
CREATE INDEX notices_task_kind_idx ON notices(task_id, kind);

-- +goose Down
DROP INDEX notices_task_kind_idx;
