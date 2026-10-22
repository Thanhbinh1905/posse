# Store contention and bounded recovery state

Task t357 addresses SQLite contention in `posse up` and `posse merge`, recovery snapshot growth, and leaked indefinite poll-only Watches.

## Reproduction

`internal/e2e/t357_repro_test.go` uses an isolated Herdr server and isolated `POSSE_HOME`. It starts several normal Project components, opens an independent SQLite writer transaction, updates the Project row, and holds the write lock for seven seconds while the command runs.

Before the change:

```text
TestT357ReproUpUnderWriter: elapsed=7.001s, error=store_busy, retryable=true
TestT357ReproMergeUnderWriter: elapsed=7.001s, error=intent_active, retryable=true, cause=SQLITE_BUSY
```

The `up` path exhausted its SQLite wait and returned `store_busy`. `merge --execute` returned its retryable intent error without retrying the intent admission after the writer released the lock.

After the change, the same bounded writer scenario retries the command-boundary store steps:

```text
TestT357ReproUpUnderWriter: elapsed=7.403s, success
TestT357ReproMergeUnderWriter: elapsed=7.361s, success; the command reached the fake forge merge path
```

The merge result is no longer a contention error. The reproduction first records a gated head and green PR observation, then proves that `merge --execute` completes after the writer releases the lock.

A stuck writer remains bounded by `commandContentionAttempts` and `commandContentionDelay`; after the retry budget, the existing typed retryable store error is returned.

## Recovery snapshot size

`TestProjectSnapshotBoundsHistoricalNoticeAndDecisionHistory` compares the capped snapshot with the old unbounded encoding using the same fixture. A representative run reports:

```text
bounded first=75,603 bytes, bounded second=75,603 bytes
unbounded first=149,127 bytes, unbounded second=298,119 bytes
```

The snapshot keeps every actionable Decision and Notice plus the most recent 128 historical records of each type. Acknowledged or obsolete records outside that tail no longer make every `PersistProject` read and encode grow with total history. The test also confirms that an older active Decision and Notice survive pruning.

## Leaked poll-only Watches

An indefinite foreground `posse watch --poll-only` has no private Project binding and is distinguishable from the detached managed Watch. Supervision now verifies its executable and Project working directory, sends the existing graceful stop sequence, and then starts or retains the managed Watch. Bounded foreground waits with `--timeout` remain untouched.

`TestT357ReapLeakedPollOnlyWatch` starts an unbound indefinite poller in the isolated Project, runs `posse list`, and verifies that the original PID is replaced by the private managed Watch PID. The old implementation left the foreground process competing for the store.

## Verification

```text
go test ./internal/store -run '^TestProjectSnapshotBoundsHistoricalNoticeAndDecisionHistory$' -count=1 -v -> pass

go test ./internal/app -run 'Test(MergeIntentFailurePreservesRetryableStoreContention|RetryBusyRepeatsOnlyTransientContention|WatchProcess|UnboundIndefinitePollOnlyWatch|SupervisedWatch|LiveLeadWatch)' -count=1 -> pass
go test -tags=e2e ./internal/e2e -run '^TestT357(Repro(Up|Merge)UnderWriter|ReapLeakedPollOnlyWatch)$' -count=1 -timeout=15m -v -> pass; up=7.403s, merge=7.361s, leaked Watch reaped
```

The full unit command for `internal/store` passed in 129.251s. A concurrent full `internal/app` run hit the ten-minute test timeout while SQLite `fsync` work was stalled under shared-host load. The concurrent race run also hit the timeout and its migration-lock test reported one environment contention failure before the timeout. The focused application tests above were rerun sequentially and passed, and vet, Staticcheck, generation stability, build and `git diff --check` passed.

## Risk and rollback

A recovery rebuild no longer restores acknowledged or obsolete history older than the bounded tail. Actionable Decisions and Notices remain durable, as do recent historical records. A foreground indefinite poll-only process inside the current Project is stopped during supervision; bounded `--timeout` waits are not stopped. Revert the change to restore the previous snapshot retention and Watch discovery behavior. No schema migration is required.
