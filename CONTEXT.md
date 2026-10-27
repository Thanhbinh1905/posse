# posse

Posse: Personal Orchestrated Supervised Separated Executors.

A lightweight harness where one person talks to a single agent that dispatches and supervises other coding agents running in Herdr.

## Language

**User**:
The one person who states intent and makes the decisions only a human can make.
_Avoid_: Owner, operator

**Lead**:
The single agent the User talks to in a Project; it plans, dispatches and supervises Workers.
_Avoid_: Primary, orchestrator, supervisor

**Worker**:
An agent that carries out exactly one Task on the Lead's behalf. Formerly: Rider.
_Avoid_: Subagent, child

**Research Child**:
A bounded read-only native child of a Lead's or Worker's harness that Posse registers as a research attempt before accepting its result; it is not a Worker and changes no files.
_Avoid_: Subagent swarm, helper Worker

**Task**:
One unit of delegated work with an explicit contract, carried out by one Worker.
_Avoid_: Job, ticket, run

**Task ID**:
The handle `t<n>` that names one Task everywhere the User or the Lead refers to it; never reused within a Project.
_Avoid_: Internal id, seq

**Task Name**:
An optional human-facing label, supplied by the Lead or generated from the title; it does not identify the Task or determine its branch, and may repeat.
_Avoid_: Id, handle, nickname

**Task Branch**:
The Git branch that holds a Task's code; it is independent of the Task Name.
_Avoid_: Task Name, slug

**Change Task**:
A Task whose outcome is a change to the Project, integrated through a Merge Mode. Formerly: Ship Task.
_Avoid_: Fix task, build task

**Closing Issue**:
The one forge issue a Change Task may name for closure when its PR Merges. A Brief uses `ticket:`; legacy `issues: [N]` is accepted only as the same singleton, and `refs:` stays non-closing.
_Avoid_: ticket (when naming a Task)

**Research Task**:
A Task whose outcome is a Report and never a change to the Project. Formerly: Scout Task.
_Avoid_: Investigation

**Review Task**:
A Research Task that examines a Change Task's change adversarially before it Merges.
_Avoid_: Code review, audit

**Review Verdict**:
The typed outcome (`pass`, `changes_requested` or `inconclusive`) of a Review Task, bound by Posse to the exact Change revision it examined. Only a current `pass` is a passing Review Verdict; selected review evidence may also include an explicit carry or User acceptance.
_Avoid_: Approval, review result

**Review Mode**:
The User's choice of review evidence, frozen into each Change Task's plan: `none` (the default), `quick` (exact-head independent review before Merge) or `full-pipeline` (a Private Candidate passes review, test, docs and lint before every publication). All three are ready.
_Avoid_: Review level, review gate

**Private Candidate**:
The unpublished checkout of one exact submitted Change head that `full-pipeline` validates before promotion pushes it or fast-forwards its Task branch; any later commit needs a fresh candidate.
_Avoid_: Draft PR, staging branch

**Review Loop**:
The optional built-in protocol that turns successive Review Tasks of one Change Task into at most three Review Rounds over a Finding Ledger; part of the built-in validation that replaced no-mistakes.
_Avoid_: Review pipeline, review cycle

**Review Round**:
One Review Task in a Review Loop that records a verdict for one exact Change head; round 1 reviews the full candidate and later rounds only the author changes since the last reviewed head.
_Avoid_: Pass, iteration, rereview

**Fix Round**:
One instruction Posse or the Lead queues to a Change Worker to fix failed CI checks or selected Finding Patterns for one head (one head per changed Member in a workspace), within a per-Project budget.
_Avoid_: Retry, fix attempt

**Finding Pattern**:
A group of findings that share one root cause or violated rule, fixed together in one fix round.
_Avoid_: Category, file group

**Finding Ledger**:
The durable record of every finding a Change Task's Review Rounds raised, keyed by stable fingerprints, so no finding is reported twice.
_Avoid_: Findings list, issue log

**Report**:
The standalone written findings a Research Task leaves behind, preserved with its attachments as an immutable, digested Report Bundle after Teardown.
_Avoid_: Summary, output

**Report Bundle**:
A Research Task's Report and saved attachments, sealed by a manifest whose digest identifies their exact bytes.
_Avoid_: Archive (unless it is a compressed file)

**Publication-bound Report**:
A Research Report marked `report_kind: spec` or `report_kind: tickets`; acceptance requires a current independent Report-review pass and explicit User approval of its exact digest.
_Avoid_: Accepted Report (before User approval)

**Report Acceptance**:
The durable record that the User accepted one exact publication-bound Report digest, including whether a Lead recorded the User's quoted approval.
_Avoid_: Signal, acknowledgement, Autonomy

**Signal**:
A Worker's own declaration of where its Task stands, such as done, needs-decision or failed.
_Avoid_: Status, report, event

**Decision**:
A question put to the User that only the User may answer, such as whether to Merge, relaunch or discard Sandbox work; answered once and recorded.
_Avoid_: Approval (for the question itself), prompt, needs-decision (that is a Signal to the Lead)

**Brief**:
The written contract a Task starts from: its type, what done means, and how it Merges.
_Avoid_: Prompt, spec, plan

**Notice**:
Something the Lead must look at, such as a Signal that needs action, a blocked or exited Worker, a Stall, or failed CI.
_Avoid_: Alert, event, wake, notification

**Pool**:
A Project's set of reusable Sandboxes, kept warm between Tasks. Formerly: Remuda.
_Avoid_: Worktree cache

**Sandbox**:
A per-Task git worktree with per-Task process containment; for a Workspace, it contains the selected Members' worktrees. It is not a security sandbox. A Task holds it from start until Teardown, after which the Pool may reuse it. Formerly: Mount.
_Avoid_: Worktree (when the reusable unit is meant), lease

**Merge**:
To integrate a Change Task's change into the Project through the Project's merge mode, either a pull request or a local merge. A pull request has Merged once the forge reports it merged, whatever its Worker is doing at the time.
Formerly: Land.
_Avoid_: Deliver

**Merge Queue**:
Posse's FIFO queue per target branch of done Change Tasks whose PR is behind that target; only its head is asked to merge the target. Used only when `defaults.require_current_target` is on.
_Avoid_: Forge merge queue, merge train

**Delivery**:
The durable record of one forge pull request or merge request of a Change Task or Workspace Member. It outlives the Worker pane that opened it.
_Avoid_: PR lifetime, Worker PR

**Steward**:
The Lead in its role as operator of a Delivery: it may inspect it, reply on it, refresh Posse-owned metadata and rerun an existing failed CI run, each recorded with actor and head. A Steward is not an author and has no merge, workflow or secret authority.
_Avoid_: Owner, maintainer

**Orphan**:
Work a Worker made on a merged Task beyond what merged, such as later commits or uncommitted changes; the User decides whether it becomes a new Task or is discarded. Formerly: Leftover.
_Avoid_: Follow-up (that is a message to a Worker), residue, diff

**Merge Mode**:
How a Project's Change Tasks Merge: `local` (merge on this machine) or `pr` (a pull request). The removed `no-mistakes` mode fails config with `config_migration_required` naming `pr`; Tasks admitted under it stay readable as history.
Formerly: Landing Mode.
_Avoid_: Delivery mode

**Autonomy**:
A standing grant the User gives a Project that lets the Lead decide review findings, Merge, or both without asking; `yolo` grants both.
_Avoid_: Permission, auto mode

**Watch**:
The mechanism that observes a Project and delivers Notices to its Lead. Formerly: Lookout.
_Avoid_: Monitor, watcher

**Stall**:
A Worker that appears to be working but shows no progress in its output or its worktree for too long.
_Avoid_: Hang, freeze, wedge

**Teardown**:
Removing a Task's Worker and releasing its custody after its work is preserved; Tasks require Merge, reporting or approved discard.
_Avoid_: Cleanup, discard

**Restore**:
The User's explicit `posse up --all` that brings back the Lead, Workers and Watch of every Project that is not down after Herdr stopped them; Posse never restores agents on its own. A restored Lead keeps the agent kind and session that were running.
_Avoid_: Auto-recovery, recover

**Project**:
One Git repository or one Workspace of repositories, with its own Tasks, state and policy.
_Avoid_: Repo (in prose), home

**Workspace**:
A Project whose root is a plain folder and whose Members are the repositories directly inside it; one Task may change several Members.
_Avoid_: Monorepo, stack, multi-repo

**Member**:
One repository of a Workspace, with its own default branch, remote and Merge Mode.
_Avoid_: Subproject, submodule, child repo

**Profile**:
A named Worker configuration, or an implicit resolution to the Lead's agent kind; a named Profile can also provide model, effort, arguments and a fallback Profile that continues its Tasks when its quota runs out.
_Avoid_: Preset, agent config

**Workflow profile**:
An optional User-defined ordered recipe of stages over the three core frames and Git delivery; the User decides each transition. It is distinct from a Worker Profile.
_Avoid_: Mandatory lifecycle, Worker Profile, bundle

**Frame**:
One of Posse's three work boundaries: Plan -> tickets, Implement, or Review. Ship is Git delivery after these frames, not a fourth frame.
_Avoid_: Stage, phase, Task type

**Stage**:
One step in a workflow profile, mapped to a frame or to Ship, with a User decision before advancing; several stages may share a frame.
_Avoid_: Frame, Task state, Gate

**Platform skills**:
The embedded skills every Posse-launched Lead and Worker loads: `posse`, `posse-setup`, `posse-down` and `posse-update`.
_Avoid_: Default skills, core skills

**Workflow skills**:
Skills a session loads on top of the platform skills, chosen per role (`lead`, `change`, `review`, `research`) in the `[skills]` config block as bundled names or skill directory paths, and frozen into each Task's plan.
_Avoid_: Skill deltas, hidden defaults

**Recommended skill set**:
The embedded workflow skills Posse suggests for each role; Posse writes them into `[skills]` only on request or once when upgrading an install that loaded them all, and the User edits the block from then on.
_Avoid_: Default skill set, bundle

**Launch Identity Record**:
A Task's launch number, Profile name and configured model value or unknown status, derived from that launch's arguments and persisted with its Task id.
_Avoid_: Runtime identity, provider identity

**Dispatch Rule**:
A User-written rule that says which Profile a kind of Task should use.
_Avoid_: Routing rule, policy

**Identity**:
The User-chosen Lead name, persona, language and form of address. Name, persona and form of address apply to User messages; language also selects neutral artifact prose. Persona is not evidence, and quoted User intent stays exact.
_Avoid_: Alias, nickname

**Knowledge**:
Opt-in curated facts and lessons for a Lead or Worker, with a source, a last-verified date and a User or Project scope, stored under `<POSSE_HOME>/knowledge/` and `<POSSE_HOME>/projects/<project>/knowledge/`. Knowledge is reference context, not authority: it cannot answer Decisions, grant Autonomy or run commands. Task outcomes point to their Report instead of being copied. A startup budget shared with preferences is enforced at write time by curation, never by truncation.
_Avoid_: Memory, learnings, notes

**Preferences**:
User-authored how-to-work guidance for a Lead or Worker, layered from `<POSSE_HOME>/preferences/` and `<POSSE_HOME>/projects/<project>/preferences/`. Project guidance follows User guidance and can change default workflow advice, but cannot grant authority or override runtime obligations. The User writes it with `posse preferences set`; a Lead's write requires User consent recorded with the quote, and Workers cannot write it. Deprecated `playbook/` paths remain readable until explicitly moved.
_Avoid_: Playbook, workflow framework, policy
