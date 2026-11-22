-- +goose Up
CREATE TABLE IF NOT EXISTS approvals (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    action TEXT NOT NULL,
    user_quote TEXT NOT NULL,
    at INTEGER NOT NULL
, branch_sha TEXT NOT NULL DEFAULT '');

CREATE TABLE IF NOT EXISTS branch_author_invalidated_verdicts (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    change_task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    invalidated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS branch_authorship (
    repository TEXT NOT NULL,
    branch TEXT NOT NULL,
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    attempt INTEGER NOT NULL CHECK (attempt > 0),
    generation INTEGER NOT NULL CHECK (generation > 0),
    head_sha TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (repository, branch)
);

CREATE TABLE IF NOT EXISTS budget_crossings (
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    scope TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    PRIMARY KEY(project_id, scope, scope_key)
);

CREATE TABLE IF NOT EXISTS ci_reruns (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repo TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL,
    head_sha TEXT NOT NULL,
    check_name TEXT NOT NULL,
    run_id TEXT NOT NULL DEFAULT '',
    job_id TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL CHECK (reason IN ('cancelled', 'flake', 'steward')),
    tests_json TEXT NOT NULL DEFAULT '[]',
    outcome TEXT NOT NULL DEFAULT 'pending' CHECK (outcome IN ('pending', 'passed', 'failed', 'request_failed')),
    requested_at INTEGER NOT NULL,
    settled_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS ci_test_failures (
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id INTEGER NOT NULL,
    repo TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL,
    head_sha TEXT NOT NULL,
    check_name TEXT NOT NULL,
    job_id TEXT NOT NULL DEFAULT '',
    package TEXT NOT NULL,
    test TEXT NOT NULL,
    observed_at INTEGER NOT NULL,
    PRIMARY KEY (project_id, pr_url, head_sha, check_name, job_id, package, test)
);

CREATE TABLE IF NOT EXISTS completed_rider_busy (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id),
    transition_id INTEGER NOT NULL REFERENCES transitions(id),
    pane_id TEXT NOT NULL,
    server_started_at TEXT NOT NULL,
    busy_since INTEGER NOT NULL
, turn_ended INTEGER NOT NULL DEFAULT 0, baseline TEXT NOT NULL DEFAULT '', resumed INTEGER NOT NULL DEFAULT 0);

CREATE TABLE IF NOT EXISTS "config_approvals" (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    key TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('set', 'unset', 'move')),
    value TEXT,
    user_quote TEXT NOT NULL CHECK (length(trim(user_quote)) > 0),
    at INTEGER NOT NULL,
    CHECK ((action IN ('set', 'move') AND value IS NOT NULL) OR (action = 'unset' AND value IS NULL))
);

CREATE TABLE IF NOT EXISTS decision_notice_cursors (
    project_id INTEGER PRIMARY KEY REFERENCES projects(id),
    last_notice_id INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS decisions (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    origin TEXT NOT NULL,
    question TEXT NOT NULL,
    options_json TEXT NOT NULL,
    answer TEXT NOT NULL DEFAULT '',
    user_quote TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    answered_at INTEGER NOT NULL DEFAULT 0, kind TEXT NOT NULL DEFAULT '', task_launches INTEGER NOT NULL DEFAULT 0, obsolete_at INTEGER NOT NULL DEFAULT 0, obsolete_reason TEXT NOT NULL DEFAULT '', answer_actor TEXT NOT NULL DEFAULT 'user', answer_reason TEXT NOT NULL DEFAULT '',
    UNIQUE(project_id, origin)
);

CREATE TABLE IF NOT EXISTS deliveries (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    repo TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL CHECK (pr_url <> ''),
    steward TEXT NOT NULL DEFAULT 'lead' CHECK (steward IN ('lead')),
    origin TEXT NOT NULL CHECK (origin IN ('publish', 'legacy')),
    created_at INTEGER NOT NULL,
    UNIQUE (task_id, repo, pr_url)
);

CREATE TABLE IF NOT EXISTS delivery_operations (
    id INTEGER PRIMARY KEY,
    delivery_id INTEGER NOT NULL REFERENCES deliveries(id),
    operation TEXT NOT NULL CHECK (operation IN ('inspect', 'reply', 'refresh', 'rerun', 'executor_policy')),
    actor TEXT NOT NULL CHECK (actor IN ('lead', 'user')),
    actor_pane TEXT NOT NULL DEFAULT '',
    head_sha TEXT NOT NULL DEFAULT '',
    subject TEXT NOT NULL DEFAULT '',
    baseline TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL CHECK (outcome IN ('pending', 'succeeded', 'failed', 'uncertain')),
    detail TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY,
    received_at INTEGER NOT NULL,
    kind TEXT NOT NULL,
    pane_id TEXT NOT NULL DEFAULT '',
    data_json TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS fix_round_provider_findings (
    fix_round_id INTEGER NOT NULL REFERENCES validation_fix_rounds(id),
    finding_id INTEGER NOT NULL REFERENCES provider_findings(id),
    pattern_seq INTEGER NOT NULL,
    PRIMARY KEY (fix_round_id, finding_id)
);

CREATE TABLE IF NOT EXISTS greenhouse_task_watches (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id),
    run_id TEXT NOT NULL,
    status TEXT NOT NULL,
    stage TEXT NOT NULL,
    run_observed_at INTEGER NOT NULL,
    stage_observed_at INTEGER NOT NULL,
    run_started_at INTEGER NOT NULL DEFAULT 0,
    stage_started_at INTEGER NOT NULL DEFAULT 0,
    stage_finished_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    pane_id TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS greenhouse_review_requests (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    repo TEXT NOT NULL,
    requested_head TEXT NOT NULL CHECK (length(requested_head) IN (40, 64)),
    provider TEXT NOT NULL CHECK (provider = 'greenhouse'),
    requested_at INTEGER NOT NULL,
    settled_at INTEGER NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('verdict', 'refused', 'failed', 'head_mismatch')),
    verdict_id INTEGER REFERENCES verdicts(id),
    grant_id TEXT,
    CHECK (outcome <> 'verdict' OR verdict_id IS NOT NULL)
);

CREATE TABLE IF NOT EXISTS intents (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    command TEXT NOT NULL,
    step TEXT NOT NULL,
    process_id INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    payload_json TEXT NOT NULL DEFAULT '{}', process_boot_id TEXT NOT NULL DEFAULT '', process_start_time TEXT NOT NULL DEFAULT '',
    UNIQUE(task_id)
);

CREATE TABLE IF NOT EXISTS interventions (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id INTEGER REFERENCES tasks(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS lead_start_claims (
    project_id INTEGER PRIMARY KEY REFERENCES projects(id),
    claimed_at INTEGER NOT NULL
, holder TEXT NOT NULL DEFAULT 'unknown', pid INTEGER NOT NULL DEFAULT 0);

CREATE TABLE IF NOT EXISTS member_pr_poll_state (
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repo TEXT NOT NULL,
    pr_polled_at INTEGER NOT NULL DEFAULT 0,
    claim_until INTEGER NOT NULL DEFAULT 0,
    claim_token TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(project_id, repo)
);

CREATE TABLE IF NOT EXISTS member_watch_errors (
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repo TEXT NOT NULL,
    phase TEXT NOT NULL,
    error TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(project_id, repo, phase)
);

CREATE TABLE IF NOT EXISTS merge_queue_entries (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repo TEXT NOT NULL DEFAULT '',
    target_branch TEXT NOT NULL,
    entered_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    dequeued_at INTEGER NOT NULL DEFAULT 0,
    dequeue_reason TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    body TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    delivered_at INTEGER,
    status TEXT NOT NULL DEFAULT 'queued'
, claim_token TEXT NOT NULL DEFAULT '', claimed_at INTEGER NOT NULL DEFAULT 0, wait_for_idle INTEGER NOT NULL DEFAULT 1);

CREATE TABLE IF NOT EXISTS model_error_episodes (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    episode INTEGER NOT NULL,
    launch INTEGER NOT NULL,
    agent TEXT NOT NULL,
    kind TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    turn_state INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    started_at INTEGER NOT NULL,
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS mounts (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    n INTEGER NOT NULL,
    path TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL,
    task_id INTEGER REFERENCES tasks(id),
    acquired_at INTEGER NOT NULL DEFAULT 0,
    released_at INTEGER NOT NULL DEFAULT 0,
    UNIQUE(project_id, n)
);

CREATE TABLE IF NOT EXISTS notice_delivery_receipts (
    delivery_id TEXT PRIMARY KEY,
    batch_id TEXT NOT NULL,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    notice_ids_json TEXT NOT NULL,
    destination TEXT NOT NULL,
    generation TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('claimed','printed','accepted','rejected','uncertain')),
    owner_token TEXT NOT NULL DEFAULT '',
    claimed_at INTEGER NOT NULL DEFAULT 0,
    lease_until INTEGER NOT NULL DEFAULT 0,
    accepted_at INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(batch_id, destination)
);

CREATE TABLE IF NOT EXISTS notice_notifications (
    project_id INTEGER PRIMARY KEY REFERENCES projects(id),
    notified_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS notices (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    task_id INTEGER REFERENCES tasks(id),
    kind TEXT NOT NULL,
    summary TEXT NOT NULL,
    data_json TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    delivered_at INTEGER,
    acked_at INTEGER
, claim_token TEXT NOT NULL DEFAULT '', claimed_at INTEGER NOT NULL DEFAULT 0, command_notified_at INTEGER NOT NULL DEFAULT 0);

CREATE TABLE IF NOT EXISTS pr_body_markers (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repo TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL DEFAULT '',
    marker_token TEXT NOT NULL UNIQUE,
    updated_at INTEGER NOT NULL, validation_digest TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(task_id, repo)
);

CREATE TABLE IF NOT EXISTS pr_observations (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    pr_url TEXT NOT NULL,
    head_sha TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT '',
    checks TEXT NOT NULL DEFAULT '',
    review TEXT NOT NULL DEFAULT '',
    mergeable TEXT NOT NULL DEFAULT '',
    merge_commit TEXT NOT NULL DEFAULT '',
    observed_at INTEGER NOT NULL
, repo TEXT NOT NULL DEFAULT '', confirmed_at INTEGER NOT NULL DEFAULT 0);

CREATE TABLE IF NOT EXISTS pr_task_watch_failures (
    project_id INTEGER NOT NULL REFERENCES projects(id),
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    repo TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(task_id, repo)
);

CREATE TABLE IF NOT EXISTS project_history_revisions (
    project_id INTEGER PRIMARY KEY,
    revision INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS project_lookout_pollers (
    project_id INTEGER PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    pid INTEGER NOT NULL,
    token TEXT NOT NULL,
    started_at INTEGER NOT NULL,
    heartbeat_at INTEGER NOT NULL,
    executable_path TEXT NOT NULL,
    executable_device INTEGER NOT NULL,
    executable_inode INTEGER NOT NULL,
    failure_notice_raised INTEGER NOT NULL DEFAULT 0,
    executable_size INTEGER NOT NULL DEFAULT 0,
    executable_modified_at INTEGER NOT NULL DEFAULT 0,
    executable_version TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS project_repos (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    name TEXT NOT NULL,
    path TEXT NOT NULL,
    default_branch TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL, origin_host TEXT NOT NULL DEFAULT '',
    UNIQUE(project_id, name),
    UNIQUE(project_id, path)
);

CREATE TABLE IF NOT EXISTS project_runtime (
    project_id INTEGER PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    server_started_at TEXT NOT NULL DEFAULT ''
, recovery_generation TEXT NOT NULL DEFAULT '', recovery_owner_pid INTEGER NOT NULL DEFAULT 0, recovery_claimed_at INTEGER NOT NULL DEFAULT 0, lookout_pane_id TEXT NOT NULL DEFAULT '', lookout_retry_at INTEGER NOT NULL DEFAULT 0, lookout_failed_starts INTEGER NOT NULL DEFAULT 0, lookout_notice_raised INTEGER NOT NULL DEFAULT 0, lead_agent_kind TEXT NOT NULL DEFAULT '', lead_agent_session TEXT NOT NULL DEFAULT '');

CREATE TABLE IF NOT EXISTS project_watch_state (
    project_id INTEGER PRIMARY KEY REFERENCES projects(id),
    pr_polled_at INTEGER NOT NULL DEFAULT 0,
    pr_consecutive_failures INTEGER NOT NULL DEFAULT 0,
    checkout_checked_at INTEGER NOT NULL DEFAULT 0,
    root_behind_head TEXT NOT NULL DEFAULT ''
, pr_poll_claim_until INTEGER NOT NULL DEFAULT 0, pr_poll_claim_token TEXT NOT NULL DEFAULT '', reconcile_blocked_since INTEGER NOT NULL DEFAULT 0, reconcile_block_cause TEXT NOT NULL DEFAULT '', reconcile_block_notice_raised INTEGER NOT NULL DEFAULT 0, pr_poll_last_error TEXT NOT NULL DEFAULT '');

CREATE TABLE IF NOT EXISTS projects (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    root TEXT NOT NULL UNIQUE,
    default_branch TEXT NOT NULL DEFAULT '',
    herdr_workspace_id TEXT NOT NULL DEFAULT '',
    lead_pane_id TEXT NOT NULL DEFAULT '',
    lead_label TEXT NOT NULL DEFAULT '',
    lead_absent_since INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'active',
    created_at INTEGER NOT NULL,
    last_activity_at INTEGER NOT NULL
, lead_launches INTEGER NOT NULL DEFAULT 0, kind TEXT NOT NULL DEFAULT 'repo', down_at INTEGER NOT NULL DEFAULT 0, project_uuid TEXT NOT NULL DEFAULT '');

CREATE TABLE IF NOT EXISTS provider_finding_events (
    id INTEGER PRIMARY KEY,
    finding_id INTEGER NOT NULL REFERENCES provider_findings(id),
    event TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    at INTEGER NOT NULL
);

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

CREATE TABLE IF NOT EXISTS publish_pre_push_heads (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repo TEXT NOT NULL DEFAULT '',
    request_url TEXT NOT NULL DEFAULT '',
    head_sha TEXT NOT NULL,
    recorded_at INTEGER NOT NULL,
    PRIMARY KEY (task_id, repo, request_url, head_sha)
);

CREATE TABLE IF NOT EXISTS queued_task_declines (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    reason TEXT NOT NULL,
    checked_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS quota_exhaustions (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    launch INTEGER NOT NULL,
    agent TEXT NOT NULL,
    profile TEXT NOT NULL,
    line TEXT NOT NULL,
    resets TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    fallback TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    owner_pid INTEGER NOT NULL DEFAULT 0,
    observed_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (task_id, launch)
);

CREATE TABLE IF NOT EXISTS remote_branch_cleanups (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repo TEXT NOT NULL DEFAULT '',
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    branch TEXT NOT NULL,
    head_sha TEXT NOT NULL,
    cause TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 1,
    failed_at INTEGER NOT NULL,
    PRIMARY KEY (task_id, repo)
);

CREATE TABLE IF NOT EXISTS repo_fetch_failures (
    project_id INTEGER NOT NULL REFERENCES projects(id),
    repo TEXT NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(project_id, repo)
);

CREATE TABLE IF NOT EXISTS repo_watch_state (
    project_id INTEGER NOT NULL REFERENCES projects(id),
    repo TEXT NOT NULL,
    root_behind_head TEXT NOT NULL DEFAULT '', checkout_checked_at INTEGER NOT NULL DEFAULT 0, checkout_status TEXT NOT NULL DEFAULT '', checkout_reason TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(project_id, repo)
);

CREATE TABLE IF NOT EXISTS report_acceptances (
    report_task_id INTEGER PRIMARY KEY REFERENCES tasks(id),
    report_digest TEXT NOT NULL,
    accepted_by TEXT NOT NULL CHECK (accepted_by = 'user'),
    recorded_by TEXT NOT NULL CHECK (recorded_by IN ('user', 'lead')),
    provenance TEXT NOT NULL CHECK (provenance IN ('user-command', 'lead-quote')),
    user_quote TEXT NOT NULL DEFAULT '',
    accepted_at INTEGER NOT NULL,
    CHECK (
        (recorded_by = 'user' AND provenance = 'user-command') OR
        (recorded_by = 'lead' AND provenance = 'lead-quote' AND length(trim(user_quote)) > 0)
    )
);

CREATE TABLE IF NOT EXISTS report_reviews (
    id INTEGER PRIMARY KEY,
    report_task_id INTEGER NOT NULL REFERENCES tasks(id),
    review_task_id INTEGER NOT NULL REFERENCES tasks(id),
    report_digest TEXT NOT NULL,
    verdict TEXT NOT NULL CHECK (verdict IN ('pass', 'changes_requested', 'inconclusive')),
    reviewer_launch INTEGER NOT NULL CHECK (reviewer_launch > 0),
    recorded_at INTEGER NOT NULL,
    UNIQUE(report_task_id, review_task_id, report_digest, reviewer_launch)
);

CREATE TABLE IF NOT EXISTS research_attempts (
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

CREATE TABLE IF NOT EXISTS resource_leases (
    id INTEGER PRIMARY KEY,
    resource_type TEXT NOT NULL CHECK(resource_type IN ('path', 'git_ref')),
    scope_root TEXT NOT NULL DEFAULT '',
    scope_path TEXT NOT NULL,
    owner_project_id INTEGER NOT NULL REFERENCES projects(id),
    owner_task_id INTEGER REFERENCES tasks(id),
    owner TEXT NOT NULL,
    source_key TEXT NOT NULL UNIQUE,
    custody_token TEXT NOT NULL,
    intent_id INTEGER NOT NULL DEFAULT 0,
    mount_id INTEGER REFERENCES mounts(id) ON DELETE CASCADE,
    owner_pid INTEGER NOT NULL DEFAULT 0,
    owner_boot_id TEXT NOT NULL DEFAULT '',
    owner_start_time TEXT NOT NULL DEFAULT '',
    custody TEXT NOT NULL DEFAULT 'held' CHECK(custody IN ('held', 'recovery')),
    acquired_at INTEGER NOT NULL,
    CHECK((resource_type = 'path' AND scope_root = '') OR (resource_type = 'git_ref' AND scope_root <> ''))
);

CREATE TABLE IF NOT EXISTS review_cap_grants (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id),
    decision_id INTEGER NOT NULL REFERENCES decisions(id),
    round INTEGER NOT NULL CHECK(round > 0),
    result_json TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS review_carries (
    ship_task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repository TEXT NOT NULL,
    from_head TEXT NOT NULL CHECK (length(from_head) IN (40, 64)),
    to_head TEXT NOT NULL CHECK (length(to_head) IN (40, 64)),
    target_commit_sha TEXT NOT NULL CHECK (length(target_commit_sha) IN (40, 64)),
    tree TEXT NOT NULL CHECK (length(tree) IN (40, 64)),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (ship_task_id, repository, from_head, to_head)
);

CREATE TABLE IF NOT EXISTS review_dispositions (
    id INTEGER PRIMARY KEY,
    ship_task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    head_sha TEXT NOT NULL CHECK (length(head_sha) IN (40, 64)),
    decision_id INTEGER REFERENCES decisions(id),
    user_quote TEXT NOT NULL DEFAULT '',
    at INTEGER NOT NULL
, actor TEXT NOT NULL DEFAULT 'user', reason TEXT NOT NULL DEFAULT '');

CREATE TABLE IF NOT EXISTS review_finding_events (
    id INTEGER PRIMARY KEY,
    finding_id INTEGER NOT NULL REFERENCES review_findings(id) ON DELETE CASCADE,
    round INTEGER NOT NULL CHECK (round >= 0),
    event TEXT NOT NULL,
    actor TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    user_quote TEXT NOT NULL DEFAULT '',
    at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS review_findings (
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

CREATE TABLE IF NOT EXISTS review_patterns (
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

CREATE TABLE IF NOT EXISTS review_receipts (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repository TEXT NOT NULL,
    run_id TEXT NOT NULL,
    head_sha TEXT NOT NULL CHECK (length(head_sha) = 40 OR length(head_sha) = 64),
    review_status TEXT NOT NULL,
    reviewer_profile TEXT NOT NULL,
    reviewer_model TEXT NOT NULL,
    report_sha256 TEXT NOT NULL CHECK (length(report_sha256) = 64),
    receipt_sha256 TEXT NOT NULL CHECK (length(receipt_sha256) = 64),
    observed_at INTEGER NOT NULL,
    PRIMARY KEY(task_id, receipt_sha256)
);

CREATE TABLE IF NOT EXISTS review_rounds (
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

CREATE TABLE IF NOT EXISTS review_targets (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repository TEXT NOT NULL,
    commit_sha TEXT NOT NULL CHECK (length(commit_sha) = 40 OR length(commit_sha) = 64),
    PRIMARY KEY(task_id, repository)
);

CREATE TABLE IF NOT EXISTS review_verdicts (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    signal_id INTEGER NOT NULL,
    verdict TEXT NOT NULL CHECK (verdict IN ('pass', 'changes_requested', 'inconclusive')),
    report_sha256 TEXT NOT NULL CHECK (length(report_sha256) = 64),
    targets_json TEXT NOT NULL,
    launch_number INTEGER NOT NULL CHECK (launch_number > 0),
    profile_name TEXT NOT NULL,
    configured_model TEXT NOT NULL,
    model_known INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS signals (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    verb TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    data_json TEXT NOT NULL DEFAULT '{}',
    at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS task_adoptions (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id),
    source_task_id INTEGER NOT NULL UNIQUE REFERENCES tasks(id),
    repository TEXT NOT NULL,
    branch TEXT NOT NULL,
    pr_url TEXT NOT NULL,
    head_sha TEXT NOT NULL,
    generation INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS task_budget_usage (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    launch INTEGER NOT NULL,
    session_id TEXT NOT NULL,
    session_kind TEXT NOT NULL,
    tokens INTEGER NOT NULL DEFAULT 0,
    local_day TEXT NOT NULL,
    day_tokens INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(task_id, session_id)
);

CREATE TABLE IF NOT EXISTS task_launch_identities (
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    launch_number INTEGER NOT NULL CHECK (launch_number > 0),
    profile_name TEXT NOT NULL DEFAULT '',
    configured_model TEXT NOT NULL DEFAULT '',
    model_known INTEGER NOT NULL DEFAULT 0 CHECK (model_known IN (0, 1)), plan_revision INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (task_id, launch_number)
);

CREATE TABLE IF NOT EXISTS task_plan_input_migration (
    id INTEGER PRIMARY KEY CHECK (id=1),
    completed INTEGER NOT NULL DEFAULT 0 CHECK (completed IN (0,1)),
    tasks_done INTEGER NOT NULL DEFAULT 0 CHECK (tasks_done IN (0,1)),
    revisions_done INTEGER NOT NULL DEFAULT 0 CHECK (revisions_done IN (0,1)),
    task_cursor INTEGER NOT NULL DEFAULT 0,
    revision_task_cursor INTEGER NOT NULL DEFAULT 0,
    revision_cursor INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO task_plan_input_migration(id,completed,tasks_done,revisions_done) VALUES(1,1,1,1);

CREATE TABLE IF NOT EXISTS task_plan_revisions (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL CHECK (revision > 0),
    plan_json TEXT NOT NULL,
    PRIMARY KEY(task_id, revision)
);

CREATE TABLE IF NOT EXISTS task_recovery (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    generation TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    owner_pid INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'pending',
    last_error TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS task_repos (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    repo TEXT NOT NULL,
    worktree_path TEXT NOT NULL,
    base_ref TEXT NOT NULL,
    landing_mode TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'open',
    gated_sha TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL DEFAULT '',
    landed_ref TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL, target_branch TEXT NOT NULL DEFAULT '',
    UNIQUE(task_id, repo)
);

CREATE TABLE IF NOT EXISTS task_skill_uses (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    skill TEXT NOT NULL,
    uses INTEGER NOT NULL DEFAULT 1,
    first_at INTEGER NOT NULL,
    last_at INTEGER NOT NULL,
    PRIMARY KEY (task_id, skill)
);

CREATE TABLE IF NOT EXISTS task_spec_checks (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    checked_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS task_target_changes (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    repo TEXT NOT NULL DEFAULT '',
    source_task_id INTEGER NOT NULL REFERENCES tasks(id),
    from_branch TEXT NOT NULL,
    to_branch TEXT NOT NULL,
    pr_url TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK(status IN ('pending', 'complete', 'blocked', 'skipped')),
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    completed_at INTEGER NOT NULL DEFAULT 0,
    UNIQUE(task_id, repo, source_task_id)
);

CREATE TABLE IF NOT EXISTS task_turn_tokens (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    token TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    seq INTEGER NOT NULL,
    type TEXT NOT NULL,
    reviews_task_id INTEGER REFERENCES tasks(id),
    title TEXT NOT NULL,
    state TEXT NOT NULL,
    profile TEXT NOT NULL DEFAULT '',
    dispatch_rule TEXT NOT NULL DEFAULT '',
    landing_mode TEXT NOT NULL,
    autonomy_review TEXT NOT NULL DEFAULT 'ask',
    autonomy_land TEXT NOT NULL DEFAULT 'ask',
    branch TEXT NOT NULL DEFAULT '',
    base_ref TEXT NOT NULL DEFAULT '',
    worktree_path TEXT NOT NULL DEFAULT '',
    herdr_workspace_id TEXT NOT NULL DEFAULT '',
    pane_id TEXT NOT NULL DEFAULT '',
    pane_label TEXT NOT NULL DEFAULT '',
    agent_name TEXT NOT NULL DEFAULT '',
    agent_session TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL DEFAULT '',
    landed_ref TEXT NOT NULL DEFAULT '',
    last_output_hash TEXT NOT NULL DEFAULT '',
    last_worktree_hash TEXT NOT NULL DEFAULT '',
    last_progress_at INTEGER NOT NULL DEFAULT 0,
    agent_absent_since INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL, mount_id INTEGER REFERENCES mounts(id), launches INTEGER NOT NULL DEFAULT 0, gated_sha TEXT NOT NULL DEFAULT '', agent_server_started_at TEXT NOT NULL DEFAULT '', idle_since INTEGER NOT NULL DEFAULT 0, short_name TEXT NOT NULL DEFAULT '', agent_session_kind TEXT NOT NULL DEFAULT '', target_branch TEXT NOT NULL DEFAULT '', pr_title TEXT NOT NULL DEFAULT '', plan_json TEXT NOT NULL DEFAULT '', report_kind TEXT NOT NULL DEFAULT '', report_digest TEXT NOT NULL DEFAULT '', report_manifest_json TEXT NOT NULL DEFAULT '', containment_mode TEXT NOT NULL DEFAULT '', containment_path TEXT NOT NULL DEFAULT '', containment_marker TEXT NOT NULL DEFAULT '', pane_shell_identity TEXT NOT NULL DEFAULT '', agent_status TEXT NOT NULL DEFAULT '', agent_status_since INTEGER NOT NULL DEFAULT 0, workflow_run_id TEXT NOT NULL DEFAULT '', plan_revision INTEGER NOT NULL DEFAULT 0,
    UNIQUE(project_id, seq)
);

CREATE TABLE IF NOT EXISTS transitions (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    from_state TEXT NOT NULL,
    to_state TEXT NOT NULL,
    source TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS validation_candidates (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repo TEXT NOT NULL DEFAULT '',
    generation INTEGER NOT NULL CHECK (generation > 0),
    submitted_sha TEXT NOT NULL CHECK (length(submitted_sha) IN (40, 64)),
    candidate_sha TEXT NOT NULL CHECK (length(candidate_sha) IN (40, 64)),
    target_sha TEXT NOT NULL CHECK (length(target_sha) IN (40, 64)),
    expected_remote_sha TEXT NOT NULL DEFAULT '' CHECK (expected_remote_sha = '' OR length(expected_remote_sha) IN (40, 64)),
    observed_author_sha TEXT NOT NULL DEFAULT '',
    ref_name TEXT NOT NULL,
    worktree_path TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('preparing', 'active', 'stale', 'promoting', 'promoted', 'superseded', 'discarded')),
    promoted_sha TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL,
    verification TEXT NOT NULL DEFAULT '',
    proof TEXT NOT NULL DEFAULT '',
    risk TEXT NOT NULL DEFAULT '',
    refresh INTEGER NOT NULL DEFAULT 0 CHECK (refresh IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (task_id, repo, generation),
    CHECK (state <> 'promoted' OR promoted_sha = candidate_sha)
);

CREATE TABLE IF NOT EXISTS "validation_fix_rounds" (
    id INTEGER PRIMARY KEY,
    ship_task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('gate', 'ci', 'review', 'freshness')),
    seq INTEGER NOT NULL CHECK (seq > 0),
    expected_base_head TEXT NOT NULL CHECK (length(expected_base_head) IN (40, 64)),
    dedupe_key TEXT NOT NULL,
    message_id INTEGER NOT NULL,
    created_at INTEGER NOT NULL, member_heads_json TEXT NOT NULL DEFAULT '[]',
    UNIQUE (ship_task_id, kind, seq),
    UNIQUE (ship_task_id, kind, dedupe_key)
);

CREATE TABLE IF NOT EXISTS validation_stage_results (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    candidate_id INTEGER NOT NULL REFERENCES validation_candidates(id) ON DELETE CASCADE,
    repo TEXT NOT NULL DEFAULT '',
    stage TEXT NOT NULL CHECK (stage IN ('test', 'docs', 'lint')),
    candidate_sha TEXT NOT NULL CHECK (length(candidate_sha) IN (40, 64)),
    target_sha TEXT NOT NULL CHECK (length(target_sha) IN (40, 64)),
    config_sha256 TEXT NOT NULL CHECK (length(config_sha256) = 64),
    status TEXT NOT NULL CHECK (status IN ('passed', 'failed', 'invalidated')),
    summary TEXT NOT NULL,
    evidence TEXT NOT NULL,
    coverage TEXT NOT NULL DEFAULT '',
    invalidated_by_sha TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL, invalidated_from_status TEXT NOT NULL DEFAULT ''
    CHECK (invalidated_from_status IN ('', 'passed', 'failed')),
    UNIQUE (candidate_id, stage, candidate_sha),
    CHECK (invalidated_by_sha = '' OR length(invalidated_by_sha) IN (40, 64))
);

CREATE TABLE IF NOT EXISTS verdicts (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    repo TEXT NOT NULL,
    provider TEXT NOT NULL CHECK (provider IN ('builtin', 'greenhouse')),
    protocol TEXT NOT NULL DEFAULT '',
    grant_id TEXT UNIQUE,
    head_sha TEXT NOT NULL CHECK (length(head_sha) IN (40, 64)),
    target_sha TEXT NOT NULL CHECK (length(target_sha) IN (40, 64)),
    review_base_sha TEXT NOT NULL CHECK (length(review_base_sha) IN (40, 64)),
    verdict TEXT NOT NULL CHECK (verdict IN ('clean', 'findings', 'inconclusive')),
    blocking INTEGER NOT NULL CHECK (blocking >= 0),
    rules_digest TEXT NOT NULL DEFAULT '',
    config_digest TEXT NOT NULL DEFAULT '',
    input_digest TEXT NOT NULL DEFAULT '',
    backend TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    findings_digest TEXT NOT NULL DEFAULT '',
    findings_path TEXT NOT NULL DEFAULT '',
    mode TEXT NOT NULL CHECK (mode IN ('parallel', 'gate')),
    recorded_at INTEGER NOT NULL,
    CHECK (verdict = 'findings' OR blocking = 0)
);

CREATE TABLE IF NOT EXISTS verified_pr_heads (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    pr_url TEXT NOT NULL,
    head_sha TEXT NOT NULL,
    PRIMARY KEY (task_id, pr_url, head_sha)
);

CREATE INDEX IF NOT EXISTS branch_author_invalidated_verdicts_change_idx ON branch_author_invalidated_verdicts(change_task_id);

CREATE INDEX IF NOT EXISTS branch_authorship_task_idx ON branch_authorship(task_id);

CREATE INDEX IF NOT EXISTS ci_reruns_by_pr ON ci_reruns(task_id, repo, pr_url, head_sha);

CREATE UNIQUE INDEX IF NOT EXISTS ci_reruns_one_flake_per_head ON ci_reruns(task_id, repo, head_sha, check_name) WHERE reason = 'flake';

CREATE INDEX IF NOT EXISTS ci_test_failures_by_test ON ci_test_failures(project_id, test, observed_at);

CREATE UNIQUE INDEX IF NOT EXISTS decisions_one_pending_recovery_idx ON decisions(task_id)
    WHERE kind='recovery' AND answered_at=0 AND obsolete_at=0;

CREATE INDEX IF NOT EXISTS decisions_project_pending_idx ON decisions(project_id, answered_at, id);

CREATE INDEX IF NOT EXISTS delivery_operations_subject_idx ON delivery_operations(delivery_id, operation, head_sha, subject);

CREATE INDEX IF NOT EXISTS events_age_idx ON events(received_at);

CREATE UNIQUE INDEX IF NOT EXISTS greenhouse_review_requests_grant_idx ON greenhouse_review_requests(task_id, repo, grant_id) WHERE grant_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS greenhouse_review_requests_project_idx ON greenhouse_review_requests(project_id, requested_at);

CREATE INDEX IF NOT EXISTS intents_updated_at_idx ON intents(updated_at);

CREATE INDEX IF NOT EXISTS interventions_project_kind ON interventions(project_id, kind);

CREATE UNIQUE INDEX IF NOT EXISTS merge_queue_entries_live_idx ON merge_queue_entries(task_id, repo) WHERE dequeued_at = 0;

CREATE INDEX IF NOT EXISTS merge_queue_entries_target_idx ON merge_queue_entries(project_id, repo, target_branch, dequeued_at);

CREATE INDEX IF NOT EXISTS messages_claim_idx ON messages(task_id, status, claimed_at);

CREATE INDEX IF NOT EXISTS messages_queue_idx ON messages(task_id, status, created_at);

CREATE INDEX IF NOT EXISTS mounts_project_state_idx ON mounts(project_id, state, n);

CREATE INDEX IF NOT EXISTS notice_delivery_active_destination_idx ON notice_delivery_receipts(project_id, destination, created_at, delivery_id) WHERE state IN ('claimed','printed');

CREATE INDEX IF NOT EXISTS notice_delivery_receipts_lookup_idx ON notice_delivery_receipts(project_id, destination, state, updated_at);

CREATE INDEX IF NOT EXISTS notices_claim_idx ON notices(project_id, claim_token, delivered_at, acked_at);

CREATE UNIQUE INDEX IF NOT EXISTS notices_message_delivery_uncertain_idx
ON notices(project_id, task_id, json_extract(data_json, '$.message_id'))
WHERE kind='message_delivery_uncertain';

CREATE INDEX IF NOT EXISTS notices_open_idx ON notices(project_id, acked_at, delivered_at);

CREATE INDEX IF NOT EXISTS notices_project_id_idx ON notices(project_id, id);

CREATE INDEX IF NOT EXISTS notices_sandbox_episode_idx ON notices(task_id, id) WHERE kind IN ('sandbox_missing','sandbox_recovered');

CREATE INDEX IF NOT EXISTS notices_task_kind_idx ON notices(task_id, kind);

CREATE UNIQUE INDEX IF NOT EXISTS notices_worker_exited_once ON notices(task_id) WHERE kind = 'worker_exited';

CREATE INDEX IF NOT EXISTS pr_observations_task_latest_idx ON pr_observations(task_id, id DESC);

CREATE UNIQUE INDEX IF NOT EXISTS projects_project_uuid_idx ON projects(project_uuid);

CREATE INDEX IF NOT EXISTS provider_findings_head_idx ON provider_findings(task_id, repo, head_sha);

CREATE INDEX IF NOT EXISTS provider_findings_task_idx ON provider_findings(task_id, state, severity);

CREATE INDEX IF NOT EXISTS report_reviews_target_idx ON report_reviews(report_task_id, report_digest, verdict, recorded_at);

CREATE INDEX IF NOT EXISTS research_attempts_child_idx ON research_attempts(harness, child_id) WHERE child_id <> '';

CREATE UNIQUE INDEX IF NOT EXISTS research_attempts_key_idx ON research_attempts(project_id, harness, idempotency_key) WHERE idempotency_key <> '';

CREATE INDEX IF NOT EXISTS research_attempts_parent_idx ON research_attempts(project_id, task_id, state);

CREATE INDEX IF NOT EXISTS resource_leases_intent_idx ON resource_leases(intent_id) WHERE intent_id != 0;

CREATE INDEX IF NOT EXISTS resource_leases_owner_idx ON resource_leases(owner_project_id, owner_task_id, custody);

CREATE INDEX IF NOT EXISTS resource_leases_scope_idx ON resource_leases(resource_type, scope_root);

CREATE UNIQUE INDEX IF NOT EXISTS review_cap_final_review_once ON notices(task_id) WHERE kind='review_cap_final_review';

CREATE INDEX IF NOT EXISTS review_dispositions_ship_idx ON review_dispositions(ship_task_id, at, id);

CREATE INDEX IF NOT EXISTS review_finding_events_finding_idx ON review_finding_events(finding_id, id);

CREATE INDEX IF NOT EXISTS review_findings_pattern_idx ON review_findings(pattern_id, seq);

CREATE INDEX IF NOT EXISTS review_findings_ship_idx ON review_findings(ship_task_id, state, severity);

CREATE INDEX IF NOT EXISTS review_patterns_ship_idx ON review_patterns(ship_task_id, seq);

CREATE INDEX IF NOT EXISTS review_rounds_ship_idx ON review_rounds(ship_task_id, round, repository);

CREATE INDEX IF NOT EXISTS task_budget_usage_day_idx ON task_budget_usage(local_day, task_id);

CREATE INDEX IF NOT EXISTS task_target_changes_pending ON task_target_changes(project_id, status, id);

CREATE INDEX IF NOT EXISTS tasks_pane_idx ON tasks(pane_id);

CREATE INDEX IF NOT EXISTS tasks_project_workflow_idx ON tasks(project_id, workflow_run_id, seq);

CREATE INDEX IF NOT EXISTS tasks_project_pane_label_idx ON tasks(project_id, pane_label, seq);

CREATE INDEX IF NOT EXISTS tasks_project_seq_idx ON tasks(project_id, seq);

CREATE INDEX IF NOT EXISTS tasks_project_state_idx ON tasks(project_id, state);

CREATE UNIQUE INDEX IF NOT EXISTS validation_candidates_one_live_idx ON validation_candidates(task_id, repo)
    WHERE state IN ('preparing', 'active', 'stale', 'promoting');

CREATE INDEX IF NOT EXISTS validation_candidates_task_idx ON validation_candidates(task_id, repo, generation);

CREATE INDEX IF NOT EXISTS validation_fix_rounds_ship_idx ON validation_fix_rounds(ship_task_id, kind, seq);

CREATE INDEX IF NOT EXISTS validation_stage_results_candidate_idx ON validation_stage_results(candidate_id, stage, created_at);

CREATE INDEX IF NOT EXISTS verdicts_task_idx ON verdicts(task_id, recorded_at DESC);

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS fix_round_provider_findings_immutable BEFORE UPDATE ON fix_round_provider_findings
BEGIN SELECT RAISE(ABORT, 'provider finding fix-round binding is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS greenhouse_review_requests_immutable BEFORE UPDATE ON greenhouse_review_requests
WHEN NOT (
    OLD.outcome = 'failed' AND OLD.grant_id IS NOT NULL AND
    NEW.id = OLD.id AND NEW.project_id = OLD.project_id AND NEW.task_id = OLD.task_id AND
    NEW.repo = OLD.repo AND NEW.requested_head = OLD.requested_head AND NEW.provider = OLD.provider AND
    NEW.requested_at = OLD.requested_at AND NEW.grant_id = OLD.grant_id AND
    NEW.settled_at >= OLD.settled_at AND NEW.outcome IN ('failed', 'verdict', 'refused', 'head_mismatch')
)
BEGIN SELECT RAISE(ABORT, 'Greenhouse review request is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS greenhouse_review_requests_no_delete BEFORE DELETE ON greenhouse_review_requests
BEGIN SELECT RAISE(ABORT, 'Greenhouse review request is insert-only'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS project_history_revision_decision_delete AFTER DELETE ON decisions
BEGIN
    UPDATE project_history_revisions SET revision = random() WHERE project_id = OLD.project_id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT OLD.project_id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = OLD.project_id);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS project_history_revision_decision_insert AFTER INSERT ON decisions
BEGIN
    UPDATE project_history_revisions SET revision = random() WHERE project_id = NEW.project_id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT NEW.project_id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = NEW.project_id);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS project_history_revision_decision_update AFTER UPDATE ON decisions
BEGIN
    UPDATE project_history_revisions SET revision = random() WHERE project_id = OLD.project_id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT OLD.project_id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = OLD.project_id);
    UPDATE project_history_revisions SET revision = random() WHERE project_id = NEW.project_id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT NEW.project_id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = NEW.project_id);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS project_history_revision_notice_delete AFTER DELETE ON notices
BEGIN
    UPDATE project_history_revisions SET revision = random() WHERE project_id = OLD.project_id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT OLD.project_id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = OLD.project_id);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS project_history_revision_notice_insert AFTER INSERT ON notices
BEGIN
    UPDATE project_history_revisions SET revision = random() WHERE project_id = NEW.project_id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT NEW.project_id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = NEW.project_id);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS project_history_revision_notice_update
AFTER UPDATE OF id, project_id, task_id, kind, summary, data_json, created_at, delivered_at, acked_at ON notices
BEGIN
    UPDATE project_history_revisions SET revision = random() WHERE project_id = OLD.project_id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT OLD.project_id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = OLD.project_id);
    UPDATE project_history_revisions SET revision = random() WHERE project_id = NEW.project_id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT NEW.project_id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = NEW.project_id);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS project_history_revision_project_insert AFTER INSERT ON projects
BEGIN
    UPDATE project_history_revisions SET revision = random() WHERE project_id = NEW.id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT NEW.id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = NEW.id);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS provider_finding_events_immutable BEFORE UPDATE ON provider_finding_events
BEGIN SELECT RAISE(ABORT, 'provider finding event is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS provider_finding_patterns_immutable BEFORE UPDATE ON provider_finding_patterns
BEGIN SELECT RAISE(ABORT, 'provider finding pattern is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS provider_findings_evidence_immutable
BEFORE UPDATE OF verdict_id, task_id, provider, repo, head_sha, finding_key, pattern_seq, path, line, excerpt, severity, origin ON provider_findings
BEGIN SELECT RAISE(ABORT, 'provider finding evidence is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS research_attempts_identity_immutable
BEFORE UPDATE OF project_id, task_id, parent_role, harness, idempotency_key, previous_id, scope, access, prompt_digest, created_at ON research_attempts
BEGIN SELECT RAISE(ABORT, 'research attempt identity is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS review_cap_grants_immutable BEFORE UPDATE ON review_cap_grants
BEGIN SELECT RAISE(ABORT, 'Review cap grant is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS review_carries_immutable BEFORE UPDATE ON review_carries
BEGIN SELECT RAISE(ABORT, 'review carry is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS review_dispositions_immutable BEFORE UPDATE ON review_dispositions
BEGIN SELECT RAISE(ABORT, 'review disposition is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS review_finding_events_immutable BEFORE UPDATE ON review_finding_events
BEGIN SELECT RAISE(ABORT, 'review finding event is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS review_findings_evidence_immutable
BEFORE UPDATE OF ship_task_id, seq, pattern_id, fingerprint, round, repository,
    path, symbol, excerpt, line_at_report, head_sha, inherited_from ON review_findings
BEGIN SELECT RAISE(ABORT, 'review finding evidence is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS review_patterns_definition_immutable
BEFORE UPDATE OF ship_task_id, seq, key, title, rule, root_cause, fix_guidance, created_round ON review_patterns
BEGIN SELECT RAISE(ABORT, 'review pattern definition is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS review_receipts_immutable BEFORE UPDATE ON review_receipts
BEGIN SELECT RAISE(ABORT, 'review receipt is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS review_rounds_binding_immutable
BEFORE UPDATE OF ship_task_id, round, review_task_id, repository, scope_kind,
    scope_base_sha, head_sha, target_commit_sha, baseline_tree, delta_sha256,
    conflicted_paths_json, created_at ON review_rounds
BEGIN SELECT RAISE(ABORT, 'review round binding is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS review_rounds_terminal_status_immutable
BEFORE UPDATE OF status, verdict ON review_rounds
WHEN OLD.status = 'void'
    OR (OLD.status = 'verdict' AND NOT (NEW.status = 'void' AND NEW.verdict IS NULL))
    OR NEW.status NOT IN ('verdict', 'void')
    OR (NEW.status = 'verdict' AND NEW.verdict IS NULL)
    OR (NEW.status = 'void' AND NEW.verdict IS NOT NULL)
BEGIN SELECT RAISE(ABORT, 'review round status is terminal'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS review_targets_immutable BEFORE UPDATE ON review_targets
BEGIN SELECT RAISE(ABORT, 'review target is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS review_verdicts_immutable BEFORE UPDATE ON review_verdicts
BEGIN SELECT RAISE(ABORT, 'review verdict is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS terminalize_queued_messages
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

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS validation_candidates_submission_immutable
BEFORE UPDATE OF task_id, repo, generation, submitted_sha, target_sha, expected_remote_sha,
    refresh, created_at ON validation_candidates
BEGIN SELECT RAISE(ABORT, 'validation candidate submission is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS validation_fix_rounds_immutable BEFORE UPDATE ON validation_fix_rounds
BEGIN SELECT RAISE(ABORT, 'validation fix round is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS verdicts_immutable BEFORE UPDATE ON verdicts
BEGIN SELECT RAISE(ABORT, 'Verdict is immutable'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS verdicts_no_delete BEFORE DELETE ON verdicts
BEGIN SELECT RAISE(ABORT, 'Verdict is insert-only'); END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS "verdicts_no_delete";
DROP TRIGGER IF EXISTS "verdicts_immutable";
DROP TRIGGER IF EXISTS "validation_fix_rounds_immutable";
DROP TRIGGER IF EXISTS "validation_candidates_submission_immutable";
DROP TRIGGER IF EXISTS "terminalize_queued_messages";
DROP TRIGGER IF EXISTS "review_verdicts_immutable";
DROP TRIGGER IF EXISTS "review_targets_immutable";
DROP TRIGGER IF EXISTS "review_rounds_terminal_status_immutable";
DROP TRIGGER IF EXISTS "review_rounds_binding_immutable";
DROP TRIGGER IF EXISTS "review_receipts_immutable";
DROP TRIGGER IF EXISTS "review_patterns_definition_immutable";
DROP TRIGGER IF EXISTS "review_findings_evidence_immutable";
DROP TRIGGER IF EXISTS "review_finding_events_immutable";
DROP TRIGGER IF EXISTS "review_dispositions_immutable";
DROP TRIGGER IF EXISTS "review_carries_immutable";
DROP TRIGGER IF EXISTS "review_cap_grants_immutable";
DROP TRIGGER IF EXISTS "research_attempts_identity_immutable";
DROP TRIGGER IF EXISTS "provider_findings_evidence_immutable";
DROP TRIGGER IF EXISTS "provider_finding_patterns_immutable";
DROP TRIGGER IF EXISTS "provider_finding_events_immutable";
DROP TRIGGER IF EXISTS "project_history_revision_project_insert";
DROP TRIGGER IF EXISTS "project_history_revision_notice_update";
DROP TRIGGER IF EXISTS "project_history_revision_notice_insert";
DROP TRIGGER IF EXISTS "project_history_revision_notice_delete";
DROP TRIGGER IF EXISTS "project_history_revision_decision_update";
DROP TRIGGER IF EXISTS "project_history_revision_decision_insert";
DROP TRIGGER IF EXISTS "project_history_revision_decision_delete";
DROP TRIGGER IF EXISTS "greenhouse_review_requests_no_delete";
DROP TRIGGER IF EXISTS "greenhouse_review_requests_immutable";
DROP TRIGGER IF EXISTS "fix_round_provider_findings_immutable";
DROP INDEX IF EXISTS "verdicts_task_idx";
DROP INDEX IF EXISTS "validation_stage_results_candidate_idx";
DROP INDEX IF EXISTS "validation_fix_rounds_ship_idx";
DROP INDEX IF EXISTS "validation_candidates_task_idx";
DROP INDEX IF EXISTS "validation_candidates_one_live_idx";
DROP INDEX IF EXISTS "tasks_project_state_idx";
DROP INDEX IF EXISTS "tasks_pane_idx";
DROP INDEX IF EXISTS "task_target_changes_pending";
DROP INDEX IF EXISTS "task_budget_usage_day_idx";
DROP INDEX IF EXISTS "review_rounds_ship_idx";
DROP INDEX IF EXISTS "review_patterns_ship_idx";
DROP INDEX IF EXISTS "review_findings_ship_idx";
DROP INDEX IF EXISTS "review_findings_pattern_idx";
DROP INDEX IF EXISTS "review_finding_events_finding_idx";
DROP INDEX IF EXISTS "review_dispositions_ship_idx";
DROP INDEX IF EXISTS "review_cap_final_review_once";
DROP INDEX IF EXISTS "resource_leases_scope_idx";
DROP INDEX IF EXISTS "resource_leases_owner_idx";
DROP INDEX IF EXISTS "resource_leases_intent_idx";
DROP INDEX IF EXISTS "research_attempts_parent_idx";
DROP INDEX IF EXISTS "research_attempts_key_idx";
DROP INDEX IF EXISTS "research_attempts_child_idx";
DROP INDEX IF EXISTS "report_reviews_target_idx";
DROP INDEX IF EXISTS "provider_findings_task_idx";
DROP INDEX IF EXISTS "provider_findings_head_idx";
DROP INDEX IF EXISTS "projects_project_uuid_idx";
DROP INDEX IF EXISTS "pr_observations_task_latest_idx";
DROP INDEX IF EXISTS "notices_worker_exited_once";
DROP INDEX IF EXISTS "notices_task_kind_idx";
DROP INDEX IF EXISTS "notices_sandbox_episode_idx";
DROP INDEX IF EXISTS "notices_project_id_idx";
DROP INDEX IF EXISTS "notices_open_idx";
DROP INDEX IF EXISTS "notices_message_delivery_uncertain_idx";
DROP INDEX IF EXISTS "notices_claim_idx";
DROP INDEX IF EXISTS "notice_delivery_receipts_lookup_idx";
DROP INDEX IF EXISTS "notice_delivery_active_destination_idx";
DROP INDEX IF EXISTS "mounts_project_state_idx";
DROP INDEX IF EXISTS "messages_queue_idx";
DROP INDEX IF EXISTS "messages_claim_idx";
DROP INDEX IF EXISTS "merge_queue_entries_target_idx";
DROP INDEX IF EXISTS "merge_queue_entries_live_idx";
DROP INDEX IF EXISTS "interventions_project_kind";
DROP INDEX IF EXISTS "intents_updated_at_idx";
DROP INDEX IF EXISTS "greenhouse_review_requests_project_idx";
DROP INDEX IF EXISTS "greenhouse_review_requests_grant_idx";
DROP INDEX IF EXISTS "events_age_idx";
DROP INDEX IF EXISTS "delivery_operations_subject_idx";
DROP INDEX IF EXISTS "decisions_project_pending_idx";
DROP INDEX IF EXISTS "decisions_one_pending_recovery_idx";
DROP INDEX IF EXISTS "ci_test_failures_by_test";
DROP INDEX IF EXISTS "ci_reruns_one_flake_per_head";
DROP INDEX IF EXISTS "ci_reruns_by_pr";
DROP INDEX IF EXISTS "branch_authorship_task_idx";
DROP INDEX IF EXISTS "branch_author_invalidated_verdicts_change_idx";
DROP TABLE IF EXISTS "verified_pr_heads";
DROP TABLE IF EXISTS "verdicts";
DROP TABLE IF EXISTS "validation_stage_results";
DROP TABLE IF EXISTS "validation_fix_rounds";
DROP TABLE IF EXISTS "validation_candidates";
DROP TABLE IF EXISTS "transitions";
DROP TABLE IF EXISTS "tasks";
DROP TABLE IF EXISTS "task_turn_tokens";
DROP TABLE IF EXISTS "task_target_changes";
DROP TABLE IF EXISTS "task_spec_checks";
DROP TABLE IF EXISTS "task_skill_uses";
DROP TABLE IF EXISTS "task_repos";
DROP TABLE IF EXISTS "task_recovery";
DROP TABLE IF EXISTS "task_plan_revisions";
DROP TABLE IF EXISTS "task_launch_identities";
DROP TABLE IF EXISTS "task_budget_usage";
DROP TABLE IF EXISTS "task_adoptions";
DROP TABLE IF EXISTS "signals";
DROP TABLE IF EXISTS "review_verdicts";
DROP TABLE IF EXISTS "review_targets";
DROP TABLE IF EXISTS "review_rounds";
DROP TABLE IF EXISTS "review_receipts";
DROP TABLE IF EXISTS "review_patterns";
DROP TABLE IF EXISTS "review_findings";
DROP TABLE IF EXISTS "review_finding_events";
DROP TABLE IF EXISTS "review_dispositions";
DROP TABLE IF EXISTS "review_carries";
DROP TABLE IF EXISTS "review_cap_grants";
DROP TABLE IF EXISTS "resource_leases";
DROP TABLE IF EXISTS "research_attempts";
DROP TABLE IF EXISTS "report_reviews";
DROP TABLE IF EXISTS "report_acceptances";
DROP TABLE IF EXISTS "repo_watch_state";
DROP TABLE IF EXISTS "repo_fetch_failures";
DROP TABLE IF EXISTS "remote_branch_cleanups";
DROP TABLE IF EXISTS "quota_exhaustions";
DROP TABLE IF EXISTS "queued_task_declines";
DROP TABLE IF EXISTS "publish_pre_push_heads";
DROP TABLE IF EXISTS "provider_findings";
DROP TABLE IF EXISTS "provider_finding_patterns";
DROP TABLE IF EXISTS "provider_finding_events";
DROP TABLE IF EXISTS "projects";
DROP TABLE IF EXISTS "project_watch_state";
DROP TABLE IF EXISTS "project_runtime";
DROP TABLE IF EXISTS "project_repos";
DROP TABLE IF EXISTS "project_lookout_pollers";
DROP TABLE IF EXISTS "project_history_revisions";
DROP TABLE IF EXISTS "pr_task_watch_failures";
DROP TABLE IF EXISTS "pr_observations";
DROP TABLE IF EXISTS "pr_body_markers";
DROP TABLE IF EXISTS "notices";
DROP TABLE IF EXISTS "notice_notifications";
DROP TABLE IF EXISTS "notice_delivery_receipts";
DROP TABLE IF EXISTS "mounts";
DROP TABLE IF EXISTS "model_error_episodes";
DROP TABLE IF EXISTS "messages";
DROP TABLE IF EXISTS "merge_queue_entries";
DROP TABLE IF EXISTS "member_watch_errors";
DROP TABLE IF EXISTS "member_pr_poll_state";
DROP TABLE IF EXISTS "lead_start_claims";
DROP TABLE IF EXISTS "interventions";
DROP TABLE IF EXISTS "intents";
DROP TABLE IF EXISTS "greenhouse_review_requests";
DROP TABLE IF EXISTS "fix_round_provider_findings";
DROP TABLE IF EXISTS "events";
DROP TABLE IF EXISTS "delivery_operations";
DROP TABLE IF EXISTS "deliveries";
DROP TABLE IF EXISTS "decisions";
DROP TABLE IF EXISTS "decision_notice_cursors";
DROP TABLE IF EXISTS "config_approvals";
DROP TABLE IF EXISTS "completed_rider_busy";
DROP TABLE IF EXISTS "ci_test_failures";
DROP TABLE IF EXISTS "ci_reruns";
DROP TABLE IF EXISTS "budget_crossings";
DROP TABLE IF EXISTS "branch_authorship";
DROP TABLE IF EXISTS "branch_author_invalidated_verdicts";
DROP TABLE IF EXISTS "approvals";
