# A merged pull request Merges its Task

Posse treated Merge as a step of the Task state machine: only a `merging` Task, or a `done` one with a safe worktree, could become `merged`. A Task sent back to `working` for a follow-up, or marked `lost`, never Merged when the User merged its pull request. It got no teardown, kept its Sandbox, and sometimes raised no Notice at all. Tasks t59, t61 and t62 (pull requests #5, #6 and #7) all ended `lost` with a merged pull request recorded as open. Commit 2bcd6e3 added a `pr_follow_up_pending` Notice for this case, but the Task still waited for the Worker to report `done`. The User chose to treat a merged pull request as the fact that decides Merge.

## Decision

- In `pr` and `no-mistakes` modes, a Task becomes `merged` as soon as the forge reports its pull request merged, from any live state and from `lost`, `failed` or `stalled`. This goes straight to `merged` and does not pass through `done`, so the invariant that only a Signal moves a Task to `done` holds.
- Posse watches every Task that has an open pull request, whatever its state, until the pull request is merged or closed.
- Work in the Sandbox beyond the merged head (later commits, or tracked and untracked non-ignored changes) is an Orphan. It never moves the Task back to `working`. Posse snapshots it as one commit on `posse/<name>-orphan`, then tears the Task down and releases its Sandbox. The Decision points at that branch: open a new Task from it, or discard it. Waiting for the User never holds a Sandbox or a tab.
- A workspace Task Merges when every Member's pull request has merged. A Member whose pull request merges is recorded as merged at once. A Member closed without merging becomes a Decision; applying its discard answer polls the forge again and proceeds only while the request remains `CLOSED` and the Task remains unmerged.
- A merged head counts as the Worker's own when that commit is on the Task branch in its Sandbox, or was recorded by `posse publish`. A head that is neither raises a Notice and is never skipped silently.
- A merge is a fact, not evidence. When the merged head lacks the Task's selected review evidence (a current D01 pass or a pipeline-owned no-mistakes receipt for that exact head), the Task still Merges, Posse records no pass or receipt, and a `review_policy_violation` Notice reports the policy violation. Posse-controlled merges recheck that evidence immediately before the merge effect.

## Considered options

- **Keep Merge behind a `done` Signal** (commit 2bcd6e3). This keeps the Worker in charge of its Task's end. It is the behavior that left Workers open after the User merged.
- **Open a follow-up Task from an Orphan automatically.** This removes a Decision but spends a Worker on work the User may not want.

## Consequences

- A Worker still mid-turn when its pull request merges is interrupted. Its Task is torn down unless an Orphan exists.
- `posse send` to a `pr` Task first refreshes the pull request state. If it has merged, the command refuses with `pr_merged` and Merges the Task.
