# Agent guidance

Before `posse publish`, run `scripts/preflight.sh`. It runs the E2E shard self-test, `go generate` with a clean diff, and a migration-number check against `origin/main`. Fix every failure first; each one has broken main after a merge.
