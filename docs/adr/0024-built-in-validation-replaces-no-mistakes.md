# 24. Built-in validation replaces no-mistakes

**Current status:** ADR 0028 removes configurable command Gates and their repair loop. The command-Gate entries below describe the design before that removal, not current CLI behavior. PR verification uses forge CI at `gated_sha`; local delivery has no automated verification.

## Context

The User asked for a built-in Posse process modelled on no-mistakes but fitted to Posse:

> "tôi muốn tìm hiểu cơ chế của no mistake để có thể tạo ra riêng 1 thứ phù hợp với posse builtin"
>
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
- "An approval covers one specific commit. If new commits merge that the system can't vouch for, like an agent's push or a merge someone resolved by hand, the approval has to be given again."
- "Every finding has to be answered. You either fix it or resolve the thread with a reason."
- "Our priority, written down, is catching real bugs first and cutting review rounds second."
- Reported results on internal PRs: mean reviews to pass fell from 2.08 to 1.32, and PRs needing five or more rounds fell from 8.9% to 1.5%.

no-mistakes (studied at v1.50.0, commit `c4bc34b`) already bounds fix attempts per step and defaults the review step to zero automatic fixes (`internal/config/config.go:1656-1665`). Its rereview is session-free, so the rereviewer is never the session that prescribed fixes, and pipeline fixes are reviewed as author code (`internal/pipeline/steps/review.go:295-305`). Unclassified findings default to `ask-user` (`internal/types/findings.go:355-363`). It differs from the four rules in three ways. A fix-mode rereview diffs the whole branch against its base again (`internal/pipeline/steps/review.go:118-130`). Finding IDs are positional (`<prefix>-<n>`, `internal/types/findings.go:118-127`), so nothing identifies a finding across rounds. Findings are flat per file and line, not grouped by cause.

Posse already binds a Review Verdict to the exact Change revision it examined and requires the Reviewer launch to differ from every author launch (ADR 0020). It requires a PR head to contain the current target before final CI and merge, and asks the Worker to merge, not rebase, `origin/<target>` into the Task branch (ADR 0019). In practice only P0/P1 defects introduced by the change block a merge; P2/P3 findings become follow-up issues. Every Review Task today reviews the full candidate, every finding is free text in a Report, and nothing limits how many Review Tasks a Change Task receives.

The User then set the goal:

> "mục tiêu của tôi là không cần dùng tới no mistake nữa"

and raised the target branch:

> "một thứ nữa là vấn đề trong việc lựa chọn target branch , no mistakes đang luôn mặc định là ở Bren chính trong khi nhiều dự án tôi làm việc ở 1 Ranch khác như là Staging, vậy nên nên có lựa chọn target caus hình cho dự án"

Posse delegates to no-mistakes in two places: the `no-mistakes` Merge Mode, where the Worker delivers only through `no-mistakes axi run` (`internal/app/tasks.go:1013-1014`), and the `no-mistakes` review policy, which requires a pipeline-owned receipt (ADR 0020, D03). no-mistakes runs a fixed pipeline: intent, rebase, review, test, document, lint, push, pr, ci (`internal/pipeline/executor.go:562-992` at v1.50.0). Posse already has its own equivalents for part of it: Gates bound to `gated_sha`, `posse publish` with a tokenized PR body, the PR watch with CI Notices, freshness by merge (ADR 0019), head pinning at merge, and exact-head review verdicts (ADR 0020).

The target branch matters because no-mistakes does not follow Posse's frozen target:

- Posse's Worker protocol tells the Worker to run `no-mistakes axi run --intent <from Brief>` and never passes the target (`internal/app/tasks.go:1013-1014`, `internal/app/pr_freshness.go:65`). Workspace Members cannot use `no-mistakes`.
- no-mistakes records the repository default branch at `init` from `origin`'s `HEAD`, falling back to `main` (`internal/gate/gate.go:139-140`, `internal/git/git.go:430-445`). In v1.50.0 every step uses it: rebase onto `origin/<default>` (`internal/pipeline/steps/rebase.go:31-33`), review and test scope (`review.go:22`, `test.go:24`), and the PR base (`pr.go:86,107`).
- Since v1.62.0 (`e08143f`), `axi run --base-branch <branch>` or the trusted `pr.base_branch` selects another integration branch. The installed v1.84.0 (`4822244`) applies it only to rebase, PR and CI (`effectivePRBaseBranch`, `rebase.go:202-217`). Review, test, lint, document and custom-gate scope still use the merge base with the default branch (`review.go:32`, `test.go:37`, `lint.go:23`, `document.go:93`, `customgate.go:48` at `4822244`). The fix that scopes them against the run's base (`e043200`) first appears in v1.86.0.
- Through v1.88.0 (`8ad4dd1`), no-mistakes reads trusted config (commands, agent, `pr.base_branch`) from `origin/<default>` (`internal/daemon/manager.go:230-243`).

So for a Task whose target is `staging`, a Worker that follows the protocol today gets a rebase onto the default branch and a PR against it. `posse signal done --pr` then refuses that PR because its base is not the frozen target (`internal/app/publish.go:277`). If the default branch is ahead of `staging`, the rebase also drags unrelated commits into the branch.

## Decision

Posse gets built-in validation for `pr` and `local` delivery, separate from the optional workflow profiles in [ADR 0027](0027-thin-core-and-user-owned-workflow-profiles.md). The target review policy is exactly `defaults.review = "none" | "quick" | "full-pipeline"`, with per-Member overrides and admission freezing. `quick` is today's `local` review. `none` keeps the direct path; `quick` keeps exact-head independent review at Merge; `full-pipeline` adds required local validation before every push. A profile or Autonomy cannot bypass the selected mode. Configurable loops are mechanisms, not switches that disable required evidence.

The `local` -> `quick` config rename is automatic on update, with no alias kept; local Git delivery is not renamed. VP-B custody and the initial VP-C single-repository PR stages/barrier are implemented, but `full-pipeline` remains unavailable for production until the remaining V09-V14 integration and acceptance proof pass. no-mistakes delivery/review remain deprecated, and V15 removal (#305) is on hold until parity and migration proof. [Spec section 25](../spec.md#25-built-in-validation) distinguishes the implemented slice from the remaining runtime and E2E work.

### Capability map

| no-mistakes capability | Built-in replacement |
|---|---|
| review step, bounded rounds | Review Loop (below): 3-round cap, delta scope, Finding Patterns, Finding Ledger |
| test and lint steps with automatic fixes (`config.go:1656-1665`: lint 3, test 3) | Gate commands run at the exact head (existing), plus Gate Fix Rounds: on a failed Gate, Posse queues the Worker one fix instruction with the failing command and output tail, up to `gate_loop.max_fix_rounds` (default 3) per Change Task |
| custom gates | more `gate` entries |
| trusted config read from the default branch (`internal/daemon/manager.go:862-876` at v1.50.0) | Gate commands, loop settings and Profiles come only from Posse config, which is User-only and never from the Change branch. Optional repository review guidance is read from `origin/<frozen target>` at admission |
| rebase onto `origin/<default>` | merge `origin/<frozen target>` into the Task branch (ADR 0019, existing), never rebase |
| push (force-with-lease to a gate remote) | `posse publish` pushes only the Task branch, without force (existing) |
| pr step with attestation (`prsummary.go:25,101`) | `posse publish` template (existing) plus a Posse-owned Validation block in the tokenized body: head SHA, Gate result, review rounds and verdict, ledger counts and CI state. It is rendering only; the evidence stays in SQLite |
| ci step: poll, fix from failed logs, dedupe by failing-check set, rerun cancelled checks (`steps/ci.go:122-551`, `ci_transient.go`) | PR watch (existing) plus the CI loop: on failed required checks for the current head, Posse queues one fix instruction with check names, log URLs and failed-log tails, deduplicated by failing-check set per head, up to `ci_loop.max_fix_rounds` (default 3). Checks that failed only as provider-cancelled are rerun on the same head up to `ci_loop.rerun_cancelled` (default 1), and only while the forge head equals `gated_sha` |
| head continuity at each step and before push (`push.go:80,140-165`) | automatic instructions retain their expected base head; `full-pipeline` additionally binds every required local stage to one exact final head, target and config before publication. Ancestry is not fresh evidence |
| intent and test evidence | the Brief carries explicit intent, quoted verbatim in every round/fix instruction; `full-pipeline` requires intent/E2E proof as well as deterministic Gates, with inconclusive evidence never counted as passed |
| document step | required Docs stage in `full-pipeline`: affected owner docs, no-comments check and complete PR description (intent, changes, verification/results, proof, risk, rollback); an explicit no-owner-doc-change disposition does not skip the other checks. `review_loop.docs` remains only the optional extra reviewer rubric in lighter modes |
| review receipt (D03) | not needed: the evidence is native (exact-head pass, carry or User acceptance) |

#### Validation flow

| Mode | Evidence and flow | Skips |
|---|---|---|
| `none` | Direct publication; review is not required, not passed. Existing Gates, target freshness, forge checks and merge authority remain | Independent review and full-pipeline stages |
| `quick` | Today's `local`: publication may precede review; Merge needs exact-head independent review (or the specified carry/User disposition), plus ordinary delivery checks | Private lane and mandatory intent-test/docs/lint bundle |
| `full-pipeline` | Private local candidate -> review -> test (intent proof) -> docs -> lint -> exact-head barrier -> publish/PR -> forge CI -> pinned Merge | No required stage is silently disabled |

For `none`/`quick`, the existing loop-enabled post-done path remains freshness -> Gates -> selected review -> Merge, with PR-watch CI allowed in parallel with review. Missing reviewer resolution raises readiness but never satisfies `quick`. For `full-pipeline`:

1. Submit author head `S` without pushing or requiring a PR/done Signal. Give a Posse-owned private local candidate `H` exclusive validation custody in a checkout isolated from concurrent author edits. Fetch frozen target `T` and merge it before validation if needed. Preserve ADR 0019's merge-only policy for published branches.
2. Validate **review -> test (intent proof) -> docs -> lint**. Docs includes the no-comments check (remove unnecessary comments, retain necessary rationale) and a complete PR description. Run the repository's workflow jobs locally where possible; record unsupported jobs and runner/secret/event differences, not a fictional remote-CI pass.
3. Bind all required local results to one exact final `H`, target `T` and frozen config digest. Any later edit/commit invalidates earlier results and re-enters the ordered sequence, ending with a no-edit verification pass. A review carry carries only review, never test/docs/lint/CI. Missing reviewer, failed/inconclusive proof or unmet required stages blocks publication; stage exceptions and User acceptance are visible, not clean passes.
4. Before every push, compare submitted author/candidate/expected remote heads and current target. Refuse moved author/candidate or stale evidence; target advancement returns to freshness and validation. Publish only validated `H` by fast-forward push, then create/update the PR/MR. No hidden remote candidate branch or draft PR substitutes for private local validation. Retry PR metadata idempotently if it fails after code publication.
5. Forge CI is the final confirmation after publication, bound to exact `H`. A CI code fix creates a private candidate and repeats freshness and local validation before any republish. Same-head cancelled-check reruns use their own budget. CI/Gate repairs do not reset the review cap. Unknown, pending or absent checks are not green proof without explicit trusted no-CI policy.
6. Merge requires the selected evidence, current target and applicable forge confirmation on one head, no blocking finding, and existing merge authority. `local-validated` and `forge-ready` are separate states; neither is merged. Local delivery records remote CI/PR as not applicable rather than passed.

Queued undelivered Gate/CI/review fix instructions for one head are still coalesced. The User decides entry to each configured stage (ADR 0027); automatic round/fix scheduling operates only inside the approved stage. Stage approval, `autonomy.review` and `autonomy.merge` do not grant automatic progression or publication bypasses.

The t335 study confirms no-mistakes v1.50.0 uses a private local lane then publishes the real branch before observing forge CI. Its later fix commits can pass ancestry checks without exact-final-head revalidation, and CI repair pushes directly. Posse adopts the local-lane idea, not those evidence gaps; it does not claim remote CI ran before publication.

### Review Loop

Successive Review Tasks of one Change Task become numbered **Review Rounds** over a durable **Finding Ledger**. The loop acts only when a Review Task targets a Change Task.

#### Rounds and the cap

- A Review Round is one Review Task that records a verdict (`pass`, `changes_requested` or `inconclusive`) for one exact Change head. A Review Task that ends without a verdict (failed, lost, discarded) is a void round. It is recorded but does not consume the cap.
- At most `review_loop.max_rounds` rounds exist per Change Task. The default is 3 and the allowed range is 1 to 3, so configuration can lower the cap but never raise it. The cap never resets for that Change Task.
- Gate fixes, CI fixes and target merges are not rounds. Their commits are author changes and fall into the next round's delta.
- When the last round has no `pass`, or author changes exist after the last allowed round, Posse refuses further rounds with `review_round_cap`, raises `review_cap_reached` and a User-only `review_cap` Decision: `accept-head`, `final-fix`, `rethink` or `discard`. `rethink` follows the guidance to reconsider the approach. It starts a new Change Task, which gets a fresh cap and inherits the old ledger so that nothing is reported again.

#### Delta scope

- Round 1 reviews the full candidate: the diff from the merge base of the head and the target to the head.
- Each later round reviews only author changes since the last reviewed head `R`. Posse computes a virtual baseline `B = git merge-tree --write-tree R T`, where `T` is the newest target commit the new head `H` contains. The round scope is `diff(B, H)`. A clean merge of `origin/<target>` therefore contributes nothing, while a conflict resolution contributes exactly the resolved hunks. If `R` already contains `T`, `B` is `R`'s tree. If `R` is not an ancestor of `H` (a rewritten local branch), the round falls back to full scope and records it.
- The Reviewer works in a checkout of `H`, may read any file, and receives the cumulative diff as context. A finding counts toward the round only if its evidence lies in the delta, or if it cites a delta hunk as its cause. Other findings are recorded as out of scope and pre-existing, never block, and are routed to follow-up issues. This is the mechanism that addresses the observation that most repeat flags concerned code present since the first pass.

#### Patterns

- A Reviewer reports findings as **Finding Patterns**: one pattern per root cause or violated rule, never per file. Each pattern has a normalized key, a title, the rule, the root cause, fix guidance that names how to find unlisted instances, a severity, an origin and its instances.
- A **Fix Round** is one instruction to the Change Worker that carries every selected pattern. The Worker fixes every instance of each pattern, including unlisted ones it finds, and answers each pattern as `fixed` or `disputed` with a reason in a resolutions file. A missing answer refuses its done Signal.

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

- Every round is a fresh Review Task with the existing ADR 0020 launch identity rule; the Reviewer never fixes, and the Change Worker (the author) applies every fix. Fixes are author code in the next delta.
- Admission binds each round to its scope base, head, contained target commit, baseline tree and delta digest; the Reviewer cannot choose them.
- A `pass` binds only its head. A clean target merge after a pass is carried forward without a round when Posse proves that `H`'s tree equals the conflict-free `merge-tree` of the passed head and `T`. The carry is a separate insert-only record. It does not rebind the verdict, so ADR 0020's rule that no operation rebinds a verdict is unchanged. Any other new commit, including a hand-resolved conflict or a CI fix, needs a new round or a cap Decision. This matches the guidance that an approval covers one commit.
- ADR 0019 is unchanged: rounds may review a behind head, and Merge still requires the current target. Carry-forward means the target merges ADR 0019 demands do not consume rounds.
- With `defaults.review = "quick"` (today's `local`), the evidence for a head is a loop `pass`, a carry, or an `accept-head` Decision bound to that exact head. Full-pipeline needs this review component plus exact-final-head test/docs/lint and applicable forge CI. An `accept-head` is visible review-risk acceptance, not a pass for other stages. Recorded blocking findings refuse Merge under every policy and publication in `full-pipeline` until fixed or accepted by the User. Disabling the Review Loop restores independent full-review Tasks, not permission to skip selected review.
- Fixes are applied in the private candidate lane under `full-pipeline`; `quick` retains the author-branch path. Custody, head/config bindings and Member evidence must survive restart. The PR body reports native evidence, never authorizes delivery.

### Target branch

Every part of the validation loop uses the Task's frozen target (Brief `target:`, `repositories.<member>.target_branch` or `defaults.target_branch`), never the repository default. This covers the round 1 base, `T` and the virtual baseline, carry proofs, freshness, Gate context, CI and Merge. Workspace Members each use their own frozen target.

### Deprecation, interim and removal of no-mistakes

- **Interim (now):** Change admission refuses `no_mistakes_target_unsupported` when the Merge Mode is `no-mistakes` and the frozen target is not the repository default branch, in every no-mistakes version. Setting `no-mistakes` as a Merge Mode or review policy warns that it is deprecated. `posse doctor` reports each Project that still uses it. This is cheaper and safer than threading `--base-branch` through Worker prose, which v1.84 would still scope incorrectly.
- **Review-name migration:** update automatically rewrites review `local` to `quick` in global/Project and Member config, including supported Project scalar forms, preserving comments and unrelated settings. No alias remains in new config/Briefs. Failed migration leaves the prior config usable with a file/key error. `merge_mode = "local"`, immutable plans and historical evidence remain unchanged; historical `local` review is interpreted with its old semantics, never upgraded to `full-pipeline`.
- **Pipeline migration:** `posse doctor` and `posse up` suggest `merge_mode = "pr"`, `review = "full-pipeline"`, loop settings and trusted test/lint commands for no-mistakes replacement; `none`/`quick` are explicitly lighter alternatives. Unlike the automatic name migration, this policy change needs the User's choice. Frozen Tasks finish under their original plans and targets. Local job equivalence is not complete forge-CI parity. The studied v1.50.0 does not implement Posse's expected `axi receipt` producer, and a PR lifecycle attestation comment cannot substitute for it.
- **Removal (V15 #305 on hold):** after parity and migration proof, a later release rejects `no-mistakes` as a Merge Mode or review policy with an explicit migration error naming the key, as D02 does for legacy review values. `posse update` refuses the upgrade while a live Task still has a frozen `no-mistakes` plan. Stored `review_receipts` stay readable for history.

### Dropped no-mistakes features

| Feature | Why it is dropped |
|---|---|
| daemon, JSON-RPC socket and separate SQLite state | Posse is a CLI over Herdr without a daemon (ADR 0001) and has its own store |
| bare gate repository, pre-receive and post-receive hooks, `no-mistakes` remote | reuse Posse Task/store concepts for private candidate custody and exact-head promotion; no second bare-gate system is required |
| rebase, force-push and the force-push guard | pushed branches are never rebased (ADR 0019); there is nothing to force |
| `branch_sync` custody return | candidate/author movement is checked before promotion; never rewrite the User's branch or silently overwrite concurrent author work |
| intent inference from transcripts | the Brief states intent explicitly |
| separate agent-drafted PR service | full-pipeline Docs prepares the complete description before publication; publish renders summary, verification, proof, risk and native evidence |
| inference that deterministic Gates alone prove intent | not adopted: full-pipeline requires explicit intent/E2E proof as well as commands; none/quick retain their lighter contracts |
| automatic staging and `protected_paths` | Posse never stages; the Worker commits its own changes |
| push-option step skipping and `--yes` gate resolution | required stages are frozen at admission; Autonomy and profile settings cannot skip the selected mode |
| review receipts | evidence is native |
| risk assessment fields | D02 has no risk classifier |
| fork-remote publishing, Pi run-profile pins, telemetry, stats, wizard, eval | not used by Posse Projects (inference) or covered by Profiles and frozen launches |
| full rereview each round and positional finding IDs | replaced by delta rounds and fingerprints |

## Consequences

- Three selectable review modes keep the direct path light while reserving validate-before-push for `full-pipeline`; each follows the frozen target everywhere. This is target behavior, not a claim that full-pipeline ships today.
- Private candidate custody, all publication entry points and exact-head evidence invalidation are required work, not covered by the later merge Gate. Local workflow jobs reduce forge failures but cannot reproduce every hosted runner, secret or PR event.
- A Change Task receives at most three reviews; a long fix-and-review exchange becomes a User Decision instead of an unbounded loop.
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
- **Hard-code a mandatory pipeline.** Rejected: the User selects `none`, `quick` or `full-pipeline`; only the latter requires the whole local sequence. Once selected, its requirements cannot be disabled by a profile or loop switch.
- **Line-number or positional finding IDs.** Rejected because they change with every edit.
- **Let the Reviewer decide what is a repeat.** Kept only as an extra claim; Posse enforces the fingerprint and scope rules mechanically.
- **Allow a configurable cap above 3.** Rejected by rule 1; `rethink` is the escape, and it is a new Change Task.
