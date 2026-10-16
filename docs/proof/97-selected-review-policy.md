# D02: selected local review policy

Issue: #97. Plan: t172 D02, epic #92.

## Contract

- `defaults.review`: `none|local|no-mistakes`, default `none`.
- `repositories.<member>.review`: User-only override, frozen with its source in version-2 admission. Both config keys require recorded User consent from a Lead.
- A scoped Task choice can lighten an explicit Brief review/delivery preference, preserving the User quote and digest without granting merge authority.
- `none` keeps Gates and merge approval; `local` requires an independent D01 pass of the exact current candidate before Land, after Gates before landing, and again before local merge.
- `no-mistakes` fails closed with `review_receipt_missing` until D03 supplies the receipt adapter. A local pass cannot substitute.
- Explicit legacy config or new Brief values fail with migration instructions. Version-1 plans keep their frozen legacy values and expose `legacy-evidence` in `show` and Rider `status`.
- No conditional risk policy, Land risk flags or risk-assessment records are introduced. Publish risk/rollback text remains documentation.

## End-to-end evidence

The real CLI runs with an isolated Herdr server, `POSSE_HOME` and harnesses that make no model calls. Fixtures and logs stay under Task scratch; Go caches remain shared.

`TestSelectedReviewPolicyLocalLand` exercises an author, a completed Ship, an independent Reviewer and two real verdict Signals. With yolo enabled:

1. Missing review refuses both Land and merge with `review_required`.
2. A current pass opens Land.
3. A new commit makes the pass stale; merge refuses and leaves `main` unchanged.
4. A new Review of that commit passes; the branch must be re-gated before it merges.

Other E2E checks prove that default `none` keeps Gate failures and merge approval, missing pipeline receipts cannot be bypassed with yolo, both policy keys require consent, consent quotes survive verbatim, and legacy config values cannot be silently selected. Existing scoped preference and Review identity E2E tests also run.

Unit tests cover failed/inconclusive verdicts, evidence invalidation by Gate activity or author lineage changes, frozen per-Member overrides, unchanged Members, legacy plan decoding, config-layer migration and Brief tightening.

## Verification

Commands used:

```sh
go test ./...
go test -tags=e2e ./internal/e2e -run '^Test(SelectedReviewPolicy.*|ReviewPolicyLegacyConfigMigration|TaskPreferencesRequireScopedUserChoice|TaskPreferenceRefusalsHaveNoTaskEffects|AuthorizedFeatureBranchChoiceKeepsDefaultAndSurvivesRestart|WorkspacePreferenceCompatibilityPrecedesBranchEffects|ReviewRejects.*|ReviewResumeTemplateRejectsSecondSessionSelector|AuthorRelaunchResumeModelOverrideIsRecordedUnknown|Admitted.*)$' -count=1
go test -race ./internal/config ./internal/dispatch ./internal/taskplan ./internal/app -run 'Test(Review.*|ExplicitLegacyReview.*|LegacyReviewPlans.*|MemberReviewSelection.*|SelectedReviewPolicy.*|LocalReviewRechecked.*|WorkspaceSelectedReview.*)$' -count=1
go vet -tags=e2e,perf ./...
go run honnef.co/go/tools/cmd/staticcheck@2026.2.1 -tags=e2e,perf ./...
go generate ./... && git diff --exit-code
scripts/run-e2e-shard.sh --self-test
```

The local-review E2E flow also passed twice consecutively. New E2E tests are assigned to the CI shards.

## Test diagnostics

An early E2E attempt reopened the completed Ship as `working`. The identity fixture left its fake pane permanently working after reading the launch prompt. This was not a confirmed Posse completion bug: reconciliation correctly treated the still-working pane as resumed work. The policy E2E helper now reports that fake turn idle after its done Signal. The shared review-admission helper also restores only an author it temporarily moved from working to done, rather than reopening a genuinely completed Ship.

## Risk and rollback

Explicit legacy configuration intentionally stops loading until the User chooses a replacement or unsets the key. The existing live global `review = "on_risk"` was observed read-only and was not changed. Installation must include that explicit User migration.

Version-1 plans remain compatible with this binary, but older binaries cannot read newly admitted version-2 plans. Prefer a forward fix. A downgrade requires a matching pre-upgrade `POSSE_HOME` backup and config together with the old binary; do not reinterpret version-2 plans or silently rewrite policy values. Revert this PR to roll back the source change. PR/pipeline-owned evidence adapters remain D03 work.
