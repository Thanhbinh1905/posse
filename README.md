![Posse: One Lead. A whole crew. Running on Herdr.](docs/banner.svg)

# Posse

**One Lead. A whole crew. Running on Herdr.**

Posse is a local-first CLI for coordinating coding agents. You talk to one Lead in a Herdr pane; the Lead writes task briefs and dispatches Workers (worker agents) into isolated worktrees. Posse tracks tasks, relays progress and decisions, and gates changes before they merge. It supports a single repository or a workspace of repositories.

## How it works

1. Start a Lead with `posse up` from a Herdr pane in your project.
2. Describe the outcome you want. The Lead splits work into tasks and starts repository Workers as worktree children under its Herdr workspace. Workspace Project Workers use tabs.
3. Workers commit changes in their own branches and report back. The Lead reviews results and asks for decisions when needed.
4. Changes merge locally or through pull requests, according to your project's merge mode and approval settings.

Posse keeps task state in a local SQLite database and uses Herdr to run the agents. See the [domain glossary](CONTEXT.md), [specification](docs/spec.md), and [architecture decisions](docs/adr/) for details.

## Install

You need [Herdr](https://github.com/herdrdev/herdr) (protocol 22 or newer), Git, and a coding agent supported by your Herdr installation. PR-based merging also requires authentication with your repository's forge CLI (`gh` for GitHub or `glab` for GitLab).

Review the [installer](install.sh) before running it:

```sh
curl -fsSL https://raw.githubusercontent.com/Thanhbinh1905/posse/main/install.sh | sh
```

The installer downloads a release to `~/.local/bin` by default and offers to set up Herdr integrations and configuration. Posse skills are injected only into sessions it launches; `posse setup --global-skills` optionally installs them for ordinary agent sessions. To install without setup, pass `--no-setup` after `sh -s --`. Run `posse setup` later to complete setup. Pass `--help` after `sh -s --` to see all options.

### Upgrading from 0.14

1. Back up the full Posse home and Project Git metadata as described in [ADR 0025](docs/adr/0025-functional-vocabulary.md).
2. From a User shell, run the installed 0.14 binary with `posse update --stop-lookouts`. That binary does not know `--stop-watches`; use the new flag only after upgrading.
3. `posse update` runs the new `posse setup` for integrations; run it manually after another installation method. If you installed optional global skills, also run `posse setup --global-skills` so agents do not learn removed commands from stale copies.
4. Restart every running Lead: from a User shell in each Project, run `posse up --replace`. Running Pi/OpenCode Leads retain their old adapters in memory. Their exact machine handoff/receipt calls remain supported during the transition, but restarting loads the new adapter and instructions.

Stored Tasks, config and paths upgrade automatically; existing worktrees are not moved. Removed User commands and flags fail with their replacement names.

## Get started

1. Open a Herdr pane in your repository or workspace folder and run `posse up`. It asks once before registering an unregistered Project.
2. State the outcome you want, for example: "Fix the CSV export bug; open a PR, don't merge." The Lead raises only missing settings that affect this request, such as forge authentication or a Gate, then starts work.

With one available agent kind, `posse up` selects it without saving a preference. With several, choose with `posse up --<kind>` or `lead.kind`. Without Dispatch Rules or `dispatch.default.use`, Workers use the Lead's kind with its default model and effort. Inspect consequential gaps with `posse`.

For a clear implementation request, the Lead can dispatch one Change directly. Research Tasks, specifications, tickets and separate review Tasks are optional. Customize workflow guidance with `posse preferences show|path|set`; User and Project preferences can change advice but not Posse's runtime obligations or authority checks. Legacy preference files remain readable; move them explicitly with `posse preferences move`. Removed commands fail with an error naming their replacement. See [ADR 0025](docs/adr/0025-functional-vocabulary.md) for the vocabulary mapping and automatic 0.14 upgrade.

The `posse-setup` skill (Claude Code: `/posse-setup`, Codex: `$posse-setup`) remains available as an optional full configuration tour. Use `posse config` to personalize later and `posse --help` to explore the CLI.

To uninstall, run the installer with `--uninstall` after `sh -s --`. Posse keeps local task history and configuration unless you confirm their removal.

## Acknowledgements

Inspired by [firstmate](https://github.com/kunchenguid/firstmate). The CLI follows [AXI](https://axi.md).

## License

[MIT](LICENSE).
