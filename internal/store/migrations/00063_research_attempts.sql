-- +goose Up
CREATE TABLE research_attempts (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id INTEGER REFERENCES tasks(id) ON DELETE CASCADE,
    parent_role TEXT NOT NULL CHECK (parent_role IN ('lead', 'worker')),
    parent_session TEXT NOT NULL DEFAULT '',
    parent_pid INTEGER NOT NULL DEFAULT 0,
    parent_identity TEXT NOT NULL DEFAULT '',
    harness TEXT NOT NULL CHECK (harness IN ('claude', 'codex', 'pi')),
    harness_version TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL DEFAULT '',
    previous_id INTEGER REFERENCES research_attempts(id) ON DELETE SET NULL,
    child_id TEXT NOT NULL DEFAULT '',
    agent_type TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    tools TEXT NOT NULL DEFAULT '[]',
    access TEXT NOT NULL DEFAULT 'read-only' CHECK (access = 'read-only'),
    scope TEXT NOT NULL,
    prompt_digest TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL CHECK (state IN ('registered', 'running', 'completed', 'interrupted', 'cancelled', 'failed', 'refused')),
    resumable INTEGER NOT NULL DEFAULT 0 CHECK (resumable IN (0, 1)),
    session_ref TEXT NOT NULL DEFAULT '',
    runner_pid INTEGER NOT NULL DEFAULT 0,
    runner_identity TEXT NOT NULL DEFAULT '',
    child_pid INTEGER NOT NULL DEFAULT 0,
    child_identity TEXT NOT NULL DEFAULT '',
    usage TEXT NOT NULL DEFAULT '{}',
    result_path TEXT NOT NULL DEFAULT '',
    result_bytes INTEGER NOT NULL DEFAULT 0,
    result_digest TEXT NOT NULL DEFAULT '',
    result_truncated INTEGER NOT NULL DEFAULT 0 CHECK (result_truncated IN (0, 1)),
    note TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    ended_at INTEGER,
    CHECK ((parent_role = 'worker') = (task_id IS NOT NULL))
);
CREATE INDEX research_attempts_parent_idx ON research_attempts(project_id, task_id, state);
CREATE INDEX research_attempts_child_idx ON research_attempts(harness, child_id) WHERE child_id <> '';
CREATE UNIQUE INDEX research_attempts_key_idx ON research_attempts(project_id, harness, idempotency_key) WHERE idempotency_key <> '';
CREATE TRIGGER research_attempts_identity_immutable
BEFORE UPDATE OF project_id, task_id, parent_role, harness, idempotency_key, previous_id, scope, access, prompt_digest, created_at ON research_attempts
BEGIN SELECT RAISE(ABORT, 'research attempt identity is immutable'); END;

-- +goose Down
DROP TRIGGER research_attempts_identity_immutable;
DROP INDEX research_attempts_key_idx;
DROP INDEX research_attempts_child_idx;
DROP INDEX research_attempts_parent_idx;
DROP TABLE research_attempts;
