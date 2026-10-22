# Posse queues Tasks and their dependencies

The v1 spec left automatic Task queueing out: at `max_workers`, `start` refused, and the Lead had to remember what to start later. Remembering when to start a Task is mechanical work, which [ADR 0001](0001-cli-owns-mechanics-on-herdr-without-daemon.md) assigns to the CLI. Asking the Lead to carry it spends context on every wake, and the Lead forgets. The User chose to make queueing part of Posse.

## Decision

- `start --queue` durably admits a Task in `queued` state at capacity. Without the flag, `start` retains the `worker_limit` refusal. A queued Task has no Sandbox, pane or branch; its admitted Brief and Playbook are frozen in the plan.
- The intended scheduler refills queued Tasks through existing CLI, plugin and Watch callers, using the normal spawn intent path and never exceeding `max_workers`. Queue admission and restart recovery are implemented; scheduler refill and safe cancellation remain incomplete.
- Local Task dependencies are not implemented. The intended model records typed `reported`, `accepted` or `landed` prerequisites atomically, uses `review_of` for Review readiness, and starts a dependent Change Task from the default branch only after `landed`. Stacked branches remain out of scope.
- Native GitHub `blocked_by` relationships are not implemented. Until that ships, core users must check ticket blockers manually.
- If a dependency becomes `failed`, `lost` or torn down without Merge, the queued Task becomes a Decision for the User: cancel it, or drop the dependency and run it.

## Considered options

- **Leave queueing to the Lead.** No durable admission record, and the Lead must remember what to start after a Worker finishes.
- **Also allow waiting for `done` and stacking.** Merge a stack in `pr` mode needs retargeting and rebasing each PR. This remains out of scope.

## Consequences

- The lifecycle gains `queued -> spawning` and `queued -> torn-down` (cancel); cancellation is not yet exposed for queued Tasks.
- As with Stalls, queued Tasks can start late if nothing calls `posse` and no event fires ([spec section 23](../spec.md#23-open-risks), "Timers need a caller").
