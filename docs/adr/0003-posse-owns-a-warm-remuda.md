# posse owns a warm worktree Remuda instead of per-Task Herdr worktrees

Each Project has a Remuda of reusable worktrees (Mounts) that posse creates, tracks in `posse.db`, resets between Tasks and opens in Herdr with `worktree.open --path`. A fresh `worktree.create` per Task would cost a full dependency install and a cold build on every Task; a reused Mount keeps ignored files (dependencies, caches) while its tracked files are reset exactly to the default branch.

## Considered Options

- **Herdr `worktree.create` per Task, removed at Teardown**: rejected; always cold, and `worktree.remove` couples closing the workspace with deleting the worktree.
- **An external worktree-pool tool**: rejected; it keeps its own lease state beside posse's, so the two need ownership claims to stay in agreement.

## Consequences

- Teardown closes only the Task's panes, using `workspace.close` when the workspace contains no foreign panes, then releases the Mount; it never calls `worktree.remove`.
- Releasing a Mount stops leftover processes whose cwd is inside it, so dev servers and watchers do not leak into the next Task.
