# posse owns a warm worktree Pool instead of per-Task Herdr worktrees

Each Project has a Pool of reusable worktrees (Sandboxes) that posse creates, tracks in `posse.db`, resets between Tasks and opens in Herdr with `worktree.open --path`. A fresh `worktree.create` per Task would cost a full dependency install and a cold build on every Task; a reused Sandbox keeps ignored files (dependencies, caches) while its tracked files are reset exactly to the default branch.

## Considered Options

- **Herdr `worktree.create` per Task, removed at Teardown**: rejected; always cold, and `worktree.remove` couples closing the workspace with deleting the worktree.
- **An external worktree-pool tool**: rejected; it keeps its own lease state beside posse's, so the two need ownership claims to stay in agreement.

## Consequences

- Teardown closes only the Task's panes, using `workspace.close` when the workspace contains no foreign panes, then releases the Sandbox; it never calls `worktree.remove`.
- Releasing a Sandbox stops orphan processes whose cwd is inside it, so dev servers and watchers do not leak into the next Task.
