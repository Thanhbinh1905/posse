# 24. Built-in validation replaces no-mistakes

## Context

The User asked for a built-in Posse process modelled on no-mistakes but fitted to Posse:

> "tôi muốn tìm hiểu cơ chế của no mistake để có thể tạo ra riêng 1 thứ phù hợp với posse builtin"

> "trong 1 bài research gần đây của claude. Các nhà phát triển đã cho 1 vài lời khuyên trong việc review và tôi muốn áp dụng nó vào app build in này luôn thứ nhất nó chỉ được sử dụng tối đa 3 lần review. thứ 2 mỗi lần review sẽ chỉ review những thay đổi mới mà không phải quét lại cả PR . thứ Ba gom các bước lại thành các pattern việc đó sẽ giúp gom các lỗi lại và sửa chung trong 1 lần sửa và cuối cùng không bao giờ báo lại các bước cũ hãy sử dụng 1 agent opus nghiên cứu kỹ để biến nó thành 1 builtin process hoàn chỉnh trong posse"

The four rules: (1) at most 3 review rounds per change; (2) each later round reviews only the new changes; (3) findings are grouped into patterns so related defects are fixed together; (4) findings from earlier rounds are never reported again.

The guidance the User refers to is "How the Claude Code team does code review" (Osmani, Lunt and Wang, claude.dev, 2026-10-15, <https://claude.dev/blog/solving-code-review-at-anthropic/>). It states:

- "After three rounds, the reviewer asks the author to step back and reconsider the approach."
- "Diff-only re-review after the first few passes." The adoption checklist says "Cap review rounds; after the first few, re-review only the new diff."
- "Flag the class of bug, so the author fixes every instance, not one line."
- "No repeats of findings already posted."
- "When we ran git blame on what the reviewer kept flagging, about 59% of it was code that had been there since the reviewer's first pass."
- "One CLI PR went through more than fifty rounds of review and fixes overnight before its author stopped it."
- Findings are tagged "Important, Nit, or Pre-existing, so a bug the PR didn't introduce doesn't look like the author's fault."
- "An approval covers one specific commit. If new commits land that the system can't vouch for, like an agent's push or a merge someone resolved by hand, the approval has to be given again."
- "Every finding has to be answered. You either fix it or resolve the thread with a reason."
- "Our priority, written down, is catching real bugs first and cutting review rounds second."
- Reported results on internal PRs: mean reviews to pass fell from 2.08 to 1.32, and PRs needing five or more rounds fell from 8.9% to 1.5%.

no-mistakes (studied at v1.50.0, commit `c4bc34b`) already bounds fix attempts per step and defaults the review step to zero automatic fixes (`internal/config/config.go:1656-1665`). Its rereview is session-free, so the rereviewer is never the session that prescribed fixes, and pipeline fixes are reviewed as author code (`internal/pipeline/steps/review.go:295-305`). Unclassified findings default to `ask-user` (`internal/types/findings.go:355-363`). It differs from the four rules in three ways. A fix-mode rereview diffs the whole branch against its base again (`internal/pipeline/steps/review.go:118-130`). Finding IDs are positional (`<prefix>-<n>`, `internal/types/findings.go:118-127`), so nothing identifies a finding across rounds. Findings are flat per file and line, not grouped by cause.

Posse already binds a Review Verdict to the exact Ship revision it examined and requires the Reviewer launch to differ from every author launch (ADR 0020). It requires a PR head to contain the current target before final CI and merge, and asks the Rider to merge, not rebase, `origin/<target>` into the Task branch (ADR 0019). In practice only P0/P1 defects introduced by the change block a merge; P2/P3 findings become follow-up issues. Every Review Task today reviews the full candidate, every finding is free text in a Report, and nothing limits how many Review Tasks a Ship Task receives.

The User then set the goal:

> "mục tiêu của tôi là không cần dùng tới no mistake nữa"

and raised the target branch:

> "một thứ nữa là vấn đề trong việc lựa chọn target branch , no mistakes đang luôn mặc định là ở Bren chính trong khi nhiều dự án tôi làm việc ở 1 Ranch khác như là Staging, vậy nên nên có lựa chọn target caus hình cho dự án"

Posse delegates to no-mistakes in two places: the `no-mistakes` Landing Mode, where the Rider delivers only through `no-mistakes axi run` (`internal/app/tasks.go:1013-1014`), and the `no-mistakes` review policy, which requires a pipeline-owned receipt (ADR 0020, D03). no-mistakes runs a fixed pipeline: intent, rebase, review, test, document, lint, push, pr, ci (`internal/pipeline/executor.go:562-992` at v1.50.0). Posse already has its own equivalents for part of it: Gates bound to `gated_sha`, `posse publish` with a tokenized PR body, the PR watch with CI Notices, freshness by merge (ADR 0019), head pinning at merge, and exact-head review verdicts (ADR 0020).

The target branch matters because no-mistakes does not follow Posse's frozen target:

- Posse's Rider protocol tells the Rider to run `no-mistakes axi run --intent <from Brief>` and never passes the target (`internal/app/tasks.go:1013-1014`, `internal/app/pr_freshness.go:65`). Workspace Members cannot use `no-mistakes`.
- no-mistakes records the repository default branch at `init` from `origin`'s `HEAD`, falling back to `main` (`internal/gate/gate.go:139-140`, `internal/git/git.go:430-445`). In v1.50.0 every step uses it: rebase onto `origin/<default>` (`internal/pipeline/steps/rebase.go:31-33`), review and test scope (`review.go:22`, `test.go:24`), and the PR base (`pr.go:86,107`).
- Since v1.62.0 (`e08143f`), `axi run --base-branch <branch>` or the trusted `pr.base_branch` selects another integration branch. The installed v1.84.0 (`4822244`) applies it only to rebase, PR and CI (`effectivePRBaseBranch`, `rebase.go:202-217`). Review, test, lint, document and custom-gate scope still use the merge base with the default branch (`review.go:32`, `test.go:37`, `lint.go:23`, `document.go:93`, `customgate.go:48` at `4822244`). The fix that scopes them against the run's base (`e043200`) first ships in v1.86.0.
- Through v1.88.0 (`8ad4dd1`), no-mistakes reads trusted config (commands, agent, `pr.base_branch`) from `origin/<default>` (`internal/daemon/manager.go:230-243`).

So for a Task whose target is `staging`, a Rider that follows the protocol today gets a rebase onto the default branch and a PR against it. `posse holler done --pr` then refuses that PR because its base is not the frozen target (`internal/app/publish.go:277`). If the default branch is ahead of `staging`, the rebase also drags unrelated commits into the branch.

## Decision

Posse gets a built-in **validation loop** that covers every capability Posse still takes from no-mistakes. Its parts are the Review Loop, Gate Fix Rounds, the CI loop, head continuity and Posse-owned PR evidence. It runs in the `pr` and `local` Landing Modes. The `no-mistakes` Landing Mode and review policy are deprecated now and removed after migration. Every part is configured per Project by the User, and each can be turned off. Review evidence stays a separate User policy (`defaults.review`), so nothing here makes review mandatory.

### Capability map

| no-mistakes capability | Built-in replacement |
|---|---|
| review step, bounded rounds | Review Loop (below): 3-round cap, delta scope, Finding Patterns, Finding Ledger |
| test and lint steps with automatic fixes (`config.go:1656-1665`: lint 3, test 3) | Gate commands run at the exact head (existing), plus Gate Fix Rounds: on a failed Gate, Posse queues the Rider one fix instruction with the failing command and output tail, up to `gate_loop.max_fix_rounds` (default 3) per Ship Task |
| custom gates | more `gate` entries |
| trusted config read from the default branch (`internal/daemon/manager.go:862-876` at v1.50.0) | Gate commands, loop settings and Profiles come only from Posse config, which is User-only and never from the Ship branch. Optional repository review guidance is read from `origin/<frozen target>` at admission |
| rebase onto `origin/<default>` | merge `origin/<frozen target>` into the Task branch (ADR 0019, existing), never rebase |
| push (force-with-lease to a gate remote) | `posse publish` pushes only the Task branch, without force (existing) |
| pr step with attestation (`prsummary.go:25,101`) | `posse publish` template (existing) plus a Posse-owned Validation block in the tokenized body: head SHA, Gate result, review rounds and verdict, ledger counts and CI state. It is rendering only; the evidence stays in SQLite |
| ci step: poll, fix from failed logs, dedupe by failing-check set, rerun cancelled checks (`steps/ci.go:122-551`, `ci_transient.go`) | PR watch (existing) plus the CI loop: on failed required checks for the current head, Posse queues one fix instruction with check names, log URLs and failed-log tails, deduplicated by failing-check set per head, up to `ci_loop.max_fix_rounds` (default 3). Checks that failed only as provider-cancelled are rerun on the same head up to `ci_loop.rerun_cancelled` (default 1), and only while the forge head equals `gated_sha` |
| head continuity at each step and before push (`push.go:80,140-165`) | every automatic instruction records its expected base head. The next done Signal must descend from it (`head_discontinuity` otherwise). Gate, round, CI observation and merge must all name one head (existing D03 pins plus the new round bindings) |
| intent step | the Brief carries explicit intent. Its quoted User words are included verbatim in every round and Fix Round instruction |
| document step | optional: `review_loop.docs = true` adds documentation gaps to the review rubric as `docs-*` patterns that default to P2, and a docs command can be one more Gate entry |
| review receipt (D03) | not needed: the evidence is native (exact-head pass, carry or User acceptance) |

#### Validation flow

For a Ship Task with the loop enabled, after its done Signal:

1. Freshness: if the head lacks the frozen target tip, Posse queues the existing merge instruction (ADR 0019).
2. Gate: `posse land` runs the Gates at the head. A failure queues a Gate Fix Round while budget remains, otherwise raises `gate_failed` for the Lead.
3. Review: with `review_loop.auto_start = true`, Posse admits the next round once the Gate passes. Otherwise the Lead runs `posse review start`.
4. CI: the PR watch runs on the published head in parallel with review. Failures queue CI Fix Rounds or cancelled-check reruns.
5. Land: requires the selected evidence, a green observation and the current target at one head (existing D03), and no blocking finding.

Fix instructions for the same head are coalesced: Gate, CI and review work still queued for that head are delivered as one instruction, so related work is fixed in one pass. Every fix produces a new head and restarts the flow at step 1. Review of that head is a delta round, or a carry when it is a clean target merge.

### Review Loop

Successive Review Tasks of one Ship Task become numbered **Review Rounds** over a durable **Finding Ledger**. The loop acts only when a Review Task targets a Ship Task.

#### Rounds and the cap

- A Review Round is one Review Task that records a verdict (`pass`, `changes_requested` or `inconclusive`) for one exact Ship head. A Review Task that ends without a verdict (failed, lost, discarded) is a void round. It is recorded but does not consume the cap.
- At most `review_loop.max_rounds` rounds exist per Ship Task. The default is 3 and the allowed range is 1 to 3, so configuration can lower the cap but never raise it. The cap never resets for that Ship Task.
- Gate fixes, CI fixes and target merges are not rounds. Their commits are author changes and fall into the next round's delta.
- When the last round has no `pass`, or author changes exist after the last allowed round, Posse refuses further rounds with `review_round_cap`, raises `review_cap_reached` and a User-only `review_cap` Decision: `accept-head`, `final-fix`, `rethink` or `discard`. `rethink` follows the guidance to reconsider the approach. It starts a new Ship Task, which gets a fresh cap and inherits the old ledger so that nothing is reported again.

#### Delta scope

- Round 1 reviews the full candidate: the diff from the merge base of the head and the target to the head.
- Each later round reviews only author changes since the last reviewed head `R`. Posse computes a virtual baseline `B = git merge-tree --write-tree R T`, where `T` is the newest target commit the new head `H` contains. The round scope is `diff(B, H)`. A clean merge of `origin/<target>` therefore contributes nothing, while a conflict resolution contributes exactly the resolved hunks. If `R` already contains `T`, `B` is `R`'s tree. If `R` is not an ancestor of `H` (a rewritten local branch), the round falls back to full scope and records it.
- The Reviewer works in a checkout of `H`, may read any file, and receives the cumulative diff as context. A finding counts toward the round only if its evidence lies in the delta, or if it cites a delta hunk as its cause. Other findings are recorded as out of scope and pre-existing, never block, and are routed to follow-up issues. This is the mechanism that addresses the observation that most repeat flags concerned code present since the first pass.

#### Patterns

- A Reviewer reports findings as **Finding Patterns**: one pattern per root cause or violated rule, never per file. Each pattern has a normalized key, a title, the rule, the root cause, fix guidance that names how to find unlisted instances, a severity, an origin and its instances.
- A **Fix Round** is one instruction to the Ship Rider that carries every selected pattern. The Rider fixes every instance of each pattern, including unlisted ones it finds, and answers each pattern as `fixed` or `disputed` with a reason in a resolutions file. A missing answer refuses its done Signal.

#### No re-reporting

- Each finding instance has a stable fingerprint: SHA-256 over the pattern key, repository, path, enclosing symbol and whitespace-normalized excerpt, without line numbers. Posse verifies the excerpt exists at the reviewed head and refuses an unanchored finding.
- The Finding Ledger is insert-only per finding with an event history. A later round cannot create a finding whose fingerprint is already in the ledger in any state (`open`, `fixed`, `accepted`, `deferred`, `withdrawn`, `disputed`); Posse drops it and counts it as a suppressed repeat. Rounds verify earlier fixes by answering the claimed patterns as `resolved` or `unresolved`. That updates existing findings and never creates new ones.
- If a delta reintroduces a `fixed` finding, Posse records a `regressed` event on the same finding ID instead of a new report, raises `review_regression`, and the finding blocks again if its severity blocks. A recurrence of an existing pattern at a new location in the delta is a new instance of that pattern, not a repeat.

#### Severity and blocking

- Severities are P0 to P3, plus an origin: `introduced` or `pre_existing`. An open `introduced` finding at or above `review_loop.block_at` (default `P1`) blocks. P2/P3 and pre-existing findings are deferred to follow-up issues.
- Fail-closed defaults: a missing or unknown severity is `unclassified` and blocks regardless of `block_at` until the Lead classifies it; a missing origin is `introduced`; a finding without a pattern becomes its own `unclassified` pattern; a malformed findings file refuses the Signal.
- Posse derives the verdict from the ledger: `pass` only when no blocking finding is open, unresolved, disputed or regressed. A Reviewer verdict that contradicts the derived one is refused; `inconclusive` is always accepted and never passes.
- Accepting, deferring, withdrawing or downgrading a blocking finding is risk acceptance and needs a User quote under every Autonomy. With `autonomy.review = "lead"` the Lead may defer or withdraw non-blocking findings.

#### Roles, binding and freshness

- Every round is a fresh Review Task with the existing ADR 0020 launch identity rule; the Reviewer never fixes, and the Ship Rider (the author) applies every fix. Fixes are author code in the next delta.
- Admission binds each round to its scope base, head, contained target commit, baseline tree and delta digest; the Reviewer cannot choose them.
- A `pass` binds only its head. A clean target merge after a pass is carried forward without a round when Posse proves that `H`'s tree equals the conflict-free `merge-tree` of the passed head and `T`. The carry is a separate insert-only record. It does not rebind the verdict, so ADR 0020's rule that no operation rebinds a verdict is unchanged. Any other new commit, including a hand-resolved conflict or a CI fix, needs a new round or a cap Decision. This matches the guidance that an approval covers one commit.
- ADR 0019 is unchanged: rounds may review a behind head, and Land still requires the current target. Carry-forward means the target merges ADR 0019 demands do not consume rounds.
- With `defaults.review = "local"`, the evidence for a head is a loop `pass`, a carry, or an `accept-head` Decision bound to that exact head. Recorded blocking findings refuse Land with `review_findings_open` under every policy until fixed or accepted by the User, because the loop's own record would otherwise be ignored. `review_loop.enabled = false` restores today's independent full-review Review Tasks.

### Target branch

Every part of the validation loop uses the Task's frozen target (Brief `target:`, `repositories.<member>.target_branch` or `defaults.target_branch`), never the repository default. This covers the round 1 base, `T` and the virtual baseline, carry proofs, freshness, Gate context, CI and Land. Workspace Members each use their own frozen target.

### Deprecation, interim and removal of no-mistakes

- **Interim (now):** Ship admission refuses `no_mistakes_target_unsupported` when the Landing Mode is `no-mistakes` and the frozen target is not the repository default branch, in every no-mistakes version. Setting `no-mistakes` as a Landing Mode or review policy warns that it is deprecated. `posse doctor` reports each Project that still uses it. This is cheaper and safer than threading `--base-branch` through Rider prose, which v1.84 would still scope incorrectly.
- **Migration:** `posse doctor` and `posse up` print the replacement config for each Project still on no-mistakes: `landing_mode = "pr"`, `review = "local"` (or the Project's current choice), `[review_loop]`, `[gate_loop]`, `[ci_loop]`, and Gate commands suggested from the no-mistakes trusted `commands.test` and `commands.lint`. Posse does not write config; the User applies it. Admitted Tasks keep their frozen Landing Mode and finish under no-mistakes.
- **Removal:** a later release rejects `no-mistakes` as a Landing Mode or review policy with an explicit migration error naming the key, as D02 does for legacy review values. `posse update` refuses the upgrade while a live Task still has a frozen `no-mistakes` plan. Stored `review_receipts` stay readable for history.

### Dropped no-mistakes features

| Feature | Why it is dropped |
|---|---|
| daemon, JSON-RPC socket and separate SQLite state | Posse is a CLI over Herdr without a daemon (ADR 0001) and has its own store |
| bare gate repository, pre-receive and post-receive hooks, `no-mistakes` remote | `posse publish` pushes the Task branch to origin; the Worker guard blocks direct pushes |
| rebase, force-push and the force-push guard | pushed branches are never rebased (ADR 0019); there is nothing to force |
| `branch_sync` custody return | the Rider owns its Mount; nothing rewrites the User's branch |
| intent inference from transcripts | the Brief states intent explicitly |
| agent-drafted PR prose | publish renders the Rider's summary, verification, proof and risk |
| agent-run test step with untested-scenario classification | Gate commands give deterministic results; the Rider supplies `--verify` and `--proof`; missing tests are a review pattern |
| automatic staging and `protected_paths` | Posse never stages; the Rider commits its own changes |
| push-option step skipping and `--yes` gate resolution | per-Project config and Autonomy |
| review receipts | evidence is native |
| risk assessment fields | D02 has no risk classifier |
| fork-remote publishing, Pi run-profile pins, telemetry, stats, wizard, eval | not used by Posse Projects (inference) or covered by Profiles and frozen launches |
| full rereview each round and positional finding IDs | replaced by delta rounds and fingerprints |

## Consequences

- One built-in path covers what Posse needed from no-mistakes, and it follows the frozen target everywhere.
- A Ship Task receives at most three reviews; a long fix-and-review exchange becomes a User Decision instead of an unbounded loop.
- Later rounds cost only the delta, at the price of not finding older defects. Those surface as pre-existing follow-ups; the cited measurement found about 59% of repeat flags were such code.
- Reviewers must emit structured, anchored findings; free-text-only Review Reports no longer qualify under the loop.
- Fingerprints are textual. A defect moved or rewritten beyond recognition can escape deduplication; the scope rule (older code is out of scope) and Reviewer `matches`/`regresses` claims bound that gap. That is an inference, not a measured rate.
- A CI or Gate fix after a final pass needs a delta round or, at the cap, a User Decision.
- Posse now queues fix instructions automatically within per-Project budgets; exhausted budgets return control to the Lead.
- Gate commands still execute repository code at the candidate head, as no-mistakes test and lint steps do.
- Projects on `no-mistakes` with a non-default target are refused immediately and must migrate to `pr`.
- New state: rounds, patterns, findings, finding events, carries, dispositions and fix-round budgets, all included in Task snapshots.

## Alternatives

- **Keep no-mistakes and add `--base-branch`.** Rejected: the User's goal is to stop needing no-mistakes, v1.84 still scopes review and tests against the default branch, and trusted config always comes from the default branch.
- **Full rereview each round (no-mistakes).** Rejected by rule 2, and by the cited measurement that most repeat flags concerned code present since the first pass.
- **Diff the commit range `R..H`.** Rejected because merges of `origin/<target>`, which ADR 0019 requires, would put target code into scope.
- **Hard-code a mandatory pipeline.** Rejected; Posse is a foundation, so each part is configurable and review evidence stays a User policy.
- **Line-number or positional finding IDs.** Rejected because they change with every edit.
- **Let the Reviewer decide what is a repeat.** Kept only as an extra claim; Posse enforces the fingerprint and scope rules mechanically.
- **Allow a configurable cap above 3.** Rejected by rule 1; `rethink` is the escape, and it is a new Ship Task.
