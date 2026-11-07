![Posse: One Lead. A whole crew. Running on Herdr.](docs/banner.svg)

# Posse

**One Lead. A whole crew. Running on Herdr.**

Posse coordinates coding agents in Git repositories. You describe an outcome to a Lead in Herdr. The Lead delegates work to Workers in separate Git worktrees, tracks their progress, and brings their changes back for review and delivery.

## Install

You need [Herdr](https://github.com/herdrdev/herdr) with protocol 22 or newer, Git, and an agent supported by your Herdr installation. To open pull requests, authenticate the forge CLI for your repository (`gh` for GitHub or `glab` for GitLab).

Review the [installer](install.sh), then run:

```sh
curl -fsSL https://raw.githubusercontent.com/Thanhbinh1905/posse/main/install.sh | sh
```

The installer downloads the `posse` binary to `~/.local/bin` by default and offers to configure Posse's Herdr integrations. To install without setup, add `--no-setup` after `sh -s --`; run `posse setup` later.

## Delegate your first task

1. Open a Herdr workspace at your Git repository and run `posse up` in a pane. If Posse has not registered the Project, it shows what it found and asks before registering it.
2. Tell the Lead what outcome you want. For example: "Fix the CSV export bug; open a PR, don't merge."
3. You should see the Lead in the workspace. When you assign work, the Lead starts a Worker in its own tab and separate worktree. The Lead reports the result and follows the Project's delivery policy.

Use one Project per Herdr workspace. Each Project has one Lead. Posse isolates Worker changes in Git worktrees, but local delivery does not run automated verification. Pull-request delivery relies on forge CI at the exact commit being merged. Merge approval follows the Project's configured policy.

## Documentation

- [Full specification](docs/spec.md)
- [Recommended skills](docs/skills.md)
- [Upgrading and vocabulary changes](docs/adr/0026-functional-vocabulary.md)
- [Herdr sidebar layout setup](docs/spec.md#herdr-agents-sidebar-layout)
- [GitHub Releases](https://github.com/Thanhbinh1905/posse/releases)

## Acknowledgements

Inspired by [firstmate](https://github.com/kunchenguid/firstmate). The CLI follows [AXI](https://axi.md).

## License

[MIT](LICENSE).
