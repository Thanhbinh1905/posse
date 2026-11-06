# Directory Projects start a Lead without execution

## Status

Accepted. Amends the registration clause of [ADR 0028](0028-git-only-orchestration-without-command-gates.md); its in-place execution prohibition stands.

## Decision

`posse up` and `posse project add` register a folder that is not inside a Git repository and has no repository directly inside it as a Directory Project, without running `git init`. One Lead per Project still applies. The Lead explores and plans with the User. Posse admits no Task there and runs no Git operation: `start`, `dispatch`, `sync`, `merge`, and Worker publication refuse with `execution_unavailable`, and Briefs selecting `execution: in-place` stay rejected. `posse project add --directory` stays removed because detection is automatic.

Interaction readiness is separate from execution: `up`, `lead` and `project show` print `capabilities{interaction,execution}`, derived from the Project kind. Execution is `git-sandbox` for repository and workspace Projects and `none` for Directory Projects.

`posse project scan` on a Directory Project converts it in place to a repository or workspace Project once its root becomes a Git top level or gains direct child repositories. The row keeps its id, name, root and UUID. Conversion is refused while legacy Tasks are unfinished, so an old in-place Task is never reactivated. `posse up` never converts on its own.

Directory and workspace Projects list nested repositories as read-only Lead context. Discovery reads directory entries and Git ignore rules, down to depth 3. It skips hidden folders, symlinks, unreadable folders and folders excluded by Git ignore rules. It reports at most 64 repositories and reads at most 4096 entries, saying `truncated: true` when it stops early. If ignore matching is unavailable, discovery stops and reports `ignore_rules: unavailable` and `truncated: true`. Workspace membership stays direct children only.

## Consequences

- An empty folder no longer offers `git init`. The #699 flow becomes `posse up`, then the User's `git init`, then `posse project scan`. Unborn-branch Tasks and the first local Merge then work as before.
- Posse admits zero writers in a Directory Project, so concurrent writers cannot be admitted there. The Lead's own reading and editing rules are instructions only: nothing confines the Lead shell, as for repository Leads.
- No isolation, rollback or review is claimed for Directory Projects.
- No schema migration: `projects.kind` already accepts `directory`, and recovery snapshots carry it. Reverting leaves registered Directory Projects readable as legacy records that admit no Tasks.

## Considered options

A single consented in-place editor per Directory Project, holding an ADR 0021 path lease, was deferred by the User in favor of this exploration-first slice. It would restore the custody, evidence and recovery machinery ADR 0028 removed, and still could not confine an agent's shell.
