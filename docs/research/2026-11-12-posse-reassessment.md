# Posse reassessment: product model, architecture and developer experience

Date: 2026-11-12. Audited commit: `85e6a980` (`posse v0.33.1-dev`). Herdr 0.9.1. Firstmate at `47df77f3` (2026-11-11). Greenhouse at `c097a1d` (2026-11-11).

This report changes no production code. It answers one question from the request:

> Has Posse become too focused on controlling how work happens, at the expense of making it easy for the Lead to get the right work done by the right Worker?

## How to read this report

Every significant claim carries a label.

- **[O] observed**: read in code at the cited `file:line`, printed by a command, or produced by a test or experiment named in the text.
- **[I] interpretation**: a conclusion drawn from observed facts.
- **[H] hypothesis**: plausible, not verified. Each one names what would verify it.

Paths are relative to the Posse repository root unless prefixed. `FM:` means the Firstmate checkout and `GH:` means the Greenhouse checkout. Experiment ids such as `E2.4` refer to [Appendix A](#appendix-a-experiment-harness-and-raw-results). All experiments ran against an isolated Herdr server and an isolated `POSSE_HOME` with fake agent binaries. No real Posse state, Herdr session or Project was touched.

Private Project names and Task titles from the real `~/.posse` home are left out. Counts from that home are aggregates.

## Contents

- [A. Executive assessment](#a-executive-assessment)
- [B. Current architecture map](#b-current-architecture-map)
- [C. Firstmate comparison](#c-firstmate-comparison)
- [D. Product friction analysis](#d-product-friction-analysis)
- [E. Dispatch and capability analysis](#e-dispatch-and-capability-analysis)
- [F. Persistence and Task model assessment](#f-persistence-and-task-model-assessment)
- [G. Responsibility boundaries](#g-responsibility-boundaries)
- [Scenario evaluation A-H](#scenario-evaluation-a-h)
- [Concept audit](#concept-audit)
- [Design tensions](#design-tensions)
- [H. Product principles](#h-product-principles)
- [I. Architectural directions](#i-architectural-directions)
- [J. Prioritized improvement opportunities](#j-prioritized-improvement-opportunities)
- [K. Validation strategy](#k-validation-strategy)
- [Where the evidence contradicts the request's assumptions](#where-the-evidence-contradicts-the-requests-assumptions)
- [Appendix A. Experiment harness and raw results](#appendix-a-experiment-harness-and-raw-results)

---

## A. Executive assessment

### The answer

Yes, in three specific places, and no in most others.

**Where Posse controls too much** [I, from the evidence in sections D to F]:

1. **The software-delivery pipeline is the core, not an option on top of it.** Review, validation, candidates, PR stewardship and merge occupy at least 18,000 of the 65,581 non-test lines in `internal/app`, 30 of 71 database tables, 43% of the words in the Lead's runtime obligations and about a fifth of the CLI command entries. A Worker that researches or draws images passes through a model built for a pull request.
2. **Skills follow the Task type, and every configured skill is mandatory.** The Lead cannot give one Worker one extra skill. A docs-only Task typed `change` must invoke `posse-tdd` before Posse accepts its `done` Signal.
3. **The Task type is a closed set of three (`change`, `research`, `review`) that fixes the outcome, the completion rule, the skill set and the delivery path together.** The type is consulted at 177 code sites.

**Where Posse does not control enough** [I]:

4. **Capability routing has no enforcement and no meaning in the data model.** A Profile is a launch configuration. Nothing records what it is for. The Lead picks a Profile by reading prose, and Posse accepts any name. In 1,335 real Tasks, the Dispatch Rules resolved a Profile without the Lead in three.

**What the request assumed that the evidence does not support** [I]:

5. **The `codex-sol` misrouting has deterministic causes inside Posse, so it cannot be assumed to be a Lead judgment failure.** Posse's own Review auto-start code picks "the first candidate in config order", and the User's first rule is the image rule [O]. `codex-sol` ran 88 Tasks since 2026-11-02, of which 85 were not image work and 73 were Review Tasks [O]. Who started those Tasks is not recorded, so how many came from auto-start is a hypothesis [H]. A second deterministic cause hides the image rule from the Lead in any Project with typed rules [O]. Section E has the reproductions.
6. **SQLite is not the problem.** The guarantees that need a transactional store sit in about 20 tables. The other 50 persist the delivery and review model. A second persistence layer (TOML snapshot files) costs more than SQLite itself.
7. **Firstmate is not simpler.** It is about 145,000 lines of bash with a 52,000-word configuration document, and its spawn script refuses to launch on more conditions than `posse start` does. Its dispatch feels natural for two reasons that Posse can adopt without copying it: intake judgment is written as prose with an explicit "do not over-plan" rule, and the captain never sees the mechanics.

### What Posse is

Posse is a Go CLI with no daemon of its own. One Lead agent per Project talks to the User and runs `posse` commands. Each Worker is a Herdr tab in the Lead's workspace, running in its own git worktree with its own process containment. State lives in one SQLite file. Herdr plugin events and a supervised background Watch per Project drive reconciliation and Notices [O: `docs/adr/0001`, `docs/adr/0025`, experiments E3.8 and E6.8].

### What Posse has become

| Measure | Value | Source |
|---|---|---|
| Age | 48 days (2026-09-25 to 2026-11-12) | [O] `git log` |
| Commits | 458, of which 227 `fix` and 112 `feat` | [O] `git log --format=%s` |
| Non-test Go | about 99,700 lines; `internal/app` 65,581, `internal/store` 22,898 | [O] `wc -l` |
| Test Go | about 123,000 lines, of which E2E 44,435 | [O] `wc -l` |
| Database | 71 tables, 646 columns, 95 migrations | [O] `internal/store/sqlc/models.go`, `internal/store/migrations` |
| CLI | 99 command entries | [O] `internal/app/service.go` |
| Config | 88 documented keys | [O] `posse config schema` |
| Vocabulary | 58 defined terms | [O] `CONTEXT.md` |
| ADRs | 40 | [O] `docs/adr/` |
| Lead instructions | 3,176 words, of which 2,530 are runtime obligations | [O] generated `lead.md` |
| Real database | 512 MB after 1,335 Tasks | [O] `ls -l ~/.posse/posse.db` |

In the real home, 549 of 1,335 Tasks (41%) are Review Tasks. Reviewing its own Change Tasks is the single most common thing Posse does [O: aggregate over `~/.posse/projects/*/tasks/*/task.toml`].

### Does the architecture still match the product identity?

The identity in the request is "one Project, one independent Lead, and a flexible crew of Workers operating within Herdr". Three layers answer differently [I]:

- **The execution layer matches it and is the strongest part of the product.** Project and Lead isolation, tab placement, Sandbox custody, verified Teardown, turn tokens, queued instructions and explicit restore all held in the experiments (E3, E5, E6, G1 to G10). These are the guarantees that make Posse trustworthy and they are what Firstmate spends most of its fix history trying to reach with files and locks.
- **The delivery layer does not match it.** It is a second product (a private CI and code-review pipeline) fused into the Task model, the database, the Lead instructions and the Worker protocol.
- **The capability layer is missing.** Profiles, Dispatch Rules and role skills are three static configurations that never meet. None of them knows what a Task needs or what a Worker is good at.

ADR 0027 already states the intended shape: "Posse is a dispatch-control layer: it assigns the right work to the right isolated Worker" [O: `docs/adr/0027-thin-core-and-user-owned-workflow-profiles.md:5`]. The implementation that followed kept the three guardrails it names (isolation, ship authority, selected review) but placed the third inside the core. Section I recommends completing ADR 0027 instead of replacing it.

### Honest assessment of complexity and experience

- **For the User, the conversation is light and the configuration is heavy.** A fresh home starts a Worker with no configuration (E1.8). The moment the User writes Dispatch Rules to express preferences, three precedence rules interact in ways that `posse config show` does not reveal (section E).
- **For the Lead, delegation costs one small file and two commands, which is acceptable. Everything after that is heavy.** The Lead must hold 2,530 words of obligations, 43% of them about PR and merge mechanics, and a vocabulary of 13 Task states and more than 200 Notice and error codes [O: section B].
- **For a Worker, the protocol is 686 words on a fresh home before the assignment starts** (E1.11). The Worker protocol for this very Task was 46 KB, because the Brief is long and five skills plus five platform skills are attached.
- **For a maintainer, the fix rate is 50% of all commits**, concentrated in the edges between Posse and Herdr and in the delivery pipeline [O: section G].

---

## B. Current architecture map

### Control flow from intent to teardown

```
User intent (chat in the Lead pane)
  |
  v
Lead reasoning            lead.md: 591 words of default guidance + 2,530 words of obligations
  |                       Intake skills (posse-research, posse-build, ...) if words match
  v
Brief file                YAML front matter: type, title, done_when are required
  |                       25 accepted keys; any other key is refused
  v
posse dispatch            preview only: resolves a Profile or returns profile_required
  |
  v
posse start               1. parse Brief            internal/dispatch/brief.go
  |                       2. resolve Profile        internal/dispatch/resolve.go
  |                       3. select role skills     internal/app/task_plan.go:262
  |                       4. freeze the plan        internal/taskplan (version 7)
  |                       5. admit in one txn       capacity, Task id, branch, intent
  |                       6. acquire Sandbox        Pool worktree + lease + cgroup
  |                       7. create Herdr tab       tab.create in the Lead workspace
  |                       8. agent.start            kind args + skill injection
  |                       9. prompt "Read <launch.md> and follow it."
  v
Worker executes           in its Sandbox; posse _guard hook checks every shell command
  |
  |  posse signal working | needs-decision | failed | done --turn <token>
  v
SQLite                    transition + Signal + Notice in one transaction
  |
  v
Notice delivery           per Lead kind: background `posse watch` (claude),
  |                       `codex queue`, in-process extension (pi, opencode),
  |                       typed prompt as fallback
  v
Lead handles result       Research: reads the Report, acks
  |                       Change: publish -> candidate -> Review Rounds -> validate
  |                               test/docs/lint -> promote -> PR -> CI loop -> merge
  v
Teardown                  verify process and pane identity, stop the Worker, save Report
                          attachments, release Sandbox to the Pool
```

Sources [O]: `internal/app/tasks.go:1420` (Worker protocol), `internal/app/lead_instructions.go`, experiment E2.9 (launch command lines), E1.8 to E1.12, E4.1 to E4.6.

### Who triggers work when the Lead is idle

Posse has no daemon of its own, so four kinds of short-lived or supervised processes move state forward [O: `internal/setup/assets/herdr-plugin.toml:8-29`, `internal/app/ingest.go:48-218`, `internal/app/watch_runtime.go:253-382`]:

| Trigger | Process | What it does |
|---|---|---|
| Any `posse` command | the command itself | reconciles the Project before answering |
| Herdr plugin event (`pane.agent_status_changed`, `pane.exited`, `pane.closed`, `workspace.closed`, `pane.focused`) | `posse _ingest`, one process per event | reconciles, delivers queued messages and Notices, repairs the Watch |
| Time | one detached Watch per Project | polls PRs, CI and the root checkout, and catches conditions that produce no Herdr event |
| Herdr start | `posse _herdr_started` | records a new server generation and raises `restore_needed` |

ADR 0001 rejected a daemon partly because "the daemon itself would need systemd-style supervision" [O: `docs/adr/0001:8`]. ADR 0025 then added the Watch, and Posse supervises it itself at almost every command boundary. The watch files total 2,259 non-test lines [O: `wc -l internal/app/watch*.go`]. ADR 0015 still says "There is no timer service" [O: `docs/adr/0015:7`], which no longer describes the system [I].

### Subsystems

| Subsystem | Owns | Guarantee it protects | Non-test size | Could be simpler? |
|---|---|---|---|---|
| Lead contract (`lead_instructions.go`, `lead.go`) | generated instructions, identity, Notice receiver setup | the Lead never edits the repo, never merges without authority | 906 lines, 3,176 generated words | Yes: 1,098 words are PR and merge procedure [O: classification of `lead.md` bullets] |
| Brief and plan (`internal/dispatch`, `internal/taskplan`) | contract parsing, frozen plan | a relaunch runs the admitted contract, not edited config | 1,756 | Partly: the plan stores input bytes, including every skill body, four times (section F) |
| Dispatch resolution (`internal/dispatch/resolve.go`) | Profile choice | none; it is advisory once any `when` rule exists | 131 | Yes (section E) |
| Skills (`launch_skills.go`, `skill_uses.go`, `internal/config/skills.go`) | role skill sets, injection, use recording | content-addressed delivery of the frozen skill set | about 1,200 | Yes (section D2) |
| Herdr adapter and ingest (`internal/herdr`, `herdr.go`, `ingest.go`) | every Herdr call, event handling | never type into a focused, blocked or foreign pane | about 2,300 | Partly; depends on Herdr features (section G) |
| Sandbox, Pool, containment, Teardown, guard | worktrees, leases, cgroups, verified stop, Worker command guard | isolation and no collateral damage | about 10,600 [O: file-name sum] | Little; this is the essential core |
| Watch, Notices, recovery, restore | time-driven observation, delivery receipts, generation holds | no lost Signal, no surprise restart | about 6,000 | Yes: four receiver mechanisms (section G) |
| Delivery (publish, PR, forge, CI loop, merge, merge queue, stewardship) | everything after `done` for a Change Task | no unauthorized merge, exact-head evidence | about 10,700 [O: file-name sum] | Separable (section I) |
| Review and validation (Review Rounds, Finding Ledger, candidates, validate stages) | built-in code review | bounded review bound to an exact head | 6,501 in `app`, 3,456 in `store` [O] | Belongs to Greenhouse (section G) |
| Native research children (`research*.go`, `internal/research`) | tracked read-only subagents | no write-capable untracked child | about 3,000 | See D7 |
| Store (`internal/store`) | SQLite, snapshots, migrations | compare-and-set transitions, idempotent Signals, capacity admission | 22,898 | Yes without leaving SQLite (section F) |

### Most important dependencies

1. **Task type is the hub.** It selects the skill role, the Worker protocol text, the `done` rule (`--report` required for research and review, [O] E4.1), the state path (`done` becomes `reported` in the same transaction for non-change Tasks, [O] `internal/store/store.go:2177-2186`), dispatch rule matching and merge behaviour. [O] `change` is tested at 125 sites in 44 files, `review` at 34 sites in 16 files, `research` at 18 sites in 12 files (`grep -rnE '(Type|type) (==|!=) "<t>"|case "<t>"' internal`).
2. **The frozen plan binds config, skills and delivery policy to a Task at admission.** Review mode, merge mode, Autonomy grants, validation loops and skill bytes all live in it [O: `internal/taskplan/plan.go:150-186`; this Task's `task.toml`].
3. **Review reaches into merge for every Task.** `requireNoBlockingReviewFinding` "applies under every review policy" [O: `internal/app/review_verdict.go:273-290`].
4. **Herdr generation gates everything.** A changed server generation holds a Project until `posse up --all` [O: experiment G6 to G9, `docs/adr/0035`].

---

## C. Firstmate comparison

The comparison uses Firstmate's source and history, not its README. Evidence was gathered by reading `bin/`, `tests/`, `docs/`, `AGENTS.md`, `VISION.md` and `git log`, and spot-checked at the cited lines. Firstmate was not run.

### What Firstmate is in fact

| Measure | Firstmate | Posse |
|---|---|---|
| Language and state | bash, plain files and hand-built locks, no database [O: `FM:bin/`, `grep -il sqlite` finds no store] | Go, SQLite |
| Size | about 145,000 lines in `bin/`, about 295,000 lines of tests, 237 `fm-*.sh` scripts [O] | about 99,700 non-test, 123,000 test |
| History | 1,189 commits in 5 months, 749 `fix` (63%) [O] | 458 commits in 7 weeks, 227 `fix` (50%) |
| Configuration doc | 52,326 words, about 91 distinct `config/` files, about 180 `FM_*` variables [O: `FM:docs/configuration.md`] | 88 keys in one schema |
| Lead instructions | 7,353 words in `AGENTS.md` plus 30 skills [O] | 3,176 words plus 15 Lead skills |
| Terminal backends | 10 (tmux reference, Herdr second) [O: `FM:bin/backends/`] | Herdr only |
| Projects per lead | one first mate for all projects, optional secondmates [O: `FM:AGENTS.md:21-24`] | one Lead per Project |
| Largest scripts | `fm-spawn.sh` 8,316 lines, `fm-watch.sh` 4,826, `fm-teardown.sh` 4,277, `backends/herdr.sh` 4,205 [O] | `tasks.go` 3,871 |

### How delegation works in each system

| Question | Firstmate | Posse |
|---|---|---|
| How does the lead decide to delegate? | Delegation is the default: "you do not do project-specific work yourself" [O: `FM:AGENTS.md:21-23`]. Kind and concurrency are judgment rules with an explicit brake: "start with the simplest direct end-to-end path" [O: `FM:AGENTS.md:179-180`]. | Same split. "Choose the lightest workflow that can verify the requested outcome" [O: `lead.md` default guidance]. The Lead may not edit the repo. |
| What must exist before a Worker starts? | Task id, project dir, a scaffolded brief with two filled sections, a backlog row, `--mode` and `--yolo` for a ship, a clean worktree, and, if dispatch rules exist, a recorded resolution that the flags match [O: `FM:bin/fm-spawn.sh:4321-4347`, `:1284-1310`, `:4929-4949`, `:3240-3283`]. | A Brief with `type`, `title`, `done_when`, and a `--profile` if any `when` rule exists [O: E1.1 to E1.8, E2.5]. |
| Task kinds | ship (default), scout, secondmate. A kind is never mandatory because ship is the default [O: `FM:bin/fm-spawn.sh:339-341`]. | `change`, `research`, `review`. `type` is mandatory [O: E1.2]. |
| Harness and model choice | Natural-language `when` rules in `config/crew-dispatch.json`. The lead matches them by judgment, or an optional external model does and a jq pipeline applies confidence floors and quota ranking. Since 2026-11-08 spawn refuses flags that differ from a clear recorded selection unless `--dispatch-override <reason>` is given [O: `FM:docs/configuration.md:1936-1939`, `FM:bin/fm-dispatch-resolve.sh:513-552`, commit `18b20eaf`]. | Natural-language `when` rules. The Lead always matches them. Nothing is recorded or checked except the chosen name [O: section E]. |
| Skills per task | None. The brief has no skill field. Workers inherit whatever the harness loads in the worktree [O: `FM:bin/fm-brief.sh`, grep for `skill` finds one fixed reference]. | Per role, frozen and mandatory. No per-Task channel [O: E1.5, section D2]. |
| Research with no diff | A scout. Still requires a project and an isolated git worktree. The worktree is discarded and `data/<id>/report.md` survives [O: `FM:bin/fm-spawn.sh:345-347`, `FM:bin/fm-brief.sh:713-715`]. | A Research Task. Requires a Git Project and a Sandbox. The Report and attachments survive [O: E1.10, E4.5]. |
| Supervision | A bash watcher the lead must keep armed, backed by Stop hooks and per-harness turn-end guards, plus a daemon in away mode [O: `FM:AGENTS.md:265-269`, `FM:bin/fm-turnend-guard.sh:7-10`]. | Herdr plugin events plus a supervised Watch. The Lead keeps one `posse watch` waiter. |
| History | Markdown backlog, per-task meta and append-only status files [O: `FM:AGENTS.md:369-380`]. | SQLite plus TOML snapshots. |
| Review | Delegated to the external no-mistakes tool [O: `FM:AGENTS.md:229-238`]. | Built in. |

### Same scenarios, both systems

| Scenario | Firstmate | Posse |
|---|---|---|
| A. Simple research | Scout: brief scaffold, fill two sections, backlog row, spawn. A worktree is created. | Research Brief with three required keys, `start`. A worktree is created (E1.8). |
| B. Parallel research | "no concurrency cap" by default [O: `FM:AGENTS.md:203`]. | Three starts work; the fifth hits `max_workers=4` and needs `--queue` (E3.5, E3.6). |
| D. Image agent only for images | A rule `"when": "The task generates images."` is a test fixture [O: `FM:tests/fm-dispatch-resolve.test.sh:59`]. With the resolver on, the choice is recorded and spawn enforces it. A profile is still only harness, model and effort. | A rule with the same wording exists in the User's config. Nothing enforces it and typed Project rules hide it (X1). |
| F. Exploration | A scout, or no worker at all: "If established evidence already answers an informational question, relay it without a design-only scout" [O: `FM:AGENTS.md:188-189`]. | A Research Task. Prose without front matter is refused (E1.1). |
| G. Interruption | Lead restart rebuilds from files; dead workers are reported, not relaunched; tmux cannot prove a missing pane [O: `FM:bin/fm-control.sh:61-75`]. | Generation hold, explicit restore, verified relaunch (G1 to G10, E5). |
| H. Multiple Projects | One lead handles all projects; separation needs secondmates with their own homes. | One Lead and one workspace per Project, enforced (E6.4, E6.5). |

### The six comparisons the request asks for

**1. What Posse does better** [I, from the observations above]

- Transactional state. Firstmate's history shows the cost of the alternative: commit `1260adce` added 787 library lines and 2,302 test lines to keep a task's record and its backlog row atomic, and the wake library carries about 30 lock and recovery functions [O: `FM:bin/fm-wake-lib.sh:542-1730`].
- Project separation as a product rule, not a convention.
- Herdr-native placement and identity checks without a backend matrix.
- Recovery that is tested end to end with an isolated Herdr server.
- A smaller surface for the User to configure: 88 keys against about 180 variables and 91 files.

**2. What Firstmate does better** [I]

- **The lead's vocabulary is hidden from the captain by rule.** `FM:AGENTS.md:323-324` tells the lead to "Use the captain's nouns" and "Do not expose internal terms such as ... task ids, briefs, worktrees ... harness names".
- **Mechanics moved out of the always-loaded prompt.** `AGENTS.md` shrank from 847 lines to 435 while scripts grew [O: line counts at `20efee2c` and HEAD]. Posse's `lead.md` carries PR procedure in every session.
- **Dispatch choices are recorded and then enforced.** The resolver writes `dispatch-resolution.json` and spawn refuses a mismatch. Posse records only the result.
- **The model that matches rules sees only the task text and the `when` text**, never quota, approvals or the `use` field, and tests assert it [O: `FM:tests/fm-dispatch-resolve.test.sh:447`]. That keeps the semantic question separate from the mechanical ranking.
- **A default kind.** Nothing must be classified for the common case.
- **Review is someone else's product.**
- **Steering uses a durable inbox plus a doorbell line**, so the message body never depends on typing into a terminal [O: `FM:bin/fm-task-inbox-lib.sh:14-35`]. Posse already does this for long instructions (`posse send --file`).

**3. What Posse added that improves the experience** [I]

Sandbox Pool with leases, verified Teardown, turn tokens that reject a stale `done`, queued and interrupting sends, explicit restore, Report Bundles with attachments (E4.5), one Lead per Project, tracked read-only research children, and the `posse dispatch` preview.

**4. What Posse added that made the experience more rigid** [I]

Mandatory `type`; the built-in review loop and its 16 tables; mandatory role skills with use recording; the validate stages and Private Candidates; the frozen plan as a carrier for review and delivery policy; a `max_workers` default of 4 where Firstmate has no cap; and Dispatch Rules that stop resolving as soon as one of them has a `when`.

**5. What Firstmate's apparent simplicity hides or sacrifices** [O unless marked]

- Every launch parameter is locked down by a validator that grew 55 times in five months (152 lines at `ccdea30b`, 8,316 at HEAD).
- Supervision exists only while the lead keeps re-arming it; the guard exists because a session "can sit blind for hours" [`FM:bin/fm-turnend-guard.sh:7-10`].
- Screen reading decides state and confirms delivery on several harnesses; `VISION.md:65-66` calls it "a standing debt".
- Documentation is the top fix target: `docs/configuration.md` appears in 243 fix commits and `AGENTS.md` in 160. Behaviour is specified in prose the model must obey [I].
- With the resolver on, brief text is sent to an external service by default [`FM:bin/fm-dispatch-resolve.sh:162-163`].
- No per-task skills and no capability on a profile. The image-agent case works only as far as harness, model and effort express it.

**6. What Posse should keep as its own identity** [I]

One Lead per Project. Herdr as the only environment. A transactional core. Isolation and Teardown that are proven, not assumed. The User decides merges and discards. Those five are absent or weaker in Firstmate and they are what the experiments show working.

### Which parts of Firstmate's flexibility are meaningful for Posse

| Firstmate trait | Applies to Posse? | Why |
|---|---|---|
| Prose intake rules with a "do not over-plan" brake | Already present | `lead.md` default guidance says the same. |
| Default kind, no required classification | Yes | Removes the E1.2 refusal and the type hub. |
| Hidden mechanics in User messages | Yes | Posse says "Report outcomes and decisions, not mechanics" but then requires a per-Notice report unless lowkey is on. |
| Recorded and enforced dispatch selection | Yes | Fixes the observability gap in section E. |
| External review tool | Yes | That is the Greenhouse direction. |
| No concurrency cap | Partly | A cap protects quota and the machine; the default of 4 with a hard refusal is the friction, not the cap. |
| One lead for all projects | No | Contradicts the product identity. |
| Files instead of a database | No | Firstmate's fix history is the argument against it. |
| Ten backends | No | Contradicts "Herdr is fundamental". |

---

## D. Product friction analysis

Each issue lists evidence, cause, impact and whether it is architectural or local.

### D1. The delivery pipeline is fused into the core

**Evidence**

- [O] `internal/app` non-test lines by file-name group: delivery 10,733 (PR, merge, publish, forge, CI, branch authorship), review and validation 7,346. Together about 28% of the package, and a lower bound because `tasks.go` mentions review 152 times.
- [O] Tables: 14 delivery and 16 review or validation out of 71; 273 of 646 columns.
- [O] Lead runtime obligations: 1,098 of 2,530 words concern PR, merge and CI, plus 244 on review.
- [O] CLI: `publish`, `validate`, `candidate abandon`, `retitle`, `pr inspect|reply|refresh|rerun`, `merge`, `review start|fix|carry|dispose|merge|followups|defer`, `accept` and `sync` account for about 21 of the 99 command entries.
- [O] A Research Task on a fresh home shows `review_evidence: selected-policy` in `posse show` (E1.9) and its Worker protocol tells it "Do not wait for PR CI after `posse signal done`" and refers to "this Change Task" (E1.11, lines 114 and 130).
- [O] This Task is a docs-only report. Its plan froze `review: full-pipeline`, `delivery: pr`, validation loops and merge method, and its protocol requires a private candidate, a Review Round and `posse validate test`, `docs` and `lint` before promotion.

**Cause** [I]. ADR 0024 replaced the external no-mistakes tool with a built-in process on 2026-10-18 at the User's request, and ADR 0027 made "selected review" a guardrail that cannot be disabled. Each later feature (candidates, Finding Ledger, caps, stewardship, merge queue) attached to the Task and the plan because that was where the state was.

**Impact on the Lead.** More than half of its obligations and most Notice kinds concern delivery. A Lead that only dispatches research still loads them.

**Impact on the User.** Posse reads as a software-delivery system. Non-code work inherits its words.

**Impact on maintainability.** Most migrations from 43 to 95 serve review, validation or delivery [O: section F]. The fix history clusters there and at the Herdr edge.

**Kind.** Architectural.

### D2. Skills are chosen by Task type and all of them are mandatory

**Evidence**

- [O] Selection is `selectedSkillInputs(cfg, task.Type, kind)`; the role is the Task type (`internal/app/task_plan.go:262-276`, `internal/config/skills.go:41-78`).
- [O] A Brief key `skills:` is refused: `skills: unknown Brief field` (E1.5). Workflow profile stage `skills` is "opaque metadata; Posse does not load skills from this field" (`docs/adr/0027`).
- [O] `posse signal done`, `posse validate docs` and `posse validate lint` fail with `role_skills_unused` while any configured role skill has no recorded use. "There is no setting to relax this" (`internal/app/skill_uses.go:153-172`, `docs/skills.md:62`).
- [O] The step text is a hard-coded map: `"posse-tdd": {when: "before changing code; write the failing test first"}` (`internal/app/skill_uses.go:28-36`).
- [O] Live case: this docs-only research report is typed `change`, so its plan carries `posse-tdd`, `posse-deslop`, `posse-no-comments`, `posse-technical-writing` and `posse-unslop`, and `done` is refused until each has a recorded use. The role `research` would have attached `posse-research`, which fits the work, but then the Task could not deliver a PR.
- [O] Every Worker also receives all five platform skills, including `posse-setup` (1,245 words), `posse-down` and `posse-update`, whose own text addresses the Lead or the User (`internal/app/launch_skills.go:46-48`; `wc -w internal/setup/skills/*/SKILL.md`).
- [O] Use is recorded by a hook for claude, self-reported for pi through `posse _skill-use`, and neither recorded nor required for codex (`internal/app/skill_uses.go:183-185`, `:311-336`).
- [O] The recommended `review` set in code includes `posse-tdd` while `docs/skills.md:47` shows `["posse-review", "posse-unslop"]` (`internal/setup/assets.go:95`).
- [O] Workers still see the User's own harness skills. Posse sets no `CLAUDE_CONFIG_DIR`, `CODEX_HOME` or Pi agent dir for Workers (`internal/app/worker_tab.go:84-87`).

**Cause** [I]. ADR 0036 fixed a real problem ("Workers carried Lead skills, which cost context, invited misfires and lengthened launch commands"). Enforcement followed in commit `6bd85306` on 2026-11-02 after advice and a warning did not change Worker behaviour. The role was the only available key because the Task type is the only thing Posse knows about a Task.

**Impact on the Lead.** It cannot act on its own judgment about what a Worker needs. Its only levers are the Task type and a file path written in the Brief body, which is not frozen or recorded.

**Impact on the User.** "The right skills reach the Worker" is true for delivery of bytes and false for fit. A recorded use proves that a tool was called, not that the skill was followed [I].

**Impact on maintainability.** Enforcement is uneven across kinds, so the same config behaves differently per Profile.

**Kind.** Architectural in selection (it depends on D3), local in enforcement.

### D3. The Task type is a closed taxonomy that carries five decisions

**Evidence**

- [O] `type: is required` and `type: must be change, research or review` (E1.2, E1.4; `internal/dispatch/brief.go:325-330`).
- [O] One value fixes: outcome and completion rule (`a Research or Review Task requires --report <file>`, E4.1), state path, skill role, Dispatch Rule matching, and whether delivery and review apply.
- [O] 177 code sites branch on it.
- [O] Real usage bends the taxonomy: artwork studies ran as Research Tasks, a logo ran as a Change Task, and this report runs as a Change Task (aggregate over the real home, 2026-11-07 to 2026-11-12).

**Cause** [I]. The first two types (formerly ship and scout, as in Firstmate) described the only two outcomes that existed. Review was added as a third type instead of as a relationship, and each later rule keyed on the type.

**Impact on the Lead.** Mixed or uncertain work must be classified before it starts. The classification silently chooses the skills and the pipeline.

**Impact on the User.** Small. The User rarely sees the type. The effects surface as wrong skills and unneeded ceremony.

**Impact on maintainability.** Any new kind of work multiplies branches at 177 sites.

**Kind.** Architectural. What Posse needs to know mechanically is narrower: does the Task write to the Project, must a Report exist, and is there a Delivery. These are independent properties, not three bundles [I].

### D4. Capability routing is advisory, opaque and partly hidden

Section E covers this in full. In summary: [O] a Profile has no purpose field; [O] any `when` rule turns resolution into `profile_required`; [O] `--profile` accepts any name; [O] typed rules hide untyped ones; [O] Review auto-start takes the first candidate.

**Kind.** Architectural for the missing capability concept; local for the three precedence defects.

### D5. The Lead's instruction and state load

**Evidence**

- [O] 28 obligation bullets, 2,530 words, in every Lead session.
- [O] 13 Task states and 99 command entries.
- [O] 211 distinct snake_case codes with Notice-like prefixes (`pr_`, `review_`, `merge_`, `ci_`, `quota_`, and others) appear in `internal/app` non-test code.
- [O] The Watch obligation alone is a 190-word procedure about starting, reading, acknowledging and replacing a background waiter (`lead.md`, last bullet).

**Cause** [I]. Each incident added a sentence to the Lead contract. Firstmate went the same way until June and then moved rules into scripts and skills.

**Impact on the Lead.** Context cost and a higher chance of skipping a rule. The request itself warns: "Do not assume the Lead can reliably follow complex prompt instructions."

**Impact on the User.** Indirect: slower turns and occasional procedural mistakes.

**Kind.** Local. The text is generated and can be made conditional on what the Project uses.

### D6. Worker protocol overhead and wrong-type text

**Evidence**

- [O] A 14-word research assignment on a fresh home becomes a 686-word `launch.md` (E1.11).
- [O] 49 of those words tell a research Worker how to isolate Herdr experiments, and the text mentions Go build caches for a Task that builds nothing.
- [O] `task.toml` for that Task is 74 KB on a fresh home (E1.12) and 208 KB for this Task. 835 Task snapshots in the real home exceed 100 KB.

**Cause** [I]. One protocol template serves all Tasks, and Project-specific Posse-development advice is in the generic template (`internal/app/tasks.go:1416`).

**Kind.** Local.

### D7. Research children: a second execution mechanism with its own limits

**Evidence**

- [O] ADR 0029 allows only read-only native children registered through Posse. During this Task, six `general-purpose` agent launches were refused (`only Explore, Plan or a custom definition with an explicit read-only tool list may run as tracked research`), and four more were refused because `2 of 2 research children are active`.
- [O] `research_attempts` has 34 columns; research code is about 3,000 non-test lines.

**Cause** [I]. A real risk: an untracked child with write tools can change a checkout that Posse believes only its Worker writes.

**Impact.** A Worker doing broad research is limited to two concurrent read-only children, and the limit is only discoverable by hitting it. The refusal messages were clear and the retry cost was low.

**Kind.** Local. The guarantee is worth keeping. The default of two and the lack of a note in the Worker protocol are tunable.

### D8. Capacity default

**Evidence.** [O] `worker_limit: shop already runs 4 Workers (max_workers=4)` on the fifth start; `--queue` admits it (E3.5, E3.6). [O] The limit is per Project; there is no machine-wide cap (`internal/config/schema.go:50`).

**Impact** [I]. Parallel research hits the wall at five. With six Projects the machine can still run 24 Workers, so the cap protects neither quota nor the machine well.

**Kind.** Local.

### Classification of the complexity found

| Class | Examples |
|---|---|
| Necessary | Sandbox and leases, verified Teardown, turn tokens, capacity admission in one transaction, Lead-only and Worker-only command modes, generation holds, User authority for merge and discard |
| Useful but optional | Queue and `after:` dependencies, Report Bundles, budgets, Knowledge, Preferences, merge queue, PR stewardship, quota fallback |
| Accidental | Typed rules hiding untyped rules; first-candidate Review auto-start; `dispatch.default` never applying beside a `when` rule; plan bytes stored four times; platform skills for Workers; change-flavoured text in research protocols |
| Historical | `landing_mode`, `mount_id`, ship and scout aliases, no-mistakes receipts, `migration_compat.go` renumbering, legacy plan-less column path |
| Unclear or duplicated responsibility | Built-in review against Greenhouse; Posse restore against Herdr native resume; four Notice receivers; TOML snapshots against SQLite |
| Missing capability that forces workarounds | No purpose on a Profile; no per-Task skill; no Task without a Git Project; no "default kind" |

---

## E. Dispatch and capability analysis

### What each concept is today

| Concept | What the code says it is | Fields | Purpose it serves |
|---|---|---|---|
| Kind | A harness adapter | `disabled`, `auto_approve_args`, `model_args`, `effort_args`, `resume_args`, `system_prompt_args`, `lead_args`, `prepare`, `notice_delivery`, `background_commands`, `steer`, `interrupt_keys` [O: `internal/config/config.go:141-154`] | How to launch, resume, steer and interrupt one CLI. The User almost never edits it; built-in defaults cover claude, codex and pi. |
| Profile | A named launch configuration | `kind`, `model`, `effort`, `args`, `attribution`, `fallback` [O: `internal/config/config.go:169-178`] | Which model at which effort, and what to relaunch on when quota runs out. |
| Dispatch Rule | A prose hint | `type`, `when`, `use` [O: `internal/config/config.go:181-184`] | Tells the Lead which Profile the User prefers for which work. |
| `dispatch.default.use` | A fallback Profile name | one string | Used only when no rule matches at all. |
| `defaults.dispatch_inheritance` | `kind` or `lead_profile` | one enum | Used only when there are no rules and no default. |
| `lead.profiles.<kind>` | The Lead's own Profile per kind | map | Model and effort for the Lead. |
| `review_loop.profile` | The reviewer Profile | one string | Overrides all rules for Review Tasks. |
| `[skills]` | Skill names per role | four lists | Unrelated to all of the above. |

**Kind and Profile solve different problems and both are needed** [I]. A Kind answers "how do I drive this CLI", which is Posse's knowledge. A Profile answers "which model and effort", which is the User's choice. Merging them would force the User to repeat adapter details per model. The trouble is not that there are two layers. It is that neither layer, nor the rule layer above them, says what a Worker is for.

### How a Profile is chosen: the actual algorithm

[O] `internal/dispatch/resolve.go:49-113`, in order:

1. `--profile <name>`: use it. No check against rules, Brief or type.
2. Review Task and `review_loop.profile` set: use that.
3. Collect rules whose `type` equals the Task type ("typed") and rules with no `type` ("generic"). **If any typed rule matches, generic rules are dropped.**
4. If exactly one rule remains, it has no `when` and names one Profile: use it.
5. If any rule remains: return `profile_required` with the candidates. Rules with a `when` are listed first, in config order.
6. If no rule remains and `dispatch.default.use` is set: use it.
7. If there are no rules at all: inherit the Lead's kind or Profile.
8. Otherwise `profile_required`.

Three properties follow.

- **Selection is model-driven as soon as one `when` exists.** The code never reads `when`. [O] `lead.md`: "Read the Brief and Dispatch Rules; interpret `when` prose and consider suitable existing Profiles."
- **A configured rule is a suggestion.** [O] E2.3: the image Brief started on `pi-luna`. [O] E2.4: a bug-fix Brief started on `codex-sol`. Both were accepted without a warning and recorded as `--profile <name>`.
- **The record cannot tell a Lead choice from a default.** [O] In the real home, 1,208 of 1,335 Tasks carry the rule `--profile ...`, 115 carry `review_loop.profile`, 9 carry `relaunch --profile ...`, 2 carry `dispatch.default` and 1 carries a typed rule. No Task records which `when` text the Lead thought applied.

### Why `codex-sol` is selected for unrelated work

The User's global config has this as its first rule [O: `~/.posse/config.toml`]:

```toml
[[dispatch]]
  use = "codex-sol"
  when = "Task requires generating images"
```

and the Profile is:

```toml
[profiles.codex-sol]
  kind = "codex"
  model = "gpt-6.1-sol"
```

**Observed usage.** The Lead's notes for the Posse Project record that the image rule was restored on 2026-11-02. Since that date, `codex-sol` ran 88 Tasks in five Projects: 73 Review Tasks, 11 Change Tasks and 4 Research Tasks. Three of the 88 have an image-related title, so 85 are not image work [O: aggregate over real `task.toml` files].

**Who started them is not recorded.** [O] Every one of the 88 stores the rule `--profile codex-sol`. [O] A read-only query of a database backup copy shows the same start transition for all `codex-sol` Review Tasks (`source = cli`, note `Task created`), with no field for Lead start or auto-start. The causes below are therefore mechanisms that the code and the reproductions establish. Their share of the 88 historical Tasks is not established.

**Cause 1, a deterministic mechanism: Review auto-start takes the first candidate in config order.**

- [O] `autoStartReviewProfile` documents itself: "otherwise the first candidate in config order whose launch starts a fresh session" (`internal/app/review_rounds.go:768-812`).
- [O] Candidates come from `Resolve`, which lists `when` rules first in config order (`internal/dispatch/resolve.go:82-92`).
- [O] Reproduction with the User's rule shape: `Resolve(cfg, "review", "")` returns `profile_required` with `first_candidate=codex-sol` (test `TestReassessReviewCandidateOrder`, Appendix A).
- [O] Auto-start then passes `--profile codex-sol` to the normal start path (`internal/app/review_rounds.go:147-151`), so the Task records `--profile codex-sol`, exactly like a Lead choice.
- [I] In a Project that has no typed `review` rule and no `review_loop.profile`, an auto-started Review Round therefore asks for the image Profile, provided that Profile passes the fresh-session check in the same function.
- [O] The first-candidate behaviour was added in commit `2c162386` on 2026-10-28, before all 88 Tasks.
- [O] Tasks in Projects that set `review_loop.profile` carry the rule `review_loop.profile` instead (115 Tasks).
- [H] Auto-start produced most of the 73 `codex-sol` Review Tasks. The pattern fits: they are Review Rounds in Projects without a reviewer setting. The records cannot confirm it, because a Lead-started review with `--profile codex-sol` looks identical. Lead transcripts for those starts, or a start-source field added to the Task record, would settle it.

**Cause 2, structural: the Profile's name carries the wrong meaning.**

- [O] `codex-sol` reads as "kind codex, model sol". Sibling Profiles are named `pi-sol`, `pi-sol-medium`, `pi-luna`, `claude-opus`. Nothing in the name or the Profile says "images".
- [O] The intent "images only" exists in one place: the `when` text of one rule. That rule says images go to `codex-sol`. It does not say other work must not.
- [H] A Lead that needs a second reviewer identity, or that reads a User preference for "sol" as a reviewer, sees `codex-sol` as a general GPT Profile. The 11 Change Tasks and 1 non-image Research Task on `codex-sol` fit this. Verifying it needs the Lead transcripts for those starts.

### Why `codex-sol` is omitted when images are requested

**Cause 3, deterministic: typed Project rules hide the User's generic image rule.**

- [O] The Posse Project config defines only typed rules (`type = "research"`, `"change"`, `"review"`).
- [O] Step 3 of the algorithm drops generic rules when a typed rule matches.
- [O] Reproduction (X1): with the User's global rules plus typed Project rules, `posse dispatch` for the Brief "Generate a set of images for this Project" lists three candidates (`claude-opus`, `pi-luna`, `claude-sonnet`). `codex-sol` and the image rule are not shown.
- [O] X2: the same for a Research Brief.
- [I] In that Project, the Lead is never told the image rule exists at dispatch time. It would have to remember it from `posse config show`, which prints the global file only unless `--effective` is passed.

**Cause 4, structural: image work has no home in the type taxonomy.**

- [O] Real image-related Tasks since 2026-11-07 ran as `research` on `codex-sol` (three), `research` on a Pi Profile (one) and `change` on `pi-luna` (one, a logo). So two of five image Tasks missed the image Profile.
- [I] Whether an image Task is `research` (the images are attachments) or `change` (the images are committed) changes which rules are visible (cause 3) and which skills load (D2).

**Cause 5, structural: capability and skills are not connected.**

- [O] Image generation is a property of the codex harness and its tools. Posse has no field that says so on the Kind, the Profile or a skill.
- [O] A codex Worker receives Posse skills only as a line in `developer_instructions` and their use is not recorded (E2.9, `internal/app/skill_uses.go:183-185`).
- [I] Even a correct selection of `codex-sol` gives the Worker no image-specific instruction unless the Lead writes it in the Brief.

### Other precedence defects found while reproducing

- [O] E2.6: with one unconditional typed rule (`research -> pi-luna`) and one conditional typed rule, `posse dispatch` returns `profile_required` and lists the unconditional rule as `when=""`. An unconditional rule stops being a default as soon as it has a conditional sibling.
- [O] E2.7: with `dispatch.default.use = "pi-luna"` set and three `when` rules, a bug-fix Brief still returns `profile_required`. The default applies only when no rule exists for the type.
- [O] The User's config contains two identical `claude-opus` rules and two rules with the same "Default for other work" text, one of them next to `[dispatch.default]`. `posse config show` renders the default as a key nested inside the third rule (X4). The config writer and the mental model have drifted apart [I].
- [O] Project rules are prepended to global rules (`internal/config/config.go:492`), so "first in config order" depends on both files.

### Is the abstraction making the system more or less predictable?

Less, for the capability case [I]:

- The deterministic parts (typed-over-generic, first candidate, default-only-when-empty) are not visible to the User and produce results the User did not ask for.
- The judgment part has no record and no check.
- Adding a rule can remove a default (E2.6, E2.7), so a more detailed config is less automatic.

More, for the launch case [I]: once a Profile is chosen, the frozen launch (args, model, effort, fallback) is exact, visible in `posse dispatch` and stable across relaunch (E2.9, `docs/adr/0022`).

### What makes capability-based dispatch hard in the current design

1. Purpose lives in rule prose, attached to a selector, instead of on the thing selected.
2. Rules are keyed by Task type, and type is orthogonal to capability.
3. There is no notion of "restricted": every Profile is eligible for everything.
4. Skills attach to the role, so capability (Profile) and know-how (skill) cannot travel together.
5. The decision leaves no trace that the User can audit.

### Options, without assuming Profiles should go

| Option | What it changes | Keeps | Risk |
|---|---|---|---|
| E-a. Fix precedence only | Review auto-start never picks from conditional rules; generic rules stay visible beside typed ones; an unconditional rule or `dispatch.default` is the default when the Lead finds no matching `when` | Everything | Leaves purpose in prose; misuse by a Lead stays possible |
| E-b. Give a Profile a purpose | Add `description` (shown to the Lead at dispatch) and `only_for` or `reserved = true` (Posse refuses the Profile unless the Lead names the matching reason; auto-start never uses it) | Kind and Profile as they are | One more field; the Lead still judges whether a Task "is image work" |
| E-c. Record and check the choice | `posse start --profile X --because "<when text or reason>"`; store it; `posse show` prints it; a reserved Profile requires a reason that matches its purpose | Lead judgment | Slightly more typing for the Lead |
| E-d. Attach skills to a Profile | A Profile may list skills that load whenever it runs | Role skills as default | Couples two configs; needs the allowlist from D2 |
| E-e. Remove Profiles, select kind and model per start | Lead passes `--kind codex --model ...` | Kind | Loses named fallback chains, quota statistics per Profile and a stable name for "the image worker". The User's actual wish (a named, specialised worker) needs a name. Not recommended [I] |

E-a is a set of bug fixes. E-b and E-c together turn "a Profile intended for image work" from a sentence into a fact Posse can show and enforce, while the semantic judgment stays with the Lead. Section J orders them.

---

## F. Persistence and Task model assessment

### What the persistence layer guarantees

[O] `internal/store/store.go`, `write_transaction.go`, `docs/spec.md:335`, `docs/adr/0021`:

| Guarantee | Mechanism |
|---|---|
| A Task changes state only from an expected state, with a history row | compare-and-set transition plus `transitions` insert in one transaction |
| A stale Worker cannot complete a Task | turn token checked inside the `done` transaction |
| `done` is refused while Lead instructions are undelivered | same transaction |
| Concurrent starts cannot overbook capacity | live count, branch uniqueness and intent insert in one transaction |
| Task ids are unique and never reused | `UNIQUE(project_id, seq)` |
| A message or Notice is claimed by one deliverer | claim rows with partial unique indexes |
| A crashed `posse` command is resumed or rolled back | `intents` |
| Two Tasks never hold the same path or ref | `resource_leases`, checked across Projects |

[O] Notice delivery is not exactly-once and the spec says so: "Posse makes no exactly-once claim" (`docs/spec.md:913`).

With no daemon, many short-lived processes write one file. [O] WAL, `BEGIN IMMEDIATE`, retry wrappers and deferrable observation writes make that safe; ten commits mention fixing `SQLITE_BUSY`, and `busy_repro_test.go:158-164` records "hundreds of SQLITE_BUSY errors out of 800 Signal writes" before the fix. These guarantees are exactly what Firstmate rebuilds with symlink locks and replay records. **A transactional store is the right tool** [I].

### What the schema spends its tables on

[O] 71 tables, counted from `internal/store/sqlc/models.go`; grouping is this report's.

| Class | Tables | Columns | Needed for "one Worker, one assignment"? |
|---|---|---|---|
| Execution and recovery | 22 | 233 | Yes |
| Delivery (PR, merge queue, branch authorship) | 14 | 112 | Only for Tasks that deliver |
| Review and validation | 16 | 161 | No |
| Workflow (decisions, budgets, report acceptance) | 7 | 42 | Partly (Decisions) |
| Observability | 10 | 89 | Partly |
| Config approval | 1 | 7 | Audit only |
| Snapshot bookkeeping | 1 | 2 | Exists only for the TOML layer |

About 20 tables hold every guarantee in the first table of this section [I].

### The Task row

[O] `tasks` began with 28 columns and has 45. Of those, 9 are about review, delivery and merge (`reviews_task_id`, `landing_mode`, `autonomy_review`, `autonomy_land`, `pr_url`, `landed_ref`, `gated_sha`, `target_branch`, `pr_title`) and 3 about publication-bound Reports. `task_repos` repeats eight of them per workspace Member.

[O] Seven facts are stored both as columns and inside `plan_json` (type, reviewed Task, Profile, rule, merge mode, base, target). Migration 43 keeps both on purpose for Tasks admitted before plans existed.

### Task states

[O] Thirteen: `queued`, `spawning`, `working`, `needs-decision`, `blocked`, `stalled`, `done`, `merging`, `merged`, `reported`, `failed`, `lost`, `torn-down` (`internal/store/store.go:338-350`).

- [O] `merging` and `merged` are entered only by merge and PR code. `reported` exists because non-change Tasks skip them.
- [O] A Research Task passes through `done` and becomes `reported` inside the same transaction (E4.2 prints `signal: done`, `state: reported`).
- [I] The Worker-facing lifecycle is five states (queued, running, waiting on someone, finished, gone). The other eight describe supervision detail (`blocked`, `stalled`, `lost`, `failed`, `spawning`) or delivery (`merging`, `merged`, `reported`). Supervision states earn their place: they map to different Lead actions. Delivery states belong to a Delivery record, which already exists as a table.

### Sources of truth

| Thing | Truth | Other copies | Divergence repair that exists |
|---|---|---|---|
| Worker pane | Herdr snapshot | 9 columns on `tasks`, `project_runtime`, `task.toml` | reconcile overwrites each pass and skips other generations [O: `internal/runtime/reconcile.go:93-159`] |
| Sandbox | git worktree and directory | `mounts` table, `tasks.worktree_path`, a lease, `project.toml` | `posse pool repair`, lease reconcile, one global `posse.db.mount-lock` [O: `internal/app/sandbox_state_lock.go:10-21`] |
| Plan | `tasks.plan_json` | `task_plan_revisions`, twice in `task.toml`, rendered into `launch.md` | `restoreAdmittedLaunchFiles` [O: `internal/app/task_plan.go:139-187`] |
| PR state | the forge | `pr_url` in five tables plus head records | reconcile-on-merge paths |
| Notices | `notices` and receipts | harness queue, `notice-delivery.toml` | transcript reading to settle uncertain receipts [O: `internal/app/lead_claude_stop.go:179-206`] |
| Config | `config.toml` | bytes frozen into each plan | none needed; edits affect new Tasks only |

### Where the persistence cost really is

[O] measurements from the real home:

- `posse.db` is 512 MB after 1,335 Tasks. Daily backups grew from 150 MB to 489 MB in six days. Only `events` has a prune query.
- 835 `task.toml` files exceed 100 KB; `project.toml` is 191 KB; `notice-delivery.toml` is 6 MB.
- This Task's plan embeds ten skill bodies and the 37 KB Brief as base64 inputs, and `task.toml` carries the plan twice.

[O] code sizes: the TOML snapshot layer is 1,539 lines (`internal/store/snapshot.go`); migration renumbering is 792 lines (`internal/store/migration_compat.go`); SQLite tuning (transactions, locks, schema guard) is about 500 lines.

[I] So the cost ranks as:

1. **The domain being persisted** (review, validation, delivery): 30 tables and most migrations after number 43.
2. **Dual persistence**: every committed change is also written to TOML so that `posse recover --rebuild` can rebuild a lost database. The snapshot is written after commit, so it can diverge, and one path already reads the previous snapshot as a source of truth (`internal/store/snapshot.go:364-383`).
3. **Frozen bytes**: freezing a plan is right; storing skill bodies per Task four times is not. The launch-skills directory is already content-addressed by digest (E2.9), so a digest would do.
4. **Migration discipline under parallel agent development**: 95 migrations in 43 days, 63 of them in October. Duplicate version numbers broke main (commit `1aa710da`), which led to `scripts/preflight.sh`, the version-uniqueness test, and is named in ADR 0019 as the reason for requiring a current target.
5. **SQLite itself**: small.

### Are migrations blocking evolution?

[I] Not yet, but the pressure is visible. Every migration number is a global identifier that a Worker's branch build must agree with, because Workers run a different binary against the shared database ([O] `internal/app/migration_gate.go:18-58`: Workers can never migrate the shared home). Removing review tables will itself need migrations and history-reading compatibility, as the no-mistakes removal did ([O] `internal/config/review.go:10-12`).

### What could go and what must stay

| Change | Tables and columns that could go | Guarantees affected |
|---|---|---|
| Built-in code review moves out | 16 review and validation tables, `tasks.reviews_task_id`, `tasks.autonomy_review`, verdict and resolution Signal paths, review sections of `task.toml` | None of the execution guarantees. Merge needs one replacement: an external verdict bound to a head (section G) |
| Task type becomes properties | `tasks.type` as a branch key, the `done -> reported` special case, `merging`, `merged`, `reported` as Task states | None. Delivery state moves to `deliveries` |
| Snapshots slim to identity plus digests | most of `snapshot.go`, 6 MB and 200 KB files | Rebuild-from-snapshot recovery. Daily `VACUUM INTO` backups already exist as the alternative [O: `internal/store/snapshot.go:724-774`] |
| Plan stores digests for skills | about 80% of each `task.toml` and plan row [H: estimated from this Task's inputs] | None |

**Must stay** [I]: projects, tasks (identity, state, branch, Sandbox, pane and session, launches, plan), transitions, signals, turn tokens, messages, notices and receipts, intents, leases, sandboxes, Lead start claims, recovery counters, one plan revision table, decisions, and deliveries if Posse keeps PR authority.

---

## G. Responsibility boundaries

### Posse and Herdr

**What Posse uses** [O: `internal/herdr/adapter.go`, `internal/app/herdr.go`, `internal/app/worker_tab.go:84`]: `session.snapshot`, `agent.get`, `agent.explain`, `pane.read`, `pane.process_info`, `agent.prompt`, `agent.send_keys`, `agent.wait`, `pane.send_input`, `agent.start`, `pane.clear_agent_authority`, `tab.create`, `workspace.create`, `pane.close`, `tab.close`, rename and metadata calls, `notification.show`, and the plugin event hooks. It requires protocol 22.

**What Posse does not use** [O]: `events.subscribe` and Herdr worktrees appear only in the method list and fakes.

**What Posse re-derives on top of Herdr**

| Posse mechanism | Why Herdr's own signal was not enough | Evidence |
|---|---|---|
| Stall detection | Herdr reports `working`; Posse compares pane text and a worktree fingerprint | [O] `internal/runtime/stall.go:22-30` |
| Quota detection by screen text | A harness at its limit leaves Herdr at `working` with no event | [O] `docs/adr/0034:7`, `internal/app/quota.go:79-186` |
| Model-error recovery by screen text | Not a Herdr state | [O] `internal/app/model_error.go:42-169` |
| Prompt confirmation with retry and Enter | Typed text sometimes stays in the composer | [O] `internal/app/herdr.go:213-285` |
| Agent stop by process group, then authority clear | Herdr has no conditional stop | [O] `internal/app/herdr.go:358-415` |
| Server generation from the socket peer | Needed to detect restart reliably | [O] `internal/herdr/server_generation_linux.go:20-24` |
| Labels as identity | "Herdr renumbers pane ids when it restores a session" | [O] `internal/herdr/adapter.go:417-436` |
| Menu check before steering | Herdr reported `done` while a picker was open | [O] `docs/adr/0006:12` |
| `_guard` on every Worker shell command | "Herdr has no per-pane API scope, token or permission" | [O] `docs/adr/0007:40`, `internal/app/guard.go` (1,846 lines) |

**Is Posse rebuilding Herdr?** [I] Not in layout, terminals or agent detection. It builds a second liveness and identity layer because Herdr does not expose three things: a per-pane permission, a conditional close or stop bound to an identity, and agent states for quota, model error and open menus. These are upstream requests, not Posse features.

**Where the two collide: restore.** [O] ADR 0035 says Posse never restores agents on its own. Herdr does resume agents natively after a restart. Incident #962 (fixed in `85e6a980`): Herdr resumed a Pi Lead as bare `pi` without the `--extension` flag Posse had passed, so the Lead had no Notice receiver; because the extension "owned" delivery, Posse typed nothing as a fallback and every Notice stayed undelivered without a warning. The fix installs a global Pi extension that self-activates after a subprocess check, adds a 30-second "receiver missing" timer and a new Notice.

[I] What #962 reveals: critical behaviour was carried on a process command line that Herdr does not preserve, a delivery mode had no liveness check, and the repair added a machine-wide artifact and a timer. [H] Workers resumed natively by Herdr may lose launch-time state in the same way; a test that restarts Herdr with native resume enabled and a real harness would show it.

**Fragility by the numbers** [O: `git log --format=%s`]: of 227 fix commits, 57 mention herdr, watch, notice, lead, pane, resume or restore, and 71 mention teardown, guard, recover, relaunch, stall, interrupt, orphan, sandbox or pool.

**Test coverage of the boundary** [O]: Herdr restart, CLI crash, Worker exit and Lead adoption have E2E tests with an isolated Herdr server (for example `TestListHoldsRestartUntilUpAll`, `TestRealCLIIntentCrashMatrix`, `TestUpAllKeepsLiveLeadAndAdoptsGeneration`). CI installs Herdr but no real harness; 46 E2E files use fake agents that report their own status. The #962 regression test skips when `pi` is absent. No test covers a reboot. No test runs Workers in two Projects at once under load.

**Recommended boundary**

| Responsibility | Owner |
|---|---|
| Workspaces, tabs, panes, terminal I/O, agent status, notifications, session resume of the harness process | Herdr |
| Which pane belongs to which Task, Task state, Signals, Notices, Decisions | Posse |
| Sandbox, leases, containment, verified stop | Posse (until Herdr offers pane permissions and conditional close) |
| "Is this agent stuck, out of quota, in a menu" | Herdr should own; Posse scrapes meanwhile |
| Bringing agents back after a server restart | One owner. Either Posse disables or wraps Herdr's native resume for panes it launched, or Posse stops restoring and only re-attaches. Today both act |
| Getting a Notice into the Lead | Posse, through one queue with one liveness check, instead of four receiver types |

### Posse and Greenhouse

**What Greenhouse is** [O: `GH:README.md`, `GH:main.go:2008-2053`, `GH:host.go`, `GH:docs/embedding.md`]. A single Go binary: a TUI, a JSON request command (`greenhouse axi`) and a self-started detached state daemon. Standalone it runs intent, sync, review, test, docs, lint, push, PR and CI with fix budgets, and "Greenhouse never merges" (`GH:README.md:257`). It has 141 commits over six days, about 14,700 non-test lines, GitHub only, single repository.

**The integration contract already exists on the Greenhouse side.** [O] "Host mode" lets a host own the run: the host calls `attach`, `apply`, `resolve-rules`, `prepare`, `review`, `watch` and reads a `host_result` with `head`, `target`, `verdict` (`clean`, `findings`, `inconclusive`), `current`, `findings[]` and digests. In host mode Greenhouse refuses the operations the host owns (`GH:host.go:34-44`):

```go
"repair":           "author repair",
"pipeline":         "publication and CI",
"handback":         "candidate custody",
"decide":           "cap decisions",
```

[O] "The Posse adapter belongs to the Posse Project. It uses host mode and the embedding contract." (`GH:docs/implementation-plan.md:125`). [O] The Posse repository contains no reference to Greenhouse (`grep -rli greenhouse`).

**Overlap**

| Responsibility | Posse today | Greenhouse | Natural owner |
|---|---|---|---|
| Independent review of a diff | Review Tasks, `posse-review` skill, `review_loop.*` | Yes, host and standalone | Greenhouse |
| Finding dedup and ledger | Fingerprints without line numbers, patterns, dispositions, 7 tables | Finding ids from a digest that includes line numbers [O: `GH:execution.go:513`] | Greenhouse, after it reaches parity |
| Fix loop | Lead or Posse sends findings to the author Worker | Standalone only; host mode refuses `repair` | Posse (the Worker is the author) |
| Test, docs, lint evidence | `posse validate test|docs|lint` records Worker claims | Standalone runs commands; host mode refuses `pipeline` | **Undecided: nobody in host mode** |
| Private candidate | `validation_candidates` | Standalone worktree; host mode says the host owns custody | Posse, or dropped with full-pipeline |
| Push and PR | `posse publish`, Deliveries, GitHub and GitLab | Standalone, GitHub only | Posse |
| CI watch and CI fix rounds | `ci_loop`, flake handling, reruns | Standalone | Posse |
| Merge queue, merge authorization, Autonomy | Posse only | Never merges | Posse |
| Report review for spec and tickets | Reuses Review Tasks | Reviews committed diffs only | Posse, as a plain Research Task with a checklist, or dropped |

**Gaps that block a direct swap** [O unless marked]

- Greenhouse reviews the whole merge-base diff each round (`GH:docs/embedding.md:41`); Posse reviews only the delta after round 1 (`docs/adr/0024`).
- Atomic re-review and quota gating for host runs exist only as unmerged drafts in the Greenhouse Project state.
- Workspace (multi-repository) review is deferred in Greenhouse.
- Greenhouse's `origin` means `correctness|policy`, not Posse's `introduced|pre_existing`; it has no Finding Patterns.
- [H] Greenhouse's detached daemon may interact badly with Posse's per-Task process containment and Teardown. A host-mode run inside a Sandbox would show it.

**Couplings Posse must cut or keep readable** [O]

- `requireNoBlockingReviewFinding` runs for every Task at merge (`internal/app/review_verdict.go:273-290`).
- `validation_fix_rounds` is shared by review and CI (`kind IN ('gate','ci','review','freshness')`).
- Decision kinds `review` and `review_cap`, with the only Lead-answerable Decision.
- `branch_author_invalidated_verdicts` joins review verdicts.
- Frozen plan fields `Review` and `ValidationLoops` in admitted Tasks.
- Lead instructions, Worker protocol text, Signal flags `--findings`, `--resolutions`, `--verdict`, the doctor check and the `review` Dispatch Rule type.

**The boundary**

Posse keeps what follows from being the author's supervisor and the holder of merge authority. Greenhouse owns the judgment about a diff.

- **Posse owns:** Task and Worker, Sandbox, publish and PR, CI observation and CI fix instructions, merge authorization, merge queue, and one gate: "this Task requires an external verdict for this exact head before Merge".
- **Greenhouse owns:** reviewing a head against a target and intent, rules, findings, their identity across rounds, the reviewer agent and its confinement.
- **The hook Posse needs** [I]: store one insert-only record per verdict (`head`, `target`, `verdict`, `current`, digests, a grant Posse issued) and check it in the same place `requireSelectedReviewForMerge` checks today. ADR 0020 already designed this shape for no-mistakes receipts. Findings travel to the author as a file through `posse send --file`.
- **The decision to make first:** who produces test, docs and lint evidence when review is external. Either `full-pipeline` becomes "Greenhouse review plus forge CI" as ADR 0028 already states for verification, or Greenhouse gains a host-mode gate stage.

---

## Scenario evaluation A-H

Each scenario lists the expected experience, what the current implementation does, where it is more complicated than needed, the guarantees involved and what it shows.

### Scenario A. Simple research

**Expected.** The User asks for a comparison of PostgreSQL connection pooling approaches. The Lead starts one research Worker and returns a report.

**Current** [O: E1.7 to E1.12, E4.1 to E4.6, fresh home, no config]

1. The Lead writes a file with `type: research`, `title`, `done_when` and the body.
2. `posse dispatch` previews `profile: implicit:claude` (67 ms).
3. `posse start` returns `state: working` in 3.8 s. It created a git worktree and branch `posse/t1`, a Herdr tab, a cgroup, and a 686-word `launch.md`.
4. The Worker must finish with `posse signal done ... --report <file>`; without `--report` it gets `signal_invalid`.
5. After Teardown, `report.md`, `report-manifest.json` and attached files remain in the Project state.

**Unneeded complication.** The mandatory `type` (E1.2). The worktree and branch for work that changes nothing; they do give isolation and a place for attachments, and cost under four seconds. Change-flavoured protocol text. Under the User's real config, step 2 instead returns `profile_required` and the Lead must pass `--profile` (E2.6, X2).

**Guarantees.** Isolation, one Worker per Task, a durable Report, no push.

**Shows.** The core path is already close to Experience A in the request. The distance to it is one required key and, once rules exist, one forced choice.

### Scenario B. Parallel research

**Expected.** Three Workers research three approaches; the Lead compares the results.

**Current** [O: E3.2 to E3.8]. Three more starts succeeded, each in 3.8 s, each as a tab in the Lead workspace. The fifth Worker was refused with `worker_limit` and admitted with `--queue`. Each Worker signals on its own; the Lead gets one Notice per Report.

**Unneeded complication.** Three Brief files and three commands are proportionate. There is no "fan out and join" concept; the Lead joins by reading three Reports, which is appropriate [I]. The `posse-research` Lead skill prescribes "2-4 source directions", which fits. The default cap of 4 is the only wall.

**Guarantees.** Capacity admission in one transaction; separate Sandboxes.

**Shows.** Parallel dispatch is natural. The limit on native research children (two, D7) is the nearby friction for a Worker that wants to fan out itself.

### Scenario C. Implementation

**Expected.** The Lead splits a feature into independent components, one Worker each, with clear ownership.

**Current** [O: E2.3, E2.4, `lead.md`, this Task]. One Change Brief per component; each gets a Sandbox and a branch `posse/t<n>`; `after: [t12:landed]` orders dependent work; `ticket:` links an issue. After `done`: publish, PR, CI watch, CI fix rounds, merge by Autonomy. With `review = "full-pipeline"` the Worker also runs candidate, Review Rounds and three validate stages.

**Unneeded complication.** None in the split itself. The cost is everything after `done`, and it scales with the review mode. In the real home, 549 Review Tasks accompanied 669 Change Tasks.

**Guarantees.** Isolation, branch authorship, no Worker merges, exact-head evidence.

**Shows.** This is the scenario Posse was built around, and the one where built-in review dominates the experience.

### Scenario D. Image generation

**Expected.** "Generate a set of images" goes to the image configuration and only image work goes there.

**Current** [O: E2.1 to E2.5, E2.10, X1, X2, E4.5]

- `posse dispatch` returns `profile_required` and lists the image rule first among four candidates. The Lead must choose.
- Starting the image Brief on `pi-luna` works. Starting a bug fix on `codex-sol` works.
- With typed Project rules, the image rule is not listed.
- The Lead must classify the work as `change` or `research`. Neither role's skills mention images.
- Images written into a Research Sandbox are saved as Report attachments (E4.5 kept `assets/logo.png`).

**Unneeded complication.** The whole selection path. Section E.

**Guarantees.** None protect the intent. The launch itself is exact once chosen.

**Shows.** Posse can run an image Worker and keep its output. It cannot express "this Worker is for images" or honour it.

### Scenario E. Mixed work

**Expected.** One research Worker, one image Worker and one implementation Worker report to one Lead.

**Current** [O: E2.8, E6.8]. Works mechanically: one Project ran research Workers on claude, a `pi` Worker and a `codex` Worker at once, as tabs of one workspace, with independent Signals. Dependencies between them are expressed with `after:`.

**Unneeded complication.** Three classifications into three types; three different skill sets fixed by type; three different completion rules. The image Worker has no type of its own.

**Guarantees.** Isolation and independent lifecycles.

**Shows.** Different Workers can do different work. They cannot do it without being sorted into the delivery taxonomy first.

### Scenario F. Unstructured exploration

**Expected.** "Explore the codebase and tell me what you discover" starts a Worker with an open brief.

**Current** [O: E1.1 to E1.4]. Prose without front matter is refused. Without `type` or `done_when` it is refused. A type outside the three is refused. With `type: research` and a loose `done_when` such as "findings are reported" it starts.

Alternatively the Lead explores by itself or through read-only research children, and the default guidance allows that ("Investigate repository facts before asking the User").

**Unneeded complication.** `done_when` is required. For exploration a truthful value is "report what you found", which is acceptable but formal. The real constraint is that exploration which turns into a change needs a second Task of another type; there is no promotion. Firstmate has `fm-promote.sh` for scout to ship.

**Guarantees.** A Worker always has a stated stopping condition, which prevents open-ended runs.

**Shows.** Posse accommodates exploration as research. The friction is the form, not the model.

### Scenario G. Interrupted execution

**Expected.** Nothing is lost; the Lead learns what happened; recovery is one explicit step.

**Current**

| Event | Observed behaviour | Evidence |
|---|---|---|
| Worker pane closed | Task stays `working`, shows `agent: no pane`, one Notice is pending; `posse relaunch` reuses the same Sandbox and starts launch 2 | [O] E5.1 to E5.4 |
| Herdr server stops | `posse` and `posse list` still answer from the database and show the last cached status (`idle 3s`); `posse send` and `posse start` fail with `herdr_unavailable`, retryable | [O] G2, G4, G5 |
| `posse start` during the outage | The command fails after 5.5 s, but leaves Task `t2` in state `failed` with a Decision "relaunch or discard" | [O] G3, G4, G6 |
| Herdr restarts | The Project is held; `posse up --all` restores the Lead and the Worker in 6.7 s | [O] G8 to G10 |
| Lead restart, CLI crash, quota exhaustion | Covered by E2E tests, not rerun here | [O] `internal/e2e/generation_adoption_test.go`, `intent_crash_test.go`, `quota_fallback_test.go` |
| Herdr native resume of a Pi Lead | Lost Notice receiver until `85e6a980` | [O] commit message and diff |
| Machine reboot | No test | [O] grep |

**Unneeded complication.** A start that fails because Herdr is down still creates a failed Task and a User Decision; refusing before admission would leave nothing to clean up [I]. During the outage the status column shows stale values without saying so. Restore competes with Herdr's own resume (section G).

**Guarantees.** No automatic restart of agents, no lost Task, no double relaunch, Sandbox preserved. These held in every experiment.

**Shows.** This is where the architecture's complexity pays for itself, and where most of the remaining fragility lives. The complexity is proportionate for Task and Sandbox ownership and high for Notice delivery and Watch supervision.

### Scenario H. Multiple Projects

**Expected.** Each Project has its own Lead and Workers; nothing crosses.

**Current** [O: E6.1 to E6.9]

- Two Projects produced two Herdr workspaces (`w1`, `w2`), each with its own Lead pane and Worker tabs.
- Task ids are per Project (both had `t1`).
- A Lead pane running `posse start` in the other Project's root got `lead_only`; `posse up` there got `lead_workspace_conflict` with the instruction to open a new workspace; `posse send` got `lead_only`. There is no `--project` flag on `show`.
- `max_workers` is per Project.

**Shared across Projects** [O]: one database file and one writer lock, one global `posse.db.mount-lock` for all Sandbox operations, one `config.toml`, one Herdr server (a restart holds every Project), one plugin and binary path, one Worker Go cache, global hooks. Migrations are refused while any Project has a live Task.

**Unneeded complication.** None for the Lead. For the User, global Dispatch Rules apply to every Project unless typed Project rules replace them, which is how the image rule disappears in one Project and can be picked for reviews in another (section E).

**Guarantees.** One Lead per Project; no cross-Project command; leases checked across Projects.

**Shows.** The Project boundary is the clearest and best-enforced concept in the product. Its weak point is configuration scope, not execution.

---

## Concept audit

"User" and "Lead" columns say whether that party must understand the concept to do ordinary work. Verdicts: **keep**, **hide** (keep the mechanism, remove it from what the party must know), **merge**, **move** (to another product or layer), **drop**.

| Concept | User | Lead | Real invariant behind it | Overlaps with | Verdict |
|---|---|---|---|---|---|
| Project, Lead | yes | yes | one conversation and one authority per codebase | none | keep |
| Worker | yes | yes | one agent, one assignment | Task | keep |
| Task, Task ID | id only | yes | identity for state, Sandbox and history | Worker (1:1 at a time; relaunch gives a Task several Worker instances) | keep |
| Task Name, Task Branch | no | name only | branch must not depend on a label | each other | hide |
| Task types (change, research, review) | no | yes | three separate facts: writes to the Project, requires a Report, has a Delivery | skill role, rule type | replace with properties (D3) |
| Brief | no | yes | a stated scope and stopping condition | launch.md, plan | keep; make only the body and a stopping condition required |
| Frozen plan | no | no | relaunch runs what was admitted | Brief, config, task columns | hide; store digests |
| Profile | yes | yes | a named model and effort with a fallback | Kind, Dispatch Rule | keep; add purpose (E-b) |
| Kind | rarely | no | how to drive one harness | Profile | hide |
| Dispatch Rule, dispatch default, inheritance, review profile | yes | yes | none; advisory | each other, four mechanisms for one question | merge into "Profiles with a purpose plus one default" |
| Platform skills | no | no | Posse CLI knowledge for agents | none | keep; split Lead-only from Worker |
| Workflow skills per role | yes | no | none; a default mapping | Task type | keep as defaults; add a per-Task channel |
| Workflow profile, Frame, Stage | optional | optional | User decides stage transitions | Lead skills (`posse-spec`, `posse-tickets`, `posse-build`) | move to skills; frames are descriptive [O: `skills` and `workers` fields are opaque metadata, `docs/adr/0027`] |
| Signal | no | yes | only the Worker declares its state | none | keep |
| Notice | no | yes | the Lead must not miss a state change | Signal (most Notices wrap one) | keep; reduce kinds |
| Decision | yes | yes | recorded authority for irreversible acts | approval, Autonomy | keep |
| Report, Report Bundle | Report only | yes | output survives Teardown | attachments | keep; Bundle is an implementation detail |
| Publication-bound Report, Report Acceptance | optional | optional | exact-digest approval of a spec | Decision | merge into Decision |
| Review Task, Verdict, Mode, Loop, Round, Fix Round, Finding Pattern, Finding Ledger, Private Candidate | yes today | yes today | a merge needs evidence about an exact head | Greenhouse | move; keep one "external verdict" gate |
| Pool, Sandbox | Sandbox | Sandbox | isolation and fast start | none | keep; Pool is an implementation detail |
| Merge, Merge Mode, Merge Queue, Delivery, Steward, Orphan, Closing Issue | merge only | yes | no unauthorized or stale merge; nothing lost after merge | each other | keep as one optional Delivery capability with fewer names |
| Autonomy | yes | yes | standing permission is explicit | Decision | keep |
| Watch, Stall, Restore | restore only | Watch today | observation without a daemon | Herdr status | hide the Watch from the Lead contract |
| Preferences, Knowledge, Conversation handoff, Identity | yes | yes | none; convenience | each other, harness memory | keep optional; three text layers are one concept to a User ("things my Lead should remember") |
| Research Child | no | yes | no untracked writer | Worker | keep; document the limit |
| Launch Identity Record | no | no | audit of what ran | plan | hide |

**How many concepts for a simple action?** [I] To start one research Worker on a configured home, the Lead coordinates: Brief, type, `done_when`, Dispatch Rules, Profile, Task Name, capacity, Signal, Notice, Report. Ten, of which four (type, rules, Profile, name) exist for the system. To land one small change under full-pipeline, add: publish, candidate, Review Round, verdict, findings, fix round, three validate stages, promotion, PR, CI loop, merge, Autonomy. About twenty.

**Has Posse become a workflow engine, project manager or policy engine?** [I]

- Workflow engine: no. ADR 0027 refused one, and workflow profiles are a recipe with opaque fields.
- Project manager: partly. Queue, dependencies, tickets, spec completion Notices and a board exist, all optional.
- Policy engine: yes, for delivery. Review mode, Autonomy, caps, candidate rules and merge conditions form a policy system with its own state, and it is the part that grew fastest.
- Execution framework: yes, and that is the product.

---

## Design tensions

### Flexibility vs reliability

[I] They conflict less than the current design suggests. Every reliability guarantee in section F attaches to the Task's identity, Sandbox and state, none to its type, skills or Profile. The Lead can be free in what it asks a Worker to do while Posse stays strict about where the Worker runs and how it stops.

### LLM judgment vs deterministic enforcement

| Decision | Should be | Today |
|---|---|---|
| Delegate or do it directly | Lead | Lead |
| How to split work | Lead | Lead |
| What the Worker needs to know (skills, context) | Lead, within a User-approved set | fixed by type |
| Which Profile | Lead, recorded, with reserved Profiles enforced | Lead, unrecorded, unenforced; or first candidate |
| Where the Worker runs, what it may touch | Posse | Posse |
| When a Task is complete | Worker Signal, checked by Posse | same |
| Merge, discard, config authority | User, enforced by Posse | same |
| Whether a skill was "used" | not mechanically knowable | enforced by tool-call count |

[I] Posse enforces one thing it cannot know (skill use) and leaves unenforced one thing it could know (a reserved Profile).

### Task structure vs natural delegation

[I] The structure a Worker needs: an assignment, a stopping condition, where output goes. The structure recovery needs: an id, a Sandbox, a state, a launch record. Everything else in the Brief's 25 keys is delivery or workflow policy. Structure becomes an obstacle at the point where the Lead must choose a type to obtain skills and a pipeline.

### Configuration vs intent

[I] The User should configure standing facts: which models exist, what each is for, what authority the Lead has. The User should not have to encode task-to-Profile routing as rules; that is the Lead's judgment once Profiles describe themselves. Intent arrives in the conversation and should reach the Worker as the Brief body plus Lead-chosen skills.

### Persistence vs complexity

[I] Posse must remember identity, state, custody, authority and messages. It additionally remembers a review ledger, validation evidence, candidates and a second copy of everything in TOML, because its abstractions require them.

### General-purpose dispatch vs specialised workflows

[O] A Task without a Git repository cannot exist ("Posse runs Tasks only in Git repositories and Workspaces"; a Directory Project admits no Task, `docs/adr/0038`). [O] Research with attachments works and survives Teardown (E4.5). [I] So non-diff work is possible inside a repository and impossible outside one. The core is coupled to Git for isolation, which is defensible, and to pull requests for vocabulary, which is not.

### Posse vs Herdr, Posse vs Greenhouse

Section G.

---

## H. Product principles

1. **The User talks to the Lead and nothing else.** Any concept the User must learn in order to get a Worker started is a defect to remove or hide.
2. **Strict about where, free about what.** Posse enforces isolation, custody, identity, completion and authority. The Lead decides what to delegate, to whom, with which context.
3. **One assignment, one Worker, one stopping condition.** Nothing else is required to start.
4. **A named Worker configuration says what it is for.** If the User intends a Profile for one kind of work, Posse can show that and refuse the opposite.
5. **Every automatic choice is visible and every Lead choice leaves a reason.** No selection happens by list position.
6. **Enforce only what Posse can verify.** If Posse cannot tell whether something happened, it reports instead of blocking.
7. **Delivery is a capability a Task may use, not the shape of every Task.** Research, assets and exploration do not carry pull-request vocabulary.
8. **Posse supervises authors. Judging their code belongs to Greenhouse.** Posse holds the merge gate and accepts a verdict bound to an exact head.
9. **Herdr is the environment.** Posse asks Herdr for missing signals before building its own, and exactly one of them owns each lifecycle step.
10. **Remember what recovery and authority need.** State that exists to serve an abstraction goes when the abstraction goes.

---

## I. Architectural directions

Three directions differ in what they treat as the core. A fourth option, a rewrite toward Firstmate's file model, is rejected first.

### Rejected: files and prose instead of a database and a CLI

Firstmate's history is the evidence. 63% of its commits are fixes; the largest classes are record atomicity, lock storms, lost signals and screen reading (section C). Posse already has what those fixes were reaching for.

### Direction 1. Repair in place

**Idea.** Keep the model. Fix the defects found: the three dispatch precedence problems, mandatory irrelevant skills, platform skills for Workers, change-flavoured research text, plan bytes, the failed Task on `herdr_unavailable`. Add a purpose to Profiles and a reason to starts.

| | |
|---|---|
| Problem addressed | D2 (enforcement), D4, D6, D8, parts of F |
| Complexity removed | Little; some rule mechanisms merge |
| Guarantees preserved | All |
| Lost | Nothing |
| New risk | Low. The delivery pipeline keeps growing; the type hub stays |
| User and Lead experience | Fewer surprises; the same vocabulary |
| Evidence that validates it | Scenario D acceptance tests pass (section K) |
| Depends on | The User accepting that non-code work stays shaped like delivery |

### Direction 2. Complete ADR 0027: a dispatch core with delivery and review as attachments

**Idea.** A Task is an assignment with an id, a Sandbox, a state and a Worker. Three independent properties replace the type: whether it writes to the Project, whether a Report is required, and whether it has a Delivery. Delivery (publish, PR, CI, merge authority, merge queue) becomes an optional record a Task may open, with its own states. Built-in code review leaves; a Task with a Delivery may require an external verdict from Greenhouse for its exact head. Skills are a default set per property plus a per-Task list the Lead chooses from a User-approved set. Profiles carry a purpose. Lead instructions load the delivery section only when the Project uses Delivery.

| | |
|---|---|
| Problem addressed | D1, D2, D3, D4, D5, the Greenhouse overlap, most of F |
| Complexity removed | 16 review tables and about 10,000 lines of review and validation code; three Task states; the type as a branch key at 177 sites; four rule mechanisms; about half of the Lead's standing obligations for Projects without Delivery |
| Guarantees preserved | Isolation, custody, completion, authority, exact-head evidence before merge (through the external verdict), CI evidence |
| Lost | Delta-only re-review and Finding Patterns until Greenhouse has them; `posse validate test|docs|lint` unless kept as Delivery evidence; Report review for specs as a built-in |
| New risk | Dependence on a six-day-old product; a migration that must keep 1,335 historical Tasks readable; a period with two review paths |
| User and Lead experience | Research, assets and exploration read as plain assignments. Code changes keep PR, CI and merge. Review appears as "Greenhouse says clean for this head" |
| Evidence that validates it | A Greenhouse host-mode run inside a Posse Sandbox with a real harness; the scenario suite in section K |
| Depends on | Greenhouse reaching host-mode re-review; a decision on who owns test, docs and lint evidence |

### Direction 3. Session-first: Posse as a thin Herdr-native layer

**Idea.** Go further than Direction 2. A Worker is a Herdr pane with a label, a worktree and an inbox. Posse keeps only identity, custody, messages and authority. Delivery leaves too, to the Worker's own use of `gh` under a push guard, or to Greenhouse standalone, which already runs push, PR and CI.

| | |
|---|---|
| Problem addressed | Everything in Direction 2 plus the delivery code itself |
| Complexity removed | A further 10,700 lines and 14 tables |
| Guarantees preserved | Isolation, custody, completion |
| Lost | Branch authorship, exact-head gating, PR stewardship, CI fix rounds, merge queue, GitLab support, "a merged PR merges its Task" |
| New risk | High. Merge authority is the User's main safety control, and Greenhouse standalone is GitHub-only and single-repository |
| User and Lead experience | Lightest. The Lead must supervise delivery by judgment |
| Evidence that validates it | A month of real use where no unauthorized or stale merge occurs without the delivery layer |
| Depends on | Herdr adding pane permissions and conditional close; Greenhouse maturing; the User accepting weaker merge guarantees |

### Recommendation

**Direction 2, reached through Direction 1.**

Reasons, each from the evidence:

- The guarantees that held in every experiment (E3, E5, E6, G) belong to the execution core that all three directions keep.
- The friction the request describes maps to D1 to D4. Direction 1 fixes the defects in D4 and part of D2 and does not touch D1 or D3, which are the cause.
- Direction 3 gives up merge authority and exact-head evidence. The request says not to remove safety mechanisms to look simpler, and the delivery layer is where the User's authority is enforced.
- ADR 0027 already records the User's own statement of Direction 2 [O: quoted in `docs/adr/0027`: "Posse chỉ nên là lớp kiểm soát việc dispatch các worker"]. The implementation diverged; the direction did not.
- Direction 1's fixes are small, independent and needed under every direction, so they come first and carry no migration risk.

**What would change this recommendation** [I]: if Greenhouse cannot offer host-mode re-review with stable finding identity, keep Posse's ledger longer and still detach it from the Task type. If the User's real work is almost entirely code changes with review, Direction 1 may be enough; the Task mix (41% reviews, 50% changes, 9% research) says the non-code share is small today, but the request states the intent to grow it.

**A challenge to this recommendation.** Replacing one type with three properties could create eight combinations where there were three, and more Lead decisions. The answer is a default, as Firstmate has: a Task with no properties stated writes to its Sandbox, may open a Delivery, and reports in whatever form the Brief asks. The Lead states a property only to restrict.

---

## J. Prioritized improvement opportunities

Priority: **P1** fix now under any direction; **P2** next, needs a short design; **P3** after the direction is confirmed. "Validate first" is what to check before implementing.

### Dispatch correctness

| # | Improvement | Why it matters | Validate first | P |
|---|---|---|---|---|
| J1 | Review auto-start must not take the first candidate from conditional rules. Use `review_loop.profile`, an unconditional `review` rule, or stop with a Notice | Can send reviews to the image Profile (section E, cause 1) | Reproduce with `TestReassessReviewCandidateOrder`; the historical share is unknown, so add a start-source field to confirm the effect after the fix | P1 |
| J2 | Show generic rules beside typed rules in `profile_required`, or state the precedence in `posse config show --effective` and `posse doctor` | Hides the image rule (cause 3, X1) | Confirm no Project relies on typed rules to suppress generic ones | P1 |
| J3 | An unconditional rule or `dispatch.default` is the default when the Lead passes no `--profile`; conditional rules are listed as alternatives in the output | Adding a rule should not remove a default (E2.6, E2.7) | Decide whether silent defaults are acceptable when conditional rules exist; the Lead still reads the list | P1 |
| J4 | Record why a Profile was chosen (`--because`), separately for Lead choice, auto-start and default; show it in `posse show` and `posse stats` | No audit trail (1,208 of 1,335 Tasks say only `--profile`) | Check that Leads supply a reason reliably in an E2E with a real harness | P2 |

### Agent capability and skill selection

| # | Improvement | Why it matters | Validate first | P |
|---|---|---|---|---|
| J5 | Add `description` and a reservation (`only_for`) to a Profile; refuse a reserved Profile for auto-start and for starts whose reason does not name its purpose | Makes "for image work only" a fact (section E) | Measure Lead selection accuracy on a fixed set of Briefs before and after (section K) | P2 |
| J6 | A per-Task skill list in the Brief, limited to a User-approved set, frozen and shown in `posse dispatch` | The Lead cannot give one Worker one skill (E1.5) | Confirm injection works for all three kinds; decide the allowlist key | P2 |
| J7 | Require only skills whose step the Task reaches, or report unused skills instead of refusing | `posse-tdd` is mandatory on a docs-only Task (D2) | Check whether the refusal has improved real Worker behaviour since `6bd85306`; `task_skill_uses` has the data | P1 |
| J8 | Stop loading `posse-setup`, `posse-down`, `posse-update` into Workers | About 1,900 words of Lead and User actions a Worker may not take | None | P1 |
| J9 | Let a Profile carry skills that load whenever it runs | Capability and know-how travel together (cause 5) | After J5 and J6 | P3 |

### Product experience

| # | Improvement | Why it matters | Validate first | P |
|---|---|---|---|---|
| J10 | Make `type` optional with a default; accept a Brief with a body and a stopping condition | Removes E1.2 and the first step of the type hub | List the 177 sites; decide the default | P2 |
| J11 | Generate the Worker protocol and Lead obligations from what the Task and Project use: no PR text for research, no review text without review, no Posse-development isolation advice outside the Posse Project | 686 words for a 14-word assignment; 43% of obligations are delivery (D5, D6) | Compare word counts and run the first-outcome E2E | P1 |
| J12 | Raise the `max_workers` default or make research Workers cheap against it; consider a machine-wide cap | Parallel research stops at five (E3.5) | Check quota and machine load at 8 Workers | P2 |
| J13 | Allow a Research Task to continue as a Change Task in the same Sandbox | Exploration that becomes a change needs a new Task today (Scenario F) | After J10 | P3 |

### Architecture and domain model

| # | Improvement | Why it matters | Validate first | P |
|---|---|---|---|---|
| J14 | Replace the Task type with properties; move `merging`, `merged`, `reported` to the Delivery record | D3; removes the hub | A design that keeps historical Tasks readable; a spike on the `done` transaction | P3 |
| J15 | Make Delivery an optional attachment with its own instruction section | D1 | After J14 | P3 |
| J16 | Fold Workflow profiles, Frames and Stages into Lead skills | Their fields are opaque metadata; the Lead skills already implement the recipe | Check whether any Project uses `[workflows]`; none of the real configs read for this report do | P2 |

### Greenhouse integration

| # | Improvement | Why it matters | Validate first | P |
|---|---|---|---|---|
| J17 | Decide who owns test, docs and lint evidence when review is external | Host mode refuses `pipeline`; removing `posse validate` leaves a gap (section G) | A User decision | P1 |
| J18 | Add the external-verdict gate: one insert-only record bound to head and target, checked where `requireSelectedReviewForMerge` checks today; run it beside the built-in loop first | Lets Greenhouse replace Review Tasks without touching merge safety | A host-mode run in a Sandbox with a real harness; confirm the Greenhouse daemon survives or is contained by Teardown | P2 |
| J19 | Remove Review Tasks, the Finding Ledger and `posse review ...` once J18 has run in real use | 16 tables, about 10,000 lines, 41% of all Tasks | Greenhouse host re-review merged; finding identity stable across a line shift | P3 |

### Herdr integration

| # | Improvement | Why it matters | Validate first | P |
|---|---|---|---|---|
| J20 | One Notice queue with one liveness check for all Lead kinds | #962 was a receiver with no liveness check; four mechanisms exist | Prototype with the durable-inbox-plus-doorbell pattern already used for `posse send --file` | P2 |
| J21 | One owner for restore: disable or wrap Herdr native resume for Posse-launched panes, or stop restoring and re-attach | Two systems resume agents (section G) | Test native resume of a Worker with a real harness | P2 |
| J22 | Ask Herdr for pane-scoped permissions, a conditional close bound to identity, and agent states for quota, model error and open menu | Each removes a screen-reading heuristic or part of `guard.go` | Upstream discussion | P3 |
| J23 | Refuse `posse start` before admission when Herdr is down; mark cached status as stale during an outage | A failed Task and a Decision are created for nothing (G3) | None | P1 |

### State and persistence

| # | Improvement | Why it matters | Validate first | P |
|---|---|---|---|---|
| J24 | Store skill and Brief inputs in the plan by digest; keep one plan copy | 835 snapshots over 100 KB; 512 MB database | Confirm relaunch can always resolve a digest from `launch-skills/` | P2 |
| J25 | Slim TOML snapshots to identity and digests, or retire them in favour of the daily backup | 1,539 lines and a second source of truth | Decide the recovery objective: is "rebuild from snapshots" needed when a daily `VACUUM INTO` exists? | P2 |
| J26 | Add retention for notices, signals, transitions and receipts | Only `events` is pruned | Decide how much history the User wants | P2 |
| J27 | Make the Sandbox state lock per Project | One global lock serializes all Projects | Check the cross-Project lease invariant still holds | P3 |

### Reliability and maintainability

| # | Improvement | Why it matters | Validate first | P |
|---|---|---|---|---|
| J28 | Run at least one real harness in CI for the Herdr boundary tests | The #962 regression test skips without `pi` | Cost and secrets policy | P2 |
| J29 | Add a two-Project concurrent E2E and a reboot E2E | Neither exists (section G) | None | P2 |
| J30 | Align code and docs for the recommended `review` skill set; update ADR 0015's "no timer service" | Documentation disagrees with the implementation | None | P1 |

---

## K. Validation strategy

The measures below are scenario outcomes. None counts files, tables or features.

### Scenario acceptance tests

Each runs against an isolated Herdr server and `POSSE_HOME`. Tests marked (real) need a real harness, because selection and skill use depend on model behaviour.

| Id | Scenario | Setup | Pass when |
|---|---|---|---|
| V-A | Simple research | fresh home | The Lead starts a Worker from a Brief that has only a body and a stopping condition. Words of system text the Worker reads before the assignment: no more than 300 (686 today). Lead commands: no more than 2 |
| V-B | Parallel research | fresh home | Five independent research Workers run at once without a config edit or a refusal |
| V-C | Implementation | Project with PR delivery and Greenhouse review | A change merges with an external verdict bound to the merged head; the Lead ran no review-specific command |
| V-D1 | Image routing (real) | the User's Profile set with `codex-sol` reserved for images; 20 Briefs: 5 image, 15 other | 5 of 5 image Briefs run on `codex-sol`; 0 of 15 others do. Run three times. Baseline today for comparison |
| V-D2 | Reserved Profile | same | `posse start --profile codex-sol` on a non-image Brief is refused or needs an explicit override that is recorded |
| V-D3 | Review never borrows | same, no `review_loop.profile` | An auto-started review does not run on `codex-sol` (fails today: `TestReassessReviewCandidateOrder`) |
| V-D4 | Rule visibility | global generic image rule plus typed Project rules | `posse dispatch` for an image Brief shows the image Profile (fails today: X1) |
| V-E | Mixed work | one Project | Research, image and implementation Workers run together; each Worker's loaded skills match what the Lead listed; none carries text about a pipeline it does not use |
| V-F | Exploration | fresh home | A one-sentence exploratory request starts a Worker; if it turns into a change, the same Sandbox continues |
| V-G1 | Herdr outage | one running Worker | `posse start` during the outage leaves no Task behind; status output says it is stale; `posse up --all` restores |
| V-G2 | Native resume (real) | Herdr restart with native resume of a Lead and a Worker | Every Notice is delivered or a missing-receiver Notice appears within the stated bound; the Worker keeps its Task identity |
| V-G3 | Reboot | simulated | All Tasks keep state; one explicit command restores |
| V-H | Two Projects | two Projects, four Workers each, concurrent Signals | No cross-Project Notice, pane or Sandbox; Signal latency in one Project is unaffected by a Watch pass in the other |
| V-S1 | Skill fit | docs-only change Task | `done` succeeds without invoking a test-first skill (fails today) |
| V-S2 | Skill choice | any | The Lead adds one approved skill to one Task; `posse show` lists it; another Task started next does not have it (impossible today: E1.5) |

### Standing measures to track per release

| Measure | Today | Why it is not a vanity metric |
|---|---|---|
| Concepts the Lead names to start one research Worker | 10 (concept audit) | Counts what the Lead must coordinate, from a transcript |
| Lead obligation words loaded in a Project with no Delivery | 2,530 | Context the Lead must obey in every turn |
| Share of Tasks whose Profile choice has a recorded reason | 0% | Auditability of routing |
| Share of starts that needed a retry after a refusal | unknown; measure from command logs | Direct friction |
| Share of fix commits touching the Herdr boundary or the delivery pipeline | 57 and 71 of 227 by subject keyword | Where the product breaks |
| Time from a Worker's `done` to the User hearing the outcome | unknown; measure from Notices | The experience the User has |
| Database growth per 100 Tasks | about 38 MB (512 MB / 1,335) | Cost of what Posse remembers |

### What would falsify the recommendation

- V-D1 does not improve after Profiles carry a purpose: then routing needs a deterministic classifier or a resolver model as Firstmate uses, not a description.
- V-C cannot pass because Greenhouse host mode cannot bind verdicts across fix rounds: then keep Posse's ledger and only detach it from the type.
- V-A's word budget cannot be met without removing a safety instruction: then the protocol is necessary and the friction is elsewhere.

---

## Where the evidence contradicts the request's assumptions

The request asks to be challenged. Five of its premises did not hold as stated.

1. **"The Lead selects `codex-sol` for unrelated tasks."** Posse itself can do this without the Lead, through first-candidate Review auto-start [O: code and reproduction R1]. Of the 88 `codex-sol` Tasks, 85 were not image work and 73 were Review Tasks [O]. Auto-start cannot have started the 15 non-review Tasks, 3 of which were image work, so at least 12 non-image starts were Lead choices [I]. How the 73 reviews divide between auto-start and Lead choice is not recorded [H: mostly auto-start].
2. **"SQLite has accumulated a significant amount of state."** It has, but the store is not the cause. The review and delivery model is, and a parallel TOML layer costs more than SQLite tuning [O].
3. **"Posse has become too system-oriented" as a general statement.** The dispatch path on a fresh home is two commands and a three-key file (E1.7, E1.8). The weight is in what happens after `done` for code changes, and in configuration.
4. **"Firstmate's dispatch is flexible."** Its intake judgment is flexible. Its launch is more locked down than Posse's and recently became stricter (`18b20eaf`, 2026-11-08) [O].
5. **"Profile, Kind and Dispatch Rule may be unnecessary indirection."** Kind and Profile are distinct and useful. The indirection that fails is the rule layer, which is four mechanisms answering one question with no data about purpose [O, I].

And two of this report's own recommendations deserve the same doubt.

- **Moving review to Greenhouse trades a mature, heavily tested subsystem for a six-day-old one.** The recommendation is to add the gate first and run both, not to delete.
- **A per-Task skill list could reintroduce the problem ADR 0036 solved** (context cost and misfires) if the Lead adds skills freely. The User-approved set and the `posse dispatch` preview are the bound; V-S2 and the context word count are the check.

---

## Appendix A. Experiment harness and raw results

### Method

- The audited tree was exported with `git archive HEAD` into the Task scratch directory. Three test files were added there and nowhere else. The repository under review was not modified.
- The E2E harness is the repository's own: `newFixtureRoot`, `isolatedE2EEnv`, `herdr.WriteIsolatedConfig`, `startServer` (an isolated Herdr server), the built `posse` binary, and fake `claude`, `codex` and `pi` scripts that report agent status to Herdr, as in `internal/e2e/first_outcome_test.go`.
- Commands:

```sh
go test -tags=e2e ./internal/e2e -run '^TestReassessmentExperiments$' -count=1 -v -timeout 12m      # PASS, 43.96s
go test -tags=e2e ./internal/e2e -run '^TestReassessmentExperimentsTwo$' -count=1 -v -timeout 10m   # PASS, 25.48s
go test ./internal/dispatch -run TestReassessReviewCandidateOrder -v -count=1                       # PASS
```

- The tests assert nothing about product behaviour. They log what the CLI prints. "PASS" means the harness ran to the end.
- Real-home figures come from read-only aggregation over `~/.posse/projects/*/tasks/*/task.toml` and `ls -l`.
- Firstmate and Greenhouse were read, not run.

### Limits

- Fake agents do not make decisions. The experiments show what Posse permits and prints, not what a real Lead chooses. Claims about Lead behaviour rest on the real Task records and are labelled.
- The origin of the 73 `codex-sol` reviews is a hypothesis. Code reading and the candidate-order reproduction establish that auto-start can select the image Profile. Neither the Task records nor the start transitions in a database backup copy record who started a Task.
- One read-only query ran against a backup copy of the database (`~/.posse/backup/`), opened immutable. The live database was not opened.
- Line counts by area use file-name patterns and are lower bounds for logic that lives in shared files.
- Research children did the wide reading for the Firstmate, Greenhouse, state, Herdr and skills sections. Claims used in this report were spot-checked against the cited lines; counts that were not rechecked are reported as given.

### Result index

`$ROOT` is the fixture directory. Output is trimmed to the lines the report relies on.

| Id | Command | Result |
|---|---|---|
| E1.1 | `posse start prose.md` (no front matter) | `brief_invalid: frontmatter: must start with --- and include a closing ---` |
| E1.2 | Brief without `type` | `brief_invalid: type: is required` |
| E1.3 | Brief without `done_when` | `brief_invalid: done_when: is required` |
| E1.4 | `type: exploration` | `brief_invalid: type: must be change, research or review` |
| E1.5 | `skills: [image-generation]` | `brief_invalid: skills: unknown Brief field` |
| E1.6 | `profile: claude` in the Brief | `brief_invalid: profile: unknown Brief field` |
| E1.7 | `posse dispatch` minimal research Brief, fresh home | `profile: "implicit:claude"`, `dispatch_rule: "implicit: lead kind"`, 67 ms |
| E1.8 | `posse start` same Brief | `task: t1`, `branch: posse/t1`, `state: working`, 3.776 s |
| E1.9 | `posse show t1` | includes `review_evidence: selected-policy` for a Research Task |
| E1.10 | `git worktree list` | `$ROOT/posse/pool/shop/sandbox-1 [posse/t1] locked` |
| E1.11 | `launch.md` | 686 words, 4,789 bytes, for a 14-word assignment |
| E1.12 | Task directory | `task.toml` 74,220 bytes |
| E3.2-E3.4 | three more research starts | each `state: working`, about 3.8 s |
| E3.5 | fifth Worker | `worker_limit: shop already runs 4 Workers (max_workers=4)` |
| E3.6 | same with `--queue` | `state: queued`, `wait_reason: Worker capacity is full` |
| E3.8 | Herdr panes | five panes in workspace `w1`, one tab each; Workers in `pool/shop/sandbox-1..4` |
| E4.1 | research `signal done` without `--report` | `signal_invalid: a Research or Review Task requires --report <file>` |
| E4.2 | with `--report report.md` | `signal: done`, `state: reported` |
| E4.4 | `posse teardown t1` | `state: torn-down`, `closed_panes: w1:p2`, `branch_removed: true` |
| E4.5 | files kept | `tasks/t1/report.md`, `report-manifest.json`, `assets/logo.png` |
| E2.1 | image Brief, User's rule shape, no `--profile` | `profile_required`; candidates in order: `codex-sol`, `claude-opus`, `pi-luna`, `pi-luna` |
| E2.3 | image Brief `--profile pi-luna` | accepted; `t6`, `model: openai/gpt-6-luna` |
| E2.4 | bug-fix Brief `--profile codex-sol` | accepted; `t7`, `model: gpt-6.1-sol` |
| E2.5 | image Brief `posse start` without `--profile` | `profile_required` |
| E2.6 | research Brief; typed unconditional rule plus typed conditional rule | `profile_required`; lists `when="science-heavy research"` and `when="" profile=pi-luna` |
| E2.7 | bug-fix Brief; three `when` rules and `dispatch.default.use = "pi-luna"` | `profile_required`; default not applied |
| E2.9 | launch command lines | claude: `--plugin-dir .../launch-skills/<digest>` and `.../claude-skill-uses`; pi: `--skill .../skills`; codex: `-c developer_instructions="Read the frozen Posse skill file ... when relevant ..."` |
| E2.10 | stored rule | `rule="--profile pi-luna"`, `rule="--profile codex-sol"` |
| E6.1-E6.3 | second Project | its own `t1`; lists do not mix |
| E6.4 | Lead A pane, `posse start` in Project B root | `lead_only` |
| E6.5 | Lead A pane, `posse up` for Project B | `lead_workspace_conflict` |
| E6.8 | Herdr panes | Project A in `w1`, Project B in `w2` |
| E5.1 | Worker pane closed | `t2 ... working, no pane`; `undelivered_notices: 1` |
| E5.3 | `posse relaunch t2` | `worker: posse-shop-t2-2`, same Sandbox |
| X1 | image Change Brief; generic User image rule plus typed Project rules | candidates: `claude-opus`, `pi-luna`, `claude-sonnet`; `codex-sol` absent |
| X2 | image Research Brief, same rules | candidates: `claude-opus`, `pi-luna`; `codex-sol` absent |
| X4 | `posse config show` | renders `default: use: pi-luna` nested inside the third rule |
| G2 | `posse` while Herdr is down | answers; shows `t1 ... working, idle 3s` |
| G3 | `posse start` while Herdr is down | `herdr_unavailable`, retryable, 5.471 s |
| G4 | `posse list` afterwards | `t2 ... state: failed` with a three-part reason |
| G5 | `posse send` while Herdr is down | `herdr_unavailable` |
| G6 | `posse` after Herdr restart | `needs_you: t2 failed`, `next_action: Run posse decisions ...` |
| G8 | `posse up --all` | `restored: shop, 1`, 6.684 s |
| G10 | Herdr panes after restore | Lead and Worker panes present in `w1` |
| R1 | `TestReassessReviewCandidateOrder` | `type=review code=profile_required first_candidate=codex-sol` |

### Harness source

The three files below reproduce every row. Place them in a scratch copy of the repository at the paths shown.

<details>
<summary><code>internal/e2e/reassess_experiment_test.go</code></summary>

```go
//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// Experiment harness for the 2026-11-12 reassessment report. It asserts nothing
// about product behaviour; it records what the installed CLI does.
func TestReassessmentExperiments(t *testing.T) {
	root := newFixtureRoot(t, herdr.TestRootName())
	bin := filepath.Join(root, "bin")
	installed := filepath.Join(root, "installed")
	for _, dir := range []string{bin, installed} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(installed, "posse")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"herdr": herdrBinary, "posse": binary} {
		if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	fake := func(name string) {
		agent := `#!/bin/sh
if [ "$1" = "--version" ]; then echo '` + name + ` 0.0.0'; exit 0; fi
printf '%s\n' "` + name + ` $*" >> "$POSSE_TEST_ROOT/launches.log"
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent ` + name + ` --state idle >/dev/null 2>&1
while IFS= read -r line; do
 printf '%s\n' "$line" >> "$POSSE_TEST_ROOT/prompts.log"
 case "$PWD/" in "$POSSE_HOME/pool/"*) herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent ` + name + ` --state working >/dev/null 2>&1 & ;; esac
done
`
		if err := os.WriteFile(filepath.Join(bin, name), []byte(agent), 0700); err != nil {
			t.Fatal(err)
		}
	}
	fake("claude")
	env := isolatedE2EEnv(t, root)
	env = setEnv(env, "POSSE_TEST_ROOT", root)
	env = setEnv(env, "PATH", bin+":/usr/bin:/bin")
	for _, dir := range []string{"home", "posse", "claude", "codex", "pi"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	makeRepo := func(name string) string {
		repo := filepath.Join(root, name)
		if err := os.MkdirAll(repo, 0700); err != nil {
			t.Fatal(err)
		}
		origin := filepath.Join(root, name+"-origin.git")
		for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.test"}, {"commit", "--allow-empty", "-m", "Initial"}, {"init", "--bare", origin}, {"remote", "add", "origin", origin}, {"push", "-u", "origin", "main"}} {
			if out, err := gitCommand(env, repo, args...); err != nil {
				t.Fatalf("git: %v %s", err, out)
			}
		}
		return repo
	}
	client := herdr.NewWithEnv("herdr", env)
	startServer(t, client)
	db, err := store.Open(filepath.Join(root, "posse"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	up := func(name string) (string, store.Project, []string) {
		repo := makeRepo(name + "-repo")
		workspace, err := createWorkspace(client, repo)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Run(context.Background(), "pane", "run", workspace.RootPane.PaneID, "posse up --name "+name+" --yes"); err != nil {
			t.Fatal(err)
		}
		var project store.Project
		if !waitForCondition(40*time.Second, func() bool {
			project, err = db.ProjectByName(context.Background(), name)
			if err != nil || project.LeadPaneID == "" {
				return false
			}
			snapshot, err := client.Snapshot(context.Background())
			if err != nil {
				return false
			}
			for _, agent := range snapshot.Agents {
				if agent.PaneID == project.LeadPaneID {
					return true
				}
			}
			return false
		}) {
			pane, _ := client.Run(context.Background(), "pane", "read", workspace.RootPane.PaneID, "--source", "recent-unwrapped", "--lines", "80")
			t.Fatalf("up %s: %s", name, pane)
		}
		leadEnv := setEnv(env, "HERDR_ENV", "1")
		leadEnv = setEnv(leadEnv, "HERDR_PANE_ID", project.LeadPaneID)
		leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", project.HerdrWorkspaceID)
		return repo, project, leadEnv
	}
	run := func(label, cwd string, env []string, args ...string) string {
		command := exec.Command(binary, args...)
		command.Dir = cwd
		command.Env = taskAuthorGenerationEnv(env)
		started := time.Now()
		output, err := command.CombinedOutput()
		code := 0
		if err != nil {
			code = -1
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			}
		}
		t.Logf("\n===== %s\n$ posse %s\nexit=%d elapsed=%s\n%s", label, strings.Join(args, " "), code, time.Since(started).Round(time.Millisecond), output)
		return string(output)
	}
	write := func(name, contents string) string {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	snapshot := func(label string) {
		snap, err := client.Snapshot(context.Background())
		if err != nil {
			t.Logf("snapshot: %v", err)
			return
		}
		var lines []string
		for _, pane := range snap.Panes {
			lines = append(lines, fmt.Sprintf("pane=%s workspace=%s tab=%s agent=%s cwd=%s", pane.PaneID, pane.WorkspaceID, pane.TabID, pane.Agent, pane.CWD))
		}
		t.Logf("\n===== %s (Herdr panes)\n%s", label, strings.Join(lines, "\n"))
	}

	repoA, projectA, leadA := up("shop")

	// --- E1: ceremony for Scenario A / F on a fresh home, zero config.
	prose := write("prose.md", "I am not sure what the right solution is yet. Explore the codebase and tell me what you discover.\n")
	run("E1.1 plain prose Brief (Scenario F)", repoA, leadA, "start", prose, "--name", "explore-codebase")
	noType := write("notype.md", "---\ntitle: Explore the codebase\ndone_when: findings are reported\n---\nExplore the codebase and tell me what you discover.\n")
	run("E1.2 Brief without type", repoA, leadA, "start", noType, "--name", "explore-codebase")
	noDone := write("nodone.md", "---\ntype: research\ntitle: Explore the codebase\n---\nExplore the codebase and tell me what you discover.\n")
	run("E1.3 Brief without done_when", repoA, leadA, "start", noDone, "--name", "explore-codebase")
	other := write("othertype.md", "---\ntype: exploration\ntitle: Explore the codebase\ndone_when: findings are reported\n---\nExplore.\n")
	run("E1.4 Brief with a type outside the taxonomy", repoA, leadA, "start", other, "--name", "explore-codebase")
	skillsField := write("skillsfield.md", "---\ntype: research\ntitle: Explore the codebase\ndone_when: findings are reported\nskills: [image-generation]\n---\nExplore.\n")
	run("E1.5 Brief that asks for a skill", repoA, leadA, "start", skillsField, "--name", "explore-codebase")
	profileField := write("profilefield.md", "---\ntype: research\ntitle: Explore the codebase\ndone_when: findings are reported\nprofile: claude\n---\nExplore.\n")
	run("E1.6 Brief that names a Profile", repoA, leadA, "start", profileField, "--name", "explore-codebase")
	research := write("research.md", "---\ntype: research\ntitle: PostgreSQL connection pooling\ndone_when: a report compares the available approaches\n---\nInvestigate how PostgreSQL handles connection pooling and compare the available approaches.\n")
	run("E1.7 minimal research Brief: dispatch preview", repoA, leadA, "dispatch", "--brief", research, "--name", "pg-pooling")
	run("E1.8 minimal research Brief: start", repoA, leadA, "start", research, "--name", "pg-pooling")
	run("E1.9 show t1", repoA, leadA, "show", "t1")
	if out, err := gitCommand(env, repoA, "worktree", "list"); err == nil {
		t.Logf("\n===== E1.10 git worktree list after one Research Task\n%s", out)
	}
	if data, err := os.ReadFile(filepath.Join(root, "posse", "projects", "shop", "tasks", "t1", "launch.md")); err == nil {
		t.Logf("\n===== E1.11 research launch.md: %d words, %d bytes; Brief body was %d words\n%s", len(strings.Fields(string(data))), len(data), 14, string(data))
	}
	entries, _ := os.ReadDir(filepath.Join(root, "posse", "projects", "shop", "tasks", "t1"))
	for _, entry := range entries {
		info, _ := entry.Info()
		t.Logf("E1.12 task dir file %s %d bytes", entry.Name(), info.Size())
	}

	// --- E3: parallel research (Scenario B) under default capacity.
	for i := 2; i <= 4; i++ {
		brief := write(fmt.Sprintf("approach-%d.md", i), fmt.Sprintf("---\ntype: research\ntitle: Approach %d\ndone_when: a report describes approach %d\n---\nResearch approach %d independently.\n", i, i, i))
		run(fmt.Sprintf("E3.%d parallel research start", i), repoA, leadA, "start", brief, "--name", fmt.Sprintf("approach-%d", i))
	}
	fifth := write("approach-5.md", "---\ntype: research\ntitle: Approach 5\ndone_when: a report describes approach 5\n---\nResearch approach 5.\n")
	run("E3.5 one more Worker than the default capacity", repoA, leadA, "start", fifth, "--name", "approach-five")
	run("E3.6 same with --queue", repoA, leadA, "start", fifth, "--name", "approach-five", "--queue")
	run("E3.7 list", repoA, leadA, "list")
	snapshot("E3.8 after parallel starts")

	// --- E4: a Research Worker that produces a non-text asset (Scenario D/E).
	task1, err := db.Task(context.Background(), projectA.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("E4.0 t1 worktree=%s branch=%q sandbox=%d pane=%s label=%s containment=%s", task1.WorktreePath, task1.Branch, task1.SandboxID, task1.PaneID, task1.PaneLabel, task1.ContainmentMode)
	workerEnv := setEnv(env, "HERDR_ENV", "1")
	workerEnv = setEnv(workerEnv, "HERDR_PANE_ID", task1.PaneID)
	workerEnv = setEnv(workerEnv, "HERDR_WORKSPACE_ID", task1.HerdrWorkspaceID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", filepath.Join(root, "posse"))
	if task1.WorktreePath != "" {
		_ = os.MkdirAll(filepath.Join(task1.WorktreePath, "assets"), 0700)
		_ = os.WriteFile(filepath.Join(task1.WorktreePath, "assets", "logo.png"), []byte("\x89PNG fake image bytes"), 0600)
		_ = os.WriteFile(filepath.Join(task1.WorktreePath, "report.md"), []byte("# Report\n\nSee assets/logo.png.\n"), 0600)
		run("E4.1 research done without a report", task1.WorktreePath, workerEnv, "signal", "done", "Finished", "--turn", "launch:1")
		run("E4.2 research done with report", task1.WorktreePath, workerEnv, "signal", "done", "Finished", "--turn", "launch:1", "--report", "report.md")
		run("E4.3 show t1 after done", repoA, leadA, "show", "t1")
		run("E4.4 teardown t1", repoA, leadA, "teardown", "t1")
		var kept []string
		_ = filepath.Walk(filepath.Join(root, "posse", "projects", "shop"), func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && (strings.Contains(path, "report") || strings.Contains(path, "logo")) {
				kept = append(kept, path)
			}
			return nil
		})
		t.Logf("\n===== E4.5 files kept under the Project state after Teardown\n%s", strings.Join(kept, "\n"))
		_, statErr := os.Stat(filepath.Join(task1.WorktreePath, "assets", "logo.png"))
		t.Logf("E4.6 asset still in the Sandbox after Teardown: %v", statErr == nil)
	}

	// --- E2: the User's real Profile and Dispatch Rule shape (Scenario D).
	fake("codex")
	fake("pi")
	config := `[defaults]
  max_workers = 8
[[dispatch]]
  use = "codex-sol"
  when = "Task requires generating images"
[[dispatch]]
  use = "claude-opus"
  when = "Heavy work: technical design, product design, architecture (use claude-opus)"
[[dispatch]]
  use = "pi-luna"
  when = "Simple bug fix whose root cause and expected behavior are already well-known"
[[dispatch]]
  use = "pi-luna"
  when = "Default for other work (prefer GPT profiles; the User has more GPT quota)"
  [dispatch.default]
    use = "pi-luna"
[kinds]
  [kinds.claude]
  [kinds.codex]
  [kinds.pi]
    effort_args = ["--thinking", "{effort}"]
    model_args = ["--model", "{model}"]
[lead]
  kind = "claude"
[profiles]
  [profiles.claude-opus]
    kind = "claude"
    model = "opus"
  [profiles.codex-sol]
    kind = "codex"
    model = "gpt-6.1-sol"
  [profiles.pi-luna]
    effort = "xhigh"
    kind = "pi"
    model = "openai/gpt-6-luna"
`
	if err := os.WriteFile(filepath.Join(root, "posse", "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	run("E2.0 doctor after config", repoA, leadA, "doctor")
	image := write("image.md", "---\ntype: change\ntitle: Generate a set of images for this Project\ndone_when: six PNG images exist under assets/\n---\nGenerate a set of images for this Project.\n")
	bugfix := write("bugfix.md", "---\ntype: change\ntitle: Fix the off-by-one in pagination\ndone_when: the pagination test passes\n---\nSimple bug fix whose root cause is known.\n")
	run("E2.1 image Brief: dispatch without --profile", repoA, leadA, "dispatch", "--brief", image, "--name", "generate-images")
	run("E2.2 image Brief: dispatch --json", repoA, leadA, "dispatch", "--brief", image, "--name", "generate-images", "--json")
	run("E2.3 image Brief started on a non-image Profile", repoA, leadA, "start", image, "--name", "generate-images", "--profile", "pi-luna")
	run("E2.4 bug-fix Brief started on the image Profile", repoA, leadA, "start", bugfix, "--name", "fix-pagination", "--profile", "codex-sol")
	run("E2.5 image Brief started without --profile", repoA, leadA, "start", image, "--name", "generate-images-two")
	typed := config + `
[[dispatch]]
  type = "research"
  use = "pi-luna"
[[dispatch]]
  type = "research"
  use = "claude-opus"
  when = "science-heavy research"
`
	if err := os.WriteFile(filepath.Join(root, "posse", "config.toml"), []byte(typed), 0600); err != nil {
		t.Fatal(err)
	}
	run("E2.6 research Brief with one unconditional typed rule plus one conditional typed rule", repoA, leadA, "dispatch", "--brief", research, "--name", "pg-pooling")
	onlyDefault := strings.Replace(config, "[[dispatch]]\n  use = \"pi-luna\"\n  when = \"Default for other work (prefer GPT profiles; the User has more GPT quota)\"\n", "", 1)
	if err := os.WriteFile(filepath.Join(root, "posse", "config.toml"), []byte(onlyDefault), 0600); err != nil {
		t.Fatal(err)
	}
	run("E2.7 does dispatch.default apply while any when-rule exists", repoA, leadA, "dispatch", "--brief", bugfix, "--name", "fix-pagination")
	run("E2.8 list with Profiles", repoA, leadA, "list")
	if data, err := os.ReadFile(filepath.Join(root, "launches.log")); err == nil {
		t.Logf("\n===== E2.9 agent command lines launched\n%s", data)
	}
	for _, id := range []string{"t6", "t7"} {
		task, err := db.Task(context.Background(), projectA.ID, id)
		if err == nil {
			t.Logf("E2.10 %s title=%q profile=%s rule=%q state=%s", id, task.Title, task.Profile, task.DispatchRule, task.State)
		}
	}

	// --- E6: two Projects (Scenario H).
	repoB, projectB, leadB := up("other")
	otherBrief := write("other.md", "---\ntype: research\ntitle: Other project research\ndone_when: a report exists\n---\nResearch.\n")
	run("E6.1 Project B start", repoB, leadB, "start", otherBrief, "--name", "other-research", "--profile", "claude-opus")
	run("E6.2 Project A list", repoA, leadA, "list")
	run("E6.3 Project B list", repoB, leadB, "list")
	run("E6.4 Lead A pane running start from Project B root", repoB, leadA, "start", otherBrief, "--name", "cross-project")
	run("E6.5 Lead A pane running up for Project B", repoB, leadA, "up", "--yes")
	run("E6.6 Lead A addressing a Project B Task", repoA, leadA, "show", "t1", "--project", "other")
	run("E6.7 Lead A sending to Project B Task by cwd", repoB, leadA, "send", "t1", "hello from the wrong Lead")
	snapshot("E6.8 two Projects")
	t.Logf("E6.9 projectA workspace=%s lead=%s; projectB workspace=%s lead=%s", projectA.HerdrWorkspaceID, projectA.LeadPaneID, projectB.HerdrWorkspaceID, projectB.LeadPaneID)

	// --- E5: a Worker pane disappears (Scenario G).
	task2, err := db.Task(context.Background(), projectA.ID, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "pane", "close", task2.PaneID); err != nil {
		t.Logf("pane close: %v", err)
	}
	time.Sleep(2 * time.Second)
	run("E5.1 Lead status after a Worker pane was closed", repoA, leadA)
	run("E5.2 show t2", repoA, leadA, "show", "t2")
	run("E5.3 relaunch t2", repoA, leadA, "relaunch", "t2")
	run("E5.4 show t2 after relaunch", repoA, leadA, "show", "t2")
	run("E5.5 decisions", repoA, leadA, "decisions")
}
```

</details>

<details>
<summary><code>internal/e2e/reassess_experiment2_test.go</code></summary>

```go
//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// Experiment harness for the 2026-11-12 reassessment report. It asserts nothing
// about product behaviour; it records what the installed CLI does.
func TestReassessmentExperimentsTwo(t *testing.T) {
	root := newFixtureRoot(t, herdr.TestRootName())
	bin := filepath.Join(root, "bin")
	installed := filepath.Join(root, "installed")
	for _, dir := range []string{bin, installed} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(installed, "posse")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	herdrBinary, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"herdr": herdrBinary, "posse": binary} {
		if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	fake := func(name string) {
		agent := `#!/bin/sh
if [ "$1" = "--version" ]; then echo '` + name + ` 0.0.0'; exit 0; fi
printf '%s\n' "` + name + ` $*" >> "$POSSE_TEST_ROOT/launches.log"
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent ` + name + ` --state idle >/dev/null 2>&1
while IFS= read -r line; do
 printf '%s\n' "$line" >> "$POSSE_TEST_ROOT/prompts.log"
 case "$PWD/" in "$POSSE_HOME/pool/"*) herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent ` + name + ` --state working >/dev/null 2>&1 & ;; esac
done
`
		if err := os.WriteFile(filepath.Join(bin, name), []byte(agent), 0700); err != nil {
			t.Fatal(err)
		}
	}
	fake("claude")
	env := isolatedE2EEnv(t, root)
	env = setEnv(env, "POSSE_TEST_ROOT", root)
	env = setEnv(env, "PATH", bin+":/usr/bin:/bin")
	for _, dir := range []string{"home", "posse", "claude", "codex", "pi"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	makeRepo := func(name string) string {
		repo := filepath.Join(root, name)
		if err := os.MkdirAll(repo, 0700); err != nil {
			t.Fatal(err)
		}
		origin := filepath.Join(root, name+"-origin.git")
		for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.test"}, {"commit", "--allow-empty", "-m", "Initial"}, {"init", "--bare", origin}, {"remote", "add", "origin", origin}, {"push", "-u", "origin", "main"}} {
			if out, err := gitCommand(env, repo, args...); err != nil {
				t.Fatalf("git: %v %s", err, out)
			}
		}
		return repo
	}
	client := herdr.NewWithEnv("herdr", env)
	server := startServer(t, client)
	db, err := store.Open(filepath.Join(root, "posse"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	up := func(name string) (string, store.Project, []string) {
		repo := makeRepo(name + "-repo")
		workspace, err := createWorkspace(client, repo)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Run(context.Background(), "pane", "run", workspace.RootPane.PaneID, "posse up --name "+name+" --yes"); err != nil {
			t.Fatal(err)
		}
		var project store.Project
		if !waitForCondition(40*time.Second, func() bool {
			project, err = db.ProjectByName(context.Background(), name)
			if err != nil || project.LeadPaneID == "" {
				return false
			}
			snapshot, err := client.Snapshot(context.Background())
			if err != nil {
				return false
			}
			for _, agent := range snapshot.Agents {
				if agent.PaneID == project.LeadPaneID {
					return true
				}
			}
			return false
		}) {
			pane, _ := client.Run(context.Background(), "pane", "read", workspace.RootPane.PaneID, "--source", "recent-unwrapped", "--lines", "80")
			t.Fatalf("up %s: %s", name, pane)
		}
		leadEnv := setEnv(env, "HERDR_ENV", "1")
		leadEnv = setEnv(leadEnv, "HERDR_PANE_ID", project.LeadPaneID)
		leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", project.HerdrWorkspaceID)
		return repo, project, leadEnv
	}
	run := func(label, cwd string, env []string, args ...string) string {
		command := exec.Command(binary, args...)
		command.Dir = cwd
		command.Env = taskAuthorGenerationEnv(env)
		started := time.Now()
		output, err := command.CombinedOutput()
		code := 0
		if err != nil {
			code = -1
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			}
		}
		t.Logf("\n===== %s\n$ posse %s\nexit=%d elapsed=%s\n%s", label, strings.Join(args, " "), code, time.Since(started).Round(time.Millisecond), output)
		return string(output)
	}
	write := func(name, contents string) string {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	snapshot := func(label string) {
		snap, err := client.Snapshot(context.Background())
		if err != nil {
			t.Logf("snapshot: %v", err)
			return
		}
		var lines []string
		for _, pane := range snap.Panes {
			lines = append(lines, fmt.Sprintf("pane=%s workspace=%s tab=%s agent=%s cwd=%s", pane.PaneID, pane.WorkspaceID, pane.TabID, pane.Agent, pane.CWD))
		}
		t.Logf("\n===== %s (Herdr panes)\n%s", label, strings.Join(lines, "\n"))
	}

	repoA, projectA, leadA := up("shop")
	_ = projectA
	_ = snapshot
	fake("codex")
	fake("pi")
	global := `[defaults]
  max_workers = 4
[[dispatch]]
  use = "codex-sol"
  when = "Task requires generating images"
[[dispatch]]
  use = "claude-opus"
  when = "Heavy work: technical design, product design, architecture (use claude-opus)"
[[dispatch]]
  use = "pi-luna"
  when = "Default for other work (prefer GPT profiles; the User has more GPT quota)"
  [dispatch.default]
    use = "pi-luna"
[kinds]
  [kinds.claude]
  [kinds.codex]
  [kinds.pi]
    effort_args = ["--thinking", "{effort}"]
    model_args = ["--model", "{model}"]
[lead]
  kind = "claude"
[profiles]
  [profiles.claude-opus]
    kind = "claude"
    model = "opus"
  [profiles.claude-sonnet]
    kind = "claude"
    model = "sonnet"
  [profiles.codex-sol]
    kind = "codex"
    model = "gpt-6.1-sol"
  [profiles.pi-luna]
    effort = "xhigh"
    kind = "pi"
    model = "openai/gpt-6-luna"
`
	project := `[[dispatch]]
  type = "research"
  use = "pi-luna"
[[dispatch]]
  type = "research"
  use = "claude-opus"
  when = "plan or design; claude-opus by default"
[[dispatch]]
  type = "change"
  use = "claude-opus"
  when = "plan or design; claude-opus by default"
[[dispatch]]
  type = "change"
  use = "pi-luna"
  when = "clear-scope implementation; pi-luna first"
[[dispatch]]
  type = "change"
  use = "claude-sonnet"
  when = "clear-scope implementation when pi-luna is out of usage"
`
	if err := os.WriteFile(filepath.Join(root, "posse", "config.toml"), []byte(global), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "posse", "projects", "shop", "config.toml"), []byte(project), 0600); err != nil {
		t.Fatal(err)
	}
	image := write("image.md", "---\ntype: change\ntitle: Generate a set of images for this Project\ndone_when: six PNG images exist under assets/\n---\nGenerate a set of images for this Project.\n")
	imageResearch := write("image-research.md", "---\ntype: research\ntitle: Generate visual studies\ndone_when: a report with six attached PNG studies exists\n---\nGenerate a set of images for this Project.\n")
	run("X1 image Change Brief under typed Project rules plus an untyped User image rule", repoA, leadA, "dispatch", "--brief", image, "--name", "generate-images")
	run("X2 image Research Brief under the same rules", repoA, leadA, "dispatch", "--brief", imageResearch, "--name", "visual-studies")
	run("X3 config show dispatch", repoA, leadA, "config", "get", "dispatch")
	run("X4 config explain", repoA, leadA, "config", "show")

	// Scenario G: the Herdr server goes away under a running Worker.
	research := write("research.md", "---\ntype: research\ntitle: PostgreSQL connection pooling\ndone_when: a report compares the available approaches\n---\nInvestigate.\n")
	run("G1 start a Worker", repoA, leadA, "start", research, "--name", "pg-pooling", "--profile", "claude-opus")
	if err := client.StopIsolatedServer(context.Background()); err != nil {
		t.Logf("stop server: %v", err)
	}
	_ = server.Wait()
	time.Sleep(time.Second)
	run("G2 Lead status while Herdr is down", repoA, leadA)
	run("G3 start while Herdr is down", repoA, leadA, "start", research, "--name", "pg-pooling-two", "--profile", "claude-opus")
	run("G4 list while Herdr is down", repoA, leadA, "list")
	run("G5 send while Herdr is down", repoA, leadA, "send", "t1", "are you there")
	if _, err := client.StartIsolatedServer(); err != nil {
		t.Logf("restart server: %v", err)
	}
	waitForCondition(30*time.Second, func() bool {
		status, err := client.Status(context.Background())
		return err == nil && status.Running
	})
	run("G6 Lead status after Herdr restarted", repoA, leadA)
	run("G7 list after Herdr restarted", repoA, leadA, "list")
	run("G8 up --all from a plain shell after restart", repoA, env, "up", "--all")
	time.Sleep(5 * time.Second)
	run("G9 list after up --all", repoA, env, "list")
	snapshot("G10 panes after restore")
}
```

</details>

<details>
<summary><code>internal/dispatch/reassess_review_test.go</code></summary>

```go
package dispatch

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
)

func TestReassessReviewCandidateOrder(t *testing.T) {
	home := t.TempDir()
	cfgText := `[[dispatch]]
  use = "codex-sol"
  when = "Task requires generating images"
[[dispatch]]
  use = "claude-opus"
  when = "Heavy work: technical design, product design, architecture (use claude-opus)"
[[dispatch]]
  use = "pi-luna"
  when = "Default for other work (prefer GPT profiles; the User has more GPT quota)"
  [dispatch.default]
    use = "pi-luna"
[kinds]
  [kinds.claude]
  [kinds.codex]
  [kinds.pi]
[lead]
  kind = "claude"
[profiles]
  [profiles.claude-opus]
    kind = "claude"
    model = "opus"
  [profiles.codex-sol]
    kind = "codex"
    model = "gpt-6.1-sol"
  [profiles.pi-luna]
    kind = "pi"
    model = "openai/gpt-6-luna"
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfgText), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, taskType := range []string{"review", "change", "research"} {
		_, err = Resolve(cfg, taskType, "")
		var profileErr *ProfileError
		if !errors.As(err, &profileErr) {
			t.Fatalf("%s: %v", taskType, err)
		}
		t.Logf("type=%s code=%s first_candidate=%s all=%v", taskType, profileErr.Code, profileErr.Candidates[0].Profile, profileErr.Candidates)
	}
}
```

</details>
