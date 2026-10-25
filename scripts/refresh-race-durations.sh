#!/bin/sh
set -eu

# Rewrites race-durations.txt from the test shard jobs of a CI run, by default
# the latest successful CI run on main, or from a local `go test -race -v ./...`
# log when given a file path. A package recorded whole takes its `ok` line
# time; a package recorded per test takes each top-level test's duration. An
# entry absent from the run keeps its recorded duration; a recorded entry that
# no longer exists is dropped.
if [ "$#" -gt 1 ]; then
  echo "usage: $0 [<ci-run-id>|<go-test-log-file>]" >&2
  exit 2
fi
dir=$(dirname "$0")
file="$dir/race-durations.txt"
module=$(go list -m)
source=${1:-$(gh run list --workflow ci.yml --branch main --event push --status success --limit 1 --json databaseId --jq '.[0].databaseId')}
if [ -f "$source" ]; then
  log=$(cat "$source")
else
  log=$(gh run view "$source" --log |
    awk -F '\t' '$1 ~ /^test shard [0-9]+$/ { sub(/^[^\t]*\t[^\t]*\t[^ ]+ /, ""); print }')
fi
measured=$(printf '%s\n' "$log" | awk -v module="$module" -v file="$file" '
  function whole_seconds(text,    seconds, whole) {
    seconds = text + 0
    whole = int(seconds); if (whole < seconds) whole++; if (whole < 1) whole = 1
    return whole
  }
  BEGIN {
    while ((getline line < file) > 0) {
      n = split(line, f, " ")
      if (n == 3) split_pkg[f[2]] = 1
    }
  }
  /^--- (PASS|FAIL|SKIP): Test/ && $4 ~ /^\([0-9.]+s\)$/ {
    pending[$3] = whole_seconds(substr($4, 2, length($4) - 3))
    next
  }
  /^(ok|FAIL)[ \t]+/ {
    pkg = $2
    time = $3
    if (index(pkg, module "/") != 1) { delete pending; next }
    pkg = substr(pkg, length(module) + 2)
    if (pkg in split_pkg) {
      for (test in pending) if (pending[test] > max[pkg " " test]) max[pkg " " test] = pending[test]
    } else if (time ~ /^[0-9.]+s$/) {
      seconds = whole_seconds(time)
      if (seconds > max[pkg " "]) max[pkg " "] = seconds
    }
    delete pending
  }
  END { for (key in max) print max[key], key }' | sed 's/ $//')
if [ -z "$measured" ]; then
  echo "$source has no race shard durations" >&2
  exit 1
fi
planned=$("$dir/run-race-shard.sh" --plan)
current=$(printf '%s\n' "$planned" | awk '{ print $3, $4 }')
{
  printf '%s\n' "$current" | sed 's/^/current /'
  printf '%s\n' "$measured"
  awk 'NF == 2 || NF == 3' "$file"
} | awk '$1 == "current" { keep[$2 " " $3] = 1; next } ($2 " " $3 in keep) && !seen[$2 " " $3]++' | LC_ALL=C sort -k2,2 -k3,3 > "$file.tmp"
mv "$file.tmp" "$file"
printf 'Updated %s from %s\n' "$file" "$source"
