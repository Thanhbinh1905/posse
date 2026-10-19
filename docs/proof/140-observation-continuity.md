# #140: idle-wake continuity and its limits

## Guarantees

While a poller lives, a forge result becomes a Notice without a model turn: `TestObservationWorkloadDiscoversForgeResultsWithoutModelTurns` runs the real poll-only Watch and foreground producers against a fake `gh`, with no Lead model, and finds the Notices in SQLite. Delivery to the Lead then depends on the harness:

| Harness | Wake path | Focused composer | Evidence |
| --- | --- | --- | --- |
| Claude | bounded `Stop` hook, `asyncRewake` | not touched | `TestClaudeStopOverlapDeadlineAndGracefulStop`, `TestRealClaudeStopRenewsAndDeliversPendingNotice`, [#111](111-claude-stop-watch.md) |
| Pi | in-process extension, `pi.sendMessage` follow-up | not touched | `TestPiExtensionSessionReplacementOwnsOneLookoutGeneration`, `pi_notice_*.mjs` fixtures |
| OpenCode | plugin, `session.prompt` after `session.idle` | not touched | `TestOpenCodeQueuesNoticeBatchDuringBusyTurn`, `TestOpenCodePromptOutcomesFollowReceiptEvidence` |
| Codex | `codex queue --thread` | draft stays | `TestCodexLeadReceivesNoticesThroughItsQueueWhileFocusedAndBusy`, `TestFailedCodexQueueFallsBackToTypedPromptOnlyWhenUnfocused` |
| Prompt only | typed prompt | **delayed while focused** | `TestFocusedPaneBlocksTypingAndNoticeRemainsUndelivered` |

A `watch` kind without `background_commands` is rejected at config load (`TestLookoutDeliveryRequiresBackgroundCommands`); a harness with no queue, extension or hook gets no automatic guarantee and uses the prompt fallback.

## Fault coverage

| Scenario | Test |
| --- | --- |
| Two simultaneous producers, repeated identical polls, new PR head, outage, recovery, live and expired poll claims, one-hour interval | `TestObservationWorkloadDiscoversForgeResultsWithoutModelTurns` |
| Receiver crash before and after an effect, restart | `TestNoticeHandoffSurvivesHarnessCrashAndAvoidsRecordedReplay`, `TestAcknowledgedPrintedNoticeRecoversAfterCrashAndRebuild` |
| Forge timeout and persistent failure | `TestLookoutKeepsWatchingAfterRetryableForgeProbeTimeout`, `TestLookoutReportsPersistentForgeProbeFailureAfterThreePolls` |
| Busy and idle transitions | `TestOpenCodeQueuesNoticeBatchDuringBusyTurn`, `pi_notice_busy_followup.mjs` |
| Update, down and shutdown | `TestClaudeStopOverlapDeadlineAndGracefulStop`, `TestDownDoesNotSignalUnrelatedExecutableNamedLookout` |
| Watch restart, repair | `TestLookoutRestartsAfterProcessExit`, `TestLookoutRepairRestoresHeldProjectWithoutExecutorRecovery` |

## Workload

Two tests count what observation costs, with API calls and CPU kept apart from model usage. Both use a private Herdr server, scratch `POSSE_HOME`, every inherited `HERDR_*` variable removed and `pr_poll = 1ms`.

`TestObservationWorkloadCountersAreDeterministic` stops the background poller and runs producers one at a time, so the counts are exact on any machine: five producers make exactly five forge calls (one each), two heads create exactly two checks Notices, one handoff arm delivers one actionable wake, and `model_turns` is zero. Producer CPU is summed separately as `poller_cpu_ns`.

`TestObservationWorkloadDiscoversForgeResultsWithoutModelTurns` runs the real poll-only Watch beside concurrent producers and asserts invariants, not totals (one run: 113 forge calls, 1 wake, 4 Notices, 0 model turns). Under a one-hour interval two producers made zero forge calls; a live foreign claim suppressed calls and kept its token; an expired claim was reclaimed.

- `forge_calls` counts `gh api graphql` invocations.
- `poller_arms` counts producer invocations; `handoff_arms` and `actionable_wakes` count receipt-backed Watch arms and printed batches. An accepted batch did not wake again.
- The tests measure API and CPU cost only and report no token savings. Claude's quiet renewal still costs one short model turn every four minutes.

## Limits

- Delivery is receipt-backed, not exactly-once: ambiguous outcomes are held and raise `notice_delivery_uncertain`. It is not exactly-once and Posse never replays a held batch automatically. Receivers must use stable IDs.
- Prompt-only Leads receive nothing while focused and no Lead-side watch is live.
- Real credentialed harness runs are outside CI and need separate approval.
