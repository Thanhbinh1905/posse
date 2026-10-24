# Serialize PR target updates through a Posse merge queue

## Context

ADR 0019 makes a PR/MR merge only when its gated head contains the current target tip, and asks every behind done or merging Task to merge `origin/<target>`. With N done PRs on one target, every landing sends N Workers that instruction. Each landing then costs about N Worker turns and N full CI runs, PRs still land about one per CI cycle, done Tasks return to working against `max_workers`, and the extra CI load surfaces flakes and proxy errors.

GitHub's merge queue is not available for repositories owned by a personal account, and GitLab merge trains need a paid tier. Posse also needs the queue to send Worker instructions, keep the Gate and selected review evidence, and work across Workspace Members on different forges. A forge queue would therefore not replace the ADR 0019 freshness check.

## Decision

Posse keeps one FIFO merge queue per target, keyed by Project, Workspace Member (empty for a repository Project) and frozen target branch. There is no batching.

- A done or merging Change Task whose open PR head lacks the target tip enters its target's queue. Entries order by the Task's latest done Signal at entry, so poll order does not decide the head. A Task that is fresh never needs the queue.
- Only the queue head receives the ADR 0019 merge-target instruction. Other entries stay done or merging with no Worker turn and no CI rerun; their observations are recorded as `STALE`, and `pr_conflict` is still reported. `posse merge` on a queued Task refuses with `pr_branch_behind`, naming its position and the head.
- An entry whose merge-target instruction is in flight, from queueing until the Worker's next done Signal, stays the head even while its Task is working.
- An entry is dequeued when its PR merges, when its fresh head fails required checks (`checks_failed`, with the existing `pr_checks_failed` handling), or when its Task leaves done and merging without an in-flight instruction, for example through `needs-decision`, `stalled` or a Lead follow-up. The next entry is then promoted. A conflicting head receives the same instruction, which asks the Worker to resolve conflicts; if it cannot, it leaves the queue through its Signal.
- A Task dequeued for failed checks re-enters only after a later done Signal or a later green observation, and then joins at the back.
- `posse`, `posse list` and `posse show` display each queued Task's position.

## Invariants

- ADR 0019 is unchanged: nothing merges unless its gated head contains the target tip, checked before the Gate, after the forge observation and under the sync lock before `merge --execute`.
- At most one Task per target is asked to merge the target at a time in normal operation. Two processes placing two new entries concurrently may each promote one; that costs one extra update and never a stale merge.
- A fresh, green Task may still merge out of queue order. It cannot merge stale, so the guarantee holds; the head merges the target again.
- Queue state is derived scheduling state. It is not part of Task snapshots; a rebuilt store recreates entries on the next poll in done Signal order.

## Consequences

- Each landing costs one Worker turn and one CI run for the next behind PR instead of N.
- A queued PR waits for the PRs ahead of it. Its `pr_conflict` Notice is informational until it is promoted; the Lead does not send a merge-target instruction itself.
- A red head cannot block the line, and a flaky red head that is rerun green rejoins without a Worker turn.

## Alternatives

- **GitHub merge queue or GitLab merge trains.** Rejected: unavailable for user-owned repositories or the free tier, and they cannot send Worker instructions or keep Posse's Gate and review evidence.
- **Keep asking every behind PR.** Rejected: N updates per landing.
- **Batch promotion.** Deferred: testing K PRs merged together needs bisection on failure. It can follow once the single-entry queue is solid.
