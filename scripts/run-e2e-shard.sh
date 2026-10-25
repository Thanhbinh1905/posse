#!/bin/sh
set -eu

# E2E tests are balanced across shards by the per-test durations recorded in
# e2e-durations.txt, shared with the plain Go test in internal/e2e
# (TestE2EDurationsCoverEveryE2ETest). Refresh it from a CI run with
# scripts/refresh-e2e-durations.sh.
#
# shard_count must match the CI matrices, which pass strategy.job-total.
# budget_seconds caps each shard's planned test time at 40% of the 15m go
# test timeout, leaving room for compilation and runner variance so a shard
# stays under about 60% of the timeout. When the self-check reports a shard
# over budget, add a shard here and to every E2E matrix in .github/workflows/ci.yml.
shard_count=12
budget_seconds=360
durations=$(cat "$(dirname "$0")/e2e-durations.txt")

validate_durations() {
  discovered=$1
  manifest=$2

  if ! printf '%s\n' "$manifest" | awk '
    NF == 0 { next }
    NF != 2 {
      printf "invalid E2E duration on line %d: expected <seconds> <test>\n", NR > "/dev/stderr"
      invalid = 1
      next
    }
    {
      if ($1 !~ /^[1-9][0-9]*$/) {
        printf "invalid E2E duration %s on line %d; expected whole seconds of at least 1\n", $1, NR > "/dev/stderr"
        invalid = 1
      }
      if ($2 !~ /^Test/) {
        printf "invalid top-level E2E test name %s on line %d\n", $2, NR > "/dev/stderr"
        invalid = 1
      }
      if (++seen[$2] > 1) {
        printf "duplicate E2E test duration: %s\n", $2 > "/dev/stderr"
        invalid = 1
      }
    }
    END { if (invalid) exit 1 }
  '; then
    return 1
  fi

  discovered_tests=$(printf '%s\n' "$discovered" | awk 'NF == 1 && $1 ~ /^Test/ { print $1 }' | LC_ALL=C sort)
  recorded_tests=$(printf '%s\n' "$manifest" | awk 'NF == 2 { print $2 }' | LC_ALL=C sort)
  if [ "$discovered_tests" != "$recorded_tests" ]; then
    echo "e2e-durations.txt must record every discovered top-level E2E test exactly once." >&2
    printf 'Discovered tests:\n%s\nRecorded tests:\n%s\n' "$discovered_tests" "$recorded_tests" >&2
    return 1
  fi
}

# plan prints "<shard> <seconds> <test>" for every recorded test. Longest
# tests go first, each to the shard with the least planned time so far.
plan() {
  printf '%s\n' "$1" |
    awk 'NF == 2' |
    LC_ALL=C sort -k1,1nr -k2,2 |
    awk -v count="$2" '{
      best = 1
      for (s = 2; s <= count; s++) if (total[s] < total[best]) best = s
      total[best] += $1
      print best, $1, $2
    }'
}

# check_plan verifies that the plan runs every recorded test in exactly one
# shard and that no shard exceeds the budget.
check_plan() {
  manifest=$1
  planned=$(plan "$manifest" "$2")
  recorded_tests=$(printf '%s\n' "$manifest" | awk 'NF == 2 { print $2 }' | LC_ALL=C sort)
  planned_tests=$(printf '%s\n' "$planned" | awk 'NF == 3 && $1 >= 1 && $1 <= count { print $3 }' count="$2" | LC_ALL=C sort)
  if [ "$recorded_tests" != "$planned_tests" ]; then
    echo "E2E shard plan does not run every recorded test exactly once." >&2
    return 1
  fi
  printf '%s\n' "$planned" | awk -v count="$2" -v budget="$3" '
    { total[$1] += $2 }
    END {
      for (s = 1; s <= count; s++) {
        printf "E2E shard %d/%d plans %ds of %ds budget\n", s, count, total[s], budget
        if (total[s] > budget) over = 1
      }
      if (over) {
        print "An E2E shard exceeds its budget; raise shard_count in scripts/run-e2e-shard.sh and every E2E matrix in .github/workflows/ci.yml." > "/dev/stderr"
        exit 1
      }
    }'
}

run_self_check() {
  discovered=$(go test -tags=e2e -list '^Test' ./internal/e2e)
  validate_durations "$discovered" "$durations"
  check_plan "$durations" "$shard_count" "$budget_seconds"

  if validate_durations 'TestAssigned
TestΩUnrecorded' '1 TestAssigned' >/dev/null 2>&1; then
    echo "self-check failed: an unrecorded Unicode test was accepted" >&2
    return 1
  fi
  if ! validate_durations 'TestΩRecorded' '2 TestΩRecorded'; then
    echo "self-check failed: a valid Unicode test duration was rejected" >&2
    return 1
  fi
  if validate_durations 'TestZero' '0 TestZero' >/dev/null 2>&1; then
    echo "self-check failed: a zero duration was accepted" >&2
    return 1
  fi
  if validate_durations 'TestRecorded' '1 TestRecorded
2 TestRecorded' >/dev/null 2>&1; then
    echo "self-check failed: a duplicate test duration was accepted" >&2
    return 1
  fi
  if [ "$(plan '5 TestA
4 TestB
3 TestC
3 TestD' 2 | awk '{ print $1 ":" $3 }' | tr '\n' ' ')" != '1:TestA 2:TestB 2:TestC 1:TestD ' ]; then
    echo "self-check failed: the plan does not balance by duration" >&2
    return 1
  fi
  if check_plan '3 TestA
3 TestB' 1 5 >/dev/null 2>&1; then
    echo "self-check failed: a shard over budget was accepted" >&2
    return 1
  fi
  printf 'E2E shard assignment self-check passed.\n'
}

if [ "$#" -eq 1 ] && [ "$1" = --self-test ]; then
  run_self_check
  exit 0
fi
if [ "$#" -eq 1 ] && [ "$1" = --plan ]; then
  plan "$durations" "$shard_count"
  exit 0
fi
if [ "$#" -ne 2 ]; then
  echo "usage: $0 <shard> <shard-total>|--self-test|--plan" >&2
  exit 2
fi
count=${POSSE_E2E_COUNT:-1}
case "$count" in
  ''|*[!0-9]*|0) echo "POSSE_E2E_COUNT must be a positive integer" >&2; exit 2 ;;
esac
timeout=${POSSE_E2E_TIMEOUT:-15m}
shard=$1
if [ "$2" != "$shard_count" ]; then
  echo "E2E shard total $2 does not match shard_count $shard_count in $0; update the CI matrix or shard_count" >&2
  exit 2
fi
case "$shard" in
  ''|*[!0-9]*|0) echo "unknown E2E shard: $shard" >&2; exit 2 ;;
esac
if [ "$shard" -gt "$shard_count" ]; then
  echo "unknown E2E shard: $shard" >&2
  exit 2
fi

listed=$(go test -tags=e2e -list '^Test' ./internal/e2e)
validate_durations "$listed" "$durations"
pattern=$(plan "$durations" "$shard_count" | awk -v shard="$shard" '$1 == shard { if (n++) printf "|"; printf "%s", $3 } END { print "" }')
printf 'Running E2E shard %s/%s\n' "$shard" "$shard_count"
started=$(date +%s)
# -v prints each top-level test's duration for scripts/refresh-e2e-durations.sh.
go test -tags=e2e -v -count="$count" -timeout="$timeout" ./internal/e2e -run "^(${pattern})$"
elapsed=$(($(date +%s) - started))
limit=$((budget_seconds * 3 / 2))
printf 'E2E shard %s/%s took %ss\n' "$shard" "$shard_count" "$elapsed"
if [ "$count" -eq 1 ] && [ "$elapsed" -gt "$limit" ]; then
  echo "::warning::E2E shard $shard took ${elapsed}s, over ${limit}s (60% of the 15m timeout); refresh scripts/e2e-durations.txt with scripts/refresh-e2e-durations.sh <run-id>"
fi
