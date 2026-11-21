#!/bin/sh
set -eu

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <coverage-dir> <commit-sha> <shard-total> <output>" >&2
  exit 2
fi
coverage_dir=$1
commit=$2
shard_total=$3
output=$4
[ -d "$coverage_dir" ] || { echo "coverage directory not found: $coverage_dir" >&2; exit 1; }
case "$commit" in ''|*[!0-9a-f]*) echo "invalid commit SHA: $commit" >&2; exit 2 ;; esac
[ "${#commit}" -eq 40 ] || { echo "invalid commit SHA: $commit" >&2; exit 2; }
case "$shard_total" in ''|*[!0-9]*|0) echo "invalid shard total: $shard_total" >&2; exit 2 ;; esac
for shard in $(seq "$shard_total"); do
  marker=$coverage_dir/shard-$shard-of-$shard_total.complete
  profile=$coverage_dir/shard-$shard.out
  [ -s "$marker" ] || { echo "missing E2E coverage shard $shard/$shard_total" >&2; exit 1; }
  [ -s "$profile" ] || { echo "missing E2E in-process coverage profile for shard $shard/$shard_total" >&2; exit 1; }
  files=$coverage_dir/shard-$shard.files
  "$(dirname "$0")/extract-e2e-coverage-files.sh" "$profile" "$files"
  while IFS= read -r test; do
    case "$test" in Test[A-Za-z0-9_]*) ;; *) echo "invalid test in $marker: $test" >&2; exit 1 ;; esac
  done < "$marker"
done
{
  printf '#commit\t%s\n' "$commit"
  for shard in $(seq "$shard_total"); do
    marker=$coverage_dir/shard-$shard-of-$shard_total.complete
    while IFS= read -r test; do printf '#test\t%s\t%s\n' "$test" "$shard"; done < "$marker"
  done
  for shard in $(seq "$shard_total"); do
    files=$coverage_dir/shard-$shard.files
    while IFS= read -r file; do printf '#shard\t%s\t%s\n' "$shard" "$file"; done < "$files"
  done
  find "$coverage_dir" -type f -name '*.tsv' -exec cat {} + |
    awk -F '\t' 'NF == 2 && $1 ~ /^Test[A-Za-z0-9_]+$/ && $2 ~ /^(internal|cmd)\/.+\.go$/ && $2 !~ /_test\.go$/ { print }'
} | LC_ALL=C sort -u > "$output"
