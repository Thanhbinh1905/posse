-- +goose Up
-- +goose StatementBegin
CREATE TRIGGER terminalize_queued_messages
AFTER UPDATE OF state ON tasks
WHEN NEW.state IN ('reported', 'failed', 'lost', 'landed', 'torn-down')
BEGIN
    INSERT INTO notices(project_id, task_id, kind, summary, data_json, created_at)
    SELECT NEW.project_id, NEW.id, 'queued_message_undeliverable',
        'Queued messages ' || (
            SELECT group_concat('#' || id, ', ')
            FROM (SELECT id FROM messages WHERE task_id=NEW.id AND status IN ('queued', 'claimed') ORDER BY id)
        ) || ' can no longer be delivered because t' || NEW.seq || ' entered ' || NEW.state || '. ' ||
        CASE
            WHEN NEW.state IN ('failed', 'lost') THEN 'Relaunch with `posse relaunch t' || NEW.seq || '`, then resend each message with `posse send t' || NEW.seq || ' <message>`.'
            ELSE 'Create a new Ship Task, then resend with `posse send <task> <message>`.'
        END,
        '{}', NEW.updated_at
    WHERE EXISTS (SELECT 1 FROM messages WHERE task_id=NEW.id AND status IN ('queued', 'claimed'));

    UPDATE messages
    SET status='undeliverable', claim_token='', claimed_at=0
    WHERE task_id=NEW.id AND status IN ('queued', 'claimed');
END;

-- Resolve legacy orphaned messages as part of the schema upgrade.
UPDATE tasks SET state=state WHERE state IN ('reported', 'failed', 'lost', 'landed', 'torn-down');
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER terminalize_queued_messages;
