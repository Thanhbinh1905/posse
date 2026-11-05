# Running the slow suites locally

CI splits `internal/app` and `internal/e2e` into shards, each run with a 15m `go test` timeout. `scripts/run-local-tests.sh` runs the same shards with the same flags.

```sh
scripts/run-local-tests.sh app 3     # internal/app tests of race shard 3
scripts/run-local-tests.sh app       # internal/app tests of every race shard, one go test per shard
scripts/run-local-tests.sh e2e 5     # E2E shard 5
```

List which test belongs to which shard with `scripts/run-race-shard.sh --plan` and `scripts/run-e2e-shard.sh --plan`. E2E needs the Herdr release pinned in `.github/workflows/ci.yml` on `PATH`.

A single package without sharding needs a longer timeout: `go test ./internal/app -timeout 45m -run <pattern>`.

## Codex memory regression

`go test -tags=e2e ./internal/e2e -run '^TestCodexMemoryCannotReopenCompletedAuthorDuringReview$' -count=1 -v` uses the installed Codex CLI, an isolated Herdr server, and a loopback Responses provider. It makes no paid model calls and uses synthetic memory state. CLI 0.166.0 generates the real Phase 2 prompt in a separate memory thread. A request barrier holds its tool response until the author completes and Review starts. The response policy does not inspect launch flags. The test also delivers an authorized Lead follow-up to the root Worker.

The test skips when Codex is absent. CI without that binary therefore covers the launch contract but cannot verify the real memory-thread behavior. The deterministic provider verifies prompt, tool, Signal, and Task boundaries; it does not predict live-model intent or establish general filesystem containment.
