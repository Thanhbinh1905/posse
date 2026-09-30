![Posse: One Lead. A whole crew. Running on Herdr.](docs/banner.svg)

# Posse

**One Lead. A whole crew. Running on Herdr.**

Posse is a local-first CLI for coordinating coding agents. You talk to one Lead in a Herdr pane; the Lead writes task briefs and dispatches Riders (worker agents) into isolated worktrees. Posse tracks tasks, relays progress and decisions, and gates changes before they land. It supports a single repository or a workspace of repositories.

## How it works

1. Start a Lead with `posse up` from a Herdr pane in your project.
2. Describe the outcome you want. The Lead splits work into tasks and starts repository Riders as worktree children under its Herdr workspace. Workspace Project Riders use tabs.
3. Riders commit changes in their own branches and report back. The Lead reviews results and asks for decisions when needed.
4. Changes land locally or through pull requests, according to your project's landing mode and approval settings.

Posse keeps task state in a local SQLite database and uses Herdr to run the agents. See the [domain glossary](CONTEXT.md), [specification](docs/spec.md), and [architecture decisions](docs/adr/) for details.

## Install

You need [Herdr](https://github.com/herdrdev/herdr) (protocol 22 or newer), Git, and a coding agent supported by your Herdr installation. PR-based landing also requires authentication with your repository's forge CLI (`gh` for GitHub or `glab` for GitLab).

Review the [installer](install.sh) before running it:

```sh
curl -fsSL https://raw.githubusercontent.com/Thanhbinh1905/posse/main/install.sh | sh
```

The installer downloads a release to `~/.local/bin` by default and offers to set up Herdr integrations, agent skills and configuration. To install without setup, pass `--no-setup` after `sh -s --`. Run `posse setup` later to complete setup. Pass `--help` after `sh -s --` to see all options.

## Get started

1. Open a Herdr pane in your repository or workspace folder and run `posse up`. It asks once before registering an unregistered Project.
2. State the outcome you want, for example: "Fix the CSV export bug; open a PR, don't merge." The Lead raises only missing settings that affect this request, such as forge authentication or a Gate, then starts work.

With one available agent kind, `posse up` selects it without saving a preference. With several, choose with `posse up --<kind>` or `lead.kind`. Without Dispatch Rules or `dispatch.default.use`, Riders use the Lead's kind with its default model and effort. Inspect consequential gaps with `posse`.

The `posse-setup` skill (Claude Code: `/posse-setup`, Codex: `$posse-setup`) remains available as an optional full configuration tour. Use `posse config` to personalize later and `posse --help` to explore the CLI.

To uninstall, run the installer with `--uninstall` after `sh -s --`. Posse keeps local task history and configuration unless you confirm their removal.

## Acknowledgements

Inspired by [firstmate](https://github.com/kunchenguid/firstmate). The CLI follows [AXI](https://axi.md).

## License

[MIT](LICENSE).
