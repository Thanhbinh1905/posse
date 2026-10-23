# Explicit Worker commands override layout focus

Herdr focus describes layout selection, not whether the User is typing. Posse therefore does not treat a focused Worker pane as proof of User-owned input for explicit Lead commands.

- Launch and relaunch pass the opening instruction as an argument to `agent.start` for Claude, Codex, Pi and OpenCode. Focus cannot delay those commands.
- Explicit `posse send`, `posse send --interrupt`, `posse interrupt` and `posse keys` may send to a focused Worker. They retain blocked-agent, unknown-UI and harness-support checks.
- Automatic Notice delivery, queued-message delivery, model-error recovery, launch retries and recovery prompts retain the focused-pane guard.

Herdr has no focus-safe input call: both `agent.prompt` and `agent.send-keys` write to the terminal. The distinction is therefore policy-based. A User interacting with the same focused Worker may see or affect explicit command input; that is accepted for commands the Lead explicitly requested. Automatic injection remains withheld to avoid surprising the User.
