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
The internal name for an agent that carries out exactly one Task on the Lead's behalf. **Rider** is its role name in messages, CLI prose and agent-facing labels; Herdr child workspace labels show the Rider name for repository Projects; Workspace Project tabs show it.
_Avoid_: Subagent, child

**Rider**:
The visible name for a Worker. Use it in message envelopes, CLI prose and agent-facing role labels; keep `Worker` in stable internal identifiers and protocol keys.
_Avoid_: Worker (as a visible role label)

**Task**:
One unit of delegated work with an explicit contract, carried out by one Worker.
_Avoid_: Job, ticket, run

**Task ID**:
The handle `t<n>` that names one Task everywhere the User or the Lead refers to it; never reused within a Project.
_Avoid_: Internal id, seq

**Task Name**:
A short slug derived from a Task's title, used for its branch and Herdr workspace or tab label; it is display only and may repeat across Tasks.
_Avoid_: Id, handle, nickname

**Ship Task**:
A Task whose outcome is a change landed into the Project.
_Avoid_: Fix task, build task

**Scout Task**:
A Task whose outcome is a Report and never a change to the Project.
_Avoid_: Research task, investigation

**Review Task**:
A Scout Task that examines a Ship Task's change adversarially before it Lands.
_Avoid_: Code review, audit

**Report**:
The standalone written findings a Scout Task leaves behind.
_Avoid_: Summary, output

**Signal**:
A Worker's own declaration of where its Task stands, such as done, needs-decision or failed.
_Avoid_: Status, report, event

**Decision**:
A question put to the User that only the User may answer, such as whether to Land, relaunch or discard; answered once and recorded.
_Avoid_: Approval (for the question itself), prompt, needs-decision (that is a Signal to the Lead)

**Brief**:
The written contract a Task starts from: its type, what done means, and how it Lands.
_Avoid_: Prompt, spec, plan

**Notice**:
Something the Lead must look at, such as a Signal that needs action, a blocked or exited Worker, a Stall, or a failed Gate.
_Avoid_: Alert, event, wake, notification

**Remuda**:
A Project's set of reusable worktrees, kept warm between Tasks.
_Avoid_: Worktree cache, pool

**Mount**:
One worktree in a Remuda; a Task holds exactly one Mount from spawn until Teardown.
_Avoid_: Worktree (when the reusable unit is meant), lease

**Land**:
To integrate a Ship Task's change into the Project through the Project's landing mode, either a pull request or a local merge. A pull request has Landed once the forge reports it merged, whatever its Worker is doing at the time.
_Avoid_: Merge (as the general term), ship, deliver

**Leftover**:
Work a Worker made on a Landed Task beyond what Landed, such as later commits or uncommitted changes; the User decides whether it becomes a new Task or is discarded.
_Avoid_: Follow-up (that is a message to a Worker), residue, diff

**Landing Mode**:
How a Project's Ship Tasks Land: `local` (merge on this machine), `pr` (a pull request) or `no-mistakes` (through the no-mistakes pipeline).
_Avoid_: Delivery mode, ship mode

**Autonomy**:
A standing grant the User gives a Project that lets the Lead decide review findings, Land, or both without asking; `yolo` grants both.
_Avoid_: Permission, auto mode

**Gate**:
A check that must pass before a Ship Task may Land.
_Avoid_: Validation, CI, pipeline

**Stall**:
A Worker that appears to be working but shows no progress in its output or its worktree for too long.
_Avoid_: Hang, freeze, wedge

**Teardown**:
Removing a finished Task's Worker and worktree; allowed only after the Task has Landed or the User explicitly agrees.
_Avoid_: Cleanup, discard

**Project**:
One git repository, or one Workspace of repositories, under management, with its own Tasks, state and policy.
_Avoid_: Repo (in prose), home

**Workspace**:
A Project whose root is a plain folder and whose Members are the repositories directly inside it; one Task may change several Members.
_Avoid_: Monorepo, stack, multi-repo

**Member**:
One repository of a Workspace, with its own default branch, remote and Landing Mode.
_Avoid_: Subproject, submodule, child repo

**Profile**:
A named Worker configuration, or an implicit resolution to the Lead's agent kind; a named Profile can also provide model, effort and arguments.
_Avoid_: Preset, agent config

**Dispatch Rule**:
A User-written rule that says which Profile a kind of Task should use.
_Avoid_: Routing rule, policy

**Identity**:
The User-chosen name, persona and language the Lead uses when talking to the User; it never appears in anything another agent reads.
_Avoid_: Alias, nickname
