-- +goose Up
CREATE TABLE IF NOT EXISTS provider_finding_patterns (
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    seq INTEGER NOT NULL CHECK (seq > 0),
    key TEXT NOT NULL,
    title TEXT NOT NULL,
    rule TEXT NOT NULL,
    fix_guidance TEXT NOT NULL,
    PRIMARY KEY (task_id, seq),
    UNIQUE (task_id, key)
);
CREATE TRIGGER IF NOT EXISTS provider_finding_patterns_immutable BEFORE UPDATE ON provider_finding_patterns
BEGIN SELECT RAISE(ABORT, 'provider finding pattern is immutable'); END;

CREATE TABLE IF NOT EXISTS provider_findings (
    id INTEGER PRIMARY KEY,
    verdict_id INTEGER NOT NULL REFERENCES verdicts(id),
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    provider TEXT NOT NULL,
    repo TEXT NOT NULL,
    head_sha TEXT NOT NULL CHECK (length(head_sha) IN (40, 64)),
    finding_key TEXT NOT NULL,
    pattern_seq INTEGER NOT NULL,
    path TEXT NOT NULL,
    line INTEGER NOT NULL CHECK (line > 0),
    excerpt TEXT NOT NULL,
    severity TEXT NOT NULL,
    origin TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open','fix_claimed','disputed','fixed')),
    UNIQUE (verdict_id, finding_key),
    FOREIGN KEY (task_id, pattern_seq) REFERENCES provider_finding_patterns(task_id, seq)
);
CREATE INDEX IF NOT EXISTS provider_findings_task_idx ON provider_findings(task_id, state, severity);
CREATE INDEX IF NOT EXISTS provider_findings_head_idx ON provider_findings(task_id, repo, head_sha);
CREATE TRIGGER IF NOT EXISTS provider_findings_evidence_immutable
BEFORE UPDATE OF verdict_id, task_id, provider, repo, head_sha, finding_key, pattern_seq, path, line, excerpt, severity, origin ON provider_findings
BEGIN SELECT RAISE(ABORT, 'provider finding evidence is immutable'); END;

CREATE TABLE IF NOT EXISTS fix_round_provider_findings (
    fix_round_id INTEGER NOT NULL REFERENCES validation_fix_rounds(id),
    finding_id INTEGER NOT NULL REFERENCES provider_findings(id),
    pattern_seq INTEGER NOT NULL,
    PRIMARY KEY (fix_round_id, finding_id)
);
CREATE TRIGGER IF NOT EXISTS fix_round_provider_findings_immutable BEFORE UPDATE ON fix_round_provider_findings
BEGIN SELECT RAISE(ABORT, 'provider finding fix-round binding is immutable'); END;

CREATE TABLE IF NOT EXISTS provider_finding_events (
    id INTEGER PRIMARY KEY,
    finding_id INTEGER NOT NULL REFERENCES provider_findings(id),
    event TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    at INTEGER NOT NULL
);
CREATE TRIGGER IF NOT EXISTS provider_finding_events_immutable BEFORE UPDATE ON provider_finding_events
BEGIN SELECT RAISE(ABORT, 'provider finding event is immutable'); END;

-- +goose Down
DROP TRIGGER provider_finding_events_immutable;
DROP TABLE provider_finding_events;
DROP TRIGGER fix_round_provider_findings_immutable;
DROP TABLE fix_round_provider_findings;
DROP TRIGGER provider_findings_evidence_immutable;
DROP INDEX provider_findings_head_idx;
DROP INDEX provider_findings_task_idx;
DROP TABLE provider_findings;
DROP TRIGGER provider_finding_patterns_immutable;
DROP TABLE provider_finding_patterns;
