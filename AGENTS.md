# Posse repository instructions

When the User asks to publish or update a Posse release, follow [the release skill](.agents/skills/posse-release/SKILL.md). A release request authorizes its commit, tag, push, and publication steps; do not ask for approval again.

Before `posse publish`, run `scripts/preflight.sh`. It runs the E2E shard self-test, `go generate` with a clean diff, and a migration-number check against `origin/main`. Fix every failure first; each one has broken main after a merge.

## Agent skills

### Issue tracker

GitHub Issues in `Thanhbinh1905/posse`, managed with the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

The five canonical labels, used as-is: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `CONTEXT.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.
