-- +goose Up
-- Preserve published schema identifiers and worktree paths. Only domain values
-- change, so active Tasks, PR observations and custody references keep their IDs.
DROP TRIGGER terminalize_queued_messages;
UPDATE tasks SET type=CASE type WHEN 'ship' THEN 'change' WHEN 'scout' THEN 'research' ELSE type END;
UPDATE tasks SET state=CASE state WHEN 'landing' THEN 'merging' WHEN 'landed' THEN 'merged' ELSE state END;
UPDATE task_repos SET state=CASE state WHEN 'landing' THEN 'merging' WHEN 'landed' THEN 'merged' ELSE state END;
UPDATE transitions SET from_state=CASE from_state WHEN 'landing' THEN 'merging' WHEN 'landed' THEN 'merged' ELSE from_state END;
UPDATE transitions SET to_state=CASE to_state WHEN 'landing' THEN 'merging' WHEN 'landed' THEN 'merged' ELSE to_state END;
UPDATE transitions SET source='worker' WHERE source='rider';
UPDATE approvals SET action='merge' WHERE action='land';
-- Only schema keys change, never substrings in Member names or audit evidence.
UPDATE config_approvals SET key=CASE
    WHEN key='preferences.rider' THEN 'preferences.worker'
    WHEN substr(key,1,7)='remuda.' THEN 'pool.' || substr(key,8)
    WHEN key='defaults.landing_mode' THEN 'defaults.merge_mode'
    WHEN key='defaults.auto_unsaddle' THEN 'defaults.auto_teardown'
    WHEN key='autonomy.land' THEN 'autonomy.merge'
    WHEN key GLOB 'repositories.*.landing_mode' THEN substr(key,1,length(key)-13) || '.merge_mode'
    ELSE key END;
UPDATE notices SET kind='worker_' || substr(kind,7) WHERE substr(kind,1,6)='rider_';
UPDATE notices SET kind='sandbox_' || substr(kind,7) WHERE substr(kind,1,6)='mount_';
UPDATE notices SET kind='watch_' || substr(kind,9) WHERE substr(kind,1,8)='lookout_';
UPDATE notices SET kind='merge_' || substr(kind,6) WHERE substr(kind,1,5)='land_';
UPDATE notices SET kind='teardown_' || substr(kind,10) WHERE substr(kind,1,9)='unsaddle_';
UPDATE notices SET kind='orphan' WHERE kind='leftover';
UPDATE decisions SET kind='worker_' || substr(kind,7) WHERE substr(kind,1,6)='rider_';
UPDATE decisions SET kind='sandbox_' || substr(kind,7) WHERE substr(kind,1,6)='mount_';
UPDATE decisions SET kind='watch_' || substr(kind,9) WHERE substr(kind,1,8)='lookout_';
UPDATE decisions SET kind='merge_' || substr(kind,6) WHERE substr(kind,1,5)='land_';
UPDATE decisions SET kind='teardown_' || substr(kind,10) WHERE substr(kind,1,9)='unsaddle_';
UPDATE decisions SET kind='orphan' WHERE kind='leftover';
UPDATE decisions SET origin='orphan:' || substr(origin,10) WHERE substr(origin,1,9)='leftover:';
UPDATE decisions SET options_json=replace(options_json,'"land"','"merge"'), answer=CASE answer WHEN 'land' THEN 'merge' ELSE answer END WHERE kind='merge_ready';
UPDATE decisions SET options_json='["merge","wait","discard"]' WHERE kind='merge_ready' AND options_json='["merge","wait"]' AND answered_at IS NULL AND obsolete_at=0;
UPDATE intents SET command=CASE command WHEN 'ride' THEN 'start' WHEN 'land --merge' THEN 'merge --execute' WHEN 'land --open-pr' THEN 'merge --open-pr' WHEN 'land --workspace' THEN 'merge --workspace' WHEN 'unsaddle' THEN 'teardown' ELSE command END;
-- Peel execution-status wrappers, then rewrite only the step name before ':';
-- the remainder binds a Workspace Member and must stay byte-for-byte intact.
WITH RECURSIVE wrapped(id,prefix,remaining) AS (
    SELECT id,'',step FROM intents
    UNION ALL
    SELECT id,prefix || substr(remaining,1,instr(remaining,':')),substr(remaining,instr(remaining,':')+1)
    FROM wrapped
    WHERE substr(remaining,1,instr(remaining,':')) IN ('done:','in_progress:','resuming:')
), steps AS (
    SELECT id,prefix,
        CASE WHEN instr(remaining,':')=0 THEN remaining ELSE substr(remaining,1,instr(remaining,':')-1) END AS name,
        CASE WHEN instr(remaining,':')=0 THEN '' ELSE substr(remaining,instr(remaining,':')) END AS argument
    FROM wrapped
    WHERE substr(remaining,1,instr(remaining,':')) NOT IN ('done:','in_progress:','resuming:')
)
UPDATE intents SET step=(SELECT prefix || CASE
    WHEN substr(name,1,6)='rider.' THEN 'worker.' || substr(name,7)
    WHEN substr(name,1,6)='mount.' THEN 'sandbox.' || substr(name,7)
    WHEN substr(name,1,9)='leftover.' THEN 'orphan.' || substr(name,10)
    WHEN name='task.landing' THEN 'task.merging'
    WHEN name='task.landed' THEN 'task.merged'
    WHEN substr(name,1,11)='landed_ref.' THEN 'merged_ref.' || substr(name,12)
    ELSE name END || argument FROM steps WHERE steps.id=intents.id);
UPDATE resource_leases SET source_key='sandbox:' || substr(source_key,7) WHERE substr(source_key,1,6)='mount:';
UPDATE resource_leases SET owner=replace(owner,' mount-',' sandbox-') WHERE mount_id IS NOT NULL;
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

-- +goose Down
-- Restore the complete pre-upgrade home and config backups before downgrading.
-- This data migration deliberately has no lossy automatic downgrade.
SELECT 1;
