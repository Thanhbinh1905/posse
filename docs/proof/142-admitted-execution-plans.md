# Issue #142: admitted execution plans

## Contract

New Tasks admit a version-1 plan before Project preparation or execution resource allocation. The plan records the existing outcome, Worker/Sandbox execution, scope and Members, review/delivery, base/target, Autonomy with provenance, managed paths and resolved launch settings. Selected Brief, preferences, skills and consumed configuration fields retain their bytes and SHA-256 digests. Unused configuration fields, including unrelated credentials, and unrelated forge comments are excluded.

Ordinary relaunch consumes the frozen argument vector, resume templates and instruction inputs. Explicit `--profile`, including the same name, appends a linked revision and clears Task/member Gate evidence before execution effects. Launch identities and Signals retain their revision binding. Existing Tasks remain legacy.

`posse status` exposes only the calling Worker's recorded state and plan. It opens SQLite read-only without config loading, migration, Herdr calls, recovery, synchronization or cleanup.

## End-user reproductions

The original isolated CLI reproduction launched `author` with `model-a`, edited the Profile to `edited-model`, then relaunched without `--profile`. Before the fix, the second launch used `edited-model`. `TestAdmittedPlanFreezesRelaunchConfiguration` now verifies ordinary relaunch keeps `model-a`, explicit reconfiguration selects `edited-model` at revision 2, old evidence is cleared and rebuild preserves history.

A second real-CLI regression edited the copied Brief after admission. Before fixing the separate PR-body reader, publish emitted `Closes #999` and `Refs #998` instead of the admitted `Closes #12` and `Refs #18`. `TestAdmittedPublishUsesSelectedBriefInsteadOfMutableCopy` now verifies the actual forge invocation uses the selected Brief. Historical multi-issue publication remains covered with an explicitly legacy Task. Title-refresh fixtures use `posse retitle` rather than editing admitted inputs.

## Acceptance coverage

- `internal/taskplan`: existing Change/Research/Review semantics, supported combinations, exact CRLF and binary input bytes, digests, detached inputs, argument replay and managed-path binding.
- Refusal tests assert no Task, intent, scratch, Sandbox, branch or pane allocation, and no Herdr calls before rejection.
- Own-status CLI coverage uses invalid current config, an unreachable Herdr socket and a sibling's unfinished intent. Status succeeds without exposing or changing that sibling; selecting another Task is refused.
- Task-create interruption coverage verifies revision 1 survives the process exit before any Sandbox exists and remains unchanged during recovery.
- Store coverage rebuilds initial and revised plans into a fresh database, preserves unknown historical model identity, rejects stale revision updates and clears both Task/member evidence.
- Concurrent snapshot coverage checks plan/history consistency during revisions. Admitted snapshot writers serialize across processes and exclude history newer than the captured Task row.
- Workspace coverage compares admitted scope, selected Members, paths, base/target and delivery with the registered repository facts.

## Verification

- `go test ./... -count=1 -timeout=20m`: passed; app 602.742 seconds, store 99.285 seconds.
- Formatting, `go generate ./... && git diff --exit-code`, `go vet -tags=e2e,perf ./...`, staticcheck 2026.2.1, shellcheck and shard-assignment self-check: passed.
- Every E2E manifest assignment passed with the local 25-minute package budget: shard 1 629.959 seconds; shard 2 482.157 seconds; shard 3 550.239 seconds; shard 4 781.031 seconds.
- `TestCommandPerformanceBudgets`: passed. CPU p95 was 6.841 ms for `_context`, 15.220 ms for owned `_ingest` and 15.143 ms for unowned `_ingest`; sampled wall p95 remained below 73 ms.
- Admission/revision/refusal/status/Codex tests passed with the race detector. The complete store and runtime race suites passed in 314.374 and 162.848 seconds.
- `go test -race -p=1 -count=1 -timeout=45m -json ./internal/app`: passed in 1127.235 seconds, with 930 test/subtest passes and no failures. Together with the other package results above, every unit package passed with the race detector. No credentialed live-agent runs were enabled.

## Main synchronization

Merge commit `c339170` incorporates `origin/main` through `da79d74` (PRs #263, #264, #266 and #268), without rebasing the published branch. The three textual conflicts were duplicate scratch-isolation and unsend-fixture fixes; both intents are preserved. The automatically merged PR helper also needed the `os` import used by main's current-target merge lock. Admission, read-only status and frozen PR inputs remain unchanged.

Post-merge verification:

- The Project Gate passed: formatting, vet with `e2e,perf`, staticcheck 2026.2.1, generation with no diff, and `go test ./...`. App tests took 595.346 seconds; store tests took 87.077 seconds.
- Targeted E2E passed in 189.249 seconds, with 21 test/subtest passes. Coverage includes admission, revision/rebuild, own-only status, selected Brief publication, legacy publication, PR title refresh, unsend isolation, launch skills, current-target merge refusal, PR merging, held-Project Watch repair, Claude Stop ownership, Pi session replacement and Workspace admission/merging.
- Targeted admission, snapshot, Claude Stop, Watch repair and merge-boundary race tests passed (app 39.077 seconds; store 17.210 seconds). The complete `internal/taskplan` race suite also passed.
- Performance budgets, shellcheck and shard-assignment self-check passed. Sampled command wall p95 remained below 58 ms.
- Read-only inspection still found the same 18 disclosed Claude trust references; no live trust-store cleanup was attempted.

## Test-suite findings

The original full E2E attempt exposed an existing unsend fixture race. It also reproduces on the unchanged base revision `ffc11ed`: the real poll-only Watch can legitimately claim a queued message before `unsend` starts. A temporary caller trace identified `watch --poll-only`, not `unsend`, as the delivery owner. The fixture now uses the existing helper to stop its own background Watch before queuing; all original assertions remain. The isolated regression passed 10 consecutive runs. Temporary tracing was removed.

The schema-43 migration required updating legacy-schema fixture teardown and current-version assertions. The first Workspace assertion assumed Git's initial branch was `main`; it now checks the actual registered default branch. A pre-admission `store_busy` refusal in the historical publication fixture now uses the existing documented retry helper, which refuses to retry if a Task was created.

An initial E2E shard hit its 10-minute aggregate timeout. Final local runs use the same manifest assignments with a 25-minute aggregate timeout; CI timeouts and individual assertions are unchanged. Failed and interrupted runs are not counted as passes.

The first complete race-enabled invocation reached its 20-minute package budget during a short fresh-setup case. It also exposed one five-second fresh-schema migration deadline under concurrent package/E2E load; no data-race diagnostic was reported. The same config-boundary test passed 10 isolated race-enabled runs in 1.52-1.88 seconds each. The complete app race suite then passed separately with `-p=1 -timeout=45m`, finishing in 18 minutes 47 seconds; assertions and production deadlines are unchanged.

## Isolation incident

Early app-test runs exposed a pre-existing isolation defect: `TestMain` redirected Posse and Codex homes but not Claude's config directory. `claude-trust` falls back to the OS user's home, so those runs wrote Task-scratch trust references into the existing `~/.claude.json`. Read-only inspection found 18 references to this Task's scratch. This was reported to the Lead. The live file was not modified to conceal or remove them. Follow-up read-only inspection after the isolated suites still counted 18 references.

The app harness now isolates HOME, POSSE_HOME, XDG and all consumed harness homes. Installed-binary fixtures also honor the configured non-`/tmp` Task scratch rather than hard-coding `/var/tmp`. Shared Go caches remain shared. The earlier affected runs were stopped and are not verification evidence.

## Risk and rollback

Migration 43 adds plan storage, revision history and launch-revision bindings without converting historical Tasks. Ordinary admitted relaunch intentionally ignores later launch-config edits; use explicit `--profile` to reconfigure. Identity remains configured launch identity, not runtime-model attestation.

A rollback build must retain additive migration 43, or the User must restore a pre-upgrade database backup before using a schema-42-only build. Do not drop plan state or downgrade a live database. Review of the disclosed Claude trust references is a separate User/Lead action, not an automatic rollback step.
