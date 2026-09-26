# A merged pull request Lands its Task

Posse treated Land as a step of the Task state machine: only a `landing` Task, or a `done` one with a safe worktree, could become `landed`. A Task sent back to `working` for a follow-up, or marked `lost`, never Landed when the User merged its pull request. It got no teardown, kept its Mount, and sometimes raised no Notice at all. Tasks t59, t61 and t62 (pull requests #5, #6 and #7) all ended `lost` with a merged pull request recorded as open. Commit 2bcd6e3 added a `pr_follow_up_pending` Notice for this case, but the Task still waited for the Worker to report `done`. The User chose to treat a merged pull request as the fact that decides Landing.

## Decision

- In `pr` and `no-mistakes` modes, a Task becomes `landed` as soon as the forge reports its pull request merged, from any live state and from `lost`, `failed` or `stalled`. This goes straight to `landed` and does not pass through `done`, so the invariant that only a Signal moves a Task to `done` holds.
- Posse watches every Task that has an open pull request, whatever its state, until the pull request is merged or closed.
- Work in the Mount beyond the merged head (later commits, or tracked and untracked non-ignored changes) is a Leftover. It never moves the Task back to `working`. Posse snapshots it as one commit on `posse/<name>-leftover`, then tears the Task down and releases its Mount. The Decision points at that branch: open a new Task from it, or discard it. Waiting for the User never holds a Mount or a tab.
- A workspace Task Lands when every Member's pull request has merged. A Member whose pull request merges is recorded as landed at once. A Member closed without merging becomes a Decision.
- A merged head counts as the Worker's own when that commit is on the Task branch in its Mount, or was recorded by `posse publish`. A head that is neither raises a Notice and is never skipped silently.

## Considered options

- **Keep Landing behind a `done` Signal** (commit 2bcd6e3). This keeps the Worker in charge of its Task's end. It is the behavior that left Riders open after the User merged.
- **Open a follow-up Task from a Leftover automatically.** This removes a Decision but spends a Worker on work the User may not want.

## Consequences

- A Worker still mid-turn when its pull request merges is interrupted. Its Task is torn down unless a Leftover exists.
- `posse send` to a `pr` Task first refreshes the pull request state. If it has merged, the command refuses with `pr_merged` and Lands the Task.
