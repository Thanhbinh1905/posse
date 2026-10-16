# #140: idle-wake continuity and its limits

## Guarantees

While a poller lives, a forge result becomes a Notice without a model turn: `TestObservationWorkloadDiscoversForgeResultsWithoutModelTurns` runs the real poll-only Lookout and foreground producers against a fake `gh`, with no Lead model, and finds the Notices in SQLite. Delivery to the Lead then depends on the harness:

| Harness | Wake path | Focused composer | Evidence |
| --- | --- | --- | --- |
| Claude | bounded `Stop` hook, `asyncRewake` | not touched | `TestClaudeStopOverlapDeadlineAndGracefulStop`, `TestRealClaudeStopRenewsAndDeliversPendingNotice`, [#111](111-claude-stop-lookout.md) |
| Pi | in-process extension, `pi.sendMessage` follow-up | not touched | `TestPiExtensionSessionReplacementOwnsOneLookoutGeneration`, `pi_notice_*.mjs` fixtures |
| OpenCode | plugin, `session.prompt` after `session.idle` | not touched | `TestOpenCodeQueuesNoticeBatchDuringBusyTurn`, `TestOpenCodePromptOutcomesFollowReceiptEvidence` |
| Codex | `codex queue --thread` | draft stays | `TestCodexLeadReceivesNoticesThroughItsQueueWhileFocusedAndBusy`, `TestFailedCodexQueueFallsBackToTypedPromptOnlyWhenUnfocused` |
| Prompt only | typed prompt | **delayed while focused** | `TestFocusedPaneBlocksTypingAndNoticeRemainsUndelivered` |

A kind that needs `background_commands` without declaring it, or whose harness has no queue, extension or hook, gets no automatic guarantee and uses the prompt fallback.

## Fault coverage

| Scenario | Test |
| --- | --- |
| Two simultaneous producers, repeated identical polls, new PR head, outage, recovery, live and expired poll claims, one-hour interval | `TestObservationWorkloadDiscoversForgeResultsWithoutModelTurns` |
| Receiver crash before and after an effect, restart | `TestNoticeHandoffSurvivesHarnessCrashAndAvoidsRecordedReplay`, `TestAcknowledgedPrintedNoticeRecoversAfterCrashAndRebuild` |
| Forge timeout and persistent failure | `TestLookoutKeepsWatchingAfterRetryableForgeProbeTimeout`, `TestLookoutReportsPersistentForgeProbeFailureAfterThreePolls` |
| Busy and idle transitions | `TestOpenCodeQueuesNoticeBatchDuringBusyTurn`, `pi_notice_busy_followup.mjs` |
| Update, down and shutdown | `TestClaudeStopOverlapDeadlineAndGracefulStop`, `TestDownDoesNotSignalUnrelatedExecutableNamedLookout` |
| Lookout restart, repair | `TestLookoutRestartsAfterProcessExit`, `TestLookoutRepairRestoresHeldProjectWithoutExecutorRecovery` |

## Workload

One run of the workload test (private Herdr server, scratch `POSSE_HOME`, every inherited `HERDR_*` variable removed, `pr_poll = 1ms` to force polling) reported:

```json
{"forge_calls": 113, "poller_arms": 138, "handoff_arms": 2, "actionable_wakes": 1,
 "notices_created": 4, "maintenance_failure_notices": 1, "model_turns": 0}
```

- `forge_calls` counts `gh api graphql` invocations, one API cost per poll claim.
- `poller_arms` counts foreground producer invocations; their summed user plus system CPU is `poller_cpu_ns` (about 10s over 17s of wall time, mostly process start-up at a 1ms interval).
- `handoff_arms` and `actionable_wakes` count receipt-backed Lookout arms and batches printed. The accepted batch did not wake again.
- `model_turns` is zero by construction. The test measures API and CPU cost only. It reports no token savings, and Claude's quiet renewal still costs one short model turn every four minutes.
- Under a one-hour interval, two producers made zero forge calls. A live foreign claim suppressed calls and kept its token. An expired claim was reclaimed.

Counts vary with machine speed; the test asserts the invariants (one Notice per head, no repeat after recovery, no call under a live claim), not the totals.

## Limits

- Delivery is receipt-backed, not exactly-once: ambiguous outcomes are held and raise `notice_delivery_uncertain`. It is not exactly-once and Posse never replays a held batch automatically. Receivers must use stable IDs.
- Prompt-only Leads receive nothing while focused and no Lead-side lookout is live.
- Real credentialed harness runs are outside CI and need separate approval.
