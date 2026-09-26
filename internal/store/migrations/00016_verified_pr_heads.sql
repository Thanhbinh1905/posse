-- +goose Up
CREATE TABLE verified_pr_heads (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    pr_url TEXT NOT NULL,
    head_sha TEXT NOT NULL,
    PRIMARY KEY (task_id, pr_url, head_sha)
);

-- +goose Down
DROP TABLE verified_pr_heads;
