#!/bin/sh
set -eu

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <shard> <shard-total> <coverage-dir>" >&2
  exit 2
fi
shard=$1
shard_total=$2
coverage_dir=$3
mkdir -p "$coverage_dir"
export POSSE_E2E_COVERAGE_DIR="$coverage_dir"
export GOFLAGS="${GOFLAGS:+$GOFLAGS }-cover -coverpkg=./..."
"$(dirname "$0")/run-e2e-shard.sh" "$shard" "$shard_total"
marker=$coverage_dir/shard-$shard-of-$shard_total.complete
profile=$coverage_dir/shard-$shard.out
[ -s "$marker" ] || { echo "missing E2E coverage shard marker: $marker" >&2; exit 1; }
[ -s "$profile" ] || { echo "missing E2E in-process coverage profile: $profile" >&2; exit 1; }
while IFS= read -r test; do
  mapping=$coverage_dir/$test.tsv
  : > "$mapping"
  "$(dirname "$0")/collect-e2e-test-coverage.sh" "$test" "$coverage_dir/$test-child.tsv" "$coverage_dir/$test"
  cat "$coverage_dir/$test-child.tsv" >> "$mapping"
  LC_ALL=C sort -u "$mapping" -o "$mapping"
  rm -rf -- "${coverage_dir:?}/$test"
  rm -f -- "$coverage_dir/$test-child.tsv"
done < "$marker"
