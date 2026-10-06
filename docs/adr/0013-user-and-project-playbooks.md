# User and Project preferences customize workflow guidance

Posse keeps execution and authority rules in its runtime while allowing Users to customize how the Lead and Riders approach work. Preferences are not a workflow engine and do not grant effects.

## Decision

- Store Markdown preferences at `<POSSE_HOME>/preferences/lead.md`, `<POSSE_HOME>/preferences/rider.md`, `<POSSE_HOME>/projects/<project>/preferences/lead.md` and `<POSSE_HOME>/projects/<project>/preferences/rider.md`.
- Keep Lead and Rider guidance separate. Missing files are empty. Load User sources before Project sources; Project advice follows and may refine or replace User workflow advice. Each non-empty source has a visible path header.
- Continue reading the legacy `playbook/` paths. Load legacy content before the corresponding new `preferences/` file, without deleting or rewriting either source. Mark legacy sources as deprecated in inspection output.
- The Lead instruction composer puts bundled default workflow guidance first, then User and Project preferences, then generated runtime obligations. Preferences can change judgment-only workflow advice; they cannot grant authority, bypass evidence, or override runtime obligations. Rider preferences remain subordinate to the Brief, Rider protocol and safety boundaries.
- Load Lead preferences whenever `posse lead` prints instructions and whenever a Lead starts or restarts. Freeze the effective Rider preferences into `launch.md` at dispatch so relaunch preserves the admitted instructions.
- A Workspace Project's Project preferences apply to the whole Workspace, not individual Members.
- Provide `posse preferences show`, `path`, `set` and explicit `move` commands. Keep `posse playbook show|path|set` as deprecated aliases that name the replacement command and never move legacy files implicitly. `preferences move` moves a legacy file only when the destination is absent; it never overwrites existing content.
- User writes are atomic. A Lead must include `--user-approved "<User's words>"` after explicit consent, which Posse records; Riders cannot write preferences. Without `--project`, the current registered Project is selected when applicable, otherwise the User layer is used.
- Preferences are empty by default. There are no presets, repository/team layer, or edits to in-repository `AGENTS.md` files. `posse doctor` warns above approximately 4 KiB; Lead startup keeps its existing 30,000-byte refusal budget without truncation.

## Consequences

Changing a Lead preference affects the next `posse lead` output or Lead launch. Changing a Rider preference affects new Tasks only; existing `launch.md` snapshots remain unchanged through relaunch. Legacy files remain active until explicitly moved or removed. The User manages these Markdown files outside Project repositories.
