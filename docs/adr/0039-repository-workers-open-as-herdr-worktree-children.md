# Repository Workers open as Herdr worktree children

Repository Workers open with `worktree.open` as linked child workspaces of the Lead, restoring the native grouping in [ADR 0012](0012-group-repository-workers-under-lead.md). This supersedes [ADR 0013](0013-all-workers-open-as-lead-tabs.md) for Repository Projects; Workspace Project Workers remain Lead tabs because their Sandboxes span multiple Git worktrees. Teardown closes only the verified child, while recovery reopens the child after a Herdr restart.
