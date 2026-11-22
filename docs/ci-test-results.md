# CI test results and quarantine

Test jobs in `CI` save JSON Lines records as run artifacts named `test-results-*`, `e2e-results-*`, `e2e-main-*-results-*`, or `stress-*-results-*`. Each record contains the test name, package, shard, commit head, pass/fail result, duration in seconds, run ID, run attempt, job and event time. A failed Go invocation still writes its completed test events and exits with the original Go exit status. Jobs upload records even when their test step fails.

Download the artifacts for the workflow runs to include, then summarize the latest N recorded test runs per package and test:

```sh
find results -type f -name '*.jsonl' -print0 | xargs -0 python3 scripts/test-results.py summarize --last 20
```

The summary reports pass and failure counts, median duration and same-head reruns. A same-head rerun is an additional attempt of the same test in a GitHub Actions run on the same commit head. It is counted as passed when the later attempt passes, and failed when it still fails. Artifacts from attempts must be included together to measure reruns.

A test may be quarantined only while an open GitHub issue assigned to a named owner records the test, the reason, and an expiry date. Quarantine is a tracking label, not a CI exclusion: quarantined tests continue to run in their normal jobs, their results are uploaded and reported, and failures remain visible in CI. Do not add skip directives, filters or allow-failure rules for quarantined tests.
