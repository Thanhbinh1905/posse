# Selected Member scope for Workspace Tasks

Status: accepted

Workspace identity and configuration remain attached to one Workspace Project, but a Task does not need every Member checkout. Acquiring, validating or cleaning unrelated Members made broken or colliding repositories block work that did not select them. Copying all shared-root files by default also exposed context the Task did not request.

## Decision

- A workspace Task acquires worktrees only for its frozen `repos:` selection. Branch availability checks inspect only those Members. A Research may select no Members; a Review inherits only the reviewed Task's Members.
- New Tasks do not copy shared-root files by default. `root_context: read-only` explicitly selects a shared-root copy, bounded to 64 MiB and 10,000 entries. Symlinks are refused. The copy has no write permission, and completion compares its full file/directory snapshot to reject changes. The root context cannot be selected in a repository Project.
- New scoped Tasks reset only their selected Member worktrees. Clean legacy unselected worktrees are removed without resetting them; a dirty, attached or unavailable legacy checkout is never forced away, so its Sandbox is quarantined and acquisition may allocate another. A Task with a legacy plan retains historical all-Member cleanup after its shared-root and Member evidence has been preserved.
- Plans created before this change, and Tasks without an admitted plan, retain the legacy all-Member/shared-root view through Teardown. No global worktree pool or Project identity change is introduced.

## Consequences

A single-Member Task is independent of unrelated Member branch collisions and checkout failures. Multi-Member Tasks still carry each selected Member's branch, Merge Mode and forge. Workspace root context is opt-in, bounded and read-only for new Tasks. Root-only Research Tasks can run without editable Member repositories. Existing Tasks keep their recorded behavior, and legacy Sandbox data is preserved when it cannot be drained safely.

[ADR 0008](0008-workspace-projects-span-member-repositories.md) remains the record of Workspace Project identity, discovery and per-Member delivery. Its all-Member Sandbox and implicit shared-root behavior is superseded for newly admitted Tasks by this decision.
