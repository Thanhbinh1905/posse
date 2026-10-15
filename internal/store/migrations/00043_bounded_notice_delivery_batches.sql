-- +goose Up
DROP INDEX notice_delivery_one_active_destination_idx;
CREATE INDEX notice_delivery_active_destination_idx ON notice_delivery_receipts(project_id, destination, created_at, delivery_id) WHERE state IN ('claimed','printed');

-- +goose Down
DROP INDEX notice_delivery_active_destination_idx;
CREATE UNIQUE INDEX notice_delivery_one_active_destination_idx ON notice_delivery_receipts(project_id, destination) WHERE state IN ('claimed','printed');
