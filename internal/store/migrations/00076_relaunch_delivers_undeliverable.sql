-- +goose Up
-- A relaunch hands undeliverable messages to the replacement Worker in its
-- relaunch prompt, so the Notice no longer asks the Lead to resend them.
DROP TRIGGER terminalize_queued_messages;
-- +goose StatementBegin
CREATE TRIGGER terminalize_queued_messages
AFTER UPDATE OF state ON tasks
WHEN NEW.state IN ('reported', 'failed', 'lost', 'merged', 'torn-down')
BEGIN
    INSERT INTO notices(project_id, task_id, kind, summary, data_json, created_at)
    SELECT NEW.project_id, NEW.id, 'queued_message_undeliverable',
        'Queued messages ' || (
            SELECT group_concat('#' || id, ', ')
            FROM (SELECT id FROM messages WHERE task_id=NEW.id AND status IN ('queued', 'claimed') ORDER BY id)
        ) || ' can no longer be delivered because t' || NEW.seq || ' entered ' || NEW.state || '. ' ||
        CASE
            WHEN NEW.state IN ('failed', 'lost') THEN 'Relaunch with `posse relaunch t' || NEW.seq || '`; the relaunched Worker receives them in its relaunch prompt.'
            ELSE 'Create a new Change Task, then resend with `posse send <task> <message>`.'
        END,
        '{}', NEW.updated_at
    WHERE EXISTS (SELECT 1 FROM messages WHERE task_id=NEW.id AND status IN ('queued', 'claimed'));

    UPDATE messages
    SET status='undeliverable', claim_token='', claimed_at=0
    WHERE task_id=NEW.id AND status IN ('queued', 'claimed');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER terminalize_queued_messages;
-- +goose StatementBegin
CREATE TRIGGER terminalize_queued_messages
AFTER UPDATE OF state ON tasks
WHEN NEW.state IN ('reported', 'failed', 'lost', 'merged', 'torn-down')
BEGIN
    INSERT INTO notices(project_id, task_id, kind, summary, data_json, created_at)
    SELECT NEW.project_id, NEW.id, 'queued_message_undeliverable',
        'Queued messages ' || (
            SELECT group_concat('#' || id, ', ')
            FROM (SELECT id FROM messages WHERE task_id=NEW.id AND status IN ('queued', 'claimed') ORDER BY id)
        ) || ' can no longer be delivered because t' || NEW.seq || ' entered ' || NEW.state || '. ' ||
        CASE
            WHEN NEW.state IN ('failed', 'lost') THEN 'Relaunch with `posse relaunch t' || NEW.seq || '`, then resend each message with `posse send t' || NEW.seq || ' <message>`.'
            ELSE 'Create a new Change Task, then resend with `posse send <task> <message>`.'
        END,
        '{}', NEW.updated_at
    WHERE EXISTS (SELECT 1 FROM messages WHERE task_id=NEW.id AND status IN ('queued', 'claimed'));

    UPDATE messages
    SET status='undeliverable', claim_token='', claimed_at=0
    WHERE task_id=NEW.id AND status IN ('queued', 'claimed');
END;
-- +goose StatementEnd
