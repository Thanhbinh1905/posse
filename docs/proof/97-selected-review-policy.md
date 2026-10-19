# D02: selected local review policy

Issue: #97. Plan: t172 D02, epic #92.

## Contract

- `defaults.review`: `none|local|no-mistakes`, default `none`.
- `repositories.<member>.review`: User-only override, frozen with its source in version-2 admission. Both config keys require recorded User consent from a Lead.
- A scoped Task choice can lighten an explicit Brief review/delivery preference, preserving the User quote and digest without granting merge authority.
- `none` keeps Gates and merge approval; `local` requires an independent D01 pass of the exact current candidate before Merge, after Gates before merging, and again before local merge.
- `no-mistakes` fails closed with `review_receipt_missing` until D03 supplies the receipt adapter. A local pass cannot substitute.
- Explicit legacy config or new Brief values fail with migration instructions. Version-1 plans keep their frozen legacy values and expose `legacy-evidence` in `show` and Worker `status`.
- No conditional risk policy, Merge risk flags or risk-assessment records are introduced. Publish risk/rollback text remains documentation.

## End-to-end evidence

The real CLI runs with an isolated Herdr server, `POSSE_HOME` and harnesses that make no model calls. Fixtures and logs stay under Task scratch; Go caches remain shared.

`TestSelectedReviewPolicyLocalLand` exercises an author, a completed Change, an independent Reviewer and two real verdict Signals. With yolo enabled:

1. Missing review refuses both Merge and merge with `review_required`.
2. A current pass opens Merge.
3. A new commit makes the pass stale; merge refuses and leaves `main` unchanged.
4. A new Review of that commit passes; the branch must be re-gated before it merges.

Other E2E checks prove that default `none` keeps Gate failures and merge approval, missing pipeline receipts cannot be bypassed with yolo, both policy keys require consent, consent quotes survive verbatim, and legacy config values cannot be silently selected. Existing scoped preference and Review identity E2E tests also run.

Unit tests cover failed/inconclusive verdicts, evidence invalidation by Gate activity or author lineage changes, frozen per-Member overrides, unchanged Members, legacy plan decoding, config-layer migration and Brief tightening.

## Verification

Commands used:

```sh
go test -timeout=20m ./...
go test -tags=e2e ./internal/e2e -run '^Test(SelectedReviewPolicy.*|ReviewPolicyLegacyConfigMigration|TaskPreferencesRequireScopedUserChoice|TaskPreferenceRefusalsHaveNoTaskEffects|AuthorizedFeatureBranchChoiceKeepsDefaultAndSurvivesRestart|WorkspacePreferenceCompatibilityPrecedesBranchEffects|ReviewRejects.*|ReviewResumeTemplateRejectsSecondSessionSelector|AuthorRelaunchResumeModelOverrideIsRecordedUnknown|Admitted.*)$' -count=1
go test -tags=e2e ./internal/e2e -run '^TestRiderAdmittedStatusDoesNotRecoverOrExposeSibling$' -count=1
go test -race ./internal/config ./internal/dispatch ./internal/taskplan ./internal/app -run 'Test(Review.*|ExplicitLegacyReview.*|LegacyReviewPlans.*|MemberReviewSelection.*|SelectedReviewPolicy.*|LocalReviewRechecked.*|WorkspaceSelectedReview.*)$' -count=1
go vet -tags=e2e,perf ./...
go run honnef.co/go/tools/cmd/staticcheck@2026.2.1 -tags=e2e,perf ./...
go generate ./... && git diff --exit-code
scripts/run-e2e-shard.sh --self-test
```

All listed checks passed, including formatting and the generated-code clean-diff check. Log summaries:

```text
full unit suite: internal/app 638.323s; all packages pass
Review E2E: internal/e2e 199.913s
Worker read-only status E2E: internal/e2e 6.162s
race-enabled policy tests: internal/app 108.105s; config/dispatch/taskplan pass
E2E shard assignment self-check passed.
```

The local-review E2E flow also passed twice consecutively. New E2E tests are assigned to the CI shards.

## Test diagnostics

An early E2E attempt reopened the completed Change as `working`. The identity fixture left its fake pane permanently working after reading the launch prompt. This was not a confirmed Posse completion bug: reconciliation correctly treated the still-working pane as resumed work. The policy E2E helper now reports that fake turn idle after its done Signal. The shared review-admission helper also restores only an author it temporarily moved from working to done, rather than reopening a genuinely completed Change.

Two full `go test ./...` runs exceeded Go's default 10-minute package timeout. They stopped at different existing workspace tests, each only one second into that test, with snapshot `fsync` in the stack. No review assertion failed. The full suite passed with `-timeout=20m`, matching the timeout already configured in CI; this changes the test bound, not the tested behavior.

## Verification after merging main

Merge commit `03ddbbf` incorporates `origin/main` at `45937bd` (#280). Both local Merge conflicts preserve resource custody: `withIntentGitResources` acquires the canonical path/ref leases, then the selected-review check runs inside its effect callback immediately before `mergeLocal`. Repository and workspace paths retain both safeguards.

Post-merge results:

```text
formatting, vet, staticcheck, generated-code clean diff: PASS
E2E shard assignment self-check: PASS
go test -timeout=20m ./...: PASS; internal/app 575.909s
recorded Review E2E plus Worker read-only status: PASS; 237.179s
race-enabled policy/lease boundary tests: PASS; app 39.918s, execution 19.025s
```

The post-merge E2E command adds `RiderAdmittedStatusDoesNotRecoverOrExposeSibling` to the recorded test-selection expression. The race run includes the selected policy tests, the overlapping-root local Merge refusal and the canonical resource-lease tests.

The Lead requested a detached done Signal delayed by 12 seconds to avoid known completion busy-grace issue #279. This is an operational workaround; this PR does not change completion-state reconciliation.

## Risk and rollback

Explicit legacy configuration intentionally stops loading until the User chooses a replacement or unsets the key. The existing live global `review = "on_risk"` was observed read-only and was not changed. Installation must include that explicit User migration.

Version-1 plans remain compatible with this binary, but older binaries cannot read newly admitted version-2 plans. Prefer a forward fix. A downgrade requires a matching pre-upgrade `POSSE_HOME` backup and config together with the old binary; do not reinterpret version-2 plans or silently rewrite policy values. Revert this PR to roll back the source change. PR/pipeline-owned evidence adapters remain D03 work.
