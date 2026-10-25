-- +goose Up
-- A workspace Review fix instruction binds one expected head, and the private
-- candidate when one exists, per changed Member. expected_base_head then holds
-- the digest of these heads.
ALTER TABLE validation_fix_rounds ADD COLUMN member_heads_json TEXT NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE validation_fix_rounds DROP COLUMN member_heads_json;
