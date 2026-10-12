# Verify Rider interruption with per-kind abort keys

## Context

`posse interrupt` sent Ctrl+C to every Rider and reported success after writing the key. Pi responds to Escape instead, and Herdr's `working` status can remain unchanged when the key does not abort the active turn.

## Decision

- Configure the Herdr logical key sequence in `kinds.<kind>.interrupt_keys`. Defaults are Ctrl+C for Claude Code and Escape for Codex, Pi and OpenCode. Unknown kinds have no default and must be configured explicitly.
- Send abort keys only to an unfocused Rider Herdr reports as `working`. Do not send keys to a blocked dialog or an idle composer.
- Poll the pane for up to three seconds. Report interruption as verified only after the status leaves `working` or the pane disappears; otherwise return `interrupt_unverified`.
- Keep interruption separate from steering. A normal `posse send` can steer only kinds whose `steer` capability is enabled; an earlier explicit `--queue` message cannot hold back a later eligible steer.

## Evidence

- Pi 1.0.4 usage guide: [Stop the current task with Escape](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/usage.md#change-direction). A private-Herdr E2E used the real Pi CLI with a local held model request; Escape produced `Operation aborted` and a verified status change.
- Claude Code 2.1.296 interactive-mode guide: [Ctrl+C interrupts a running operation](https://code.claude.com/docs/en/interactive-mode#keyboard-shortcuts). Escape also stops a response, but Ctrl+C is the configured default.
- Codex CLI 0.160.0 source: [the default `interrupt_turn` key binding is Escape](https://github.com/openai/codex/blob/main/codex-rs/tui/src/keymap.rs).
- OpenCode 1.18.35 keybinding reference: [the default `session_interrupt` binding is Escape](https://opencode.ai/docs/keybinds/).
- Cross-kind E2E exercises steering and verified interruption for Claude, Codex, Pi and OpenCode fake Riders on an isolated Herdr server. It makes no provider requests and does not touch the live Herdr fleet.
