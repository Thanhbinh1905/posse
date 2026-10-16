# PR merges require the current target at the gated head

## Context

A green PR/MR observation identifies the checked head, but does not prove that the branch includes the target branch's latest commit. Two Tasks can therefore pass checks against the same target and land sequentially, leaving the second branch stale after the first Land. Duplicate migration versions are one semantic conflict this permits.

## Decision

Before merging a PR/MR, Posse fetches the Task's frozen target branch and requires its latest commit to be an ancestor of the gated head. A stale branch is refused with `pr_branch_behind`; the Task stays `landing`, and the Rider must merge the target into the branch, rerun the Gate, and report done before retrying. The freshness check and forge merge hold the repository sync lock, so concurrent Posse merges cannot both pass against the same target tip. This applies to repository PRs and workspace member PRs/MRs.

## Consequences

- The check is independent of approval and applies to both automatic and User-approved Land.
- Rebasing a published branch remains prohibited; merge the target into it so the branch remains pushable.
- The local preflight cannot atomically pin the forge's target ref. Configure the forge to require up-to-date branches or use a merge queue to cover target changes from external actors during the check-to-merge interval.

## Alternatives

- **Trust green checks and forge mergeability.** Rejected because they can describe a head that was checked against an older target.
- **Reject duplicate migration versions only.** Rejected because the stale-base hazard is not limited to migrations.
- **Require forge branch protection or a merge queue only.** Recommended as an additional race guard, but Posse must still refuse a stale branch before `land --merge`.
