# Review verdicts bind completed revisions

**Current status:** ADR 0028 removes configurable command Gates. PR review evidence is still bound to `gated_sha` and forge CI; local delivery has no automated verification. [ADR 0024](0024-built-in-validation-replaces-no-mistakes.md) and spec section 25.17 record the removal of no-mistakes (V15, #305): the `no-mistakes` review policy fails config with `config_migration_required`, Posse no longer queries receipts, and stored `review_receipts` stay readable as history.

## Context

A Review Task read a moving Change branch and its Report was prose, so nothing recorded which revision a review covered or whether it passed. A Reviewer could not be prevented from reviewing something other than the candidate that would Merge.

## Decision

A Review Task is admitted only for a completed Change Task, and Posse pins the branch tip commit of each repository (or changed workspace Member) in insert-only `review_targets`. The Reviewer reports `--verdict pass|changes_requested|inconclusive` with its done Signal. Posse supplies the target and the Reviewer's Launch Identity Record, adds the Report digest, and writes the verdict in the Signal transaction. Qualification is computed when read: a pass counts only if its targets equal the Change branch tips now and the Reviewer launch still differs from every author launch. The selected `local` review policy makes `posse merge` require such a pass.

## Consequences

- A moved candidate has no current pass, and no operation rebinds an old verdict; a new revision needs a new Review Task.
- `changes_requested`, `inconclusive`, a missing verdict and a malformed Signal never satisfy Merge.
- Review Tasks created earlier have no target and cannot record a verdict.
- Verdicts and targets are part of Task snapshots, so rebuilds keep them.
- Posse still does not attest the Reviewer's runtime model, and Research Report review is out of scope.

### D02: selected evidence policy, separate from authority

`defaults.review` is `none|local|no-mistakes`, default `none`, with a `repositories.<member>.review` override. Both are User-only. Version-2 admission freezes the effective policy and provenance for each selected Member. Briefs may tighten; an explicit scoped User quote may lighten delivery/review only, never merge or executable authority.

`none` requires no independent-review record but keeps Gates and merge approval. `local` checks the exact current D01 pass before Merge, after Gates before `done -> merging`, and again immediately before each local merge. Evidence is independent of findings disposition (`autonomy.review`) and merge authority (`autonomy.merge`); neither Lead review disposition nor yolo bypasses it. `no-mistakes` requires a pipeline-owned receipt (D03). A local pass or ordinary CI is not a pipeline receipt.

Explicit legacy `always|on_risk|never` config and new Brief values require an actionable, explicit migration, with no inferred mapping. Version-1 Task plans remain readable and keep their frozen old behavior with visible `legacy-evidence` status. This is compatibility, not a risk classifier or evidence guarantee. There are no Merge risk flags or risk-assessment records; publish risk/rollback text is documentation only.

### D03: PR heads and pipeline-owned receipts

For PR delivery the candidate is the gated PR head. Before `done -> merging` and again under the merge lock immediately before the forge merge, the forge head, the newest green observation and the selected evidence must name that one commit. The merge command pins the same head. `local` requires a D01 pass targeting that commit. `no-mistakes` queries `no-mistakes axi receipt --head <sha>` and requires a `no-mistakes.review-receipt.v1` receipt with the Project origin repository, run id, the exact head, a completed run, a passed `review` stage, the reviewer Profile and model, and the report digest. The reviewer must differ from every author launch. Any missing field, unsupported upstream or mismatch fails closed with a specific error; Posse never supplies a field itself. Qualifying receipts are stored insert-only with Posse's digest of the receipt bytes.

The trust boundary is the no-mistakes implementation: Posse verifies what the pipeline reports about its own run and does not attest the runtime model. no-mistakes v1.84 has no receipt query, so the policy refuses until upstream implements the contract. An external merge without evidence follows ADR 0011 and is reported as `review_policy_violation`.

## Alternatives

- **Pin the branch name.** Rejected because the branch moves.
- **Let the Reviewer pass the SHA.** Rejected because the Reviewer could select an unrelated revision.
- **Mark verdicts stale when the candidate moves.** Rejected as unnecessary state; comparing targets at read time cannot go out of sync.
- **Accept an overall successful no-mistakes run, CI, forge approval or a Worker-reported run id.** Rejected because none binds a passed review stage and reviewer identity to the exact merged head.
