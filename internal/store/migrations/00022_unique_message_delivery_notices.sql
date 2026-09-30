-- +goose Up
CREATE UNIQUE INDEX notices_message_delivery_uncertain_idx
ON notices(project_id, task_id, json_extract(data_json, '$.message_id'))
WHERE kind='message_delivery_uncertain';

-- +goose Down
DROP INDEX notices_message_delivery_uncertain_idx;
