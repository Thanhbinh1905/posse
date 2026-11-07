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

1. Create a new Herdr workspace: press `Ctrl+B`, then `Shift+N`.
2. In the new workspace's shell, change to your Git repository and run `posse up`. If Posse has not registered the Project, it shows what it found and asks before registering it. You should then see the Lead in this workspace.
3. Tell the Lead what outcome you want, for example: "Fix the CSV export bug; open a PR, don't merge." When the Lead starts a Worker, you should see it in a tab. The Worker uses a separate Git worktree.

To create the workspace from a Herdr shell instead, run `herdr workspace create --cwd /path/to/your/repository --focus`. Then run `posse up` in a shell in the new workspace.

Each Herdr workspace holds one Project and its Lead. Workers use tabs inside that workspace. To work on another Project, create another workspace, not another tab or pane. Local delivery does not run automated verification. Pull-request delivery relies on forge CI at the exact commit being merged. Merge approval follows the Project's configured policy.

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
