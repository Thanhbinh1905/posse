# Task branches are independent of display names

A Task Name is presentation and may repeat or be localized; deriving a Git branch from it makes a durable ref depend on a mutable or ambiguous label. New Tasks therefore use `posse/t<n>` from their never-reused Task ID, while existing `short_name` and `branch` values remain unchanged.

## Consequences

- The Task's recorded `branch` is the source of truth for checkout, publish, landing, recovery and teardown.
- Dispatch and `ride` accept an optional safe display name; repeated names require `t<n>` for unambiguous lookup.
- A generated branch collision with Task history or a local or remote ref refuses admission rather than renaming or reusing a branch.
