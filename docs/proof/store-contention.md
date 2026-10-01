# Store contention isolation

## Reproduction

Review t150's counterexample lets t1's observation commit, then holds an
independent SQLite write transaction for seven seconds while observations of
255 other Tasks run. A Lead Lookout and eight `roster` processes share the same
isolated POSSE_HOME. Before the fix, `holler done` rejects t1's Signal with a
non-retryable `internal_error` caused by other Tasks' observation deadlines.

`TestHollerDoneSurvivesOtherTasksObservationContention` reproduces this at the
CLI Signal preparation boundary. It failed before the context-isolation change.
It now verifies that the Scout's Signal commits and that the final Task's
observation does not inherit an expired whole-Project context.

The real CLI counterexample from commit `29af740` also passes against the final
binary, using a real isolated Herdr server and separate writer process:

```text
holler done: exit 0, signal=done, state=reported, task=t1
lookout: exit 1, code=store_busy, retryable=true
8 roster processes: exit 1, code=store_busy, retryable=true
```

The review harness expected a completed Scout's state to be `done`; its oracle
was corrected to `reported`, matching `RecordWorkerSignal`. Its socket proxy's
shutdown race was also handled. Neither adjustment changes the lock injection.

## Change

- Snapshot acquisition, each Task reconcile, Lead observation, and stall
  evaluation have separate bounded contexts. One Task cannot exhaust another
  Task's budget.
- Observation writes use a 250 ms SQLite busy handler within their own 3 s
  reconcile context. The connection's normal 5 s busy handler is restored before
  it returns to the pool.
- Signals defer only retryable observation contention, emit a diagnostic, and
  acquire their own transaction independently. Other reconcile failures remain
  blocking.
- All CLI command boundaries classify store contention as retryable `store_busy`.
- Remembering an already-known Herdr generation does not request a write lock.

## Verification

The store race suite and full race suite ran concurrently:

```text
GOMAXPROCS=4 go test -race -count=20 -timeout=25m ./internal/store
ok internal/store 1072.481s

GOMAXPROCS=4 go test -race -count=1 -timeout=15m ./...
all packages passed; internal/app 549.898s

go test -tags=e2e -count=1 -timeout=30m ./internal/e2e
passed twice: 440.485s and 426.858s

go test -tags='e2e perf' -count=1 ./internal/e2e -run '^TestCommandPerformanceBudgets$'
passed: 1.677s
```

Formatting, vet, staticcheck, generated-code checks, ShellCheck, installer tests
under dash and bash, and all four release-target cross-builds passed.

## Risk and rollback

A contended observation may remain stale until a later reconcile pass. An entire
pass can exceed 3 s because Tasks now have independent budgets; the stress
counterexample completes in about 24 s. A Signal whose own transaction cannot
acquire the write lock still returns retryable `store_busy`.

The synthetic writer proves the failure mechanism, not the identity of the
original production lock owner. No schema changes are needed. Roll back by
reverting this PR.
