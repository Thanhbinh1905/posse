# Direction 2 design: a dispatch core with Delivery and review as attachments

Status: proposal for User approval. Nothing in this document is implemented, and the migration has not started.

Source: [Posse reassessment](2026-11-12-posse-reassessment.md), Direction 2 and the "challenge to this recommendation" (section I). This document builds on the dispatch precedence that `eab53125` (#973) shipped and does not redesign it.

## How to read this document

Evidence labels follow the reassessment:

- **[O] Observed**: read in code, a document, a command output or an experiment.
- **[I] Inferred**: a conclusion drawn from observations.
- **[H] Hypothesis**: plausible but not verified.

Paths are relative to the Posse repository unless prefixed `GH:` (Greenhouse, `/data/Personal/greenhouse`, at commit `11b2d7c`). "The report" means the reassessment.

## Contents

1. [Decisions in one page](#1-decisions-in-one-page)
2. [Phase 1: review moves to Greenhouse](#2-phase-1-review-moves-to-greenhouse)
3. [Challenge to the three Task properties](#3-challenge-to-the-three-task-properties)
4. [Domain model](#4-domain-model)
5. [Kind and Profile](#5-kind-and-profile)
6. [Skill selection](#6-skill-selection)
7. [Target architecture](#7-target-architecture)
8. [Phase 2: Task model migration](#8-phase-2-task-model-migration)
9. [Compatibility for historical Tasks](#9-compatibility-for-historical-tasks)
10. [Acceptance criteria](#10-acceptance-criteria)
11. [Decisions](#11-decisions)
12. [Risks and what would falsify this design](#12-risks-and-what-would-falsify-this-design)
13. [Appendix: new observations](#appendix-new-observations)

---

## 1. Decisions in one page

| Question | Decision | Section |
|---|---|---|
| Do three independent Task properties replace the type? | No. Two of the three are not independent, and one cannot be declared or enforced. One optional requirement, `outcome`, replaces the type. Its default asks the Lead for nothing | 3 |
| What is a Task? | An assignment with a stopping condition, one Sandbox, one state and one Worker at a time. Nothing else is required to start | 4 |
| Where does Delivery live? | In its own record with its own states, opened by `posse publish`. `merging`, `merged` and `reported` stop being Task states | 4 |
| Do Kind and Profile stay separate? | Yes as mechanisms, because owner, cardinality and change cadence differ. Only Profile stays a product concept. Kind becomes "harness adapter" in prose and leaves what the User and the Lead must know | 5 |
| How are skills selected? | The User defines skill sets and an allowlist. The Lead picks a set and may add allowlisted skills per Task. A selected skill is required only at a step the Task reaches | 6 |
| Who reviews code? | Greenhouse, in host mode. Posse keeps the author, the fix loop, publish, CI, merge authority and one gate: a verdict for the exact head | 2 |
| When does built-in review leave? | Only after Greenhouse meets the exit criteria of section 2.6, first in a parallel run on the same heads and then as an opt-in gate | 2 |
| Who produces test, docs and lint evidence? | Forge CI only, at the gated head (User decision J17 = a). `posse validate` and the Private Candidate leave with built-in review | 2.7 |
| What happens to the 1,335 historical Tasks? | Their rows are never rewritten. Readers map old values to the new model. Tasks admitted under an old plan finish under their frozen rules | 9 |

What does not change: Project and Lead isolation, Sandbox custody, verified Teardown, turn tokens, queued instructions, capacity admission, explicit Restore, User authority over merge, discard and config, and exact-head evidence before Merge. Every experiment in the report that held (E3, E5, E6, G1 to G10) tests one of these, and none of them depends on the Task type [I, report section "Design tensions"].

---

## 2. Phase 1: review moves to Greenhouse

This phase is self-contained. It can be approved and built before sections 3 to 8, and it uses only concepts that exist today: a Change Task, its PR or local merge, and the merge check. Where this section says "Delivery" it means the Change Task's publication and Merge as they work now.

The User decided the order on 2026-11-12 ("đồng ý, chọn (a)"):

1. Build the Greenhouse verdict gate in Posse (report J18) and run it in parallel with built-in review on the same heads, comparing results.
2. Remove Posse review (report J19) only after the parallel run meets the exit criteria in 2.6.
3. J17 = (a): once review is external, test, docs and lint evidence comes only from forge CI, together with the Greenhouse verdict (2.7).

**State of Greenhouse, verified for this document.** [O] `GH:docs/embedding.md` at `11b2d7c` documents host-mode review over several rounds with one `grant` per round, successor runs after an author repair, and `current` plus `head` binding. [O] `TestEmbeddedHostOwnsDelivery`, `TestHostOwnershipBoundaries` and `TestHostReviewResumesUnderItsGrant` exist in `GH:internal/engine/embedding_test.go`. The report saw these as unmerged drafts. Still open: Posse has no adapter, no review has run inside a Posse Sandbox (appendix N1 ran `attach` only), Workspace review is deferred in Greenhouse, and the stable-finding-id claim does not match the code (2.5).

### 2.1 Ownership

| Responsibility | Owner | Evidence that the other side refuses it |
|---|---|---|
| Judging a diff against intent and rules; findings; reviewer agent and its confinement | Greenhouse | [O] host mode "runs one fresh-context review per host grant" (`GH:docs/embedding.md`) |
| Author Worker, Sandbox, author repair | Posse | [O] host mode refuses `repair`, `resolve-conflict` (`GH:internal/engine/host.go`) |
| Candidate custody, publish, PR, CI | Posse | [O] refuses `handback`, `pipeline` |
| Round budget and cap decisions | Posse | [O] refuses `decide`, `rerun-from-start`; ignores its own `limit` in host mode |
| Which head may merge; merge authority; Autonomy | Posse | [O] "Greenhouse never merges" (report section G) |
| Whether a finding blocks | Posse, by `review_loop.block_at` | Greenhouse reports priorities; it does not know the User's merge policy |
| Rules and reviewer model selection | User, through Posse config passed in `apply` | [O] refuses `save`, `trust`, `migrate` from a host |

### 2.2 How Posse calls Greenhouse

Posse is the host. One adapter in Posse speaks protocol version 2 to `greenhouse axi`, one JSON request per invocation, from the Task worktree (per Member in a Workspace).

```
Posse (merge gate)                                  Greenhouse daemon
  on publish, or on the done Signal of a
  Change Task that requires review:      
    grant = "<project>/t<id>/<member>/r<n>"   ->  attach {host}
                                              ->  apply {rules, reviewer}      (first round)
                                              ->  resolve-rules
                                              ->  prepare {intent, target sha}
                                              ->  review {grant}
                                              ->  watch ... until settled
    read host_result                          <-  {head, target, review_base, verdict,
                                                   current, findings[], digests, usage}
  accept only if current == true and head == the Task branch head
  insert Verdict row (insert-only)
  verdict has blocking findings:
    write findings file -> posse send --file to the author (one Fix Round)
    author commits, signals done            ->  new {host}  (successor run)
    next round with a new grant
```

Host identity is `{"name": "posse", "id": "<project>/t<id>/<member>"}`. `intent` is the Brief's `done_when` and title. `target` is the exact commit of the Task's target branch.

### 2.3 The Verdict record and the gate

One insert-only table, in the shape ADR 0020 gave review targets:

| Column | From |
|---|---|
| `task_id`, `repo` (the Change Task and, in a Workspace, the Member) | Posse |
| `provider` (`greenhouse` or `builtin`), `protocol` | Posse |
| `grant` (unique) | Posse issues it; Greenhouse refuses a reused grant |
| `head`, `target`, `review_base` | `host_result` |
| `verdict` (`clean`, `findings`, `inconclusive`) | `host_result` |
| `blocking` (count of findings at or above the frozen `block_at`; meaningful only when `verdict = findings`) | Posse computes |
| `rules_digest`, `config_digest`, `input_digest`, `backend`, `model` | `host_result` |
| `findings_digest`, findings file path | Posse |
| `mode` (`parallel` or `gate`), `recorded_at` | Posse |

Qualification is computed when read, as today: a Verdict passes only if all three hold: `mode = gate`; `verdict` is `clean`, or `verdict` is `findings` and `blocking = 0`; and `head` equals the head being published or merged now. An `inconclusive` Verdict never passes, whatever its `blocking` count, and neither does a missing one. No operation rebinds a Verdict to another head. A moved head has no passing Verdict.

Exact-head safety therefore has two independent checks: Greenhouse sets `current: false` "once the worktree branch or head moved" (`GH:docs/embedding.md`), and Posse compares the recorded `head` with the head being merged inside the merge transaction. Posse's check does not trust Greenhouse's.

### 2.4 Process placement (from an experiment)

[O] In an isolated run (appendix N1), `greenhouse axi attach` from a shell inside a Posse Task started the Greenhouse daemon with `setsid`, and the daemon stayed in the caller's Task cgroup and inherited its `POSSE_TASK_ID`. [O] Teardown stops cgroup members and processes that carry the Task marker (`docs/spec.md:634`).

[I] So a Greenhouse daemon first started from inside a Task is killed by that Task's Teardown while it may be reviewing for other Tasks. The report listed this as a hypothesis; the mechanism is now observed. The design rule:

- Only Posse's own Lead-side processes (the command or the Watch) call Greenhouse. A Worker never does, which also keeps the author from requesting its own verdict.
- Posse starts the daemon itself, outside every Task cgroup and without Task markers, with `GREENHOUSE_HOME=<POSSE_HOME>/greenhouse` (mode 0700, which Greenhouse requires: appendix N1). It never uses the User's own Greenhouse home, so a standalone Greenhouse session and Posse cannot own each other's runs.
- `posse down` stops that daemon after its work settles. `posse doctor` reports its protocol range.

### 2.5 Gaps on the Greenhouse side

These are requests to the Greenhouse Project. Posse does not implement them.

| Gap | Evidence | Needed before |
|---|---|---|
| Finding identity includes line numbers, so a fix that shifts lines produces a new id for the same root cause. No Greenhouse test shifts lines between rounds | [O] `f.ID = digest([]any{f.Origin, f.File, f.Start, f.End, f.Rule, ...})` (`GH:internal/engine/execution.go:563`), while the contract says "an ID is stable across rounds for the same root cause" | gate mode (repeat findings burn Fix Rounds) |
| Every round reviews the whole merge-base diff; Posse reviews only the delta after round 1 | [O] `GH:docs/embedding.md`; ADR 0024 | not blocking; measure cost and repeat rate in the parallel run |
| Host mode has no channel for the author's responses to earlier findings | [O] `pendingResponses` applies to Greenhouse's own fixer (`GH:internal/engine/execution.go`) | gate mode with more than one round |
| Multi-round host review has only been exercised with a fake reviewer and a fake host | [O] `TestEmbeddedHostOwnsDelivery` "drives a fake host through the daemon, AXI and terminal with ... a fake reviewer" (`GH:docs/embedding.md`) | gate mode; the parallel run is the first real use |
| No documented way to stop the daemon except SIGTERM to the recorded PID | [O] `GH:docs/compatibility.md` | default mode |
| Single repository | [O] `GH:SPEC.md:170` | Workspace Projects: Posse runs one host run per changed Member (Decision D6) |
| Findings must lie inside changed hunks, and `origin` is `correctness` or `policy` | [O] `GH:internal/engine/execution.go:548-563` | none; Posse's `introduced` or `pre_existing` distinction disappears because pre-existing code is out of range by construction |

### 2.6 Validation strategy and exit criteria

Four stages. Built-in review remains the gate until G2 passes for a Project, and remains available until G3 passes.

| Stage | What runs | Gate for Merge | Passes when |
|---|---|---|---|
| **G0 Contract** | Adapter against the real `greenhouse` binary with a fake reviewer, in CI with an isolated home and Herdr server | built-in | every case of the fault table below passes; the adapter survives a daemon restart and a protocol refusal |
| **G1 Parallel run** | For each head the built-in loop reviews, Posse also requests a Greenhouse Verdict and records it with `mode = parallel`. Its findings are stored and compared, not sent to the author | built-in | the parallel-run thresholds below hold over at least 50 reviewed heads in at least 2 Projects |
| **G2 Opt-in gate** | A Project sets `review_provider = "greenhouse"`. New Tasks freeze it. The built-in reviewer does not run for them | Greenhouse, in that Project | the gate thresholds below hold over at least 30 merged Deliveries, at least 10 of them with two or more rounds |
| **G3 Default** | Greenhouse is the default provider. Built-in is frozen: fixes only | Greenhouse | 4 weeks of use with no Verdict accepted for a wrong head and no return to built-in. Then removal may be scheduled (2.8, R-1) |

Fault table for G0 (each is an automated test; the pass condition is "no passing Verdict is recorded for a head that was not reviewed"):

| Fault | Expected |
|---|---|
| Author commits while the review runs | `current: false`; no Verdict row qualifies; a new round starts for the new head |
| Head moves between the settled result and the insert | Posse's own head comparison refuses |
| Grant reused | Greenhouse refuses; Posse records no second Verdict |
| `posse` killed between `review` and reading the result | replay with the same `action_id` returns the receipt; exactly one reviewer invocation |
| Daemon killed mid-review | the run parks as recovery-required; Posse raises one Notice and issues no new grant until it is resolved |
| Reviewer quota pause | no Verdict; the same grant resumes; the Fix Round budget is not spent |
| Task Teardown while another Task's review runs | the other review completes (process placement rule, 2.4) |
| Daemon protocol range excludes version 2 | Posse refuses to request a Verdict and names both ranges; Merge stays blocked, never downgraded |
| `inconclusive` verdict, with zero findings listed | never satisfies the gate; `blocking = 0` alone is not a pass (2.3) |

Parallel-run thresholds (G1), measured per reviewed head from stored rows by one command (`posse stats` gains a review comparison):

For the initial shadow run, only Project `posse` has the built-in review loop. Project `greenhouse` has `defaults.review = none`, so its gated PR heads add reliability, inconclusive-share, wrong-head and latency data, but not recall. This measures the adapter without enabling built-in Review Tasks in `greenhouse`. G1 does not measure precision from findings disputed by the author and upheld by the User. Parallel findings never reach the author, so measure precision in G2.

| Measure | Threshold | Why |
|---|---|---|
| Verdicts recorded for a head other than the one requested | 0 | exact-head binding |
| Requests that settle with a verdict without manual action | at least 95% | reliability of the host path |
| `inconclusive` share | at most 10% | a gate that often cannot answer blocks delivery |
| Recall of blocking findings: built-in P0 or P1 findings that the author then fixed, also reported by Greenhouse on the same head | at least 90% | Greenhouse must not be a weaker gate |
| Greenhouse blocking findings the author disputes and the User upholds as false | Not measured in G1; measure in G2 | Parallel findings never reach the author |
| Median time from request to settled verdict | no more than 1.5 times the built-in Review Round | the User's waiting time |

Gate thresholds (G2), in addition:

| Measure | Threshold |
|---|---|
| Round 2 or later settles on the successor run for the repaired head | at least 95% of such rounds |
| Same root cause reported again after a fix that shifted its lines | at most 5% of findings in rounds after the first |
| Deliveries that reach the round cap | no higher than under built-in review in the same Project over the previous 30 Deliveries |
| Merges whose merged head lacks a qualifying Verdict | 0 |

If G1 fails on recall or G2 fails on finding identity, the fallback is the one the report names: keep Posse's ledger, and still detach it from the Task type by putting it behind the Verdict interface as the `builtin` provider.

### 2.7 What `full-pipeline` and the merge gate mean under J17 = (a)

[O] Today `full-pipeline` means a Private Candidate passes review, then `posse validate test`, `docs` and `lint`, before every publication (`CONTEXT.md`, Review Mode). [O] Those three stages record what the Worker claims it ran (report section G). [O] ADR 0028 already makes forge CI at the recorded head the verification for PR delivery and states that local delivery has no automated verification.

With Greenhouse as the reviewer the three modes mean:

| `defaults.review` | Evidence required | Checked |
|---|---|---|
| `none` | forge CI at the gated head, for `pr` mode | before Merge, as today |
| `quick` | a current passing Verdict for the head being merged, plus forge CI at that head for `pr` mode | before Merge, where `requireSelectedReviewForMerge` checks today (`internal/app/review_verdict.go:239`) |
| `full-pipeline` | a current passing Verdict for the head being published, then forge CI at that same head | before `posse publish` pushes, and again before Merge |

Consequences:

- **`posse validate test|docs|lint` leaves with built-in review.** Nothing replaces it inside Posse or Greenhouse. A Project that wants tests, docs checks and lint to gate a merge puts them in its forge workflows, where they run at the exact head and cannot be self-reported. No Greenhouse host-mode pipeline stage is added.
- **The Private Candidate leaves.** [O] Host mode reviews a local commit in the worktree and "Creates no private worktree, candidate ref or lease" (`GH:docs/embedding.md`). Posse refuses to push a head without a passing Verdict. "Nothing reaches the target before validation" becomes one comparison of two commit ids. The order under `full-pipeline` is review, publish, CI, Merge: review is the only evidence available before a push, and CI is the only evidence about tests, docs and lint.
- **The merge gate is one conjunction** for a Task that requires review: the PR head equals `gated_sha`, a passing Verdict exists for that head, forge CI is green at that head, and Merge is authorized by `autonomy.merge` or the User. The first, third and fourth conditions exist today. Only the source of the second changes.
- **`local` merge mode** has the Verdict and no CI. That is today's guarantee for `local` and is stated to the User at admission, not hidden.
- **Documentation review** that `full-pipeline` did through `posse validate docs` is covered by Greenhouse rules ([O] the embedding example selects the rules `unslop` and `no-comments`) and by any docs job in CI.

### 2.8 Steps

Every step ships alone, leaves `main` releasable and names how to undo it. Admitted Tasks keep the review provider frozen in their plan, so a config change never moves a running Task between reviewers.

| Step | Change | Ships | Undo |
|---|---|---|---|
| G-1 | Verdict table (insert-only) and provider interface. The built-in reviewer writes through it as provider `builtin` beside its existing rows | additive migration; no behaviour change | stop writing; the table stays unused |
| G-2 | Greenhouse adapter, Posse-owned daemon placement, `posse doctor` check. Reachable only by a hidden command | stage G0 tests in CI | remove the command; no state depends on it |
| G-3 | Parallel run: `review_parallel = "greenhouse"` per Project | stage G1 | unset the key |
| G-4 | Opt-in gate: `review_provider = "greenhouse"` per Project, frozen per Task. Such a Task follows 2.7: no Review Task, no Private Candidate, no `posse validate` stage | stage G2 | unset the key; admitted Tasks finish under their frozen provider |
| G-5 | Greenhouse default for new Projects; built-in frozen | stage G3 | set `review_provider = "builtin"` |

Removal, only after G-5 passes. It removes behaviour, never data:

| Step | Change | Undo |
|---|---|---|
| R-1 | Stop admitting Review Tasks; remove `posse review ...`, `posse validate ...`, candidate commands and the built-in provider's write paths. Readers and tables stay. Removed commands fail with an error naming the replacement, as removed commands do today | release revert; no data was dropped |

The 16 review and validation tables stay as read-only history. This migration contains no step that drops them, so every step above can be undone. Dropping them later is a separate proposal outside this design; it would need a lossless export and a tested restore before it could be considered (D7 in section 11).

Acceptance for this phase: AC-D2 to AC-D5 and AC-G2 in section 10, and the fault table and thresholds of 2.6. Historical Review Tasks stay readable under the rules of section 9.

---

## 3. Challenge to the three Task properties

The report proposes three independent properties: the Task writes to the Project (W), a Report is required (R), the Task has a Delivery (D). The User asked whether this trades one rigid taxonomy for a combination of flags. It does, for the reasons below.

### 3.1 The eight combinations

| # | W | R | D | Is it an assignment someone gives? | Evidence |
|---|---|---|---|---|---|
| 1 | no | no | no | Yes, rarely: "check whether X still fails", an operational errand. The answer fits in the Signal note | [O] `posse signal done --no-change` exists for a Change Task that changes nothing (`docs/spec.md:634`) |
| 2 | no | yes | no | Yes: research, review, a study | [O] 50% of real Tasks (9% research, 41% review; report section I) |
| 3 | no | no | yes | No. A Delivery with nothing written delivers nothing | contradiction |
| 4 | no | yes | yes | No, same contradiction | contradiction |
| 5 | yes | no | no | No. Work that is neither delivered nor reported is discarded. That is an end state, reached through an approved discard, not an assignment | [O] "A Sandbox is never released while its Task holds unmerged work, except through an approved discard" (`docs/spec.md:634`) |
| 6 | yes | yes | no | Yes: a prototype, an image set kept as attachments | [O] real image Tasks ran as `research` (report section E, cause 4). To Posse this is identical to row 2: see 3.2 |
| 7 | yes | no | yes | Yes: a change | [O] 50% of real Tasks |
| 8 | yes | yes | yes | Rarely: a change plus a write-up. The Report repeats the PR description or the committed document | [O] the reassessment itself: a report delivered as a PR |

Two rows are contradictions and one is an end state. Five are real, and two of those five (2 and 6) are the same thing to Posse.

### 3.2 Why "writes to the Project" is not a property

- **It follows from Delivery.** D implies W (rows 3 and 4). Given D, W carries no information.
- **Without Delivery it has no effect on the Project.** Every Worker writes in its Sandbox: build output, scratch files, generated images. [O] A Research Task today may edit and even commit, and Teardown preserves those files beside the Report (`docs/spec.md:751`). The Project changes only when a Delivery merges.
- **It cannot be enforced.** [O] `internal/app/guard.go` has no rule keyed on research. Nothing stops a research Worker from writing in its worktree. A property that Posse cannot check violates the report's own principle 6, "Enforce only what Posse can verify".
- **It cannot be declared truthfully for exploration.** Scenario F in the report is an assignment whose answer to "will this write?" is unknown at start.

What Posse can observe is narrower and exact: at `done`, does the Task branch differ from its base (commits or a dirty worktree)? This document calls that observed fact **Work**. Nobody declares it.

### 3.3 Why "has a Delivery" is not a property of the assignment

The User's principle 4 says Delivery is "optional and independent from the underlying Worker assignment". A flag set at start contradicts that: the Lead would still classify the Task before it runs. Delivery is an act. A Worker with Work runs `posse publish`, and that opens a Delivery record. Whether a Task has a Delivery is history, not configuration.

### 3.4 What remains: one requirement, one axis

After removing W (derived, unenforceable) and D (an act), one question is left that Posse can check mechanically and cheaply: **what must exist when the Worker says `done`?**

| `outcome` in the Brief | `done` is accepted when | `posse publish` | Typical assignment |
|---|---|---|---|
| absent (stored as `open`) | a Report, Work, or both exist; with neither, the Worker passes `--no-change` and the note is the result | allowed when Work exists | anything; exploration; mixed work |
| `report` | a Report exists | refused | research, a study, an image set as attachments |
| `change` | Work exists, or `--no-change` | allowed | a code or document change |

This is the report's own answer to its challenge ("the Lead states a property only to restrict"), reduced from three flags to one optional key.

**The field already exists.** [O] Every admitted plan freezes `outcome`, `change` or `report`, derived from the type (`internal/taskplan/plan.go:170`, `:375-381`; `docs/spec.md:691`). [O] 27 non-test lines read it, against 174 that branch on the type (the report counted 177 before #973). The design promotes an existing field, adds the value `open`, and retires the field it was derived from.

### 3.5 Is this the old taxonomy under a new name?

`report` and `change` resemble `research` and `change`, and that is evidence that those two are real assignments. Three things differ, and they are the three the report names as the cause of friction (D3):

| What the type decides today | What decides it in the target model |
|---|---|
| Whether the Lead must classify before starting | Nothing. `outcome` is optional and `open` is the default |
| Completion rule and state path (`done` becomes `reported` for non-change Tasks) | `outcome` constrains the `done` check only. Every Task ends in `done` |
| Skill set | Skill sets chosen by the Lead, required at reached steps (section 6) |
| Which Dispatch Rules apply | Profile purpose and the #973 precedence (section 5.4) |
| Whether delivery and review text and state apply | Whether the Task opened a Delivery (section 4.3) |

`review` is not a value. A review is a relation between a verdict and a Delivery's head (section 2), not a kind of assignment.

A restriction can be lifted. If a `report` Task turns into a change, the Lead revises the requirement to `open` in the same Sandbox. [O] `task_plan_revisions` already records plan revisions with a digest chain (`internal/taskplan/plan.go:167-168`). This is the report's J13 with no new mechanism.

### 3.6 Alternatives considered

| Alternative | Why not |
|---|---|
| Three boolean properties (the report's proposal) | Eight combinations, two contradictory, one unenforceable flag, and the Lead decides three things instead of one |
| No declaration at all; Posse only observes | The Lead loses two cheap checks that exist today: `done` without the required Report is refused (report E4.1), and a research Worker cannot push. With `autonomy.merge` on, an unrequested PR could merge |
| Keep `type`, make it optional (report J10 alone) | Leaves the 174 branch sites and the coupling of skills, rules, state path and pipeline to one value |

---

## 4. Domain model

### 4.1 Concepts

| Concept | Definition in the target model | Change from today |
|---|---|---|
| Project, Lead, Worker, Sandbox, Pool, Signal, Notice, Decision, Autonomy, Teardown, Restore | unchanged | none |
| **Task** | One assignment with a stopping condition, carried out by one Worker in one Sandbox | no type; `title` generated when absent |
| **Brief** | The assignment text and `done_when`. Optional keys only restrict or select: `outcome`, `skills`, `after`, `base`, `target`, budget and Delivery policy keys | `type` no longer required; legacy keys stay readable (section 9) |
| **Outcome** | The optional requirement on `done`: `open`, `report` or `change` | replaces Change Task, Research Task and Review Task as kinds of Task |
| **Work** | The observed fact that the Task branch differs from its base | new name for what `docs/spec.md:634` calls "unmerged work" |
| **Report** | The written result and attachments, sealed as a Report Bundle at Teardown | available to every Task, required only under `outcome: report` |
| **Delivery** | The record of integrating a Task's Work: PR or local merge, CI observations, merge authorization, queue position, verdict requirement. Opened by `posse publish`. Has its own states | gains states and the columns that sit on `tasks` today; covers local merge, not only PRs |
| **Verdict** | An insert-only record that a named reviewer judged one exact head against one target | replaces Review Task, Review Verdict, Review Round, Finding Ledger, Finding Pattern, Private Candidate as product concepts |
| **Profile** | A named Worker configuration with a purpose | gains `description`, `only_for`, `skills` (section 5) |
| **Harness adapter** (config key `kinds`) | How Posse drives one agent CLI | leaves the product vocabulary (section 5) |
| **Skill set** | A User-defined, named list of skills | replaces "workflow skills per role" (section 6) |

Concepts that leave the product vocabulary: Change Task, Research Task, Review Task, Review Mode, Review Loop, Review Round, Finding Pattern, Finding Ledger, Private Candidate, Recommended skill set per role. Fix Round stays, as a Delivery concept. `CONTEXT.md` has 58 terms today (report section A); this removes 10 and adds 4 (Outcome, Work, Verdict, Skill set).

### 4.2 Task states

| Target Task state | Meaning |
|---|---|
| `queued`, `spawning`, `working`, `needs-decision`, `blocked`, `stalled` | unchanged |
| `done` | the Worker finished and the Outcome requirement held |
| `failed`, `lost`, `torn-down` | unchanged |

Ten states instead of thirteen. [O] `merging` and `merged` are entered only by merge and PR code, and `reported` exists because non-change Tasks skip them (report section F). They describe a Delivery or a saved Report, so they move there.

A Task is **settled** when it is `done` and holds no unresolved Work: its Report is saved, and its Work is merged, discarded with approval, or absent. Settled is derived, not stored. It is the condition for Teardown and for `after:` dependencies, which today read `merged` or `reported`.

### 4.3 Delivery states

```
(no Delivery) --posse publish--> open --authorized--> merging --> merged
                                   |                                 
                                   +--closed (PR closed or discard approved)
```

- `open`: published. For `pr` mode a PR exists; for `local` mode the branch is ready. CI observation, Fix Rounds, the merge queue and the verdict requirement attach here.
- `merging`, `merged`: what the Task states of the same name mean today, per Delivery (per Member in a Workspace).
- `closed`: the forge reports the PR closed, or the User approved a discard.

A Task shows as `done` with a result line: `report: saved`, `delivery: open|merging|merged|closed`, or `note only`. The Lead reads one Task state and, only for a Task that delivered, one Delivery state.

### 4.4 Verdict requirement on a Delivery

`defaults.review` keeps its three values with the meaning of section 2.7. In the target model the requirement and its Verdict rows hang off the Delivery, so no review concept touches the Task: a Task that never publishes has no review state at all.

### 4.5 Entity sketch

```
Project 1--* Task 1--1 Plan (frozen: outcome, skills, Profile, Delivery policy)
                  1--* Launch (Profile, harness adapter, session)
                  1--0..1 Report Bundle
                  1--* Delivery (one per Member)  1--* Verdict (insert-only, per head)
                                                  1--* Fix Round
                                                  1--* CI observation
```

Everything left of `Delivery` is the core. A Task that never publishes creates no row to the right of it.

---

## 5. Kind and Profile

### 5.1 Decision

They remain two mechanisms. Only Profile remains a concept that the User and the Lead work with. Kind is renamed "harness adapter" in prose and output, keeps its config key `kinds`, and is removed from the Lead's instructions and from Task output except inside `posse config`.

### 5.2 Evidence that the responsibilities are distinct

| Test | Kind | Profile | Source |
|---|---|---|---|
| What question it answers | How does Posse launch, resume, steer, interrupt and notify this CLI? | Which model at which effort does the User want, and what continues its Tasks when quota runs out? | [O] `internal/config/config.go:141-178` |
| Fields | 13, all adapter mechanics (`resume_args`, `notice_delivery`, `steer`, `interrupt_keys`, ...) | 6 (`kind`, `model`, `effort`, `args`, `attribution`, `fallback`) | [O] same |
| Shared fields | none | none | [O] same |
| Owner | Posse; built-in defaults cover the supported harnesses | the User | [O] report section E |
| Cardinality in the User's real config | 3 in use | 12 (6 on `pi`, 4 on `claude`, 2 on `codex`) | [O] `grep` over `~/.posse/config.toml` |
| How often the User edits it | 2 argument templates in total, in 2 `[kinds.*]` sections | 12 sections | [O] same |
| When it changes | when a harness CLI changes; ships in a Posse release | when the User adds a model or changes a preference | [I] |
| Is it in the glossary? | No. `CONTEXT.md` defines Profile and has no entry for Kind | Yes | [O] `CONTEXT.md` |

Merging them would copy 13 adapter fields into each of 12 Profiles, and a harness CLI change would become 6 config edits by the User instead of one Posse release. Removing Profile (report option E-e) loses named fallback chains and the stable name the User wants for "the image worker". So the split is real [I].

### 5.3 Why it is still not understandable today, and the fix

The User's doubt is justified for a different reason: the word "kind" leaks. It appears in `posse dispatch` output, in `profile_required` help, in the Lead instructions and in `lead.profiles.<kind>`. A reader meets two nouns where only one is a choice.

- Lead instructions, `posse dispatch`, `posse show` and Notices name the Profile only. The harness appears as an attribute of the Profile (`harness: codex`), never as something to select.
- `posse config` keeps `[kinds.<name>]` and labels the section "harness adapters: change only when a harness CLI changes its flags".
- The implicit Profile stays: [O] an unconfigured home resolves a Worker to the Lead's harness with its defaults (`CONTEXT.md`, Profile). It is displayed as `Profile: <harness> (built-in)`.

Acceptance: a transcript of the Lead starting any Worker contains no occurrence of "kind" (section 10, AC-K1).

### 5.4 What a Profile gains

```toml
[profiles.codex-sol]
  kind = "codex"
  model = "gpt-6.1-sol"
  description = "Image generation. Not for code, research or review."
  only_for = "images"          # reserved: needs a matching reason at start
  skills = ["image-brief"]     # load whenever this Profile runs
```

- `description` is shown to the Lead beside every candidate. Purpose moves from a rule's `when` prose onto the thing selected (report section E, "what makes capability-based dispatch hard", item 1).
- `only_for` reserves the Profile. Posse refuses it for any automatic selection and for a start without `--because "<reason>"`. The reason is stored and shown. Posse does not judge whether the reason is true; it makes the choice explicit and auditable (report E-b plus E-c).
- `skills` lets know-how travel with capability (report J9, cause 5).

Selection keeps the #973 precedence unchanged (`dispatch.Precedence`, `internal/dispatch/resolve.go`). Two adaptations follow from the Task model:

- A Dispatch Rule's `type` is read as an Outcome scope: `change` matches `outcome: change`, `research` matches `outcome: report`. An `open` Task matches rules with no `type`. No config edit is needed.
- `type = "review"` rules and `review_loop.profile` apply only to the built-in reviewer and leave with it.

---

## 6. Skill selection

### 6.1 Constraints taken as given

- [O] "A role skill listed in `[skills]` is required. ... There is no setting to relax this." (`docs/skills.md`). The User chose enforcement after advice and a warning did not change Worker behaviour (report D2, commit `6bd85306`). This design keeps "selected means required" and fixes fit instead.
- [O] ADR 0036 removed Lead skills from Workers because they "cost context, invited misfires and lengthened launch commands". A per-Task channel must not reopen that.
- [O] Making a skill available costs its name and description; the body loads on use. The cost that hurts is the mandatory invocation of a skill that does not fit (report D2: `posse-tdd` on a docs-only Task).

### 6.2 Model

Three User-controlled boundaries and one Lead choice.

```toml
[skills]
  lead    = ["posse-research", "posse-build"]          # unchanged
  default = "auto"                                     # set used when a Brief names none
  allowed = ["diagram-design", "image-brief"]          # extra skills the Lead may add

  [skills.sets]
  change   = ["posse-tdd", "posse-deslop", "posse-no-comments", "posse-technical-writing", "posse-unslop"]
  research = ["posse-research", "posse-unslop"]
  docs     = ["posse-technical-writing", "posse-unslop"]
```

| Layer | Who writes it | What it does |
|---|---|---|
| Skill sets | User (`[skills.sets]`, User-only key) | Names lists the Lead may select. The Lead never edits a set |
| Allowlist | User (`[skills].allowed`, User-only key) | Individual skills the Lead may add to one Task |
| Profile skills | User (`profiles.<name>.skills`) | Added whenever that Profile runs |
| Brief `skills:` | Lead | Names at most one set and any allowlisted skills, for example `skills: docs, diagram-design`. A name outside the sets and the allowlist is refused at `posse dispatch` |

Resolution at admission, frozen in the plan and printed by `posse dispatch` and `posse show`:

1. The Worker platform skill `posse`. The Lead-only and User-only platform skills (`posse-setup`, `posse-down`, `posse-update`) are not loaded into Workers (report J8).
2. The set the Brief names. Without one: the set for the stated Outcome (`change` for `outcome: change`, `research` for `outcome: report`). For an `open` Task under `default = "auto"`, both sets are made available and each is required only if its step is reached (6.3).
3. Allowlisted skills from the Brief.
4. The Profile's skills.

**Defaults need no config edit.** Today's `[skills] change = [...]` and `research = [...]` are read as the sets `change` and `research`. `[skills] review` is used by the built-in reviewer until it leaves.

### 6.3 A selected skill is required at the step the Task reaches

Each skill has a step. [O] The step map already exists in code (`internal/app/skill_uses.go:28-36`). The design moves it into the skill's own metadata and limits steps to three moments Posse can observe:

| Step | Reached when | Checked at |
|---|---|---|
| `work` | the Task has Work | `posse publish`, and `done` with Work |
| `report` | `done` carries a Report | `done` |
| `always` | every Task | `done` |

A Task that ends with a Report and no Work is not asked for `posse-tdd`. A docs-only change gets `skills: docs` from the Lead and is asked for two skills instead of five. Enforcement stays exactly as strict as today for every skill whose step is reached, and it applies only where the harness records use ([O] Codex uses are not recorded, `docs/skills.md`); that gap is unchanged by this design.

### 6.4 What bounds the Lead

- It cannot load a skill the User did not list in a set, the allowlist or a Profile.
- It cannot remove a skill from a set. It can only choose another User-defined set.
- Every choice is visible before start (`posse dispatch`) and after (`posse show`).

The report's doubt that a per-Task list "could reintroduce the problem ADR 0036 solved" is answered by the allowlist and checked by AC-S3 (section 10).

---

## 7. Target architecture

### 7.1 Layers and dependency rule

```
                +--------------------------------------------------+
  User <-> Lead |  Lead contract (core section; sections on demand) |
                +--------------------------------------------------+
                                     |
        +----------------------------v-----------------------------+
        |  CORE: dispatch and supervision                          |
        |  Project, Task, Brief, Plan, Outcome check at done       |
        |  Profile resolution, skills resolution, launch           |
        |  Sandbox, Pool, containment, guard, verified Teardown    |
        |  Signals, turn tokens, messages, Notices, Watch          |
        |  Decisions, Autonomy, Reports                            |
        +----------------------------^-----------------------------+
                                     | imports core, never the reverse
        +----------------------------+-----------------------------+
        |  DELIVERY attachment (exists only for Tasks that publish)|
        |  publish, PR and local merge, CI loop and Fix Rounds,    |
        |  merge authorization, merge queue, stewardship           |
        |  Verdict gate: "current passing Verdict for this head"   |
        +----------------------------^-----------------------------+
                                     | interface: request, read, qualify
        +----------------------------+-----------------------------+
        |  VERDICT providers                                       |
        |  greenhouse (host mode)  |  builtin (legacy, transition) |
        +----------------------------------------------------------+
```

Rules:

1. Core code does not import Delivery code and never reads the Outcome to choose a pipeline. It reads it in one place, the `done` check.
2. Delivery subscribes to two core events: `publish` requested and Task `done` with Work. It owns every row and Notice about PRs, CI, merge and Verdicts.
3. The Verdict gate is an interface with two implementations during the transition. Removing built-in review deletes one implementation, not a branch at every merge site.
4. Text follows the same layering. The Worker protocol and the Lead contract are assembled from the core section plus the sections of attachments the Task or Project uses.

[O] Today all of this is one package, `internal/app`, with 65,581 non-test lines (report section A). The target splits it along rule 1. Package names are an implementation detail; the measurable property is the import direction (AC-A2).

### 7.2 One `done` path

```
posse signal done "<note>" [--report f] [--attach p] [--no-change] --turn <token>
  1. turn token current, no undelivered Lead instruction       (unchanged)
  2. observe: Report given? Work present?
  3. check Outcome: open | report | change                     (section 3.4)
  4. check skills whose step was reached                       (section 6.3)
  5. one transaction: Signal + state done + Notice task_done
  6. if Work and a Delivery exists: Delivery handles the rest
```

[O] Today step 5 has a special case: a non-change Task passes through `done` and becomes `reported` in the same transaction (`internal/store/store.go:2177-2186`). That case and the `--report`-by-type rule both become step 3.

### 7.3 Generated text

| Text | Contains by default | Added when |
|---|---|---|
| Worker protocol | isolation, Signals, scratch, the Outcome requirement, selected skills with their steps | one line "to deliver changes, commit and run `posse publish`; `posse publish --help` has the rules" unless `outcome: report`. The full delivery and verdict text is printed by `posse publish` and its `help[]` lines, when the Worker gets there |
| Lead contract | dispatch, supervision, Decisions, authority | a Delivery section, printed by `posse lead delivery` and named in the first Delivery Notice of the session |

[O] The CLI already works this way: "the `help[]` lines at the end of every output name the next step" (platform skill `posse`). The design applies it to the two largest texts. Targets are in section 10.

Project-specific advice in the generic protocol (Herdr experiment isolation, Go caches; report D6) moves to that Project's Worker preferences, which already exist.

### 7.4 What is deliberately not in this design

These report items are independent of the Task model and keep their own priority: one Notice queue (J20), one owner for Restore (J21), Herdr upstream requests (J22), plan inputs by digest (J24), snapshot slimming (J25), retention (J26), capacity defaults (J12), research child limits (D7). Workflow profiles, Frames and Stages are unchanged; their `task_type` field is read as an Outcome alias like any Brief.

---

## 8. Phase 2: Task model migration

Phase 1 is section 2.8. This phase changes the Task model. Every step ships alone, leaves `main` releasable, keeps historical Tasks readable, and names how to undo it. Nothing here has started.

Two mechanisms carry the migration:

- **Plan version as the switch.** [O] A Task runs under the plan it was admitted with, and older plan versions stay readable with their frozen behaviour (`internal/taskplan/plan.go:385-414`, ADR 0024). New behaviour applies to new plan versions only. In-flight Tasks are never converted.
- **One accessor before any removal.** The 174 type branches are first routed through one small set of questions on the plan (`RequiresReport`, `MayPublish`, `IsLegacyReview`). Behaviour does not change in that step. Later steps change the answers in one place.

### Track T: Task model

| Step | Change | Ships | Undo |
|---|---|---|---|
| T-1 | Route every type branch through the plan accessor. Add a test that fails on a new `Type ==` branch outside the compatibility package | no behaviour change; the full E2E suite is the proof | revert the commit |
| T-2 | `outcome:` accepted in Briefs; `type:` becomes an alias and stops being required; plan gains `open`. One `done` check by Outcome | a Brief with a body and `done_when` starts a Worker | old Briefs keep working unchanged; stop writing new-version plans by a release revert while none is live, otherwise let live ones finish first |
| T-3 | Skill sets, allowlist, Brief `skills:`, Profile `skills`, step-bound requirement. Role keys read as sets | AC-S1 to AC-S3 | config keys are additive; unset them |
| T-4 | Profile `description` and `only_for`; `--because` stored and shown; harness wording in output | AC-K1, AC-P1 | additive keys; unset them |
| T-5 | Delivery states. New-version Tasks end in `done`; `merging` and `merged` are written on the Delivery; `reported` is no longer entered. Columns `pr_url`, `gated_sha`, `landed_ref`, `target_branch`, `pr_title` are written to the Delivery and kept mirrored on `tasks` for one release | AC-M2, AC-H1 | the mirror makes the previous release able to read new Tasks; revert within that release |
| T-6 | Generated Worker protocol and Lead contract by section | AC-X1 to AC-X3 | text only; revert |
| T-7 | `settled` replaces `merged or reported` for `after:` dependencies and Teardown | existing queue E2E | revert |

T-5 is the riskiest step: it touches the `done` and merge transactions. It starts with a spike that runs the store's concurrency tests ([O] `busy_repro_test.go`) against the new transitions before any production change.

### Order across both phases

```
Phase 1:  G-1 -> G-2 -> G-3 -> G-4 -> G-5 ------------------> R-1
Phase 2:          T-1 -> T-2 -> T-3 -> T-4
                           \-> T-5 -> T-6 -> T-7
```

T-1 starts after G-1 has merged, so that the provider interface exists before the accessor is introduced. After that, phase 2 never waits for Greenhouse to pass a stage, and phase 1 never waits for the Task model: the Verdict gate keys on a Task and a head, which exist in both models. At T-5 the Verdict rows gain a Delivery reference; the Task reference stays.

---

## 9. Compatibility for historical Tasks

[O] The real home has 1,335 Tasks, 549 of them Review Tasks, admitted under plan versions 1 to 7 and, before migration 43, under no plan at all (report sections A and F).

### 9.1 Rules

1. **Rows are never rewritten.** No migration updates `tasks.type`, a stored state, a plan or a snapshot of a finished Task.
2. **Readers map, writers move on.** One compatibility package holds every mapping below. The test from T-1 keeps legacy branches from spreading.
3. **A Task finishes under the plan it was admitted with.** A Task that is live at upgrade keeps its type, its state path and its review provider.
4. **Removal keeps reading.** R-1 removes commands and write paths. The review and validation tables and their readers stay, so a historical Review Task renders the same after R-1 as before.

### 9.2 Mappings

| Historical value | Read as | Displayed as |
|---|---|---|
| `type = change`, `ship` | `outcome: change` | `outcome: change` |
| `type = research`, `scout` | `outcome: report` | `outcome: report` |
| `type = review` | `outcome: report`, legacy review of Task N | `review of t<N> (built-in, legacy)` |
| no plan (before migration 43) | Outcome from `tasks.type` | as above |
| state `reported` | `done`, Report saved | `done (reported)` |
| state `merged` | `done`, Delivery `merged` | `done (merged)` |
| state `merging` on a live legacy Task | unchanged; it completes on the legacy path | `merging` |
| Review Verdict, Finding Ledger rows | read-only history | under `posse show <task> --full`, labelled built-in |
| Private Candidate rows | read-only history | same |
| `review_evidence: legacy-evidence` (version 1 plans) | unchanged | unchanged |

`posse list --json` and `posse show --json` keep the fields `type` and the historical `state` value for historical Tasks, and add `outcome` and `delivery`. A consumer that reads the old fields sees no change.

### 9.3 Config, Briefs and commands

| Input | Compatibility |
|---|---|
| Brief `type:` | accepted indefinitely as an alias for `outcome:`. `type: review` and `review_of:` are accepted until R-1 |
| `[skills] change`, `research` | read as skill sets of the same names; no edit needed |
| `[[dispatch]] type = ...` | read as an Outcome scope (section 5.4) |
| `defaults.review`, `review_loop.max_rounds`, `block_at` | kept; they configure the Delivery's Verdict requirement and Fix Round budget |
| `review_loop.profile`, `auto_start`, `[skills] review`, `type = "review"` rules | used by the built-in provider; at R-1 they fail with `config_migration_required` naming `review_provider`, as the removed `no-mistakes` mode does today ([O] `CONTEXT.md`, Merge Mode) |
| Removed commands | fail with an error that names the replacement |

### 9.4 Proof

- **Fixture home.** A synthetic `POSSE_HOME` holding Tasks of every plan version, every terminal state and every review mode, built from the existing migration fixtures and containing no real Project data. For every step in sections 2.8 and 8, `posse list --json`, `posse show <task> --json` and `posse stats --json` over that home are compared before and after; legacy fields must be identical.
- **Real history check.** A read-only command run by the User on a backup copy of the real database renders every Task and reports failures. It is a release gate for T-2, T-5 and R-1.
- **Live Task check.** An E2E upgrades the binary while one Change Task waits in `merging` and one Review Round runs, and both finish on their frozen path.

---

## 10. Acceptance criteria

Each criterion is a test or a measurement with a threshold. None counts lines, tables or files. "Today" values are from the report unless marked.

### Product model

| Id | Criterion | Today | Target |
|---|---|---|---|
| AC-M1 | A Brief with only a body and `done_when` starts a Worker on a fresh home | refused: `type: is required` | accepted |
| AC-M2 | Task states a Lead can observe on a new Task | 13 | 10 |
| AC-M3 | Concepts the Lead names in a transcript to start one research Worker | 10 | at most 6 (Brief, `done_when`, Profile, Signal, Notice, Report) |
| AC-M4 | An exploratory Task that produces Work is published from the same Sandbox, with no second Task | impossible for a Research Task | passes (report V-F) |
| AC-M5 | A `report` Task whose Worker runs `posse publish` | n/a | refused, and nothing is pushed |
| AC-M6 | `done` without the required Report or Work | refused by type | refused by Outcome with the same error class |
| AC-M7 | Glossary terms in `CONTEXT.md` | 58 | at most 52 |

### Text the agents must read

| Id | Criterion | Today | Target |
|---|---|---|---|
| AC-X1 | Words of system text in the Worker protocol of a 14-word research assignment on a fresh home | 686 | at most 300 |
| AC-X2 | Occurrences of `PR`, `merge`, `review`, `candidate`, `validate`, `CI` in the protocol of an `outcome: report` Task | several ("this Change Task", "Do not wait for PR CI") | 0 |
| AC-X3 | Lead obligation words loaded at session start in a Project that has opened no Delivery | 2,530 | at most 1,300 |

### Skills, Profiles, harness

| Id | Criterion | Today | Target |
|---|---|---|---|
| AC-S1 | A docs-only change with `skills: docs` signals `done` without invoking a test-first skill | refused (`role_skills_unused`) | accepted |
| AC-S2 | The Lead adds one allowlisted skill to one Task; `posse show` lists it; the next Task does not have it | impossible | passes |
| AC-S3 | A Brief naming a skill outside the sets and the allowlist | n/a | refused at `posse dispatch` |
| AC-S4 | A Task ending with only a Report, started with no `outcome` | n/a | not asked for any `work`-step skill |
| AC-P1 | Share of new Tasks whose Profile choice has a recorded source and reason | 0% | 100% |
| AC-P2 | The report's V-D1 and V-D2 with `codex-sol` reserved for images | fails | 5 of 5 image Briefs on the reserved Profile, 0 of 15 others, three runs |
| AC-K1 | Occurrences of "kind" in the Lead contract and in `posse dispatch`, `posse start`, `posse show` output | present | 0 |

### Delivery and review

| Id | Criterion | Target |
|---|---|---|
| AC-D1 | A Task that never publishes creates no Delivery, Verdict, CI or Fix Round row | passes |
| AC-D2 | A change merges with a Greenhouse Verdict bound to the merged head, and the Lead ran no review command (report V-C) | passes |
| AC-D3 | Fault table of section 2.6 | every row passes |
| AC-D4 | Parallel-run and gate thresholds of section 2.6 | all hold before G-4 and G-5 respectively |
| AC-D5 | Under `full-pipeline`, a head without a passing Verdict | is not pushed |

### Safety and history

| Id | Criterion | Target |
|---|---|---|
| AC-G1 | Existing E2E for isolation, Teardown, merge authority and Restore (for example `TestWorkerGuardAgainstIsolatedHerdr`, `TestTeardownNeverClosesAReplacementAtThePaneCloseBoundary`, `TestListHoldsRestartUntilUpAll`) | pass unchanged at every step |
| AC-G2 | Each built-in review test removed at R-1 | has a Verdict-gate test that asserts the same guarantee |
| AC-H1 | Fixture home output, legacy fields | identical before and after every step |
| AC-H2 | Real history check on a backup copy | 100% of Tasks render |
| AC-H3 | Binary upgrade with a live legacy Change Task and Review Round | both finish on their frozen path |
| AC-A1 | Type branches outside the compatibility package | 0 after T-2 (174 today [O]) |
| AC-A2 | Imports from core packages to Delivery or Verdict packages | 0 |
| AC-R1 | Each step of sections 2.8 and 8 has a rehearsed undo on the fixture home, including R-1 by a release revert that finds every review and validation row intact | passes |

AC-A1 and AC-A2 measure the structure, not its size: they hold whether the code grows or shrinks.

---

## 11. Decisions

Decided already: Greenhouse first, parallel run before removal, and D2. The rest are open.

| Id | Decision | Recommendation |
|---|---|---|
| D1 | Adopt one optional `outcome` with `open` as the default, instead of three properties | Yes (section 3) |
| D2 | Who produces test, docs and lint evidence once review is external (report J17) | **Decided 2026-11-12: (a) forge CI only.** Designed in section 2.7 |
| D3 | Keep "a selected skill is required", bound to the step the Task reaches | Yes (section 6.3). It preserves the 2026-11-02 enforcement decision and removes the misfit |
| D4 | The thresholds of section 2.6 | As written; they can be tightened after G1 produces a baseline |
| D5 | Review of publication-bound Reports (spec, tickets). Greenhouse reviews committed diffs only | A spec or ticket set that needs independent review is committed as a document and reviewed as a Delivery. Report Acceptance by exact digest stays |
| D6 | Workspace Projects | One host run per changed Member. Cross-Member context is lost; measure in G1 before choosing G-4 for a Workspace |
| D7 | Whether to ever drop the review and validation tables | Not part of this design. They stay as read-only history. A later, separate proposal must bring a lossless export and a tested restore path |
| D8 | Rename the config key `kinds` to `harnesses` | No. The key is User-only and rarely edited; a rename costs a config migration and buys nothing once the word leaves output |

---

## 12. Risks and what would falsify this design

| Risk | Mitigation |
|---|---|
| Greenhouse is days old and replaces a heavily tested subsystem | Parallel run before gate, opt-in before default, measured exit criteria, built-in kept until G3 |
| `open` Tasks make results less predictable for the Lead | The Lead states `outcome` whenever it knows. Measure the share of `open` Tasks that end with neither Report nor Work; above 10% means the default is wrong |
| A period with two review paths doubles the code that can break | The provider interface (G-1) makes them two implementations of one gate; the Task model never sees which one ran |
| T-5 moves merge state under concurrency | Spike first; one-release column mirror; AC-H3 |
| Skill sets become a second taxonomy | Sets are User-defined names, not a closed list, and nothing but the skill list keys on them |
| Reviewer runs outside Posse's quota tracking and Profiles | `host_result.usage` is stored per Verdict; reviewer model is User config passed in `apply` |

The design is wrong if:

- G1 recall stays under 90% after Greenhouse rule tuning. Then review stays in Posse behind the Verdict interface, and tracks T still stand.
- AC-X1 cannot be met without dropping a safety instruction. Then the protocol weight is necessary and the saving must come from skills and Brief length.
- More than half of new Briefs state `outcome` anyway and no `open` Task ever changes course. Then `open` buys nothing and the default should be `change`, which is a one-line change.
- AC-P2 does not improve with Profile purpose. Then routing needs a deterministic classifier, as the report says.

---

## Appendix: new observations

Observations made for this document, beyond the report.

**N1. Greenhouse host attach from inside a Posse Task** [O]. Isolated `GREENHOUSE_HOME` under the Task scratch directory, a throwaway Git repository, the installed `greenhouse` binary, no reviewer invoked.

- First attempt failed: `state home must be private (chmod 700)`.
- After `chmod 700`: `outcome: ok`, `protocol: {version 2, min 1, max 2}`, `run.host = {posse, exp/t1}`, `actions = [attach, inspect, apply, resolve-rules, prepare, new]`, and `blocked` names the host ownership: "the host owns author repair, candidate custody, publication, CI and cap decisions".
- The daemon ran as its own session leader (`setsid`), reparented away from the shell, in the same `posse-task-...scope` cgroup as the calling shell, with one `POSSE_TASK_ID` entry in its environment.
- It stopped on SIGTERM to the PID in `GREENHOUSE_HOME/pid`.

**N2. Finding identity** [O]. `GH:internal/engine/execution.go:563` hashes origin, file, start line, end line, rule and normalized explanation. A finding outside the changed hunks is rejected as invalid (`:548-560`).

**N3. Outcome already frozen** [O]. `internal/taskplan/plan.go:170` and `:375-381`; 27 non-test lines read `Outcome`; 174 non-test lines branch on the type (`grep -rnE '(Type|type) (==|!=) "(change|research|review)"|case "(change|research|review)"' internal`).

**N4. Research Tasks may write** [O]. `docs/spec.md:751`: edited files and commits beyond the Task base are preserved beside `report.md` at Teardown. `internal/app/guard.go` contains no research rule. `posse publish` is the only write path refused by type (`internal/app/publish.go:177`).

**N5. Kind and Profile in a real config** [O]. 12 `[profiles.*]` sections over 3 kinds (pi 6, claude 4, codex 2); 2 `[kinds.*]` sections setting `model_args` and `effort_args` once each. `CONTEXT.md` has no Kind entry.

**N6. Precedence after #973** [O]. `dispatch.Precedence` in `internal/dispatch/resolve.go` is the single definition; Review auto-start uses a Profile only when exactly one unconditional `review` Profile is eligible. This design changes neither.
