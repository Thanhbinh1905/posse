# Go, as one static binary, with our own AXI layer

posse is written in Go and ships as a single static `posse` binary. It is called constantly and in short bursts (every Herdr event runs `posse _ingest`, and Leads and Workers call it on every step), it mostly drives processes (`git`, `gh`, `herdr`, agent CLIs), and it has to keep working for years without a runtime to install or upgrade. Go gives a dependency-free binary, a stable toolchain, and a pure-Go SQLite (`modernc.org/sqlite`) with no cgo.

## Considered Options

- **TypeScript on Bun with `axi-sdk-js`**: rejected. It gets the AXI conventions and the official TOON library for free, but it needs Bun on every machine, ties state to `bun:sqlite`, and makes a 60-90 MB compiled binary. A first scaffold in TypeScript is kept on the `ts-scaffold` branch for reference.
- **Rust**: rejected. It fits Herdr's own stack, but it costs more than this CLI needs.

## Consequences

- No maintained Go AXI SDK exists and `toon-go` is stale, so posse owns a small internal AXI package. Its TOON encoder covers only what posse emits (objects, primitive arrays, tabular arrays) and must pass the matching official conformance fixtures from `toon-format/spec`.
