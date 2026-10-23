# Verify Worker interruption with per-kind abort keys

## Context

`posse interrupt` sent Ctrl+C to every Worker and reported success after writing the key. Pi responds to Escape instead, and Herdr's `working` status can remain unchanged when the key does not abort the active turn.

## Decision

- Configure the Herdr logical key sequence in `kinds.<kind>.interrupt_keys`. Defaults are Ctrl+C for Claude Code and Escape for Codex, Pi and OpenCode. Unknown kinds have no default and must be configured explicitly.
- Explicit `posse interrupt` and `send --interrupt` may send abort keys to a focused or unfocused busy Worker. For Pi, a standalone `Auto-compacting...` line on the current visible pane may identify a busy turn when Herdr reports `unknown`; historical or quoted mentions do not. Do not send keys to a blocked dialog or an idle composer.
- Poll the pane for up to three seconds. Report interruption as verified only after the busy state ends or the pane disappears; otherwise return `interrupt_unverified`.
- Keep ordinary steering separate from interruption. `posse send <task> --interrupt <message>` is the explicit abort-and-redirect path: claim queued messages before sending abort keys, verify the turn stopped, deliver the redirect, then restore the queued messages in their original order. This prevents the idle event caused by interruption from racing background delivery.
- Add `posse unsend <message-id>` for atomic withdrawal while a message is still `queued`; its project preparation does not deliver messages. Claimed or submitting messages cannot be withdrawn.
- Background delivery and send-time delivery both select the oldest steerable message past a `wait_for_idle` head. The send result reason is derived from the message just sent.
- Treat only Pi's standalone current-visible `Auto-compacting...` indicator as busy when Herdr reports `unknown`. Attempt steering through Herdr; if Herdr returns an error after `agent.prompt` submission begins, keep the message held as uncertain. Even `agent_not_ready` does not prove the prompt was not applied, so do not retry or withdraw it automatically.
- Refresh message claims while a Herdr submission is active, and refresh the whole interrupt reservation while its command is alive, so an expired lease cannot permit concurrent delivery or release queued messages ahead of an in-flight redirect. An expired uncertain submission remains `submitting` with its Notice and is never replayed, but no longer blocks fresh delivery indefinitely.

## Evidence

- Pi 1.0.4 usage guide: [Stop the current task with Escape](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/usage.md#change-direction). A private-Herdr E2E used the real Pi CLI with a local held model request; Escape produced `Operation aborted` and a verified status change.
- Claude Code 2.1.296 interactive-mode guide: [Ctrl+C interrupts a running operation](https://code.claude.com/docs/en/interactive-mode#keyboard-shortcuts). Escape also stops a response, but Ctrl+C is the configured default.
- Codex CLI 0.160.0 source: [the default `interrupt_turn` key binding is Escape](https://github.com/openai/codex/blob/main/codex-rs/tui/src/keymap.rs).
- OpenCode 1.18.35 keybinding reference: [the default `session_interrupt` binding is Escape](https://opencode.ai/docs/keybinds/).
- Cross-kind E2E exercises steering and verified interruption for Claude, Codex, Pi and OpenCode fake Workers on an isolated Herdr server. The busy-Pi E2E covers queue-head bypass in background and send-time delivery, `send --interrupt`, message withdrawal and refusal after claim. A separate real-Pi E2E holds a local model request, verifies the abort, and checks that the redirect is the next model input while the older queued message remains queued. These tests do not touch the live Herdr fleet or use a remote model provider.
