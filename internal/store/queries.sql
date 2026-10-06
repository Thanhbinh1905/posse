-- name: InsertApproval :exec
INSERT INTO approvals(task_id, action, user_quote, at)
VALUES (?, ?, ?, ?);

-- name: PRBodyMarkerByTaskRepo :one
SELECT pr_url, marker_token FROM pr_body_markers WHERE task_id = ? AND repo = ?;

-- name: PRBodyMarkersByTask :many
SELECT task_id, repo, pr_url, marker_token, updated_at FROM pr_body_markers
WHERE task_id = ? ORDER BY repo;

-- name: UpsertPRBodyMarker :exec
INSERT INTO pr_body_markers(task_id, repo, pr_url, marker_token, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(task_id, repo) DO UPDATE SET
    pr_url = excluded.pr_url,
    marker_token = excluded.marker_token,
    updated_at = excluded.updated_at;

-- name: BindPRBodyMarker :execresult
UPDATE pr_body_markers SET pr_url = ?, updated_at = ?
WHERE task_id = ? AND repo = ? AND marker_token = ? AND (pr_url = '' OR pr_url = ?);

-- name: CountApproval :one
SELECT COUNT(*) FROM approvals WHERE task_id = ? AND action = ?;

-- name: UpdateTaskState :execresult
UPDATE tasks SET state = ?, updated_at = ?,
    last_output_hash = CASE WHEN ? = 'working' THEN '' ELSE last_output_hash END,
    last_worktree_hash = CASE WHEN ? = 'working' THEN '' ELSE last_worktree_hash END,
    last_progress_at = CASE WHEN ? = 'working' THEN 0 ELSE last_progress_at END
WHERE id = ? AND state = ?;

-- name: InsertTransition :exec
INSERT INTO transitions(task_id, from_state, to_state, source, note, at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: TouchProjectForTask :exec
UPDATE projects SET last_activity_at = ? WHERE projects.id = (SELECT project_id FROM tasks WHERE tasks.id = ?);

-- name: InsertTask :execresult
INSERT INTO tasks(
    project_id, seq, type, reviews_task_id, title, short_name, state, profile, dispatch_rule,
    landing_mode, autonomy_review, autonomy_land, branch, base_ref, worktree_path,
    herdr_workspace_id, pane_id, pane_label, agent_name, agent_session, pr_url,
    landed_ref, last_output_hash, last_worktree_hash, last_progress_at,
    agent_absent_since, idle_since, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: InsertProject :execresult
INSERT INTO projects(name, root, default_branch, status, created_at, last_activity_at)
VALUES (?, ?, ?, 'active', ?, ?);

-- name: SetProjectLead :exec
UPDATE projects SET herdr_workspace_id = ?, lead_pane_id = ?, lead_label = ?,
    lead_absent_since = 0, down_at = 0, status = 'active', last_activity_at = ? WHERE id = ?;

-- name: ClearProjectLead :exec
UPDATE projects SET lead_pane_id = '', lead_label = '', lead_absent_since = 0, status = 'missing' WHERE id = ?;

-- name: MarkProjectDown :exec
UPDATE projects SET down_at = ? WHERE id = ?;

-- name: ClearDownProjectLead :exec
UPDATE projects SET lead_pane_id = '', lead_label = '', lead_absent_since = 0 WHERE id = ? AND down_at != 0;

-- name: InsertLeadStartClaim :execresult
INSERT INTO lead_start_claims(project_id, claimed_at) VALUES (?, ?)
ON CONFLICT(project_id) DO NOTHING;

-- name: RefreshLeadStartClaim :execresult
UPDATE lead_start_claims SET claimed_at = ? WHERE project_id = ? AND claimed_at <= ?;

-- name: ReleaseLeadStart :exec
DELETE FROM lead_start_claims WHERE project_id = ?;

-- name: MoveProject :exec
UPDATE projects SET root = ?, default_branch = ?, status = 'active', last_activity_at = ? WHERE id = ?;

-- name: NextTaskSeq :one
SELECT COALESCE(MAX(seq), 0) + 1 FROM tasks WHERE project_id = ?;

-- name: ActiveWorkerCount :one
SELECT COUNT(*) FROM tasks
WHERE project_id = ? AND state IN ('spawning', 'working', 'needs-decision', 'blocked', 'stalled');

-- name: AllocateTaskSequence :one
SELECT COALESCE(MAX(seq), 0) + 1 FROM tasks WHERE project_id = ?;

-- name: AcquireIdleMount :execresult
UPDATE mounts SET state = 'held', task_id = ?, acquired_at = ?, released_at = 0
WHERE mounts.id = (
  SELECT candidate.id FROM mounts candidate WHERE candidate.project_id = ? AND candidate.state = 'idle' ORDER BY candidate.n LIMIT 1
) AND mounts.state = 'idle';

-- name: NextMountNumber :one
SELECT COALESCE(MAX(n), 0) + 1 FROM mounts WHERE project_id = ?;

-- name: InsertMount :execresult
INSERT INTO mounts(project_id, n, path, state, task_id, acquired_at, released_at)
VALUES (?, ?, ?, ?, ?, ?, 0);

-- name: MountByTask :one
SELECT * FROM mounts WHERE task_id = ?;

-- name: Mounts :many
SELECT * FROM mounts WHERE project_id = ? ORDER BY n;

-- name: ReleaseMount :exec
UPDATE mounts SET state = 'idle', task_id = NULL, released_at = ? WHERE id = ? AND task_id = ? AND state = 'held';

-- name: BreakMount :exec
UPDATE mounts SET state = 'broken', task_id = NULL, released_at = ? WHERE id = ?;

-- name: DeleteMount :execresult
DELETE FROM mounts WHERE id = ? AND project_id = ? AND state IN ('idle', 'broken');

-- name: UpdateTaskMount :exec
UPDATE tasks SET mount_id = ?, worktree_path = ?, updated_at = ? WHERE id = ?;

-- name: UpdateTaskWorkspace :exec
UPDATE tasks SET herdr_workspace_id = ?, pane_id = ?, updated_at = ? WHERE id = ?;

-- name: AllocateTaskLaunch :one
UPDATE tasks SET launches = launches + 1, updated_at = ? WHERE id = ? RETURNING launches;

-- name: AllocateLeadLaunch :one
UPDATE projects SET lead_launches = lead_launches + 1 WHERE id = ? RETURNING lead_launches;

-- name: SetTaskGatedSHA :exec
UPDATE tasks SET gated_sha = ?, updated_at = ? WHERE id = ? AND state = 'done';

-- name: ClearTaskGatedSHA :exec
UPDATE tasks SET gated_sha = '', updated_at = ? WHERE id = ? AND state = 'done';

-- name: ClearGatedSHALandingToDone :exec
UPDATE tasks SET gated_sha = '' WHERE id = ? AND state = 'done';

-- name: TaskGatedSHA :one
SELECT gated_sha FROM tasks WHERE id = ?;

-- name: ClearWorkspaceToken :exec
INSERT INTO notice_notifications(project_id, notified_at) VALUES (?, 0)
ON CONFLICT(project_id) DO UPDATE SET notified_at = 0;

-- name: UpdateTaskLaunch :exec
UPDATE tasks SET worktree_path = ?, herdr_workspace_id = ?, pane_id = ?, pane_label = ?,
    agent_name = ?, updated_at = ? WHERE id = ?;

-- name: UpdateTaskLanding :exec
UPDATE tasks SET pr_url = ?, landed_ref = ?, updated_at = ? WHERE id = ?;

-- name: InsertSignal :execresult
INSERT INTO signals(task_id, verb, note, data_json, at) VALUES (?, ?, ?, ?, ?);

-- name: TaskSignals :many
SELECT id, task_id, verb, note, data_json, at FROM signals
WHERE task_id = ? ORDER BY id DESC LIMIT ?;

-- name: TaskTransitions :many
SELECT id, task_id, from_state, to_state, source, note, at FROM transitions
WHERE task_id = ? ORDER BY id DESC LIMIT ?;

-- name: QueueMessage :execresult
INSERT INTO messages(task_id, body, created_at, status) VALUES (?, ?, ?, 'queued');

-- name: OldestQueuedMessage :one
SELECT id, task_id, body, created_at, COALESCE(delivered_at, 0) AS delivered_at, status
FROM messages WHERE task_id = ? AND status = 'queued' ORDER BY created_at, id LIMIT 1;

-- name: MarkMessageDelivered :exec
UPDATE messages SET status = 'delivered', delivered_at = ? WHERE id = ? AND status = 'queued';

-- name: Notices :many
SELECT id, project_id, COALESCE(task_id, 0) AS task_id, kind, summary, data_json,
    created_at, COALESCE(delivered_at, 0) AS delivered_at, COALESCE(acked_at, 0) AS acked_at
FROM notices WHERE project_id = ? ORDER BY id;

-- name: OpenNotices :many
SELECT id, project_id, COALESCE(task_id, 0) AS task_id, kind, summary, data_json,
    created_at, COALESCE(delivered_at, 0) AS delivered_at, COALESCE(acked_at, 0) AS acked_at
FROM notices WHERE project_id = ? AND acked_at IS NULL ORDER BY id;

-- name: UndeliveredNotices :many
SELECT id, project_id, COALESCE(task_id, 0) AS task_id, kind, summary, data_json,
    created_at, 0 AS delivered_at, 0 AS acked_at
FROM notices WHERE project_id = ? AND delivered_at IS NULL AND acked_at IS NULL AND claim_token = '' ORDER BY id;

-- name: MarkNoticeDelivered :exec
UPDATE notices SET delivered_at = ?
WHERE id = ? AND project_id = ? AND delivered_at IS NULL AND acked_at IS NULL;

-- name: LastNoticeDelivery :one
SELECT CAST(COALESCE(MAX(delivered_at), 0) AS INTEGER) FROM notices WHERE project_id = ?;

-- name: LastNoticeNotification :one
SELECT CAST(COALESCE((SELECT notified_at FROM notice_notifications WHERE project_id = ?), 0) AS INTEGER);

-- name: RecordNoticeNotification :exec
INSERT INTO notice_notifications(project_id, notified_at) VALUES (?, ?)
ON CONFLICT(project_id) DO UPDATE SET notified_at = excluded.notified_at;

-- name: AckAllNotices :execresult
UPDATE notices SET acked_at = ? WHERE project_id = ? AND acked_at IS NULL AND claim_token = '';

-- name: AckNotice :execresult
UPDATE notices SET acked_at = ? WHERE id = ? AND project_id = ? AND acked_at IS NULL AND claim_token = '';

-- name: ProjectByRoot :one
SELECT * FROM projects WHERE root = ?;

-- name: ProjectByName :one
SELECT * FROM projects WHERE name = ?;

-- name: ProjectByID :one
SELECT * FROM projects WHERE id = ?;

-- name: ProjectByLeadPane :one
SELECT * FROM projects WHERE lead_pane_id = ?;

-- name: Projects :many
SELECT * FROM projects ORDER BY name;

-- name: Task :one
SELECT * FROM tasks
WHERE project_id = sqlc.arg(project_id) AND (
    CAST('t' || seq AS TEXT) = CAST(sqlc.arg(identifier) AS TEXT)
);

-- name: TaskByID :one
SELECT * FROM tasks
WHERE project_id = ? AND id = ?;

-- name: TaskByPane :one
SELECT * FROM tasks
WHERE pane_id = ? AND state != 'torn-down' ORDER BY id DESC LIMIT 1;

-- name: TaskByWorktree :one
SELECT * FROM tasks
WHERE worktree_path = ? AND state != 'torn-down' ORDER BY id DESC LIMIT 1;

-- name: Tasks :many
SELECT * FROM tasks
WHERE project_id = ? AND state NOT IN ('landed', 'reported', 'torn-down') ORDER BY seq;

-- name: AllTasks :many
SELECT * FROM tasks WHERE project_id = ? ORDER BY seq;

-- name: LiveTasks :many
SELECT * FROM tasks
WHERE project_id = ? AND state IN ('spawning', 'working', 'needs-decision', 'blocked', 'stalled', 'done', 'landing') ORDER BY seq;

-- name: StallTasks :many
SELECT * FROM tasks
WHERE project_id = ? AND state IN ('working', 'stalled') ORDER BY seq;

-- name: UpdateTaskObservation :exec
UPDATE tasks SET pane_id = ?, herdr_workspace_id = ?, agent_session = ?, agent_absent_since = ?, agent_server_started_at = ?, idle_since = ?,
    updated_at = ? WHERE id = ?;

-- name: UpdateProjectObservation :exec
UPDATE projects SET lead_pane_id = ?, herdr_workspace_id = ?, lead_absent_since = ? WHERE id = ?;

-- name: UpdateProgress :exec
UPDATE tasks SET last_output_hash = ?, last_worktree_hash = ?, last_progress_at = ?, updated_at = ?
WHERE id = ? AND state = 'working';

-- name: ResetProgress :exec
UPDATE tasks SET last_output_hash = '', last_worktree_hash = '', last_progress_at = 0, updated_at = ?
WHERE id = ? AND state = 'working';

-- name: ResetTaskProgress :exec
UPDATE tasks SET last_output_hash = '', last_worktree_hash = '', last_progress_at = 0, idle_since = 0, updated_at = ?
WHERE id = ? AND state = 'working';

-- name: InsertNotice :execresult
INSERT INTO notices(project_id, task_id, kind, summary, data_json, created_at, delivered_at, acked_at)
VALUES (?, ?, ?, ?, ?, ?, NULL, NULL);

-- name: InsertEvent :exec
INSERT INTO events(received_at, kind, pane_id, data_json) VALUES (?, ?, ?, ?);

-- name: PruneEvents :exec
DELETE FROM events WHERE received_at < ?;
