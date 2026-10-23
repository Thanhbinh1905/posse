---
name: posse
description: "Work inside posse, the CLI where one Lead agent dispatches and supervises Worker agents in Herdr. Use when your session context or pane says you are a posse Lead or Worker, when the User asks to start posse in a repo or mentions its Leads, Workers, Tasks, Briefs or Pool, or when a `posse` command returns an error you must act on."
---

# posse

Your role comes from the pane you run in, not from your own judgment. Find it before acting:

1. Run `posse _context`. It prints your role, your Task (for a Worker) and the next command. Empty output means this pane is neither a posse Lead nor a Worker.
2. Take the path for that role:
   - **Lead**: run `posse lead` and follow it for the whole session. Run it again after a restart, a resumed session, or whenever a rule is unclear.
   - **Worker**: run `posse brief` and follow your launch Brief. Report progress and results only through `posse signal`.
   - **Neither, and the User wants posse here**: run `posse doctor`, then run `posse up` inside Herdr from a Git repository or a folder whose subfolders are the repositories of one stack. Posse orchestrates Git repositories and workspaces; edit non-Git directories directly outside Posse. Setup is optional for the first outcome; use `/posse-setup` for the full configuration tour.

Choose the lightest workflow that can verify the outcome. A clear implementation request can go straight to one Change Brief; Research Tasks, specifications, tickets and separate review Tasks are optional. Follow User and Project preferences for default workflow advice. Clarify only when an unresolved choice could change the result; investigate repository facts first, bundle consequential questions, and resolve mechanics silently. Before the first Start, briefly state the deliverable, verification, permitted effects and return conditions as a status update, not another approval step. Optional setup and personalization are not prerequisites to the first outcome.

The step is done when you are following `posse lead` or `posse brief`, or the User has the next command.

## Working with the CLI

- The CLI is the source of truth for commands and rules: `posse --help`, `posse <command> --help`, and the `help[]` lines at the end of every output name the next step.
- Output is TOON. Add `--json` only when you must parse strictly.
- Errors arrive as `error{code,message,retryable,help}`. Act on `help`, and retry only when `retryable` is true.
- If the User directly gives follow-up work after your Change Task is `done` or `merging`, or any Task is `stalled` or `needs-decision`, first run `posse signal working "<reason>"` and wait for success before acting. Posse cancels a pending Merge; if its merge has started, the Signal is refused, so stop and ask the Lead to resolve it.
- Posse orchestrates Git repositories and Workspaces only. Edit non-Git directories directly outside Posse. PR delivery is verified by forge CI at the exact `gated_sha`; local delivery is a fast-forward with no automated verification, so the User is responsible for checking it before approval.
- Read and write config only through `posse config` (`schema`, `show --effective`, `set`, `unset`), so every value is validated. User-only keys, including Sandbox setup commands, Autonomy, kind argument templates/trust preparation and Profile kind/arguments, require the User's explicit choice. From a Lead, apply that choice with the corresponding `posse config set ... --user-approved "<User's words>"` or `posse config unset ... --user-approved "<User's words>"` command; Workers cannot write config.
- To apply the User's explicit lighter delivery/review for one Task, select `merge_mode:`/`review:` in its Brief and run `posse start ... --user-approved "<User's words>"`. The admitted plan records the quote and selected fields; this never grants merge, discard or executable authority. Without the flag, Briefs only tighten. Consent quotes are provenance records, not authentication.
- Native research children are bounded, read-only and tracked. In Claude, use Explore, Plan or a custom agent with an explicit read-only tool list; Posse hooks register it and refuse write-capable or untracked children. For Codex or Pi, run `posse research start --harness codex|pi --scope <dir> --prompt "<question>" --wait`; a Pi Lead can use the `posse_research` tool. Inspect children with `posse research list|show`, continue one with `posse research follow-up` and stop one with `posse research cancel`. Implementation still belongs to a Worker.
- User and Project preferences are how-to-work guidance, not authority. Inspect or update them with `posse preferences show|path|set`; a Lead's write requires explicit consent and `--user-approved "<User's words>"`. Legacy preference files remain readable; move their content explicitly with `posse preferences move`. Removed commands fail with an error naming their replacement.
