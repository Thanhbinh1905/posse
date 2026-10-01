# Store contention and stopped lock owners

Addresses #120 and #119. The quoted User intent remains "yes do all what u suggest".

## Reproductions

### Signal coupling and complete-Run budget

Read the adversarial reports from t150, t152 and t154. The t150/t152 harnesses are at
`29af740` and `aeb0a82`. The t150 counterexample commits t1's observation, then
holds an independent SQLite writer for seven seconds while 255 unrelated Tasks,
a Lead Lookout and eight `roster` processes share an isolated POSSE_HOME.
Previously this rejected t1's Signal with non-retryable `internal_error`.

The follow-up on `aa7fb88` exposed two remaining design failures:

```text
TestHollerCommitsBeforeUnrelatedPreparationFailure:
exit=1, internal_error, "unrelated snapshot failed"; Signal not committed

TestRunDefersRemainingTasksWhenBudgetSpent:
32 contended Tasks; full Run elapsed=8.3046s, budget=3s

TestRunBoundsMissingPaneTransition:
full Run elapsed=5.0071s, budget=3s
```

These regressions now pass. The Signal commits before preparation starts, and
Run stops admitting work before its budget expires. Deferred observations
progress on subsequent passes after writer release.

The t150 real-CLI harness also passes after the rework:

```text
256 Tasks, seven-second writer hold, real isolated Herdr server
holler done: exit 0, state=reported, elapsed=3.045s
Lead lookout: exit 0, delivered Notice after release, elapsed=7.132s
8 roster processes: exit 1, store_busy, retryable=true, elapsed=2.792-2.929s
```

Only the harness's Scout success oracle (`done` to `reported`) and teardown-only
socket listener race were corrected. Neither changes the contention injection.

The t152 `progress.py` harness, with four Tasks and an eight-second writer, now
keeps the same `lookout --timeout 20000` process armed through release. Status
and roster return retryable `store_busy`. Post-release Signal, Notice delivery
and status checks pass without a product workaround.

The full Gate exposed another ordering race: a queued fix could become visible
before its dispatcher reopened the Task, so an immediate Signal saw the previous
`done` state. `TestSendReopensDoneShipTaskBeforeImmediateSignal` reproduced it
at prompt submission. Reopening and Gate invalidation now commit atomically with
the message's `submitting` marker, before Herdr is called; late delivery updates
cannot overwrite an immediate Signal. The original lifecycle E2E and an atomic
rollback regression pass after this change.

A preliminary 250 ms Task context also expired during ordinary persistence under
race-suite load. The final design retains the existing three-second observation
context bound, capped by the pass deadline; only SQLite lock waits are 250 ms.
This separates the operation's budget from the busy handler's budget.

The t152 complete-Run generation-write regression passes twice with the race
detector. Its timeout-restoration harness passes five times: a blocked Project
observation takes about 252 ms, restores the pooled connection to 5000 ms, and
succeeds after writer release. Its eight-process migration-flock harness also
passes, returning retryable `store_busy` within the unchanged Open deadline.

### t154 many-Task follow-up

The independent t154 review exercised `aa7fb88`, before the Signal-first and
whole-pass rework in `51745ff`. Its exact 16-Task counterexample is now retained
as `TestReviewRunBudgetWithManyContendedTasks`, with an additional typed-error
assertion and a tighter 250 ms scheduling allowance. Re-running it confirms the
review finding on the old revision and the enclosing deadline on the new one:

```text
aa7fb88: 16 Tasks, held writer, Run=4.023854719s, FAIL
51745ff plus new regressions: 20 race-enabled passes under parallel load
Run=2.792140549-2.815011529s; retryable contention and remaining work deferred
Both new tests: PASS, 106.442s
```

`TestRunPrioritizesDeferredTaskObservations` audits actual SQLite observation
writes. It starts with t1/t2 recently observed and t3/t4 still older, then checks
that the pass writes `[4,3,1,2]`. After making t2 stale, the next pass starts with
t2. The old revision fails with `[1,2,3,4]`; the new revision passes all twenty
repetitions. Combined with the 32-Task budget/release regression, this covers
bounded admission, deferred progress and oldest-first scheduling. No additional
production change was needed for this follow-up.

### Stopped process holding a file lock

The requested comparison of `TestLeadLookoutOwnsNoticeUntilItExits` found no
observed rate increase:

```text
Task branch before the file-lock fix: 20/20 passed, 69.786s
origin/main 486045b23676ecb98ce6a630365149677709804d: 20/20 passed, 69.531s
Final lock and error-propagation fix: 30/30 passed under parallel load, 128.559s
```

Zero failures in these samples does not establish equal underlying flake rates.
The existing CI and t141 evidence establish that the stall predates this PR.

A deterministic real-CLI regression acquires `.mount-lock` in a separate
process, waits for acquisition, SIGSTOPs the owner, and runs Project status.
Before the fix the wake exceeded an eight-second watchdog. The final regression
covers both `.mount-lock` and `.git/posse-fetch.lock`: status returns retryable
`store_busy`, leaves the owned lock intact, and succeeds after the owner is killed.

The wake calls `ensureHeldMountLocks`, whose flock acquisition previously
retried forever under a background context. Repository-fetch acquisition had
the same unbounded loop. Excluding a SQLite transaction in the original test
does not exclude either file lock. A repeated run under parallel load caught
the repository-fetch lock path twice while the watcher was paused. Bounding
that wait initially exposed a second defect: `syncRepository` converted local
contention into an extra `root_behind` Notice despite the original Task Notice
remaining undelivered. Those two failures were not stolen Notices. Local fetch
contention now propagates as retryable `store_busy`, without fabricating a
behind-root warning. The final 30-run repetition passes.

This identifies a reproducible lock mechanism and the affected wake path; the
original CI process is no longer available to inspect. Both acquisition loops
are now bounded. The earlier hypothesis about SQLite transactions held
across external commands remains ruled out; these are separate filesystem locks.
The original test has a watchdog and accepts only a typed, retryable refusal
while paused; its Notice-ownership assertions are unchanged.

## Design

- Validate and atomically record only the Rider's own Signal first. Project
  preparation afterward cannot reject it, regardless of the unrelated failure.
- Bound post-Signal maintenance separately. Committed Signals and Notices remain
  durable when maintenance or delivery is deferred.
- Keep one overall three-second runtime budget. Give each admitted observation
  a fresh short context, and skip work that no longer fits. Include generation
  persistence in the pass, not an unbudgeted defer. Prioritize stale observations
  on later passes.
- Bound both direct/generated SQL writes and transaction acquisition when a
  caller has a short deadline. Restore the normal five-second SQLite busy timeout
  before returning each connection, or discard it if restoration fails.
- Preserve diagnostic text while classifying SQLite BUSY/LOCKED and contention
  deadlines as `store.ErrBusy`. Every CLI handler maps contention to retryable
  `store_busy`, preserving existing structured failures.
- Commit instruction authorization and its message-submission marker together,
  before exposing a fix to a Rider. Never reopen the Task after the prompt.
- Keep Lookout armed when preparation encounters transient contention.
- Bound Mount-state and repository-fetch flock acquisition to three seconds;
  do not remove or steal another process's lock. Propagate local contention
  instead of treating it as evidence that the Project checkout is behind origin.

## Verification at 51745ff

All final checks passed. The store stress suite overlapped the full race suite,
E2E runs and real-CLI reproductions.

```text
GOMAXPROCS=4 go test -race -count=20 -timeout=30m ./internal/store
ok internal/store 1293.518s

GOMAXPROCS=4 go test -race -count=1 -timeout=15m ./...
all packages passed; internal/app 528.875s

go test -tags=e2e -count=1 -timeout=15m ./internal/e2e
passed twice: 483.941s and 432.938s

go test -tags=e2e -count=30 -timeout=10m ./internal/e2e \
  -run '^TestLeadLookoutOwnsNoticeUntilItExits$'
30 consecutive passes: 128.559s

go test -tags='e2e perf' -count=1 ./internal/e2e \
  -run '^TestCommandPerformanceBudgets$'
passed: 2.625s
```

The t150/t152 real-CLI harnesses, complete-Run budget checks, timeout-restoration
checks, stopped-owner CLI tests and immediate-Signal regression passed.
Formatting, `git diff --check`, vet and staticcheck with `e2e,perf`, generation
with no generated changes, ShellCheck, installer tests under dash/bash and
CGO-disabled cross-builds for Linux/macOS amd64/arm64 also passed.

## t154 follow-up verification

This follow-up changes only tests and this proof document. Production code is
unchanged from `51745ff`. Fresh checks all passed:

```text
GOMAXPROCS=4 go test -race -count=1 -timeout=15m ./...
all packages passed; internal/app 454.195s, internal/runtime 40.634s

GOMAXPROCS=4 go test -race -count=20 -timeout=10m ./internal/runtime \
  -run '^(TestReviewRunBudgetWithManyContendedTasks|TestRunPrioritizesDeferredTaskObservations)$'
20 passes of each regression under parallel load: 106.442s

go test -tags=e2e -count=1 -timeout=20m ./internal/e2e
passed twice: 453.510s and 421.703s

go test -tags=e2e -count=30 -timeout=10m ./internal/e2e \
  -run '^TestLeadLookoutOwnsNoticeUntilItExits$'
30 consecutive passes: 110.033s

go test -tags='e2e perf' -count=1 ./internal/e2e \
  -run '^TestCommandPerformanceBudgets$'
passed: 1.599s
```

Formatting, vet, staticcheck, generation/no generated changes, ShellCheck,
installer tests under dash/bash and all four release cross-builds passed again.
The earlier 20-run store race result remains evidence for the unchanged store
implementation; the fresh full race suite also passed its store package in
62.362s. Herdr/CLI tests used isolated fixture homes and servers without inherited
`HERDR_*` variables.

## Risk and rollback

A bounded pass may leave observations and stall evaluation stale until a later
pass. Persistent contention in the Signal's own transaction still rejects that
Signal with retryable `store_busy`. A successful Signal may print a deferred
maintenance or delivery diagnostic; its durable Notice remains available for
Lookout. An instruction whose submission result is uncertain leaves its Task
reopened and raises `message_delivery_uncertain`, rather than assuming Herdr
rejected it or automatically retrying it. A stopped lock holder now causes a bounded retryable refusal instead
of an indefinite wait.

Synthetic writers and lock holders establish failure mechanisms, not the
identity of the original production lock owner. No schema changes. Roll back
by reverting this PR; no database rollback is needed.
