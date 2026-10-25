#!/bin/sh
set -eu

# Rewrites e2e-durations.txt from the E2E shard jobs of a CI run. A test that
# ran in several jobs keeps its longest duration; a test absent from the run
# keeps its recorded one.
if [ "$#" -ne 1 ]; then
  echo "usage: $0 <ci-run-id>" >&2
  exit 2
fi
file="$(dirname "$0")/e2e-durations.txt"
measured=$(gh run view "$1" --log |
  awk -F '\t' '$1 ~ /^e2e (main duplicate )?shard [0-9]+$/ { print $3 }' |
  awk '/^[^ ]+ --- / && $3 ~ /^(PASS|FAIL|SKIP):$/ && $4 ~ /^Test/ && $5 ~ /^\([0-9.]+s\)$/ {
    seconds = substr($5, 2, length($5) - 3) + 0
    whole = int(seconds); if (whole < seconds) whole++; if (whole < 1) whole = 1
    if (whole > max[$4]) max[$4] = whole
  }
  END { for (test in max) print max[test], test }')
if [ -z "$measured" ]; then
  echo "run $1 has no E2E shard test durations" >&2
  exit 1
fi
{
  printf '%s\n' "$measured"
  awk 'NF == 2' "$file"
} | awk '!seen[$2]++' | LC_ALL=C sort -k2,2 > "$file.tmp"
mv "$file.tmp" "$file"
printf 'Updated %s from run %s\n' "$file" "$1"
