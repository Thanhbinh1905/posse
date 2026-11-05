# Task branches are independent of display names

A Task Name is presentation and may repeat or be localized; deriving a Git branch from it makes a durable ref depend on a mutable or ambiguous label. New Tasks therefore use `posse/t<n>` from their never-reused Task ID, while existing `short_name` and `branch` values remain unchanged.

## Consequences

- The Task's recorded `branch` is the source of truth for checkout, publish, merging, recovery and teardown.
- Dispatch and `start` accept an optional safe display name; repeated names require `t<n>` for unambiguous lookup.
- A generated branch collision with Task history or a local or remote ref refuses admission rather than renaming or implicitly reusing a branch.
- Explicit `adopt: t12` on a repository Change Brief is the exception: admission atomically transfers the open PR's exclusive branch claim, delivery custody, and held Sandbox to the new Task after checking the expected head and stopped writer. Both Tasks record lineage; the source retains its forge history and becomes `torn-down`. Old gates and approvals do not transfer. The first maintenance publish replaces the old body token only through recorded custody. Refusals leave branch, claim, Sandbox, and forge state unchanged. See the maintenance-adoption contract in [the spec](../spec.md).
