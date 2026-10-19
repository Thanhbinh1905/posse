# Canonical resource leases coordinate filesystem and Git writers

## Context

Posse coordinates Tasks and repository sync across registered Projects and Workspace Members. A Sandbox's existing state prevents two Tasks from sharing that Sandbox, but it does not detect another writer registered through a symlink, a parent or child path, or another Project. Git worktrees also share refs through their common directory, so independent checkout paths can still mutate the same ref.

Process-local mutexes and Git's individual lock files do not coordinate these cross-process, cross-Project effects. Expiring a claim after a timeout can let a second writer enter while the first executor or one of its effects still has custody.

## Decision

Persist active resource leases in the shared SQLite store and include them in each owning Project's recovery snapshot.

- Canonical path scopes resolve symlinked existing components and preserve missing suffixes. Acquisition uses one write transaction to refuse equality and ancestor/descendant overlap across all Projects.
- Git-ref scopes are `(canonical git common directory, fully qualified ref)`. Ref ancestry conflicts; separate branches can proceed independently.
- Reconcile active Sandbox rows into path leases. Held, broken and pruning Sandboxes retain their claim; the claim is removed only when the Sandbox is safely idle or deleted.
- Local Merge claims its target ref and, when the target is checked out in the Project root, the canonical root path. Repository sync claims the root path and its local and remote default refs before fetching. Sync fetches only the selected remote branch and disables tag following.
- A lease has no TTL. Successful operations release it after the effect completes. Interrupted Merge recovery re-reads the target ref before reconciling its intent's claims. A running intent cannot finish while its resource leases remain. Status displays active lease IDs, scopes, owners and custody across registered Projects.

## Consequences

- Symlink aliases and nested Project roots cannot bypass Posse's path conflict check.
- A root writer blocks sync and local root merge, while independent Git refs are not serialized by a repository-wide resource claim.
- A crashed executor may leave a claim that needs recovery. Posse does not steal it based on age or process death alone. After inspecting the scope, a User can explicitly reconcile a non-Sandbox lease with `posse pool reconcile-resource <id> --user-approved "<words>"`; active owners and intents are refused.
- Resource leases survive database rebuilds with Project snapshots. Recovery must re-read any effect before releasing a claim.
- These leases coordinate Posse-managed operations. They do not sandbox arbitrary editor, shell or Git processes that ignore Posse.

## Alternatives

- **Per-process mutexes or only Git lock files.** Rejected because they do not atomically coordinate path scopes or unrelated Posse processes.
- **One lock per Project or repository.** Rejected because separate Projects can alias or nest on disk, and independent Git branches should remain available.
- **Expire leases after a timeout or dead PID.** Rejected because a child process or incomplete external effect may still hold custody.
