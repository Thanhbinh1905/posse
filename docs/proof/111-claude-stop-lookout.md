# #111: Claude owns bounded Notice waits

## Reproduction

`TestRealClaudeLookoutBackgroundLimit` runs Claude Code 2.1.300 against a local Anthropic response stream in an isolated Herdr/Posse fixture. The model requests the actual CLI as `exec <posse> lookout --timeout 5000`, with Bash `run_in_background=true` and `timeout=1000`. Claude starts the background command, then reports:

```text
status: stopped
summary: Background command "Observe Lookout at the background ceiling" was stopped after reaching its background time limit
```

The measured ceiling is the configured **1000ms background deadline**, not a claim that the default is one second. No harness code or clock is replaced. The local response stream removes external model latency and credentials from the reproduction. The test closes its harness input and terminates its owned process group during cleanup.

Run:

```sh
go test -tags=e2e -v -count=1 -timeout=2m ./internal/e2e \
  -run '^Test(RealClaude|ClaudeStop)'
```

The real-harness tests skip when Claude is not installed. The CLI-level overlap, deadline, pending Notice and SIGTERM test runs without Claude. All fixture files, homes and caches specific to this Task use its scratch directory; shared Go caches remain unchanged. Installed-binary test fixtures now honor a supplied scratch directory outside `/tmp` instead of always writing to `/var/tmp`.

## Replacement and evidence

The Lead launch adds a generated `Stop` command hook only for Claude's `lookout` delivery mode. It invokes the absolute Posse executable with the Project root and `POSSE_HOME`, preserving other plugins and settings. Claude owns the asynchronous hook through `asyncRewake`; nothing is detached.

- The hook waits four minutes, with a five-second bound on maintenance beyond that wait, below its five-minute harness timeout. A quiet expiry exits 2 with a renewal reminder. The next Stop owns the next wait; no model-issued background command is needed.
- A process-owned Project flock excludes overlapping Stop invocations. A terminated owner releases it without stale lease recovery.
- The existing receipt-backed Lookout path owns exact Notice IDs, generation, destination and lease. Printing a reminder does not mark the batch delivered. The next Stop accepts it only after finding its stable receipt marker in persisted User-text transcript content. A missing or unreadable transcript holds the batch as uncertain instead of blindly repeating it. After its warning is recorded, the hook pauses until explicit receipt resolution; an unreadable transcript limits repeated warnings to one wait interval rather than a hot wake loop.
- Receipt takeover after lease expiry retains the batch, exact IDs and any acknowledgment recorded while the reminder was being handled. Assistant quotations and tool output are not receipt evidence.
- Existing durable PR-poll claims remain authoritative across restarts. A test holds another poller's real claim through quiet renewal, graceful stop and pending delivery, and verifies that the token is neither overwritten nor released.
- SIGTERM, Project down and session teardown do not request a renewal. Process discovery recognizes `_claude-stop` for idle-pane fallback, `down` and binary updates. Update output names the next Stop hook as the restart owner.

The real-harness replacement test shortens Posse's wait to 700ms but leaves Claude's hook lifecycle unchanged. It observes ten quiet renewals without a User turn, injects a pending Notice, sees its stable receipt in Claude's next request, acknowledges it, verifies receipt acceptance without losing the acknowledgment, and closes the session normally. Exactly one receipt exists after the restart. The CLI-level test also verifies overlapping hooks return without claiming a second wait and SIGTERM exits without a wake reminder.

## Verification

- Focused unit tests and configuration tests passed.
- Focused real-Claude and CLI E2E passed, including the native background-deadline reproduction.
- Full Gate, unit suite and E2E results are recorded in the PR verification section.

The first full unit run found four launch/instruction expectations still describing the old background path; these now assert the added Stop plugin and `posse ack` guidance. A monolithic E2E invocation exhausted its 30-minute aggregate budget with `TestLegacyRiderTabsAfterWorktreeUpgrade` only 32 seconds into its own run, so full verification uses the repository's four bounded shards. That run also exposed a Pi fixture race: a live Lookout legitimately claimed an eligible instruction before the interrupt command could reserve it. The fixture now gates the abort, makes the older instruction steerable only after reservation, and checks redirect-before-steering order while permitting normal delivery after release. Five isolated repetitions passed; no production interrupt behavior changed.

The harness contract is documented in [Claude Code's command-hook fields and background-hook lifecycle](https://code.claude.com/docs/en/hooks#command-hook-fields): `asyncRewake` wakes Claude on exit 2, its timeout remains enforced, and session teardown owns the process. Synchronous Stop continuation limits are avoided by asynchronous wakes.

## Risk and rollback

Claude must support command-hook `asyncRewake`; the real-harness proof uses 2.1.300. Quiet renewal requires one short model turn every four minutes. A changed or unavailable transcript format deliberately raises delivery uncertainty rather than risking duplicate effects. The existing typed fallback still applies when no live Lead-side wait exists. Other harness adapters and the dedicated poll-only tab are unchanged; custom non-Claude background delivery remains available.

Rollback: revert this change and restart the Claude Lead to regenerate its launch instructions and plugins. No migration or daemon is introduced. Existing durable receipt records remain inspectable through `posse lookout --receipt`; resolve an uncertain receipt only after checking the recorded session.
