# Isolate Workers from the User's Herdr session and posse home

On 2026-09-24 a Worker cleaned up a workspace it had created in the User's Herdr session with `herdr workspace close wGN --group`. Herdr closed the Lead and all four Workers with it. The same day Workers created a real Task with `posse ride`, typed into the Lead pane and registered test Projects in the real posse home. Workers inherit the pane's `HERDR_SOCKET_PATH` and `HERDR_*`, so every `herdr` or `posse` command they ran reached the real session and home. The cause was the Worker's close, not the Lead's quota.

posse now enforces isolation at four layers:

1. **No shared group.** A Worker workspace is opened with `workspace.create` (cwd = Mount), never `worktree.open`. Closing it, with or without `--group`, closes only itself. *Amended by [ADR 0009](0009-riders-open-as-tabs-of-the-lead-workspace.md) and [ADR 0012](0012-group-repository-riders-under-lead.md): repository Riders now open as linked Herdr worktree children of the Lead; Workspace Project Riders remain tabs. A group close reaches the Lead and every repository Rider.*
2. **Worker mode in posse.** From a Worker, any command against that Worker's own posse home is refused with `worker_forbidden`, except `brief`, `holler`, `_context` and `config show|schema`. A process is a Worker of a home when it or any process it descends from carries `POSSE_WORKER_HOME` for that home in the environment it was started with, or runs inside one of the home's Mounts, or when its `HERDR_PANE_ID` is a Task's pane. Ancestors are read from `/proc`. `env -u`, `cd` or a subshell change only the child: the agent process keeps its start environment and its Mount cwd. A different `POSSE_HOME` is not that home, so experiments under a temp home still work.
3. **Guard on agent tool calls.** `posse setup` installs a `PreToolUse` hook running `posse _guard` for Claude Code and Codex, and a pi extension that calls it. The guard applies only to Workers. It parses each shell command and follows `export`, `unset`, `cd`, `env` (`-u`, `-i`, `-C`, `-S`), inline assignments, wrappers (`command`, `exec`, `nohup`, `timeout`, `sudo`, `setsid`, `xargs`, `find -exec` and others), `sh -c`, `eval`, shell script files, `source`, heredocs, here-strings and piped stdin. For each `herdr` call it works out which socket the call reaches, and refuses state changes to the Worker's session. After a conditional branch or loop changes a socket-setting variable, it treats the target as unknown rather than assuming a branch ran. It compares socket file identity as well as path, so an existing hard link is still the same socket, and refuses a direct `ln` alias of the session socket. Help flags are read-only only before the `--` argument delimiter. Code it cannot follow (python, node, perl, ruby and other interpreters, socket tools such as `socat`, `nc` and `curl --unix-socket`, non-literal program names) is refused when it refers to Herdr or its socket. Ordinary command data, such as an `rg` pattern or file path, does not trigger Herdr detection by itself. Only narrowly matched, single-purpose Python inspections invoked with exactly `-c <literal-program>` and no extra arguments, redirected or piped stdin, or interpreter startup overrides (`PYTHONINSPECT`, `PYTHONSTARTUP`, `PYTHONPATH`, `PYTHONHOME` or `PYTHONUSERBASE`) are exempt: `print(<literal>)`, `Path(<literal>).read_text()`, `importlib.metadata.version(<literal>)` and `sys.version` queries in a simple `print` expression. Other Python modes and interpreter programs remain opaque. A target the guard cannot resolve, including one assembled dynamically, is refused as unknown and is not treated as isolated. Code targeting a known different socket may run if it does not name the session's socket itself. Any recognized posse executable other than the one running the guard (including a renamed Go build recognized by build metadata, a copy, a Worker's build, or `go run ./cmd/posse`) is refused against the Worker's home, because it may not enforce Worker mode. The demonstrated `printf` plus redirection into a shell script or Make recipe is refused when the same tool command visibly names Herdr.
4. **Schema checks in the database.** Every applied migration's SHA-256 is recorded. Opening a database refuses `schema_unknown` when it has migrations this build does not know, and `schema_diverged` when a known migration was applied with different contents. Pending migrations on an existing database are applied only by the installed posse (the binary `posse setup` recorded), never by a Worker for its own home. `posse setup` migrates, so an upgrade is applied from the User's own run. The User can override every check except the Worker rule with `POSSE_FORCE_MIGRATION=1`.

The Rider protocol also tells Riders to experiment only against an isolated Herdr server and temp `POSSE_HOME`.

## Boundary

The threat is a Worker agent that makes mistakes, including ones that route around a single check: a script, a subshell, an interpreter, a copied binary. It is not an agent working to defeat posse.

posse enforces:

- Commands run by this version of posse check Worker identity against the process tree, including commands typed into the Worker's pane shell. Older builds do not enforce this check.
- Every shell command a Worker agent runs through its tool (Claude Code `Bash`, Codex `Bash`, pi `bash`) is checked by the guard before it runs, with the coverage in layer 3.
- A database change by this version of posse is refused when its schema check fails or the caller is a Worker. Older builds cannot be constrained by the database's own checks.

posse does not enforce, and ADR 0001's no-daemon design leaves no place to:

- **Anything that is not an agent tool call.** The guard can refuse `herdr` calls only in tool calls. Text typed into a pane by the User, the Lead, a test harness or `herdr pane run` runs in that pane's shell with its authority. In the review, `herdr agent prompt` and `herdr pane run` into the Lead and another Worker ran this way. Keystrokes into a pane come from whoever controls it.
- **Code whose Herdr access posse cannot see.** A shell hook is not a sandbox. Compiled programs (including test binaries), scripts or recipes generated with encoded or computed text, downloaded code, alternate shell entry points, and socket paths computed at runtime can evade static inspection. The demonstrated redirection-and-execution commands are refused when they visibly name Herdr, but the guard cannot reliably inspect future file contents. A binary without recognizable Go build metadata may be an older posse build or another program that changes the database. Database file permissions currently grant Worker processes write access, so schema checks cannot constrain an older build. Tests of posse itself reach Herdr only through `herdr.ValidateIsolatedEnvironment`.
- **Processes that leave the Worker's process tree** (double fork, `systemd-run`, `at`, cron) and also change directory out of the Mount. `/proc` is Linux-only; elsewhere Worker mode falls back to the process's own environment, pane and cwd.
- **An agent that disables or lacks the guard**, including an agent kind without the supported hooks, a different tool, or a modified hook configuration. Older posse builds can still change the Worker home if launched without detection by a tool hook.

Durable Herdr confinement needs server-side per-pane API permissions. Durable posse home protection against old binaries needs a separate file authority boundary.

## Evidence

Herdr 0.9.0, protocol 22, isolated server:

- Herdr groups worktree-aware workspaces by repository. `worktree.open --workspace <w>` makes `<w>` the group's primary and the new workspace a linked member. Closing a linked member needs no flag. Closing a primary refuses with `workspace_group_close_required`, and `close_group=true` closes every workspace with the same `repo_key`, including other primaries. A second repo-root workspace with its own linked worktree, closed with `--group`, closed the Lead's workspace and every Worker's. This matches the incident log: one `workspace.close` request closed `wAM` (Lead), `wAR`, `wAS`, `wGK`, `wGM` and `wGN`.
- A workspace from `workspace.create` has no worktree metadata, even at a Mount path, after git commands and after a server restart. It never joins a group. The e2e test replays the incident: with `worktree.open`, the Lead and Worker panes die; with `workspace.create`, both survive.
- At the time, grouping was rejected because it was cosmetic. ADR 0012 reverses that decision with explicit locks, recovery and group-close warnings.
- Herdr has no per-pane API scope, token or permission. The schema's only related call is `pane.clear_agent_authority`, which is about agent-status reporting. Herdr sets `HERDR_SOCKET_PATH` itself in every pane, after and over any `env` given to `workspace.create`.

## Considered options

- **Strip or redirect `HERDR_SOCKET_PATH` for Workers.** Herdr overrides it. Unsetting it only moves the CLI to the default socket, which is the same server. The Herdr integrations also need the real socket to report agent state and sessions, which reconcile and resume depend on.
- **A filtering socket proxy per Worker.** It could scope requests, but Herdr would still force the real socket path into the pane. It also adds a long-lived process per Worker, against ADR 0001.
- **A `herdr` shim first on the Worker's `PATH`.** Shell profiles prepend their own directories. In the User's panes `~/.local/bin` (the real `herdr`) came before any directory the pane started with.
- **An OS sandbox** (mount namespace hiding the socket). Linux-only, and the integrations would still need a filtered socket.

Agent tool hooks run inside the agent, see the exact command, get the same environment as the shell, and all three built-in kinds support blocking hooks: Claude Code and Codex with `PreToolUse` exit code 2, pi with a `tool_call` handler returning `block`.

## Consequences

- A Worker cannot change even its own workspace through `herdr` in the User's session. It has no reason to, and Teardown is posse's job.
- Workers can no longer run `posse config set`, which they previously could for keys that were not `user_only`.
- A Worker's scripts and opaque interpreter code that mention Herdr are refused unless they run against an isolated server. Narrowly matched Python literal-print, file-read and version-inspection commands in the default non-interactive mode and ordinary search data are allowed. A dynamically assembled or otherwise unverifiable target is refused rather than assumed isolated; the refusal says how to run experiments against an isolated server.
- The guard runs before every tool call of every Claude Code, Codex and pi session on the machine and costs about 4 ms of CPU for a caller that is not a Worker (`TestCommandPerformanceBudgets`).
- A posse built from a branch cannot migrate the User's home by accident. Running one against the real home needs `POSSE_FORCE_MIGRATION=1` from the User's shell.
- `posse project remove <name> [--yes]` unregisters Projects registered by mistake, but only Projects with no Tasks and no Mounts, so no Task history or worktree is ever dropped.
