# Functional vocabulary and 0.14 upgrade

Implements epic #333 and slices #334-#338 in one release cut. ADR 0025 records the mapping and compatibility boundary.

## Observable contract

The CLI exposes `start`, `signal`, `teardown`, `watch`, `pool`, `list`, `merge` and `preferences`. Removed commands are hidden rejection entries, not working aliases. Rejection includes `--help` and does not invoke an operation.

```text
$ posse ride
error{code,message,retryable,help}: "usage","posse ride was removed; use posse start",false,[]
exit=2

$ posse land --help
error{code,message,retryable,help}: "usage","posse land was removed; use posse merge",false,[]
exit=2
```

Merge preparation is `posse merge <task>`. Integration is `posse merge <task> --execute`, with the existing approval, Gate, Review and freshness checks. New Briefs use `change`, `research`, `merge_mode`, `autonomy.merge` and `execution: sandbox`.

## Upgrade regression

`Test014ProjectVocabularyUpgradePreservesTasksSandboxesAndOpenPR` runs the real CLI with an isolated Herdr server and home. It creates historical Task and Notice values, an existing worktree and branch, an open PR observation and old config. Opening the upgraded Project migrates stored vocabulary, preserves Task IDs, paths, PR identity and quoted User text, lists the existing Sandbox, prepares the existing PR for Merge and rejects removed commands.

Additional regressions cover:

- new Brief rejection versus historical Brief interpretation, preserving literal User wording;
- one-time config migration, original-byte backup, idempotence, conflicts and rejection of reintroduced keys;
- admitted-plan versions 1/2 retaining exact serialized bytes, hashes and selected-input evidence;
- historical snapshot Notice/Decision/lease normalization without changing evidence or ownership;
- preserving a literal User option `land`, interrupted Workspace Merge intent and historical discard process authority;
- replacement of only the recorded historical observer, preserving unrelated panes and replacement agents;
- canonical root help, config schema and replacement-specific command/flag errors.

## Persisted compatibility

Published SQL migrations and identifiers remain unchanged: `mounts`, `mount_id`, `landing_mode`, `autonomy_land`, `landed_ref`, `ship_task_id`, `lookout_*` and `completed_rider_busy`. Snapshot TOML tags and existing filesystem lock names retain their spelling. Go fields, accessors and query names are canonical, generated through `sqlc.yaml` overrides. Historical preference paths, worktree paths and immutable admitted-plan bytes remain readable as stored data, not executable input aliases.

New Projects allocate `pool/<project>/sandbox-<n>`. Existing `remuda/<project>/mount-<n>` paths are not relocated because processes and Git registrations depend on them. A Sandbox is worktree/process containment, not a security boundary.

## Verification evidence

Commands and final outcomes are recorded in the PR verification entries. Task-local transcripts include `gate-unit.log`, `canonical-vocabulary.log`, `contracts-final.log`, `upgrade-e2e.log`, `retained-shell-settlement.log`, `e2e-final-{1,2,3,4}.log`, `vet-canonical.log` and `staticcheck-canonical.log`.

The full unit suite passed with a 30-minute timeout before the final migration refinements and merge from main: app tests took 1044 seconds on the shared host. The earlier timeout stack was in SQLite `fsync`, not a lock deadlock. Final-code vocabulary regressions also pass under the race detector across app, config, dispatch, store and taskplan. Formatting, vet, staticcheck, regeneration and shard-assignment checks pass.

All four E2E shards were attempted. Shard 1's test process passed in 877 seconds, but its shell wrapper was invalidated by an in-flight manifest edit after the test returned; this was verification sequencing, not a product failure. Shard 2 passed in 736 seconds. Shard 3 exposed the fixture races described below. Shard 4 exhausted its 15-minute budget while its current test had run for only 15 seconds. The post-merge focused E2E run passed in 196.559 seconds: `Test014ProjectVocabularyUpgradePreservesTasksSandboxesAndOpenPR`, `TestTeardownStopsRetainedShellByVerifiedIdentity`, `TestWorkspaceResearchTeardownPreservesMemberAndRootAttachments`, `TestPRMergeLifecycleAndExternalMerge` and `TestPRMergeAcceptsFollowUpBeforeFailureNotice`. The remaining full final-head Gate/E2E verification is a CI dependency under the Brief's shared-host-load exception, not a claim that all four final-head shards passed.

Main's #339 stabilization was merged before final checks. Its merged-PR freshness fix is retained, and its new text uses the canonical names. Main's #328 findings-ingestion implementation was subsequently merged without rebasing: the structured Finding Ledger, verdict checks and shared Signal-file resolver are retained, while incoming commands, Go fields and visible text use functional vocabulary. Published `ship_task_id` SQL columns remain unchanged. This PR must merge after the other release work.

A teardown E2E setup race was reproduced with the real CLI: Herdr had cleared `pane.Agent` but still exposed the exited agent in its agent registry. The fixture now waits for both observations to settle. The retained-shell regression passed three consecutive runs with that change; production ownership checks were not weakened. Workspace fixtures now respect retryable interrupted-intent conflicts, use main's busy-Lead/direct-ack stabilization and avoid exposing an unbound synthetic reviewer as a live Task to the Watch.

## Risk and rollback

This is a deliberate breaking vocabulary cut for scripts and new input. The migration changes persisted domain values, so an old binary must not open the upgraded database. Config migration keeps changed original files as `.v014`; unchanged files keep their bytes and receive a `.vocabulary` marker. Conflicting keys stop the upgrade without choosing a value.

Before deployment, stop Project processes and back up the complete Posse home, including the database and SQLite sidecars, config, snapshots, Task plans and preferences, together with each Project's Git metadata and worktree registrations. To roll back, stop upgraded processes, preserve post-upgrade commits and PRs, restore the pre-upgrade home and Git metadata plus `.v014` config backups, then reinstall 0.14. Do not downgrade against migrated state or relocate existing worktrees. Reverting the code alone does not reverse stored domain values.

Closes #333, #334, #335, #336, #337, #338.
