-- +goose Up
-- Review rounds bind an independent Review Task to one exact Ship candidate.
CREATE TABLE review_rounds (
    ship_task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    round INTEGER NOT NULL CHECK (round > 0),
    review_task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repository TEXT NOT NULL,
    scope_kind TEXT NOT NULL CHECK (scope_kind IN ('full', 'delta', 'full_fallback')),
    scope_base_sha TEXT NOT NULL CHECK (length(scope_base_sha) IN (40, 64)),
    head_sha TEXT NOT NULL CHECK (length(head_sha) IN (40, 64)),
    target_commit_sha TEXT NOT NULL CHECK (length(target_commit_sha) IN (40, 64)),
    baseline_tree TEXT NOT NULL CHECK (length(baseline_tree) IN (40, 64)),
    delta_sha256 TEXT NOT NULL CHECK (length(delta_sha256) = 64),
    conflicted_paths_json TEXT NOT NULL DEFAULT '[]',
    status TEXT NOT NULL CHECK (status IN ('admitted', 'verdict', 'void')),
    verdict TEXT CHECK (verdict IN ('pass', 'changes_requested', 'inconclusive')),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (ship_task_id, review_task_id, repository),
    CHECK ((status = 'verdict' AND verdict IS NOT NULL) OR (status IN ('admitted', 'void') AND verdict IS NULL))
);
CREATE INDEX review_rounds_ship_idx ON review_rounds(ship_task_id, round, repository);
CREATE TRIGGER review_rounds_binding_immutable
BEFORE UPDATE OF ship_task_id, round, review_task_id, repository, scope_kind,
    scope_base_sha, head_sha, target_commit_sha, baseline_tree, delta_sha256,
    conflicted_paths_json, created_at ON review_rounds
BEGIN SELECT RAISE(ABORT, 'review round binding is immutable'); END;
CREATE TRIGGER review_rounds_terminal_status_immutable
BEFORE UPDATE OF status, verdict ON review_rounds
WHEN OLD.status <> 'admitted'
    OR NEW.status NOT IN ('verdict', 'void')
    OR (NEW.status = 'verdict' AND NEW.verdict IS NULL)
    OR (NEW.status = 'void' AND NEW.verdict IS NOT NULL)
BEGIN SELECT RAISE(ABORT, 'review round status is terminal'); END;

-- Pattern definitions are stable; merging changes only merged_into.
CREATE TABLE review_patterns (
    id INTEGER PRIMARY KEY,
    ship_task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    seq INTEGER NOT NULL CHECK (seq > 0),
    key TEXT NOT NULL,
    title TEXT NOT NULL,
    rule TEXT NOT NULL,
    root_cause TEXT NOT NULL,
    fix_guidance TEXT NOT NULL,
    merged_into INTEGER REFERENCES review_patterns(id),
    created_round INTEGER NOT NULL CHECK (created_round > 0),
    UNIQUE (ship_task_id, seq),
    UNIQUE (ship_task_id, key),
    CHECK (merged_into IS NULL OR merged_into <> id)
);
CREATE INDEX review_patterns_ship_idx ON review_patterns(ship_task_id, seq);
CREATE TRIGGER review_patterns_definition_immutable
BEFORE UPDATE OF ship_task_id, seq, key, title, rule, root_cause, fix_guidance, created_round ON review_patterns
BEGIN SELECT RAISE(ABORT, 'review pattern definition is immutable'); END;

-- Finding identity and evidence are immutable; its disposition state can evolve.
CREATE TABLE review_findings (
    id INTEGER PRIMARY KEY,
    ship_task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    seq INTEGER NOT NULL CHECK (seq > 0),
    pattern_id INTEGER NOT NULL REFERENCES review_patterns(id),
    fingerprint TEXT NOT NULL CHECK (length(fingerprint) = 64),
    round INTEGER NOT NULL CHECK (round > 0),
    repository TEXT NOT NULL,
    path TEXT NOT NULL,
    symbol TEXT NOT NULL,
    excerpt TEXT NOT NULL,
    line_at_report INTEGER NOT NULL CHECK (line_at_report > 0),
    head_sha TEXT NOT NULL CHECK (length(head_sha) IN (40, 64)),
    severity TEXT NOT NULL CHECK (severity IN ('P0', 'P1', 'P2', 'P3', 'unclassified')),
    origin TEXT NOT NULL CHECK (origin IN ('introduced', 'pre_existing')),
    in_scope INTEGER NOT NULL CHECK (in_scope IN (0, 1)),
    state TEXT NOT NULL CHECK (state IN ('open', 'fix_claimed', 'unverified', 'fixed', 'disputed', 'regressed', 'accepted', 'deferred', 'withdrawn')),
    follow_up_url TEXT NOT NULL DEFAULT '',
    inherited_from INTEGER REFERENCES review_findings(id),
    UNIQUE (ship_task_id, seq),
    UNIQUE (ship_task_id, fingerprint)
);
CREATE INDEX review_findings_pattern_idx ON review_findings(pattern_id, seq);
CREATE INDEX review_findings_ship_idx ON review_findings(ship_task_id, state, severity);
CREATE TRIGGER review_findings_evidence_immutable
BEFORE UPDATE OF ship_task_id, seq, pattern_id, fingerprint, round, repository,
    path, symbol, excerpt, line_at_report, head_sha, inherited_from ON review_findings
BEGIN SELECT RAISE(ABORT, 'review finding evidence is immutable'); END;

-- Event history is append-only; review_findings.state is its query-friendly projection.
CREATE TABLE review_finding_events (
    id INTEGER PRIMARY KEY,
    finding_id INTEGER NOT NULL REFERENCES review_findings(id) ON DELETE CASCADE,
    round INTEGER NOT NULL CHECK (round >= 0),
    event TEXT NOT NULL,
    actor TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    user_quote TEXT NOT NULL DEFAULT '',
    at INTEGER NOT NULL
);
CREATE INDEX review_finding_events_finding_idx ON review_finding_events(finding_id, id);
CREATE TRIGGER review_finding_events_immutable BEFORE UPDATE ON review_finding_events
BEGIN SELECT RAISE(ABORT, 'review finding event is immutable'); END;

-- Carry records prove that a clean target merge preserves an exact passed tree.
CREATE TABLE review_carries (
    ship_task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repository TEXT NOT NULL,
    from_head TEXT NOT NULL CHECK (length(from_head) IN (40, 64)),
    to_head TEXT NOT NULL CHECK (length(to_head) IN (40, 64)),
    target_commit_sha TEXT NOT NULL CHECK (length(target_commit_sha) IN (40, 64)),
    tree TEXT NOT NULL CHECK (length(tree) IN (40, 64)),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (ship_task_id, repository, from_head, to_head)
);
CREATE TRIGGER review_carries_immutable BEFORE UPDATE ON review_carries
BEGIN SELECT RAISE(ABORT, 'review carry is immutable'); END;

-- Dispositions retain the exact head and approval that resolved risk.
CREATE TABLE review_dispositions (
    id INTEGER PRIMARY KEY,
    ship_task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    head_sha TEXT NOT NULL CHECK (length(head_sha) IN (40, 64)),
    decision_id INTEGER REFERENCES decisions(id),
    user_quote TEXT NOT NULL DEFAULT '',
    at INTEGER NOT NULL
);
CREATE INDEX review_dispositions_ship_idx ON review_dispositions(ship_task_id, at, id);
CREATE TRIGGER review_dispositions_immutable BEFORE UPDATE ON review_dispositions
BEGIN SELECT RAISE(ABORT, 'review disposition is immutable'); END;

-- Each queued validation instruction consumes one immutable budget entry.
CREATE TABLE validation_fix_rounds (
    id INTEGER PRIMARY KEY,
    ship_task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('gate', 'ci', 'review', 'freshness')),
    seq INTEGER NOT NULL CHECK (seq > 0),
    expected_base_head TEXT NOT NULL CHECK (length(expected_base_head) IN (40, 64)),
    dedupe_key TEXT NOT NULL,
    message_id INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (ship_task_id, kind, seq),
    UNIQUE (ship_task_id, kind, dedupe_key),
    UNIQUE (message_id)
);
CREATE INDEX validation_fix_rounds_ship_idx ON validation_fix_rounds(ship_task_id, kind, seq);
CREATE TRIGGER validation_fix_rounds_immutable BEFORE UPDATE ON validation_fix_rounds
BEGIN SELECT RAISE(ABORT, 'validation fix round is immutable'); END;

-- +goose Down
DROP TRIGGER validation_fix_rounds_immutable;
DROP INDEX validation_fix_rounds_ship_idx;
DROP TABLE validation_fix_rounds;
DROP TRIGGER review_dispositions_immutable;
DROP INDEX review_dispositions_ship_idx;
DROP TABLE review_dispositions;
DROP TRIGGER review_carries_immutable;
DROP TABLE review_carries;
DROP TRIGGER review_finding_events_immutable;
DROP INDEX review_finding_events_finding_idx;
DROP TABLE review_finding_events;
DROP TRIGGER review_findings_evidence_immutable;
DROP INDEX review_findings_ship_idx;
DROP INDEX review_findings_pattern_idx;
DROP TABLE review_findings;
DROP TRIGGER review_patterns_definition_immutable;
DROP INDEX review_patterns_ship_idx;
DROP TABLE review_patterns;
DROP TRIGGER review_rounds_terminal_status_immutable;
DROP TRIGGER review_rounds_binding_immutable;
DROP INDEX review_rounds_ship_idx;
DROP TABLE review_rounds;
