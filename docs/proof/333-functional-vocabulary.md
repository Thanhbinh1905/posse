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
- replacement of only the recorded historical observer, preserving unrelated panes and replacement agents;
- canonical root help, config schema and replacement-specific command/flag errors.

## Persisted compatibility

Published SQL migrations and identifiers remain unchanged: `mounts`, `mount_id`, `landing_mode`, `autonomy_land`, `landed_ref`, `ship_task_id`, `lookout_*` and `completed_rider_busy`. Snapshot TOML tags and existing filesystem lock names retain their spelling. Go fields, accessors and query names are canonical, generated through `sqlc.yaml` overrides. Historical preference paths, worktree paths and immutable admitted-plan bytes remain readable as stored data, not executable input aliases.

New Projects allocate `pool/<project>/sandbox-<n>`. Existing `remuda/<project>/mount-<n>` paths are not relocated because processes and Git registrations depend on them. A Sandbox is worktree/process containment, not a security boundary.

## Verification evidence

Commands and final outcomes are recorded in the PR verification entries. Task-local transcripts include `gate-unit.log`, `canonical-vocabulary.log`, `contracts-final.log`, `upgrade-e2e.log`, `retained-shell-settlement.log`, `e2e-final-{1,2,3,4}.log`, `vet-canonical.log` and `staticcheck-canonical.log`.

The full unit suite passed with a 30-minute timeout: app tests took 1044 seconds on the shared host. The earlier timeout stack was in SQLite `fsync`, not a lock deadlock. The final PR verification distinguishes complete-suite results from focused reruns and any remaining CI dependency.

A teardown E2E setup race was reproduced with the real CLI: Herdr had cleared `pane.Agent` but still exposed the exited agent in its agent registry. The fixture now waits for both observations to settle. The retained-shell regression passed three consecutive runs with that change; production ownership checks were not weakened.

## Risk and rollback

This is a deliberate breaking vocabulary cut for scripts and new input. The migration changes persisted domain values, so an old binary must not open the upgraded database. Config migration keeps changed original files as `.v014`; unchanged files keep their bytes and receive a `.vocabulary` marker. Conflicting keys stop the upgrade without choosing a value.

Before deployment, stop Project processes and take a complete Posse-home backup, including the database and SQLite sidecars, config, snapshots, Task plans and preferences. To roll back, stop upgraded processes, restore that complete pre-upgrade home and `.v014` config backups, then reinstall 0.14. Do not downgrade against migrated state or relocate existing worktrees. Reverting the code alone does not reverse stored domain values.

Closes #333, #334, #335, #336, #337, #338.
