# The Lead stewards PR deliveries

## Context

A PR/MR outlives the Worker pane that opened it, but every forge follow-up went back to that Worker: a flaky CI failure, a reviewer question or stale Posse-owned PR metadata each cost a Worker turn, and failed when the Worker was gone. The Lead had no recorded way to act on a PR without becoming an author, and nothing stopped a retried forge command from posting the same reply or rerunning the same CI twice after an ambiguous response.

## Decision

- Each recorded PR/MR is a Delivery (`deliveries`): Task, member, URL, steward and origin. The Lead is its operational steward. Authorship stays with the Task's recorded launches; a delivery does not copy them. Deliveries opened by `posse publish` have origin `publish`; PR URLs recorded earlier or by another path become `legacy` deliveries, on migration or on their first steward command. The PR watch, its Notices and `posse merge` remain canonical.
- `posse pr inspect|reply|refresh|rerun` are the only steward operations. Each is recorded in `delivery_operations` with actor (`lead` or `user`), Lead pane, PR head, subject and outcome. Effects require a forge observation confirmed during the command; cached state is never presented as current.
- Rerun is limited to an existing failed CI run on the current observed head, at most twice per run and head. It never starts or dispatches a workflow.
- A forge error after an effect was requested records the operation `uncertain`; a `pending` operation past a two-minute lease counts as interrupted. Before the same operation runs again Posse observes the forge (reply marker, run attempt, newer GitLab job). An effect that happened is recorded and not repeated. If the forge cannot be read, the command refuses without acting.
- Stewardship grants no merge, approval, branch, push, workflow or secret authority. Merging keeps `posse merge` with its approval and evidence; code changes go through a Worker.

## Consequences

- The Lead triages trivial PR failures without waking the original Worker, and the record shows who did what to which head.
- An ambiguous forge response costs one extra observation instead of a duplicated comment or CI run.
- Operation history and unknown outcomes are part of Task snapshots, so rerun limits survive a store rebuild.
- `pr_observations.confirmed_at` records the latest forge confirmation, because an unchanged poll does not insert a row.
- Executor handoff, branch-writing claims and resource release for deliveries are later work (flexible-core plan P02 and P03).
