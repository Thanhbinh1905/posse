# #360: Claude background Watch waits without quiet turns

## Behavior

Claude Leads with `notice_delivery = "watch"` keep one receipt-backed `posse watch --json --quiet-routine --handoff --destination "claude:$CLAUDE_CODE_SESSION_ID"` running as a background Bash command. In a local interactive Claude Code session, it waits without model requests. When the Notice arrives, Claude Code reports task completion with an output-file path; the Lead reads that file, handles the exact batch, acknowledges its Notice IDs, and starts one replacement Watch.

The former bounded Stop hook remains callable for existing sessions, but new Claude Leads do not load its plugin or depend on its four-minute renewal.

## Claude Code contract

The [background Bash documentation](https://code.claude.com/docs/en/interactive-mode) says local interactive tasks have no time limit, unattended sessions enforce task deadlines, and completed output is written to a file Claude can read. The [environment variable reference](https://code.claude.com/docs/en/env-vars) documents `CLAUDE_CODE_SESSION_ID` in Bash tool subprocesses.

## E2E evidence

`TestRealClaudeBackgroundWatchWakesOnlyForNotice` runs the installed Claude Code harness against a local Anthropic response stream in an isolated Herdr and Posse fixture. It starts the real Posse CLI as a background Bash task, checks for zero model requests during a quiet period of 4m10s, and then creates a Notice. The harness wakes on task completion, passes the output-file path to Claude, and a real Claude `Read` tool call retrieves the receipt-backed batch. The scripted model acknowledges the batch with `posse ack`, starts a replacement Watch, and verifies that a second Notice also wakes it. Both receipts settle as accepted and both Notice acknowledgments remain recorded.

Claude Code 2.1.304 was installed for the focused run:

```sh
go test -tags=e2e -count=1 -timeout=9m ./internal/e2e \
  -run '^TestRealClaudeBackgroundWatchWakesOnlyForNotice$'
```

Result: passed in 254.281s. The `-p` test session supplies a 600000ms Bash background timeout because unattended Claude sessions enforce deadlines. The production Lead is a local interactive Herdr session and omits that timeout. Full Gate and all four E2E shards remain PR-CI verification under User instruction #778.

## Limitation

This wake path depends on Claude Code's local interactive background Bash lifecycle. Do not use it for unattended `-p` or cloud sessions without a suitable timeout and recovery policy. A missing or ambiguous Notice receipt remains held for explicit resolution; the Lead must not retry it blindly.
