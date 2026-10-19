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
A Task whose outcome is a change to the Project, integrated through a Merge Mode or recorded as an in-place source change. Formerly: Ship Task.
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
The typed outcome (`pass`, `changes_requested` or `inconclusive`) of a Review Task, bound by Posse to the exact Change revision it examined. Only a current `pass` satisfies local review.
_Avoid_: Approval, review result

**Review Loop**:
The optional built-in protocol that turns successive Review Tasks of one Change Task into at most three Review Rounds over a Finding Ledger; part of the built-in validation that replaces no-mistakes.
_Avoid_: Review pipeline, review cycle

**Review Round**:
One Review Task in a Review Loop that records a verdict for one exact Change head; round 1 reviews the full candidate and later rounds only the author changes since the last reviewed head.
_Avoid_: Pass, iteration, rereview

**Fix Round**:
One instruction Posse or the Lead queues to a Change Worker to fix a failed Gate, failed CI checks or selected Finding Patterns for one head, within a per-Project budget.
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
A question put to the User that only the User may answer, such as whether to Merge, relaunch, discard Sandbox work or preserve and stop an in-place Task; answered once and recorded.
_Avoid_: Approval (for the question itself), prompt, needs-decision (that is a Signal to the Lead)

**Brief**:
The written contract a Task starts from: its type, what done means, and how it Merges.
_Avoid_: Prompt, spec, plan

**Notice**:
Something the Lead must look at, such as a Signal that needs action, a blocked or exited Worker, a Stall, or a failed Gate.
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

**Orphan**:
Work a Worker made on a merged Task beyond what merged, such as later commits or uncommitted changes; the User decides whether it becomes a new Task or is discarded. Formerly: Leftover.
_Avoid_: Follow-up (that is a message to a Worker), residue, diff

**Merge Mode**:
How a Project's Change Tasks Merge: `local` (merge on this machine), `pr` (a pull request) or the deprecated `no-mistakes` (through the no-mistakes pipeline, replaced by built-in validation).
Formerly: Landing Mode.
_Avoid_: Delivery mode

**Autonomy**:
A standing grant the User gives a Project that lets the Lead decide review findings, Merge, or both without asking; `yolo` grants both.
_Avoid_: Permission, auto mode

**Gate**:
A configured check Posse runs before a Change Task Merges or an in-place Task reports completion.
_Avoid_: Validation, CI, pipeline

**Watch**:
The mechanism that observes a Project and delivers Notices to its Lead. Formerly: Lookout.
_Avoid_: Monitor, watcher

**Stall**:
A Worker that appears to be working but shows no progress in its output or its worktree for too long.
_Avoid_: Hang, freeze, wedge

**Teardown**:
Removing a Task's Worker and releasing its custody after its work is preserved; Sandbox Tasks require Merge, reporting or approved discard, while in-place Tasks preserve source files and release path leases.
_Avoid_: Cleanup, discard

**Project**:
One Git repository, one Workspace of repositories, or one explicitly registered directory under management, with its own Tasks, state and policy.
_Avoid_: Repo (in prose), home

**Workspace**:
A Project whose root is a plain folder and whose Members are the repositories directly inside it; one Task may change several Members.
_Avoid_: Monorepo, stack, multi-repo

**Directory Project**:
A folder explicitly registered as a local Project, whether or not it contains Git metadata. Its Tasks require explicit in-place execution, User consent and selected relative paths.
_Avoid_: Workspace, repository Project

**In-place execution**:
A consented Task execution mode that works in a Directory Project's registered source root, leases selected paths, runs approved checks and preserves changes without creating a branch or Merge.
_Avoid_: Sandbox execution, unscoped local work

**Member**:
One repository of a Workspace, with its own default branch, remote and Merge Mode.
_Avoid_: Subproject, submodule, child repo

**Profile**:
A named Worker configuration, or an implicit resolution to the Lead's agent kind; a named Profile can also provide model, effort and arguments.
_Avoid_: Preset, agent config

**Launch Identity Record**:
A Task's launch number, Profile name and configured model value or unknown status, derived from that launch's arguments and persisted with its Task id.
_Avoid_: Runtime identity, provider identity

**Dispatch Rule**:
A User-written rule that says which Profile a kind of Task should use.
_Avoid_: Routing rule, policy

**Identity**:
The User-chosen Lead name, persona, language and form of address. Name, persona and form of address apply to User messages; language also selects neutral artifact prose. Persona is not evidence, and quoted User intent stays exact.
_Avoid_: Alias, nickname

**Preferences**:
User-authored how-to-work guidance for a Lead or Worker, layered from `<POSSE_HOME>/preferences/` and `<POSSE_HOME>/projects/<project>/preferences/`. Project guidance follows User guidance and can change default workflow advice, but cannot grant authority or override runtime obligations. The User writes it with `posse preferences set`; a Lead's write requires User consent recorded with the quote, and Workers cannot write it. Deprecated `playbook/` paths remain readable until explicitly moved.
_Avoid_: Playbook, workflow framework, policy
