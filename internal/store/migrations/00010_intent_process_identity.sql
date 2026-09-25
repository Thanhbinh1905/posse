-- +goose Up
ALTER TABLE intents ADD COLUMN process_boot_id TEXT NOT NULL DEFAULT '';
ALTER TABLE intents ADD COLUMN process_start_time TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE intents DROP COLUMN process_start_time;
ALTER TABLE intents DROP COLUMN process_boot_id;
