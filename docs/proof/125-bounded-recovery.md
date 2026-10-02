# Issue #125: bounded Rider recovery

## Isolation

Tests use disposable `/tmp/posse-e2e-*` roots. The fixture strips inherited `HERDR_*` values and redirects `XDG_CONFIG_HOME`, `POSSE_HOME` and harness state before creating its own server. No live Project, live lab or installed binary was modified.

## Reproduction

Base: `e4c3781` (current `origin/main`, including #123).

The isolated CLI regression supplied prompt-wait failures through its Herdr socket and started real process groups for replacement agents. Before the fix:

```text
Eight plugin events: launches 0 -> 8, replacement process groups=8
show t1: prompt_not_delivered
ack all: prompt_not_delivered
lead: prompt_not_delivered
```

Failed project recovery restored the previous recovery generation, leaving every event eligible to relaunch again. There was no persistent Task retry budget. Inspection and Notice commands used the same launch-capable preparation path.

## Acceptance evidence

`TestFailedRecoveryBoundWithRealHerdr` uses the real isolated Herdr server. Its fake harness remains idle after every recovery prompt, causing Herdr's actual prompt-wait timeout:

```text
configured bound=2, launches=2, Rider process starts=2, exhaustion Notices=1
last error: the agent did not start working after 4 prompt attempts:
call Herdr method agent.wait: timed out waiting for agent status
```

`TestFailedRecoveryIsBoundedUnderPluginEvents` holds prompt-wait open while `show`, `ack`, `lead` and `lookout` run. Each succeeds within its two-second command deadline. Repeated plugin events, Notice acknowledgement and another Herdr generation leave the exhausted budget unchanged. Successful explicit relaunch resets it. `TestExplicitRelaunchSettlesPendingRecovery` also verifies that a successful explicit launch during backoff settles the pending generation without another automatic launch. `TestAutomaticRecoveryPreservesLateFailedSignal` verifies that recovery does not overwrite a Rider's failed Signal received during prompt delivery.

`TestRecoveryCrashConsumesFinalAttempt` exits immediately after agent startup. Later events raise one exhaustion Notice without starting another Rider. Store tests cover concurrent claims, persisted backoff, reopening the database and generation changes. Backoff tests cover doubling and the one-minute cap. The full suite exposed a second group close during the Lead's opening prompt: a completed Lead-only recovery claim could hide missing Riders from the older group episode. `TestRecoveryRepairsRidersFromAnInterruptedGroupEpisode` locks down that case and passes in under a second. The existing real-Herdr group-close regression passed three consecutive runs without weakening its assertions.

The full suite also exposed a test-fixture race, not a recovery failure: the fake Rider completed its queued instruction before the test sampled the first completion's Notice. The failure showed two legitimate `task_done` Notices, while the assertion expected exactly one at that stage. Fixture gates now separate the first completion, idle report and queued completion; the original exact-count and second-completion assertions remain intact. Failure diagnostics reopen the database instead of reading a closed handle.

## Verification

- Project Gate: formatting, `go vet -tags=e2e,perf ./...`, `go run honnef.co/go/tools/cmd/staticcheck@2026.2.1 -tags=e2e,perf ./...`, `go generate ./... && git diff --exit-code`, and `go test -count=1 ./...` passed.
- `go test -tags=e2e -count=1 -timeout=20m ./internal/e2e`: passed, 443.671 seconds.
- All six isolated CLI recovery regressions passed with `-race`.
- `go test -tags=e2e -count=3 ./internal/e2e -run '^TestRidersAsGroupedChildrenRecoverAfterAnotherPrimaryClosesGroup$'`: passed all three runs.
- `go test -tags=e2e -count=2 ./internal/e2e -run '^TestPosseSpawnNoticeLandTeardownAndRecovery$'`: passed both fixture-sequencing runs.
- `go test -tags='e2e perf' -count=1 ./internal/e2e -run '^TestCommandPerformanceBudgets$'`: passed.

## Risk and rollback

Automatic recovery now stops after the configured budget (default three attempts). An exhausted Rider requires inspection and an explicit successful `posse relaunch`; its Mount, branch and final process are retained. Inspection and Notice commands no longer relaunch Riders implicitly.

Migration 24 adds recovery state. A rollback build must retain this additive migration, or the User must restore a pre-upgrade database backup before using a build that supports only schema 23. Do not downgrade or drop state in a live database.
