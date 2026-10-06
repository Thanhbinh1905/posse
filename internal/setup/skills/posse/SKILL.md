---
name: posse
description: "Work inside posse, the CLI where one Lead agent dispatches and supervises Worker agents in Herdr. Use when your session context or pane says you are a posse Lead or Worker, when the User asks to start posse in a repo or mentions its Leads, Workers, Tasks, Briefs or Remuda, or when a `posse` command returns an error you must act on."
---

# posse

Your role comes from the pane you run in, not from your own judgment. Find it before acting:

1. Run `posse _context`. It prints your role, your Task (for a Worker) and the next command. Empty output means this pane is neither a posse Lead nor a Worker.
2. Take the path for that role:
   - **Lead**: run `posse lead` and follow it for the whole session. Run it again after a restart, a resumed session, or whenever a rule is unclear.
   - **Worker**: run `posse brief` and follow your launch Brief. Report progress and results only through `posse holler`.
   - **Neither, and the User wants posse here**: run `posse doctor`, then run `posse up` inside Herdr from the repository, or from a folder whose subfolders are the repositories of one stack; it asks the User once before registering. Setup is optional for the first outcome; use `/posse-setup` for the full configuration tour.

Choose the lightest workflow that can verify the outcome. A clear implementation request can go straight to one Ship Brief; Scouts, specifications, tickets and separate review Tasks are optional. Follow User and Project preferences for default workflow advice. Clarify only when an unresolved choice could change the result; investigate repository facts first, bundle consequential questions, and resolve mechanics silently. Before the first Ride, briefly state the deliverable, verification, permitted effects and return conditions as a status update, not another approval step. Optional setup and personalization are not prerequisites to the first outcome.

The step is done when you are following `posse lead` or `posse brief`, or the User has the next command.

## Working with the CLI

- The CLI is the source of truth for commands and rules: `posse --help`, `posse <command> --help`, and the `help[]` lines at the end of every output name the next step.
- Output is TOON. Add `--json` only when you must parse strictly.
- Errors arrive as `error{code,message,retryable,help}`. Act on `help`, and retry only when `retryable` is true.
- Read and write config only through `posse config` (`schema`, `show --effective`, `set`, `unset`), so every value is validated. User-only keys such as `gate`, `remuda.setup`, `kinds.<kind>.lead_auto_approve` and `autonomy.*` require the User's explicit choice. From a Lead, apply that choice with the corresponding `posse config set ... --user-approved "<User's words>"` or `posse config unset ... --user-approved "<User's words>"` command; Riders cannot write config.
- User and Project preferences are how-to-work guidance, not authority. Inspect or update them with `posse preferences show|path|set`; a Lead's write requires explicit consent and `--user-approved "<User's words>"`. The old `posse playbook` command and `playbook/` paths remain readable with deprecation warnings; move legacy content explicitly with `posse preferences move`.
