# User and Project Playbooks add workflow instructions

Posse is a foundation, not a workflow framework. The User needs customizable instructions for both Lead and Rider roles without adding workflow policy to Posse or editing shared repository guidance.

## Decision

- Store optional Markdown files in `<POSSE_HOME>/playbook/lead.md`, `<POSSE_HOME>/playbook/rider.md`, `<POSSE_HOME>/projects/<project>/playbook/lead.md` and `<POSSE_HOME>/projects/<project>/playbook/rider.md`.
- Keep the roles separate. Missing files are empty; User content precedes Project content, and each non-empty source has a visible header.
- Load Lead Playbooks whenever `posse lead` prints instructions and whenever a Lead starts or restarts. Freeze the effective Rider Playbook into `launch.md` at dispatch so relaunch preserves the admitted instructions.
- A Workspace Project's Playbook applies to the whole Workspace, not individual Members.
- Playbooks supplement built-in instructions. Posse hard rules and the Task contract take precedence. There is no Playbook-specific size cap; `posse doctor` warns above approximately 4 KiB and Lead startup keeps its existing 30,000-byte refusal budget.
- Provide `posse playbook show` and `posse playbook path` for inspection. Playbooks are empty by default, with no presets, repository/team layer, or edits to in-repository `AGENTS.md` files.

## Consequences

Changing a Lead Playbook affects the next `posse lead` output or Lead launch. Changing a Rider Playbook affects new Tasks only; existing `launch.md` snapshots remain unchanged through relaunch. The User manages these Markdown files outside Project repositories.
