# Use functional names for Posse concepts

Posse uses functional names because themed names need a glossary and are harder for agents and non-native English speakers to interpret. This release implements epic #333 and slices #334-#338 together. It replaces the proposed one-release alias period with immediate rejection of old input, while retaining automatic data and config migration.

| Formerly | Now |
|---|---|
| Rider | Worker |
| `posse ride` | `posse start` |
| `posse holler` | `posse signal` |
| `posse unsaddle` | `posse teardown` |
| Lookout / `posse lookout` | Watch / `posse watch` |
| Remuda / `posse remuda` | Pool / `posse pool` |
| Mount | Sandbox |
| Sheriff default persona | Lead, with no default persona |
| `posse roster` | `posse list` |
| Ship Task / `type: ship` | Change Task / `type: change` |
| Scout Task / `type: scout` | Research Task / `type: research` |
| Land / `posse land` | Merge / `posse merge` |
| Landing Mode / `landing_mode` | Merge Mode / `merge_mode` |
| Leftover / `--from-leftover` | Orphan / `--from-orphan` |
| `posse sweep` | `posse pool prune` |
| `posse playbook` | `posse preferences` |
| `remuda.*` | `pool.*` |
| `auto_unsaddle` / `landed` | `auto_teardown` / `merged` |
| `autonomy.land` | `autonomy.merge` |
| `--stop-lookouts` | `--stop-watches` |
| `execution: mount` | `execution: sandbox` |
| `notice_delivery: lookout` | `notice_delivery: watch` |

Notice kinds use the same replacements, including `rider_resumed` to `worker_resumed`, `mount_missing` to `sandbox_missing`, `land_ready` to `merge_ready`, and `unsaddle_incomplete` to `teardown_incomplete`. Task states are `merging` and `merged`. Product name `posse`, `lowkey`, Brief, Stall, `peek`, and other neutral terms stay unchanged. Herdr is external and unchanged.

## Merge is a two-step operation

`posse merge <task>` prepares integration: it runs the Gate, requires current Review evidence and checks the candidate head and target. It does not integrate changes. `posse merge <task> --execute` integrates the prepared candidate, retaining the existing User-approval or Autonomy requirement and rechecking freshness. It may also prepare a done Task before integrating it. This preserves the former two-step flow without the redundant `merge --merge` spelling. Publication remains `posse publish`.

## Sandbox is not a security boundary

A Sandbox is a per-Task git worktree with per-Task process containment. For Workspace Tasks it contains selected Members' worktrees. It separates changes and lets Posse identify and stop Task-owned processes before releasing custody. It does not restrict filesystem access, network access or credentials, and is not a security sandbox. Pool reuse occurs only after Teardown preserves the outcome and releases custody.

## Input rejection and persisted compatibility

Removed User commands are hidden entries that reject input and name their replacements, including `--help`. Old flags, config keys and Brief values are rejected with replacement-specific errors. There are no User aliases. Runtime compatibility is limited to the exact stored 0.14 Pi/OpenCode adapter protocol: hidden `lookout --json --handoff --destination pi:<session>|opencode:<session>` (optionally `--quiet-routine`) and `lookout --json --receipt <id> --receipt-outcome accepted|rejected|uncertain` (optionally `--receipt-token`). A supervised 0.14 poll-only Watch may also re-enter through `lookout --poll-only` after its PID, token, start time, Project and executable identity match the stored runtime record; the adapter restores canonical Watch bindings before continuing. Other Watch options remain rejected under the old name. Legacy stored Briefs and admitted plans are decoded only through historical-data paths, not admitted as new input.

The database migration changes domain values without changing IDs, PR observations, branches or custody references. Published SQL migrations, database table/column names and snapshot field names remain stable where they encode persisted data. SQL generator overrides give Go models and accessors canonical names without changing those columns. Task snapshot TOML likewise retains `landing_mode`, `autonomy_land`, `mount_id` and `landed_ref`; canonical Go field names do not rename these persisted identifiers. Existing filesystem lock names also remain stable so pre-upgrade processes retain custody. Immutable plan inputs, digests, User quotes and Report evidence remain byte-for-byte intact. Existing worktree paths are not moved: running processes and Git registrations depend on them. New Projects use `pool/<project>/sandbox-<n>`; existing Pool directories remain usable in place.

The vocabulary migration is version 55, after main's published Watch-runtime migration 53 and Task-containment migration 54. An exact-checksum receipt from this branch's unreleased vocabulary migration 53 moves to 55 before those published migrations run; other version-53 receipts are not reinterpreted.

When Watch maintenance resumes, the recorded pre-upgrade observer pane is closed within its Project workspace and the hidden supervised Watch from [ADR 0025](0025-hidden-supervised-project-watches.md) takes over without a visible pane. Unrelated workspaces stay untouched; an agent replacing the observer blocks the upgrade. Custody discovery can read old process argv and private `POSSE_LOOKOUT_*` bindings, rejecting conflicting bindings. Retained Pi/OpenCode handoffs and receipts keep their narrow compatibility path.

Stored global and Project config is migrated once before validation. A migration marker prevents reintroduced old keys from becoming aliases. Migration preserves a `.v014` copy of changed config for rollback and fails on conflicting old/new keys instead of choosing one. It rewrites only migrated keys and values, preserving comments and layout. Unchanged config retains its exact bytes and receives a sidecar marker. Legacy preference files remain data sources; the old command is removed.

## Script and layout compatibility

Public output and Herdr tokens change immediately; custom scripts and layouts must use the new spellings. Persisted SQL names and immutable evidence keep the compatibility rules above.

| Previous spelling | Release spelling |
| --- | --- |
| `ack.auto_unsaddled` | `ack.auto_torn_down` |
| Project summary `lookout` | `watch` |
| Update `lookouts`, `stopped_lookouts`, `reexec_lookouts` | `watches`, `stopped_watches`, `reexec_watches` |
| Update process kind `poller` | `watch` |
| Pool `mount`, `mounts` | `sandbox`, `sandboxes` |
| Sweep `orphans` | Pool prune `orphan_panes` |
| Herdr token `posse_mount` | `posse_sandbox` |
| Private process bindings `POSSE_LOOKOUT_*` | `POSSE_WATCH_*`; historical bindings are read for custody discovery and verified poller re-entry only |

## User upgrade steps

The first upgrade runs through the installed 0.14 binary, so use `posse update --stop-lookouts`, not `--stop-watches`. The updater runs the new `posse setup` for integrations; run it manually after another installation method. If `review = "on_risk"` prevents setup, the new binary remains installed so the User can run `posse config set defaults.review none` or `posse config unset defaults.review`, then `posse setup`; add `--project <name>` for Project config. Refresh optional global skills with `posse setup --global-skills` if previously installed. Restart every running Lead with `posse up --replace` from a User shell in that Project. Pi/OpenCode Leads keep their old adapter in memory until restart; the hidden machine protocol prevents silent Notice loss during this transition. Later updates use `--stop-watches`.

Rollback requires restoring the pre-upgrade home/database backup and any `.v014` config backups before reinstalling the previous binary. Do not run an older binary against migrated state.
