# CLI owns the mechanics, on Herdr, without a daemon of our own

Everything that must happen the same way every time (Task state, worktrees, spawn, reconcile, stall detection, teardown) lives in the `posse` CLI; skills and instructions hold only judgment (writing briefs, reviewing, deciding when to ask the User). The runtime is Herdr only, and events reach us through a Herdr plugin event hook that runs `posse _ingest` per event instead of a long-running `posse` daemon. Herdr already keeps agent processes, worktrees and agent-state detection alive, so a daemon would add a process we must supervise without adding a guarantee; Herdr does not replay events, so the event journal is only a fast change signal and every CLI call reconciles against `session.snapshot` as the source of truth.

## Considered Options

- **A supervising agent with its policy written as prose, backed by shell scripts**: rejected; that shape has to rebuild daemon guarantees (watcher re-arm, turn-end guards, locks, wake queues) around an LLM, and those scripts grow into the bulk of the system.
- **Own daemon (`posse daemon`)**: rejected for v1; `events.subscribe` needs one subscription per pane and the daemon itself would need systemd-style supervision.
- **Multiple runtime backends (tmux, zellij)**: rejected; all Herdr calls sit behind one adapter module, but no second implementation is written.

## Consequences

- State is SQLite (WAL) because several short-lived processes (CLI calls, plugin hooks, Lead, Workers) write concurrently.
- Missed events while Herdr is down are recovered by reconcile, never by the journal.
