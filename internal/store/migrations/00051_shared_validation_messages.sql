-- +goose Up
-- A coalesced message may represent independent fix rounds for multiple kinds.
CREATE TABLE validation_fix_rounds_new (
    id INTEGER PRIMARY KEY,
    ship_task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('gate', 'ci', 'review', 'freshness')),
    seq INTEGER NOT NULL CHECK (seq > 0),
    expected_base_head TEXT NOT NULL CHECK (length(expected_base_head) IN (40, 64)),
    dedupe_key TEXT NOT NULL,
    message_id INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (ship_task_id, kind, seq),
    UNIQUE (ship_task_id, kind, dedupe_key)
);
INSERT INTO validation_fix_rounds_new(id,ship_task_id,kind,seq,expected_base_head,dedupe_key,message_id,created_at)
SELECT id,ship_task_id,kind,seq,expected_base_head,dedupe_key,message_id,created_at FROM validation_fix_rounds;
DROP TRIGGER validation_fix_rounds_immutable;
DROP INDEX validation_fix_rounds_ship_idx;
DROP TABLE validation_fix_rounds;
ALTER TABLE validation_fix_rounds_new RENAME TO validation_fix_rounds;
CREATE INDEX validation_fix_rounds_ship_idx ON validation_fix_rounds(ship_task_id, kind, seq);
CREATE TRIGGER validation_fix_rounds_immutable BEFORE UPDATE ON validation_fix_rounds
BEGIN SELECT RAISE(ABORT, 'validation fix round is immutable'); END;

-- +goose Down
CREATE TABLE validation_fix_rounds_old (
    id INTEGER PRIMARY KEY,
    ship_task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('gate', 'ci', 'review', 'freshness')),
    seq INTEGER NOT NULL CHECK (seq > 0),
    expected_base_head TEXT NOT NULL CHECK (length(expected_base_head) IN (40, 64)),
    dedupe_key TEXT NOT NULL,
    message_id INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (ship_task_id, kind, seq),
    UNIQUE (ship_task_id, kind, dedupe_key),
    UNIQUE (message_id)
);
INSERT INTO validation_fix_rounds_old(id,ship_task_id,kind,seq,expected_base_head,dedupe_key,message_id,created_at)
SELECT id,ship_task_id,kind,seq,expected_base_head,dedupe_key,message_id,created_at FROM validation_fix_rounds;
DROP TRIGGER validation_fix_rounds_immutable;
DROP INDEX validation_fix_rounds_ship_idx;
DROP TABLE validation_fix_rounds;
ALTER TABLE validation_fix_rounds_old RENAME TO validation_fix_rounds;
CREATE INDEX validation_fix_rounds_ship_idx ON validation_fix_rounds(ship_task_id, kind, seq);
CREATE TRIGGER validation_fix_rounds_immutable BEFORE UPDATE ON validation_fix_rounds
BEGIN SELECT RAISE(ABORT, 'validation fix round is immutable'); END;
