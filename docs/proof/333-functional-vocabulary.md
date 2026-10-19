# Functional vocabulary and 0.14 upgrade

Implements epic #333 and slices #334-#338 in one release cut. ADR 0026 records the mapping and compatibility boundary.

## Observable contract

The CLI exposes `start`, `signal`, `teardown`, `watch`, `pool`, `list`, `merge` and `preferences`. Removed User commands are hidden rejection entries, not working aliases. Rejection includes `--help` and does not invoke an operation. The stored 0.14 Pi/OpenCode adapter's exact JSON `lookout --handoff`/`--receipt` protocol is the sole runtime exception, using canonical Watch caller and receipt-ownership checks.

```text
$ posse ride
error{code,message,retryable,help}: "usage","posse ride was removed; use posse start",false,[]
exit=2

$ posse land --help
error{code,message,retryable,help}: "usage","posse land was removed; use posse merge",false,[]
exit=2
```

Merge preparation is `posse merge <task>`. Integration is `posse merge <task> --execute`, with the existing approval, Gate, Review and freshness checks. New Briefs use `change`, `research`, `merge_mode`, `autonomy.merge` and `execution: sandbox`.

## User upgrade steps

The installed 0.14 binary requires `posse update --stop-lookouts`; it cannot accept the new `--stop-watches` flag. The updater runs the new `posse setup` for integrations; run it manually after another installation method. Refresh previously installed global skills with `posse setup --global-skills` and restart every running Lead with `posse up --replace` from a User shell in each Project. This reloads the adapter and instructions held in memory. The hidden compatibility path keeps in-flight Pi/OpenCode Notice handoffs and receipts working until restart; it does not make other old commands or flags valid.

## Upgrade regression

`Test014ProjectVocabularyUpgradePreservesTasksSandboxesAndOpenPR` runs the real CLI with an isolated Herdr server and home. It creates historical Task and Notice values, an existing worktree and branch, an open PR observation and old config. Opening the upgraded Project migrates stored vocabulary, preserves Task IDs, paths, PR identity and quoted User text, lists the existing Sandbox, prepares the existing PR for Merge and rejects removed commands.

Additional regressions cover:

- new Brief rejection versus historical Brief interpretation, preserving literal User wording;
- one-time config migration, original-byte backup, idempotence, conflicts and rejection of reintroduced keys;
- admitted-plan versions 1/2 retaining exact serialized bytes, hashes and selected-input evidence;
- historical snapshot Notice/Decision/lease normalization without changing evidence or ownership;
- preserving a literal User option `land`, interrupted Workspace Merge intent and historical discard process authority;
- removal of only the recorded historical observer without a visible Watch pane, preserving unrelated panes and replacement agents;
- canonical root help, config schema and replacement-specific command/flag errors;
- retained 0.14 Pi/OpenCode handoff and receipt argv through the real CLI, while non-adapter old-name calls still fail;
- malformed first-open config retaining its filename and `config_invalid` diagnostic;
- partial `pool prune --yes` results retaining applied rows alongside an orphan-pane failure;
- interrupted intent steps preserving Member names and nested execution-status wrappers;
- anchored config approval keys, including `preferences.rider`, without rewriting names, values or User quotes;
- interrupted Orphan snapshots reusing historical `-leftover` refs and one Decision instead of creating duplicate branches.

## Persisted compatibility

Published SQL migrations and identifiers remain unchanged: `mounts`, `mount_id`, `landing_mode`, `autonomy_land`, `landed_ref`, `ship_task_id`, `lookout_*` and `completed_rider_busy`. Snapshot TOML tags and existing filesystem lock names retain their spelling. Go fields, accessors and query names are canonical, generated through `sqlc.yaml` overrides. Historical preference paths, worktree paths and immutable admitted-plan bytes remain readable as stored data, not executable input aliases.

New Projects allocate `pool/<project>/sandbox-<n>`. Existing `remuda/<project>/mount-<n>` paths are not relocated because processes and Git registrations depend on them. A Sandbox is worktree/process containment, not a security boundary.

## Delta review #771: persisted Task snapshot keys

The `b9b8d5e` merge normalization incorrectly changed four persisted Task TOML tags. The real CLI reproduction deleted SQLite and rebuilt from a literal 0.14 `task.toml`: Task 41 survived, but `MergeMode`, `AutonomyMerge` and `MergedRef` were empty and `SandboxID` was zero, without a decoder error. This was a production snapshot-compatibility regression introduced in this branch, not a Posse harness failure.

The canonical Go fields now retain the historical persisted identifiers `landing_mode`, `autonomy_land`, `mount_id` and `landed_ref`. No CLI/config aliases were introduced. A handwritten 0.14 fixture avoids generating legacy test data with the current serializer; `Test014TaskSnapshotRebuildPreservesMergeAuthorityAndSandbox` runs `recover --rebuild` as the User after database loss, asserts all four fields, Task/PR identity and User text, and verifies the held Sandbox link/path. It failed before the fix and passed afterward. Independent decoder and writer contract tests also failed before the fix and now pass, including zero undecoded fixture keys.

The exact retained `lookout` adapter command now skips Watch supervision, with a red/green regression asserting that delivery-only commands do not probe supervision. Clean help uses the verb "tear down".

Verification on unchanged delta Go sources:

- Full `go test ./internal/store ./internal/app -count=1 -timeout=30m`: exit 0; store 121.980s, app 560.640s.
- Eight recovery/0.14 compatibility E2Es, including cold database loss, moved Projects and stale sidecars, stopped Projects, legacy Workspace recovery, Notice receipts and retained adapters: exit 0, 37.790s. The new Task snapshot E2E is assigned to shard 1.
- Focused snapshot, supervision and CLI recovery race tests: exit 0; store 1.031s, app 15.648s.
- Formatting, build, tagged vet/staticcheck 2026.2.1, stable generation, tagged all-package compilation and both shard self-checks: exit 0.

Task-local evidence is `instruction-771-{rebuild,snapshot-keys,supervision}-{red,green}.log`, `instruction-771-full-unit.log/.exit`, `instruction-771-recovery-e2e.log/.exit`, `instruction-771-focused-race.log/.exit` and `instruction-771-static.log/.exit`. Main remained `2422226`; ordinary merge reported already up to date. The four-shard results below remain evidence for their named earlier heads, not this changed executable head. All final refreshed-head CI checks remain required before authorized Merge, with this PR last in release ordering.

## Release integration #770

Main `2422226` adds #348's E2E stabilization, #341's hidden supervised observation and update re-exec, and #323's Task process containment. Their behavior is retained with canonical names, including `posse clean`, cgroups/marker ownership, residual-process refusal, hidden Watch identity/heartbeat/repair and binary-replacement re-exec. Historical-pane cleanup retains the recorded 0.14 pane and Project-workspace checks, and refuses an agent replacing that pane.

Main now owns migrations 53/54 and ADR 0025. The unreleased vocabulary migration moves unchanged to 55 and its ADR to 0026. The exact original migration-53 checksum can be normalized to 55; other receipts are not relabelled. A persisted-state regression reproduced `migrations 53 differ from this build` without that normalization, then passed after the fix, including repeated opens and preserved Task IDs. Published migrations 53/54 remain unchanged. `sqlc` regeneration gives the retained `project_lookout_pollers` table the canonical Go model `ProjectWatchRuntime`.

`Test014LookoutReexecReportsReplacement` executes retained old observer argv into the new binary. Poll-only, timeout and help calls exit 2 with `posse lookout was removed; use posse watch`; the exact retained Pi/OpenCode handoff/receipt protocols still pass. The real binary-inode replacement E2E verifies canonical Watch re-exec separately. Historical private process bindings remain custody readers, not input aliases. `pool prune` reports `orphan_panes`, distinct from saved Orphan work; native ownership names and stale test/comment wording are corrected.

At integrated head `b9b8d5e`, full units passed (app 759.155s, store 140.091s); build, tagged vet/staticcheck, stable generation, both shard self-checks and focused race checks passed. E2E shards 1/2/4 passed in 762.681/632.257/783.215s. Shard 3 failed its immediate Decision delivery assertion while the answered Notice existed with `DeliveredAt=0`.

Seventy unchanged focused repetitions passed. Holding an earlier Notice's claim also passed, so that experiment did not reproduce the failure. Pinning the answer Notice itself in a valid in-flight claim reproduced the immediate assertion failure in 3/3 runs. The fixture now releases that claim after the CLI commits the User answer, then requires the real hidden Watch to deliver within ten seconds; it retains the exact User quote, delivered-state and correct Lead-prompt checks. Ten pinned runs passed in 36.966s. No production delivery or ownership guard changed. The original failing run's exact interleaving was not captured; the pinned case demonstrates the supported asynchronous state, not a proven trace of that run.

Final verification passed at unchanged Go head `58b7be0`:

| Check | Result |
| --- | --- |
| `go build ./...` | pass |
| Tagged `go vet` and staticcheck 2026.2.1 | pass |
| `go generate ./... && git diff --exit-code` | pass |
| Formatting and both shard self-checks | pass |
| `go test ./... -count=1 -timeout=30m` | pass; app 758.762s, store 141.172s |
| `scripts/run-e2e-shard.sh 1` | exit 0; 757.390s |
| `scripts/run-e2e-shard.sh 2` | exit 0; 624.442s |
| `scripts/run-e2e-shard.sh 3` | exit 0; 640.105s |
| `scripts/run-e2e-shard.sh 4` | exit 0; 781.679s |

All four original runners used their unchanged 15-minute budgets and count 1, with no Go or manifest edits during execution. Focused race checks passed at the production-equivalent `b9b8d5e` (app 61.017s, store 17.805s, plus config, dispatch and taskplan; AXI had no tests selected by that pattern). The subsequent commit changes only the Decision E2E fixture and documentation. Experiments used isolated homes/config and removed inherited `HERDR_*`; normal shared Go caches stayed in place. Post-run `git fetch origin; git merge --no-ff origin/main` reported already up to date, and the Task scratch process scan found zero remaining CWDs.

Task-local evidence: `instruction-770-rerun-{unit,shard-1,shard-2,shard-3,shard-4,static}.log` with exit receipts; `instruction-770-final-race.log`; `instruction-770-receipt-{red,green}.log`; `instruction-770-decision-answer-claim-{red,green}.log`; and `instruction-770-vocabulary-audit-final.log`. Before #771, only proof documentation and the stale `orphanBase` Go comment followed the verified head; executable code and test behavior were unchanged. Final published-head CI remains required before Merge; do not infer it from older green CI. The verification below records earlier heads, not this final integration.

## Verification evidence before #770

Latest complete local E2E verification before #770: all four shards passed at `8332042`, including the R2 config fix and CI fixture hardening. Details are in CI follow-up #747 below. The previously published R2 head `af1669b` also passed all CI checks; the final refreshed head still requires CI before Merge.

Commands and final outcomes are recorded in the PR verification entries. Task-local transcripts include `gate-unit.log`, `canonical-vocabulary.log`, `contracts-final.log`, `upgrade-e2e.log`, `retained-shell-settlement.log`, `e2e-final-{1,2,3,4}.log`, `vet-canonical.log` and `staticcheck-canonical.log`.

The full unit suite passed with a 30-minute timeout before the final migration refinements and merge from main: app tests took 1044 seconds on the shared host. The earlier timeout stack was in SQLite `fsync`, not a lock deadlock. Final-code vocabulary regressions also pass under the race detector across app, config, dispatch, store and taskplan. Formatting, vet, staticcheck, regeneration and shard-assignment checks pass.

All four E2E shards were attempted. Shard 1's test process passed in 877 seconds, but its shell wrapper was invalidated by an in-flight manifest edit after the test returned; this was verification sequencing, not a product failure. Shard 2 passed in 736 seconds. Shard 3 exposed the fixture races described below. Shard 4 exhausted its 15-minute budget while its current test had run for only 15 seconds. The post-merge focused E2E run passed in 196.559 seconds: `Test014ProjectVocabularyUpgradePreservesTasksSandboxesAndOpenPR`, `TestTeardownStopsRetainedShellByVerifiedIdentity`, `TestWorkspaceResearchTeardownPreservesMemberAndRootAttachments`, `TestPRMergeLifecycleAndExternalMerge` and `TestPRMergeAcceptsFollowUpBeforeFailureNotice`. The remaining full final-head Gate/E2E verification is a CI dependency under the Brief's shared-host-load exception, not a claim that all four final-head shards passed.

Main's #339 stabilization was merged before final checks. Its merged-PR freshness fix is retained, and its new text uses the canonical names. Main's #328 findings-ingestion implementation was subsequently merged without rebasing: the structured Finding Ledger, verdict checks and shared Signal-file resolver are retained, while incoming commands, Go fields and visible text use functional vocabulary. Published `ship_task_id` SQL columns remain unchanged. This PR must merge after the other release work.

A teardown E2E setup race was reproduced with the real CLI: Herdr had cleared `pane.Agent` but still exposed the exited agent in its agent registry. The fixture now waits for both observations to settle. The retained-shell regression passed three consecutive runs with that change; production ownership checks were not weakened. Workspace fixtures now respect retryable interrupted-intent conflicts, use main's busy-Lead/direct-ack stabilization and avoid exposing an unbound synthetic reviewer as a live Task to the Watch.

## Round 1 review

The review's concrete failures were reproduced before fixing them. `review-round1-e2e-red.log` records both retained adapters exiting 2 and malformed config returning `internal_error` without a filename. `review-round1-red.log` records hidden partial prune effects, duplicate Orphan refs, rewritten intent Member names, over-matched approval keys and unchanged real-format lease owners. Regression commands and their green results are recorded in the refreshed PR.

The #328 merge at `0f32f67` already resolves P1-2: its new files use `ChangeTaskID`, Change Task, Worker, `start` and `signal`. The merge retained the Finding Ledger and shared Signal-file resolver without rebasing. Main's #325 Gate Fix Rounds were then merged at `dfba532`, preserving bounded fix rounds and carrying functional vocabulary into the new code and fixtures. A further red/green regression ensures Gate fix instructions read admitted historical Briefs as stored data and retain the User's intent verbatim.

After the #325 merge, verification passed: full store, AXI, config, dispatch and taskplan suites (store 166.913 seconds); focused app/store race coverage (75.842/30.136 seconds); and eight real CLI E2E flows (211.170 seconds), including both retained adapters, malformed config, the 0.14 upgrade, local review/Gate authority, PR Merge, retained-shell Teardown and Workspace Research attachments. Formatting, vet, staticcheck, regeneration, tagged compilation and both shard self-checks passed. The new E2Es are assigned to shards 1 and 2. Logs are `review-round1-data-suites.log`, `review-round1-race.log`, `review-round1-e2e-final.log` and `review-round1-final-static.log`. This does not replace the final full Gate/all-four-shard CI dependency described above.

Main's #332 target, Signal-file and steering follow-ups were merged at `092f135`, retaining deleted-target PR cleanup, worktree-relative Signal paths, interrupt refusal summaries and blocked-message identity with canonical vocabulary. Expanded app/store race coverage passed (117.391/36.199 seconds). Additional Report assertions initially used an invalid fixture: repeating a needs-decision Signal or expecting a Change Task's needs-decision Signal to save a Report. The corrected E2E uses a separate Research Task's done Report, preserving both scratch and subdirectory-path coverage without changing production state checks. The final post-#332 six-flow E2E batch passed in 75.712 seconds (`review-round1-main-332-e2e-green.log`); expanded race and static checks also passed (`review-round1-main-332-race.log`, `review-round1-main-332-static-final.log`). The updater already runs setup for integrations; optional global skill refresh remains an explicit User step.

## Round 2 review

The real CLI reproduced R2-P1 before the fix: both `local` and the documented 0.14 `no-mistakes` Project shorthand failed with `config_invalid` (`review-round2-e2e-red.log`). Config regressions also reproduced completion markers being written for invalid shorthand and conflicting keys (`review-round2-config-red.log`).

Top-level `landing_mode` and `auto_unsaddle` now migrate to `merge_mode` and `auto_teardown`, including `landed` to `merged`. The rewritten schema is validated before writing the config, backup or completion marker. Failed validation/conflicts preserve original bytes and leave no completion marker, allowing a repaired file to retry. Load still enforces global versus Project scope, and removed shorthand remains invalid as new input. Regression coverage includes the 0.14 spec's Project example, per-repository settings, repeated reads, exact backups and conflicts. The new E2E is assigned to shard 1. The optional remaining Review Round error now says Change Task, and its local identifiers are canonical.

Main's #331 observation-contention coverage was merged without rebasing at `5529e5d`. Its conditional-write helpers and concurrent-observation/Lead protections remain intact; incoming tests use `change`, `pool`, `sandboxes` and canonical fixtures.

Post-merge verification passed: full config, AXI, dispatch and taskplan suites under race; focused app/store race coverage including the new observation-contention tests (104.661/57.794 seconds); and seven real E2E flows (77.316 seconds), including both shorthand modes, both retained adapters, malformed config, Signal paths, busy steering, PR Merge and the 0.14 upgrade. Formatting, tagged vet/staticcheck, tagged compilation, regeneration and both shard self-checks passed. Logs are `review-round2-config-race.log`, `review-round2-main-race.log`, `review-round2-main-e2e.log`, `review-round2-main-static.log` and `review-round2-main-compile.log`. Full final-head Gate/all-four-shard CI remains a dependency, not a local pass claim.

## CI follow-up (#747)

CI run `39203912594` at `c72fc08` failed the no-mistakes receipt refusal and PR failed-check Notice assertions. The captured log is `instruction-747-failed-ci.log`. The initial local run and six unmodified repeats passed (248.357 seconds), so the exact CI interleaving was not reproduced locally and is not claimed as a proven production root cause.

The fixtures now make observation ordering explicit. Review-policy completion waits for the fake to consume its launch prompt; the no-mistakes case stops the independent Watch and checks that both Merge phases refuse the missing receipt while leaving the candidate done and the target unchanged. The PR lifecycle case stops the Watch before its foreground forge-state assertions, retaining both the `pr_checks_failed` assertion and a new `ci_fix_exhausted` assertion for the zero budget, plus zero queued instructions and zero fix rounds. Background Watch behavior remains covered by dedicated E2Es. Production state, review and ownership checks are unchanged.

At `8332042`, all four unmodified shard runners exited 0: `scripts/run-e2e-shard.sh 1` (751.491 seconds), shard 2 (643.744), shard 3 (643.737) and shard 4 (830.392). Logs and exit receipts are `instruction-747-shard-{1,2,3,4}.log/.exit` in Task scratch. The focused two-test rerun also passed, and tagged vet/staticcheck passed. No manifest or Go source changed during the shard runs. CI for the previously published R2 head `af1669b` independently passed all 13 checks, including all four unit and E2E shards (`instruction-747-ci-current.log`). The final refreshed head must still pass CI before Merge.

## Risk and rollback

This is a deliberate breaking vocabulary cut for scripts and new input. The migration changes persisted domain values, so an old binary must not open the upgraded database. Config migration keeps changed original files as `.v014`; changed files are re-encoded without comments/layout, which remain in that backup. Unchanged files keep their bytes and receive a `.vocabulary` marker. Conflicting keys stop the upgrade without choosing a value.

Before deployment, stop Project processes and back up the complete Posse home, including the database and SQLite sidecars, config, snapshots, Task plans and preferences, together with each Project's Git metadata and worktree registrations. To roll back, stop upgraded processes, preserve post-upgrade commits and PRs, restore the pre-upgrade home and Git metadata plus `.v014` config backups, then reinstall 0.14. Do not downgrade against migrated state or relocate existing worktrees. Reverting the code alone does not reverse stored domain values.

Closes #333, #334, #335, #336, #337, #338.
