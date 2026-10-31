# Running the slow suites locally

CI splits `internal/app` and `internal/e2e` into shards, each run with a 15m `go test` timeout. `scripts/run-local-tests.sh` runs the same shards with the same flags.

```sh
scripts/run-local-tests.sh app 3     # internal/app tests of race shard 3
scripts/run-local-tests.sh app       # internal/app tests of every race shard, one go test per shard
scripts/run-local-tests.sh e2e 5     # E2E shard 5
```

List which test belongs to which shard with `scripts/run-race-shard.sh --plan` and `scripts/run-e2e-shard.sh --plan`. E2E needs the Herdr release pinned in `.github/workflows/ci.yml` on `PATH`.

A single package without sharding needs a longer timeout: `go test ./internal/app -timeout 45m -run <pattern>`.
