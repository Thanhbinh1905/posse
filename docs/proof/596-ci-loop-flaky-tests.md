# #596 flaky test evidence

Run with `go test -tags e2e ./internal/e2e -run '^<test>$' -count=N`, the same flags as the CI e2e shard (`scripts/run-e2e-shard.sh`).

| Test | Runs | Result |
| --- | --- | --- |
| TestTeardownStopsTaskProcessesAndPruneProtectsHeldBranches | 5 (about 63s each) | pass |
| TestDoneTaskWithVanishedPaneCanBeDiscarded | 20 | pass |
| TestStoreLoadWithProjectHistoryKeepsWatchAndLeadFree | 20 | pass |
| TestLaunchSkillsAreInjectedOnlyIntoPosseSessions | 20 | pass |

`-count=20` on the Teardown test exceeds go's default 10m test timeout (20 x 63s), so it was run at 5. No run failed.

Under `-race -count=3`, TestStoreLoadWithProjectHistoryKeepsWatchAndLeadFree fails 3 of 3: the race detector slows Workers enough that Watch logs `SQLITE_BUSY` heartbeat, reconcile and maintenance deferrals, which the test counts as contention. The other three pass under `-race`. CI runs e2e shards without `-race` (the race shards do not build the `e2e` tag), so this does not reproduce a CI failure and the contention budget is left unchanged.
