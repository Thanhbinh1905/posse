# User and Project preferences customize workflow guidance

Posse keeps execution and authority rules in its runtime while allowing Users to customize how the Lead and Workers approach work. Preferences are not a workflow engine and do not grant effects.

## Decision

- Store Markdown preferences at `<POSSE_HOME>/preferences/lead.md`, `<POSSE_HOME>/preferences/worker.md`, `<POSSE_HOME>/projects/<project>/preferences/lead.md` and `<POSSE_HOME>/projects/<project>/preferences/worker.md`.
- Keep Lead and Worker guidance separate. Missing files are empty. Load User sources before Project sources; Project advice follows and may refine or replace User workflow advice. Each non-empty source has a visible path header.
- Continue reading the legacy `playbook/` paths. Load legacy content before the corresponding new `preferences/` file, without deleting or rewriting either source. Mark legacy sources as deprecated in inspection output.
- The Lead instruction composer puts bundled default workflow guidance first, then User and Project preferences, then generated runtime obligations. Preferences can change judgment-only workflow advice; they cannot grant authority, bypass evidence, or override runtime obligations. Worker preferences remain subordinate to the Brief, Worker protocol and safety boundaries.
- Load Lead preferences whenever `posse lead` prints instructions and whenever a Lead starts or restarts. Freeze the effective Worker preferences into `launch.md` at dispatch so relaunch preserves the admitted instructions.
- A Workspace Project's Project preferences apply to the whole Workspace, not individual Members.
- Provide `posse preferences show`, `path`, `set` and explicit `move` commands. [ADR 0026](0026-functional-vocabulary.md) removes the former command aliases: old input fails with its replacement, while legacy files remain readable. `preferences move` moves a legacy file only when the destination is absent; it never overwrites existing content.
- User writes are atomic. A Lead must include `--user-approved "<User's words>"` after explicit consent, which Posse records; Workers cannot write preferences. Without `--project`, the current registered Project is selected when applicable, otherwise the User layer is used.
- Preferences are empty by default. There are no presets, repository/team layer, or edits to in-repository `AGENTS.md` files. `posse doctor` warns above approximately 4 KiB; Lead startup keeps its existing 30,000-byte refusal budget without truncation.

## Consequences

Changing a Lead preference affects the next `posse lead` output or Lead launch. Changing a Worker preference affects new Tasks only; existing `launch.md` snapshots remain unchanged through relaunch. Legacy files remain active until explicitly moved or removed. The User manages these Markdown files outside Project repositories.
