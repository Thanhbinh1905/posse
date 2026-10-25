# PR merges require the current target at the gated head

Amended 2026-10-24: the rule is optional and off by default; see [Amendment](#amendment-optional-and-off-by-default).

**Current status:** [ADR 0024](0024-built-in-validation-replaces-no-mistakes.md) and spec section 25.17 record the removal of no-mistakes (V15, #305); its recovery-run rule below no longer applies.

## Context

A green PR/MR observation identifies the checked head, but does not prove that the branch includes the target branch's latest commit. Two Tasks can therefore pass checks against the same target and merge sequentially, leaving the second branch stale after the first Merge. Duplicate migration versions are one semantic conflict this permits.

## Decision

Posse fetches each PR/MR's frozen target before running its Gate and before accepting final CI evidence. The current target tip must be an ancestor of the gated or observed PR head. The watcher checks freshness before and after each forge observation, so a target that advances during CI invalidates that result. Stale checks are recorded as non-ready, never as final merging evidence.

When a done or merging PR/MR is behind, including a clean-but-behind branch, Posse queues the Worker an instruction to merge `origin/<target>` into the Task branch, resolve conflicts, rerun the Gate, republish through the Task's delivery mode, and signal done again. It never rebases a pushed branch. In no-mistakes mode, the recovery run skips the pipeline's rebase step after the Worker has merged the target. Workspace Members use the same check and receive a member-specific instruction. Only the head of the target's merge queue receives the instruction; other behind Tasks wait in the queue ([ADR 0031](0031-serialize-target-updates-through-a-merge-queue.md)).

Before merging, Posse fetches the frozen target again under the repository sync lock and refuses with `pr_branch_behind`, naming the missing target commit, unless the target tip is an ancestor of the gated head. The forge merge remains pinned to the gated head. A stale branch cannot be merge-ready, regardless of approval or Autonomy.

## Consequences

- Clean-but-behind and conflicting PRs both require a Worker update, Gate rerun, republish and new done Signal before fresh CI can qualify.
- A target advance during a forge check makes that observation stale. The merge-time check also catches advances after the watcher poll.
- Rebasing a published branch remains prohibited; merge the target into it so the branch remains pushable.
- The local preflight cannot atomically pin the forge's target ref. Configure the forge to require up-to-date branches or use a merge queue to cover target changes from external actors during the check-to-merge interval.

## Alternatives

- **Trust green checks and forge mergeability.** Rejected because they can describe a head that was checked against an older target.
- **Update only when the forge reports a conflict.** Rejected because a clean-but-behind branch also lacks CI evidence for the current target.
- **Require forge branch protection or a merge queue only.** Recommended as an additional race guard, but Posse must still refuse a stale branch before `merge --execute`.

## Amendment: optional and off by default

The User decided on 2026-10-24 that the requirement is unnecessary (issue #483). With many open PRs it serialized merges to about one per CI cycle and multiplied CI load, even with the merge queue of [ADR 0031](0031-serialize-target-updates-through-a-merge-queue.md).

`defaults.require_current_target` (User-only, default `false`) now gates the decision above.

- **Off (default).** Posse skips the freshness check before the Gate, around forge observations and under the sync lock before `merge --execute`. A done Task whose gated PR head has green checks and forge mergeability `MERGEABLE` merges while behind its target, and clean-but-behind PRs receive no merge-target instruction. A PR the forge reports `CONFLICTING` still cannot merge, because Merge requires `MERGEABLE`; Posse emits `pr_conflict` and queues the done or merging Worker the same merge-target instruction, outside the merge queue. The merge queue is unused.
- **On.** The decision above and ADR 0031 apply unchanged.

Trade-off: semantic conflicts between PRs that each passed CI against an older target, such as the duplicate migration versions in Context, are no longer caught before merge. CI on the target branch after merge is the backstop. Projects that need the stronger guarantee set `defaults.require_current_target = true` or configure forge branch protection to require up-to-date branches.
