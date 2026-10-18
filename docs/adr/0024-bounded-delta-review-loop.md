# 24. Bounded delta review loop

## Context

The User asked for a built-in Posse review process modelled on no-mistakes but fitted to Posse:

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

## Decision

Posse adds a **Review Loop**: an optional, built-in protocol that turns successive Review Tasks of one Ship Task into numbered **Review Rounds** over a durable **Finding Ledger**. It is configured by the User (`[review_loop]`), is on by default, and acts only when a Review Task targets a Ship Task. It does not make review mandatory: the existing `defaults.review` policy still decides whether Land requires evidence.

### Rounds and the cap

- A Review Round is one Review Task that records a verdict (`pass`, `changes_requested` or `inconclusive`) for one exact Ship head. A Review Task that ends without a verdict (failed, lost, discarded) is a void round. It is recorded but does not consume the cap.
- At most `review_loop.max_rounds` rounds exist per Ship Task. The default is 3 and the allowed range is 1 to 3, so configuration can lower the cap but never raise it. The cap never resets for that Ship Task.
- Gate fixes, CI fixes and target merges are not rounds. Their commits are author changes and fall into the next round's delta.
- When the last round has no `pass`, or author changes exist after the last allowed round, Posse refuses further rounds with `review_round_cap`, raises `review_cap_reached` and a User-only `review_cap` Decision: `accept-head`, `final-fix`, `rethink` or `discard`. `rethink` follows the guidance to reconsider the approach. It starts a new Ship Task, which gets a fresh cap and inherits the old ledger so that nothing is reported again.

### Delta scope

- Round 1 reviews the full candidate: the diff from the merge base of the head and the target to the head.
- Each later round reviews only author changes since the last reviewed head `R`. Posse computes a virtual baseline `B = git merge-tree --write-tree R T`, where `T` is the newest target commit the new head `H` contains. The round scope is `diff(B, H)`. A clean merge of `origin/<target>` therefore contributes nothing, while a conflict resolution contributes exactly the resolved hunks. If `R` already contains `T`, `B` is `R`'s tree. If `R` is not an ancestor of `H` (a rewritten local branch), the round falls back to full scope and records it.
- The Reviewer works in a checkout of `H`, may read any file, and receives the cumulative diff as context. A finding counts toward the round only if its evidence lies in the delta, or if it cites a delta hunk as its cause. Other findings are recorded as out of scope and pre-existing, never block, and are routed to follow-up issues. This is the mechanism that addresses the observation that most repeat flags concerned code present since the first pass.

### Patterns

- A Reviewer reports findings as **Finding Patterns**: one pattern per root cause or violated rule, never per file. Each pattern has a normalized key, a title, the rule, the root cause, fix guidance that names how to find unlisted instances, a severity, an origin and its instances.
- A **fix round** is one Lead instruction to the Ship Rider that carries every selected pattern. The Rider fixes every instance of each pattern, including unlisted ones it finds, and answers each pattern as `fixed` or `disputed` with a reason in a resolutions file. A missing answer refuses its done Signal.

### No re-reporting

- Each finding instance has a stable fingerprint: SHA-256 over the pattern key, repository, path, enclosing symbol and whitespace-normalized excerpt, without line numbers. Posse verifies the excerpt exists at the reviewed head and refuses an unanchored finding.
- The Finding Ledger is insert-only per finding with an event history. A later round cannot create a finding whose fingerprint is already in the ledger in any state (`open`, `fixed`, `accepted`, `deferred`, `withdrawn`, `disputed`); Posse drops it and counts it as a suppressed repeat. Rounds verify earlier fixes by answering the claimed patterns as `resolved` or `unresolved`. That updates existing findings and never creates new ones.
- If a delta reintroduces a `fixed` finding, Posse records a `regressed` event on the same finding ID instead of a new report, raises `review_regression`, and the finding blocks again if its severity blocks. A recurrence of an existing pattern at a new location in the delta is a new instance of that pattern, not a repeat.

### Severity and blocking

- Severities are P0 to P3, plus an origin: `introduced` or `pre_existing`. An open `introduced` finding at or above `review_loop.block_at` (default `P1`) blocks. P2/P3 and pre-existing findings are deferred to follow-up issues.
- Fail-closed defaults: a missing or unknown severity is `unclassified` and blocks regardless of `block_at` until the Lead classifies it; a missing origin is `introduced`; a finding without a pattern becomes its own `unclassified` pattern; a malformed findings file refuses the Signal.
- Posse derives the verdict from the ledger: `pass` only when no blocking finding is open, unresolved, disputed or regressed. A Reviewer verdict that contradicts the derived one is refused; `inconclusive` is always accepted and never passes.
- Accepting, deferring, withdrawing or downgrading a blocking finding is risk acceptance and needs a User quote under every Autonomy. With `autonomy.review = "lead"` the Lead may defer or withdraw non-blocking findings.

### Roles, binding and freshness

- Every round is a fresh Review Task with the existing ADR 0020 launch identity rule; the Reviewer never fixes, and the Ship Rider (the author) applies every fix. Fixes are author code in the next delta.
- Admission binds each round to its scope base, head, contained target commit, baseline tree and delta digest; the Reviewer cannot choose them.
- A `pass` binds only its head. A clean target merge after a pass is carried forward without a round when Posse proves that `H`'s tree equals the conflict-free `merge-tree` of the passed head and `T`. The carry is a separate insert-only record. It does not rebind the verdict, so ADR 0020's rule that no operation rebinds a verdict is unchanged. Any other new commit, including a hand-resolved conflict or a CI fix, needs a new round or a cap Decision. This matches the guidance that an approval covers one commit.
- ADR 0019 is unchanged: rounds may review a behind head, and Land still requires the current target. Carry-forward means the target merges ADR 0019 demands do not consume rounds.

### Target branch

The User added:

> "một thứ nữa là vấn đề trong việc lựa chọn target branch , no mistakes đang luôn mặc định là ở Bren chính trong khi nhiều dự án tôi làm việc ở 1 Ranch khác như là Staging, vậy nên nên có lựa chọn target caus hình cho dự án"

Posse already freezes a target per Task from `defaults.target_branch`, `repositories.<member>.target_branch` or Brief `target:`. The loop uses that frozen target, never the repository default, for: the round 1 base (merge base with `origin/<target>`), the contained target commit `T` and virtual baseline, carry-forward proofs, ADR 0019 freshness, and CI and Land checks. Workspace Members each use their own frozen target. Review configuration comes from Posse config, which the User owns. Repository-supplied review guidance, if a later slice reads any, is read from `origin/<target>` at admission and never from the Ship branch, because the Ship branch is author content.

Today's `no-mistakes` landing mode does not honor a non-default target:

- Posse's Rider protocol tells the Rider to run `no-mistakes axi run --intent <from Brief>` and never passes the target (`internal/app/tasks.go:1013-1014`, `internal/app/pr_freshness.go:65`). Workspace Members cannot use `no-mistakes`.
- no-mistakes records the repository default branch at `init` from `origin`'s `HEAD`, falling back to `main` (`internal/gate/gate.go:139-140`, `internal/git/git.go:430-445`). In v1.50.0 every step uses it: rebase onto `origin/<default>` (`internal/pipeline/steps/rebase.go:31-33`), review and test scope (`review.go:22`, `test.go:24`), and the PR base (`pr.go:86,107`).
- Since v1.62.0 (`e08143f`), `axi run --base-branch <branch>` or the trusted `pr.base_branch` selects another integration branch. The installed v1.84.0 (`4822244`) applies it only to rebase, PR and CI (`effectivePRBaseBranch`, `rebase.go:202-217`). Review, test, lint, document and custom-gate scope still use the merge base with the default branch (`review.go:32`, `test.go:37`, `lint.go:23`, `document.go:93`, `customgate.go:48` at `4822244`). The fix that scopes them against the run's base (`e043200`) first ships in v1.86.0.
- Through v1.88.0 (`8ad4dd1`), no-mistakes reads trusted config (commands, agent, `pr.base_branch`) from `origin/<default>` (`internal/daemon/manager.go:230-243`).

As a result, for a Task whose target is `staging`, a Rider that follows the protocol today gets a rebase onto the default branch and a PR against it. `posse holler done --pr` then refuses that PR because its base is not the frozen target (`internal/app/publish.go:277`). If the default branch is ahead of `staging`, the rebase also drags unrelated commits into the branch. A Rider that adds `--base-branch staging` on v1.84 gets a correct PR, but review and test scope cover every `staging` commit missing from the default branch.

Until the loop replaces `no-mistakes` delivery for such Projects, Posse:

- adds `--base-branch <frozen target>` to the no-mistakes Rider protocol and catch-up instruction whenever the frozen target differs from the repository default;
- refuses admission of a `no-mistakes` Ship Task whose frozen target differs from the default when the installed no-mistakes is older than v1.86.0 or lacks `--base-branch`. The refusal is `no_mistakes_target_unsupported`, and `posse doctor` reports the same gap;
- reports in `posse doctor`, for v1.86.0 and later, that no-mistakes reads trusted commands and agent config from the default branch, not the target. Posse Gates are unaffected because they come from Posse config.

### Coexistence

- `review_loop.enabled = false` restores today's independent full-review Review Tasks.
- With `defaults.review = "local"`, a loop `pass` (or a carry or an `accept-head` Decision bound to the exact head) is the required evidence. With `no-mistakes`, the pipeline receipt stays the evidence, and no-mistakes rounds keep their own full-diff behavior; Posse does not alter them. Importing no-mistakes `ask-user` findings into the ledger is a later, optional slice.
- Recorded blocking findings refuse Land with `review_findings_open` under every policy until fixed or accepted by the User, because the loop's own record would otherwise be ignored.

## Consequences

- A Ship Task receives at most three reviews; a long fix-and-review exchange becomes a User Decision instead of an unbounded loop.
- Later rounds cost only the delta, at the price of not finding older defects. Those surface as pre-existing follow-ups; the cited measurement found about 59% of repeat flags were such code.
- Reviewers must emit structured, anchored findings; free-text-only Review Reports no longer qualify under the loop.
- Fingerprints are textual. A defect moved or rewritten beyond recognition can escape deduplication; the scope rule (older code is out of scope) and Reviewer `matches`/`regresses` claims bound that gap. That is an inference, not a measured rate.
- A CI fix after the final allowed round always needs a User Decision.
- `no-mistakes` Ship Tasks targeting a non-default branch are refused on no-mistakes older than v1.86.0, and the Rider must pass `--base-branch`.
- New state: rounds, patterns, findings, finding events and carries, all included in Task snapshots.

## Alternatives

- **Full rereview each round (no-mistakes).** Rejected by rule 2, and by the cited measurement that most repeat flags concerned code present since the first pass.
- **Diff the commit range `R..H`.** Rejected because merges of `origin/<target>`, which ADR 0019 requires, would put target code into scope.
- **Hard-code the loop as a mandatory stage.** Rejected; Posse is a foundation and review evidence stays a User policy.
- **Line-number or positional finding IDs.** Rejected because they change with every edit.
- **Let the Reviewer decide what is a repeat.** Kept only as an extra claim; Posse enforces the fingerprint and scope rules mechanically.
- **Allow a configurable cap above 3.** Rejected by rule 1; `rethink` is the escape, and it is a new Ship Task.
