![Posse: One Lead. A whole crew. Running on Herdr.](docs/banner.svg)

# Posse

**One Lead. A whole crew. Running on Herdr.**

Posse is a local-first CLI for coordinating coding agents. You talk to one Lead in a Herdr pane; the Lead writes task briefs and dispatches Riders (worker agents) into isolated worktrees. Posse tracks tasks, relays progress and decisions, and gates changes before they land. It supports a single repository or a workspace of repositories.

## How it works

1. Start a Lead with `posse up` from a Herdr pane in your project.
2. Describe the outcome you want. The Lead splits work into tasks and starts Riders in separate Herdr workspaces.
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

1. Open your agent and run the `posse-setup` skill (Claude Code: `/posse-setup`, Codex: `$posse-setup`). It finishes configuration and registers your projects.
2. Open a Herdr pane in a registered project and run `posse up`. An unregistered repository or workspace can also be registered when you start it.
3. Tell the Lead what you want done. Use `posse --help` to explore the CLI.

To uninstall, run the installer with `--uninstall` after `sh -s --`. Posse keeps local task history and configuration unless you confirm their removal.

## License

[MIT](LICENSE).
