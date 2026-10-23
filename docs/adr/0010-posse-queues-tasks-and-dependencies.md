# Posse queues Tasks and their dependencies

The v1 spec left automatic Task queueing out: at `max_workers`, `start` refused, and the Lead had to remember what to start later. Remembering when to start a Task is mechanical work, which [ADR 0001](0001-cli-owns-mechanics-on-herdr-without-daemon.md) assigns to the CLI. Asking the Lead to carry it spends context on every wake, and the Lead forgets. The User chose to make queueing part of Posse.

## Decision

- `start --queue` durably admits a Task in `queued` state at capacity. Without the flag, `start` retains the `worker_limit` refusal. A queued Task has no Sandbox, pane or branch; its admitted Brief and Playbook are frozen in the plan.
- Existing Project reconciliation, including CLI, plugin and Watch callers, refills queued Tasks through the normal spawn-intent path. The claim transaction reserves capacity, assigns the Task branch and creates the intent atomically; ordinary spawn admission also checks `max_workers` inside its Task-creation transaction, so concurrent starts cannot overbook a slot. A queued Task has no Sandbox, pane or branch until it is claimed. No daemon is added.
- `after: [t12:landed, t14:accepted]` records typed prerequisites in the admitted plan. Admission validates predecessor existence and type in the Task creation transaction; unmet milestones keep the Task queued and `show` reports their wait reason. Ready queued Tasks are claimed and launched from their frozen plan, including the admitted Brief, Playbook, launch identity and execution configuration.
- Review readiness comes from `review_of`, not `after`. A Change Task with a `landed` prerequisite cannot select a custom base; it starts from the default branch after the prerequisite Lands. Stacked branches remain out of scope.
- Native GitHub `blocked_by` relationships are queried before starting ticketed Change Tasks. Open blockers refuse dispatch; unavailable or invalid blocker data fails closed with a visible wait reason. Core users should still check ticket blockers manually when the forge result is unavailable.
- If a predecessor becomes `failed`, `lost` or torn down without satisfying its milestone, the edge remains in the admitted plan and `show` identifies the predecessor state; the queued Task is not dispatched. Reconciliation raises a Decision on the dependent Task to cancel it or drop that prerequisite and run. Applying the latter records a new plan revision before attempting normal dispatch.

## Considered options

- **Leave queueing to the Lead.** No durable admission record, and the Lead must remember what to start after a Worker finishes.
- **Also allow waiting for `done` and stacking.** Merge a stack in `pr` mode needs retargeting and rebasing each PR. This remains out of scope.

## Consequences

- The lifecycle gains `queued -> spawning`, `queued -> queued` (drop an unsatisfied prerequisite through a revised plan) and `queued -> torn-down` (cancel). `posse teardown <task>` cancels a queued Task without touching Worker resources.
- Queue admission, inspection, cancellation, restart recovery, caller-driven scheduler refill, dependency waits, failed-prerequisite Decisions, frozen-plan launch and GitHub blocker checks are implemented.
- As with Stalls, queued Tasks can start late if nothing calls `posse` and no event fires ([spec section 23](../spec.md#23-open-risks), "Timers need a caller").
