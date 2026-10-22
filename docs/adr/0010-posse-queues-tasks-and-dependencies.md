# Posse queues Tasks and their dependencies

The v1 spec left automatic Task queueing out: at `max_workers`, `start` refused, and the Lead had to remember what to start later. Remembering when to start a Task is mechanical work, which [ADR 0001](0001-cli-owns-mechanics-on-herdr-without-daemon.md) assigns to the CLI. Asking the Lead to carry it spends context on every wake, and the Lead forgets. The User chose to make queueing part of Posse.

## Decision

- `start --queue` creates the Task in a new `queued` state when no Worker slot is free. `start --after <task-id>` creates it `queued` until that Task has Merged.
- A `queued` Task holds no Sandbox, pane or branch. Posse spawns it through the normal `start` path as soon as a slot is free and every dependency has Merged. The check runs from the same callers that evaluate Stalls: every `posse` call, plugin events and the Project observer tick. Adapter-owned `watch --handoff` waits are receiver-only and never run Task, executor or Project maintenance. No daemon is added.
- A dependency waits for `merged` only. The new Task branches from a default branch that already contains the dependency's change. Stacked branches (waiting for `done` and branching from the dependency's branch) are out of scope.
- If a dependency becomes `failed`, `lost` or torn down without Merge, the queued Task becomes a Decision for the User: cancel it, or drop the dependency and run it.

## Considered options

- **Leave queueing to the Lead.** No state change, but the Lead holds a timer in its context and loses it across restarts.
- **Also allow waiting for `done` and stacking.** Merge a stack in `pr` mode needs retargeting and rebasing each PR, which is a much larger change. It can be added later as a separate dependency kind.

## Consequences

- The lifecycle gains `queued -> spawning` and `queued -> torn-down` (cancel).
- As with Stalls, a queued Task can start late if nothing calls `posse` and no event fires ([spec section 23](../spec.md#23-open-risks), "Timers need a caller").
