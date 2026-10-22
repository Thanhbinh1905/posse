# All Workers open as tabs of their Lead workspace

This amends [ADR 0009](0009-workers-open-as-tabs-of-the-lead-workspace.md) and supersedes [ADR 0012](0012-group-repository-workers-under-lead.md) for new Worker placement.

## Decision

Every new Worker, in Repository and Workspace Projects, opens with `tab.create` as an unfocused tab in its Lead's Herdr workspace. Its tab label is the Worker name, its pane receives `POSSE_WORKER_HOME`, and its `posse_row` token uses the existing tree label convention. The Remuda Mount, Git worktree lock, Task identity, pane ownership and tab-scoped Teardown remain unchanged. Posse no longer calls `worktree.open` or `workspace.move_block` to place Workers.

The Lead workspace is the Project orchestrator. This leaves room for a future top-level orchestrator without assuming that Project Leads are the only orchestrators in a Herdr session.

## Existing Tasks and recovery

Existing linked-child Workers remain in their current workspaces, retain their native workspace labels, and continue to reconcile and tear down using the existing linked-child ownership checks. New Worker launches and relaunches, including recovery after a Herdr restart, create a tab in the Lead workspace. A label and a fresh Herdr snapshot establish identity; recorded workspace IDs alone do not.

Teardown of a tab Worker closes only its owned tab, preserves foreign panes, the Lead and sibling tabs, and returns focus to the Lead only when appropriate. Teardown of a legacy linked child still closes only the verified child workspace and never its group.

## Consequences

- Repository and Workspace Workers have the same tab layout.
- Git worktree registration and locking remain independent from Herdr workspace placement.
- Closing the Lead workspace can close its Worker tabs. Posse Teardown remains scoped to the individual Worker tab.
- Existing linked-child workspaces may coexist with new Lead tabs until their Tasks are torn down.
