-- +goose Up
CREATE TABLE notice_delivery_receipts (
    delivery_id TEXT PRIMARY KEY,
    batch_id TEXT NOT NULL,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    notice_ids_json TEXT NOT NULL,
    destination TEXT NOT NULL,
    generation TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('claimed','printed','accepted','rejected','uncertain')),
    owner_token TEXT NOT NULL DEFAULT '',
    claimed_at INTEGER NOT NULL DEFAULT 0,
    lease_until INTEGER NOT NULL DEFAULT 0,
    accepted_at INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(batch_id, destination)
);
CREATE INDEX notice_delivery_receipts_lookup_idx ON notice_delivery_receipts(project_id, destination, state, updated_at);
CREATE UNIQUE INDEX notice_delivery_one_active_destination_idx ON notice_delivery_receipts(project_id, destination) WHERE state IN ('claimed','printed');

-- +goose Down
DROP INDEX notice_delivery_one_active_destination_idx;
DROP INDEX notice_delivery_receipts_lookup_idx;
DROP TABLE notice_delivery_receipts;
