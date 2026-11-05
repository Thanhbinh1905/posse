-- +goose Up
CREATE TABLE review_cap_grants (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id),
    decision_id INTEGER NOT NULL REFERENCES decisions(id),
    round INTEGER NOT NULL CHECK(round > 0),
    result_json TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
-- +goose StatementBegin
CREATE TRIGGER review_cap_grants_immutable BEFORE UPDATE ON review_cap_grants
BEGIN SELECT RAISE(ABORT, 'Review cap grant is immutable'); END;
-- +goose StatementEnd

-- +goose Down
DROP TABLE review_cap_grants;
