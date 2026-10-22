# Git-only orchestration without command Gates

## Status

Accepted

## Decision

Posse orchestrates Git repositories and Workspaces only. Non-Git directories are edited directly outside Posse. `posse project add --directory` and Briefs selecting `execution: in-place` are rejected. Existing Directory Projects remain readable but cannot admit Tasks; stored Tasks and plans remain readable and their existing Sandboxes remain tear-downable.

PR delivery uses forge CI observed green at the exact `gated_sha` as its verification source. Local delivery fast-forwards to the Task branch tip without automated verification. Users accept responsibility for verifying local changes before approving Merge. `gated_sha` remains the exact commit pin, and a moved branch remains a refusal.

The configurable command Gate, its fix loop and its notices are removed. Legacy config is tolerated with a deprecation warning but is not executed. The deprecated `no-mistakes` Merge Mode remains until its separate parity work is complete. Workflow stage `gate = "user"` continues to mean a User decision, not an executable command.

## Consequences

- New Tasks use Git Sandbox execution and branches.
- There is no command Gate run at Merge or during full-pipeline lint.
- PR freshness, CI observations, review evidence, merge authority and exact-head checks remain enforced.
- Local Merges have no automated verification.
- Persisted plans and snapshots remain backward-readable; no Directory Project is migrated to Git.

## Rationale

A local command Gate duplicates PR CI and adds substantial time to PR Merges. Local Merges are useful for offline repositories, but the Gate is their only remaining automated-verification justification; accepting that verification is the User's responsibility keeps local integration simple. Directory/in-place execution duplicates direct editing while adding a separate custody, consent and evidence subsystem. Git Sandboxes and Workspaces are the single orchestration model.
