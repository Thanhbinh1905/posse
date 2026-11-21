#!/bin/sh
set -eu

dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
printf 'TestAlpha\tcmd/posse/main.go\nTestAlpha\tinternal/app/start.go\n' > "$dir/TestAlpha.tsv"
printf 'TestBeta\tinternal/store/store.go\n' > "$dir/TestBeta.tsv"
printf 'TestAlpha\nTestBeta\n' > "$dir/shard-1-of-1.complete"
printf 'mode: count\ngithub.com/thanhbinh1905/posse/internal/app/extra.go:1.1,1.2 1 1\n' > "$dir/shard-1.out"
"$(dirname "$0")/build-e2e-coverage-map.sh" "$dir" 0123456789abcdef0123456789abcdef01234567 1 "$dir/map.tsv"
printf '#commit\t0123456789abcdef0123456789abcdef01234567\n#shard\t1\tinternal/app/extra.go\n#test\tTestAlpha\t1\n#test\tTestBeta\t1\nTestAlpha\tcmd/posse/main.go\nTestAlpha\tinternal/app/start.go\nTestBeta\tinternal/store/store.go\n' > "$dir/expected.tsv"
diff -u "$dir/expected.tsv" "$dir/map.tsv"

coverage_required() {
  printf '%s\n' "$@" | "$(dirname "$0")/e2e-coverage-map-required.sh"
}
[ "$(coverage_required .github/workflows/ci.yml)" = true ]
[ "$(coverage_required scripts/select-e2e-tests.sh)" = true ]
[ "$(coverage_required scripts/build-e2e-coverage-map.sh)" = true ]
[ "$(coverage_required internal/app/tasks.go)" = false ]

mkdir -p "$dir/source" "$dir/data" "$dir/coverage-output"
printf 'module github.com/thanhbinh1905/posse\n\ngo 1.25\n' > "$dir/source/go.mod"
mkdir -p "$dir/source/cmd/probe" "$dir/source/internal/app"
cat > "$dir/source/internal/app/start.go" <<'EOF'
package app

func Start() {}
EOF
cat > "$dir/source/cmd/probe/main.go" <<'EOF'
package main

import "github.com/thanhbinh1905/posse/internal/app"

func main() { app.Start() }
EOF
(cd "$dir/source" && go build -cover -coverpkg=./... -o "$dir/covered" ./cmd/probe)
GOCOVERDIR="$dir/data" "$dir/covered"
"$(dirname "$0")/collect-e2e-test-coverage.sh" TestCovdata "$dir/coverage-output/data.tsv" "$dir/data"
awk -F '\t' '$1 == "TestCovdata" && $2 == "internal/app/start.go" { found = 1 } END { exit !found }' "$dir/coverage-output/data.tsv"
