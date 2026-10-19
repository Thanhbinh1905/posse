-- +goose Up
UPDATE decisions
SET options_json='["land","wait","discard"]'
WHERE kind='land_ready'
  AND options_json='["land","wait"]'
  AND answered_at=0
  AND obsolete_at=0;

-- +goose Down
UPDATE decisions
SET options_json='["land","wait"]'
WHERE kind='land_ready'
  AND options_json='["land","wait","discard"]'
  AND answered_at=0
  AND obsolete_at=0;
