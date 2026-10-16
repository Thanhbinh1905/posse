# Review verdicts bind completed revisions

## Context

A Review Task read a moving Ship branch and its Report was prose, so nothing recorded which revision a review covered or whether it passed. A Reviewer could not be prevented from reviewing something other than the candidate that would Land.

## Decision

A Review Task is admitted only for a completed Ship Task, and Posse pins the branch tip commit of each repository (or changed workspace Member) in insert-only `review_targets`. The Reviewer reports `--verdict pass|changes_requested|inconclusive` with its done Signal. Posse supplies the target and the Reviewer's Launch Identity Record, adds the Report digest, and writes the verdict in the Signal transaction. Qualification is computed when read: a pass counts only if its targets equal the Ship branch tips now and the Reviewer launch still differs from every author launch. `defaults.review = "always"` makes `posse land` require such a pass.

## Consequences

- A moved candidate has no current pass, and no operation rebinds an old verdict; a new revision needs a new Review Task.
- `changes_requested`, `inconclusive`, a missing verdict and a malformed Signal never satisfy Land.
- Review Tasks created earlier have no target and cannot record a verdict.
- Verdicts and targets are part of Task snapshots, so rebuilds keep them.
- Posse still does not attest the Reviewer's runtime model, and Scout Report review is out of scope.

## Alternatives

- **Pin the branch name.** Rejected because the branch moves.
- **Let the Reviewer pass the SHA.** Rejected because the Reviewer could select an unrelated revision.
- **Mark verdicts stale when the candidate moves.** Rejected as unnecessary state; comparing targets at read time cannot go out of sync.
