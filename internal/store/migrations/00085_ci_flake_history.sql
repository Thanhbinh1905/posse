-- +goose Up
-- Go tests named in failed CI log tails Posse fetched, per PR head and job.
CREATE TABLE ci_test_failures (
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
CREATE INDEX ci_test_failures_by_test ON ci_test_failures(project_id, test, observed_at);

-- Every automatic or steward rerun of a failed CI check and its outcome.
CREATE TABLE ci_reruns (
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
CREATE UNIQUE INDEX ci_reruns_one_flake_per_head ON ci_reruns(task_id, repo, head_sha, check_name) WHERE reason = 'flake';
CREATE INDEX ci_reruns_by_pr ON ci_reruns(task_id, repo, pr_url, head_sha);

-- +goose Down
DROP TABLE ci_reruns;
DROP TABLE ci_test_failures;
