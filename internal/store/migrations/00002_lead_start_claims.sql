-- +goose Up
CREATE TABLE lead_start_claims (
    project_id INTEGER PRIMARY KEY REFERENCES projects(id),
    claimed_at INTEGER NOT NULL
);

-- +goose Down
DROP TABLE lead_start_claims;
