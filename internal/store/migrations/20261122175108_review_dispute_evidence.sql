-- +goose Up
CREATE TABLE IF NOT EXISTS decision_evidence (
    decision_id INTEGER PRIMARY KEY REFERENCES decisions(id),
    evidence BLOB NOT NULL,
    digest TEXT NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS decision_evidence;
