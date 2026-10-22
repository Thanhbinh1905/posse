# 29. Tracked read-only native research children

A Lead or Worker may delegate bounded read-only research to a native child of its harness, but only through a Posse research attempt registered before any result is accepted. Native children never implement changes, claim Tasks or adopt worktrees.

## Context

Audit t172 (decision D5, epic #92 issues #151-#153) found that harness subagents save Lead context, yet none of them is a Posse executor by default:

- Claude Code's Agent tool runs children in the parent's checkout. Explore and Plan are read-only, but general and custom agents can edit, a same-named project definition can override a built-in, and a bypass-permissions parent overrides a child's declared mode.
- Codex children inherit the parent's sandbox unless configured otherwise, and the Codex Lead runs with `--sandbox danger-full-access` so it can write the Posse home.
- Pi's example subagent extension starts `pi --no-session` processes in the parent's cwd; context isolation is not filesystem isolation and the children cannot be resumed.

Untracked children make usage, interruption and cancellation invisible, and a write-capable child could change a checkout that Posse believes only its Worker writes.

## Decision

### One attempt interface for every harness

`research_attempts` records each child: parent (the Lead or one Task) with its harness session and process instance, harness and verified version, native child identity (Claude agent ID, Codex thread ID, Pi session ID), agent type, model, tool loadout, access (`read-only` only), scope, prompt digest, state, resumability, session reference, runner and child process instances, usage, a bounded result pointer with its digest, and a note. States are `registered`, `running`, `completed`, `interrupted`, `cancelled`, `failed` and `refused`. A refused child is recorded too, so refusals stay visible.

`posse research list|show|cancel` read and control attempts. `posse status`, `posse show <task>` and `posse doctor` report a Task's children and per-Project active counts. A result is accepted only from the bound child of a `running` attempt; output after cancellation is discarded.

Every Lead and Worker may launch research children within `research.max_active` (default 2) per parent. `research.native = "off"` (User-only) leaves harness children unmanaged and refuses `posse research start`.

### Claude: hooks register the child the harness launches

Tracked Claude Lead and Worker launches load a Posse-owned plugin whose PreToolUse, SubagentStart and SubagentStop hooks call `posse _research-hook claude --version <installed>`:

- **Admission.** An Agent call registers an attempt only for Explore, Plan, or a project or user definition with an explicit tool list of read-only tools and no bypass or accept-edits permission mode, MCP servers or isolation. A definition overriding a built-in name is checked as a custom agent. General agents, plugin agents, isolation worktrees, named teammates, an unknown or pre-2.1.0 Claude, a scope outside the parent's checkout and an exhausted budget are refused and recorded.
- **Binding.** SubagentStart binds the agent ID to the parent session's single unbound attempt of that type. A second unbound child of the same type is refused until the first starts, so binding is never ambiguous.
- **Enforcement.** Inside the child, PreToolUse refuses write tools, nested agents and shell commands not parsed as read-only, whatever permission mode the parent holds. An unregistered child is refused all tools.
- **Completion.** SubagentStop stores the final message (truncated at `research.max_result_bytes`) and transcript usage.
- **Cancellation and restart.** Claude cannot stop one child from outside, so cancellation refuses the child's next tool call and discards its result. When the parent process instance exits, a bound child with a transcript becomes interrupted and resumable only by the same parent session; others are interrupted without resume.

### Codex and Pi: Posse launches and supervises the process

`posse research start --harness codex|pi --scope <dir>` requires an explicit scope inside the parent's checkout (the Lead's Project root or Task Sandboxes, or the Worker's own Sandbox). It refuses a missing, unknown or old harness version (Codex before 0.160.0, Pi before 1.2.0) before starting anything. A detached `posse _research-run` supervisor runs the child in its own process group, survives the parent and records the child's identity, usage, a bounded event log (`research.max_output_bytes`) and the bounded result. `research.timeout` stops a child that runs too long. The child environment drops Herdr and parent-session bindings and marks it as a Worker of the Posse home.

- **Codex.** The child runs `codex exec --json` with `--sandbox read-only`, `sandbox_mode="read-only"` and `approval_policy="never"`; the parent's launch arguments, including danger-full-access, are never inherited. Posse reads the sandbox policy Codex records for every child turn in the thread's rollout and stops and refuses any thread whose turn ran without read-only or left no record. `posse research follow-up` resumes the same thread with the same pinned settings.
- **Pi.** The child runs `pi --mode json -p` with `--tools read,grep,find,ls` and without extensions, skills, prompt templates, MCP or project-local files, in a Posse-owned persistent session (`--session-dir` under the attempt, Posse-chosen `--session-id`). A child announcing another session is refused. A follow-up opens the recorded session file and counts as resumed only if Pi appended to that file; otherwise the attempt is interrupted. The trusted per-Lead Pi extension adds a `posse_research` tool. It keys each start by session and tool call ID, so a reload never duplicates a child, and cancels exactly the attempts it owns on abort or session shutdown. No repository-controlled child code is loaded.

Cancellation records `cancelled` first, then signals only the recorded runner and child instances after verifying them through pidfd, plus the child's own process group. A lost runner leaves the attempt interrupted. It is resumable when a Codex thread or persisted Pi session exists; nothing is relaunched automatically.

## Consequences

- Native research is visible, bounded and cancellable. It does not count against `defaults.max_workers`, does not create Tasks, and is not review evidence or a Report by implication.
- Research children share their parent's checkout read-only. Posse assumes no worktree isolation and adopts no harness worktree.
- Enforcement is cooperative same-user hardening, not a sandbox. The Claude read-only shell parser is conservative and refuses unknown programs, redirection to files and substitutions. Codex relies on its own sandbox plus Posse's verification of the recorded policy; Pi relies on its tool loadout.
- Mutating native execution remains unsupported. It needs a separate adapter that proves Posse-owned acquisition, instance-bound cancellation, custody and recovery.

## Alternatives

- **Trust agent names or declared modes.** Rejected: overriding definitions and bypass-permissions parents defeat them.
- **Let Codex children inherit the Lead's settings.** Rejected: the Lead runs danger-full-access.
- **Reuse the Pi example extension.** Rejected: it runs `--no-session`, defaults to the parent cwd and would load repository-controlled agent definitions.
- **Swarms or automatic parallel children.** Rejected: the per-parent budget keeps delegation bounded; no default fan-out exists.
