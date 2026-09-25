# Start the Lead in the calling pane

`posse up` runs inside a Herdr shell pane, so Herdr's `agent.start` API rejects that pane as busy while the CLI is its foreground process. Starting a new tab contradicts the requested workflow. Instead, `posse up` prepares and labels its calling pane, prints its result, then replaces its own process with the configured agent executable and arguments. Herdr detects and tracks the agent process. A short detached Posse process waits for detection, assigns the canonical agent name through Herdr, delivers fallback instructions when no system-prompt argument is configured, and handles Notice redelivery. With `--replace`, Posse exits the old agent in its pane, waits for the shell, and clears its Lead label and metadata without closing the pane.

## Considered options

- **Create a new Lead tab and call `agent.start`**: rejected because the Lead does not occupy the pane that invoked `posse up`.
- **Call `agent.start` in the calling pane**: rejected because Herdr returns `agent_pane_busy` while `posse up` is its foreground process.
- **Execute the configured agent in place and finalize Herdr metadata**: chosen. A real Herdr check confirmed that directly started Codex processes appear in the agent list and accept a Herdr-assigned name. This preserves Herdr lifecycle tracking and `agent.prompt` targeting while using the caller pane.

## Consequences

- `posse up` replaces its process with the Lead agent after printing the startup result; the shell prompt does not return while that Lead is running.
- A detached finalizer is needed because Herdr can assign the canonical agent name only after it detects the in-place process.
- `posse recover` has no caller pane and continues to use Herdr's `agent.start` in the recorded Lead pane or a new tab.
