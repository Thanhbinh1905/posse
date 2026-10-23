---
name: posse-update
description: "Check for Posse updates and guide safe installation and Lead restarts."
disable-model-invocation: true
---

# posse update

Do not install an update from an agent pane. Run `posse update --check` and report `current`, `latest`, the changelog summary and the running `watches` list. If versions match, stop and say no restart is needed.

Then run `posse --full` and `posse doctor`. Report live Tasks, failed or lost Tasks, pending Decisions, undelivered Notices, and setup, skill or Watch warnings that will survive an update. An update can refuse with `migration_refused` if the release requires no live Tasks and one is live. Never suggest `--force` unless the User explicitly accepts that risk.

Explain that running Project Watches and the Lead watcher re-exec the installed binary automatically. Do not pass `--stop-watches` unless the update cannot re-exec a Watch or the User explicitly asks to force-stop it. The next Project command repairs a force-stopped Watch. A running Lead retains its old adapter and instructions in memory until restart, so restart it for the new binary's behavior.

Give the User these exact commands, to run from a User shell, never run them yourself:

1. In the Project folder: `posse update`
2. If `posse doctor` reports Posse-owned global skills, also run `posse setup --global-skills`; the updater does not refresh optional global copies.
3. In each Project folder with a running Lead: `posse up --replace`. If the User exits the Lead agent first, `posse up` in that same pane is enough.
4. Verify with `posse doctor` and `posse`.

For errors, report the code and follow its recovery:

- `config_repair_required`: the new binary is installed. Repair the named config key, then run `posse setup`.
- `setup_failed`: report whether the previous binary was restored. Do not retry blindly.
- `migration_refused`: report the Task. Do not use `--force` without explicit User approval.
- `release_unavailable`: GitHub is unreachable; retry later.

Do not use `--version` unless the User requests a specific release. Hand off the commands and wait for the User to run them.
