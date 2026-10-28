#!/bin/sh
set -eu

# E2E tests are balanced across shards by the per-test durations recorded in
# e2e-durations.txt. A discovered test without a recorded duration is planned
# at default_seconds and a recorded test that no longer exists is ignored, so
# parallel PRs that add or remove tests never fail the check. Refresh the file
# from CI timing data with scripts/refresh-e2e-durations.sh.
#
# shard_count must match the CI matrices, which pass strategy.job-total.
# budget_seconds caps each shard's planned test time at 40% of the 15m go
# test timeout, leaving room for compilation and runner variance so a shard
# stays under about 60% of the timeout. The self-check warns once a shard
# plans over warn_percent of the budget and fails over the budget. On a
# warning, add a shard here and to every E2E matrix in .github/workflows/ci.yml.
shard_count=15
budget_seconds=360
warn_percent=85
# default_seconds is about twice the 95th percentile of recorded durations.
default_seconds=60
durations=$(cat "$(dirname "$0")/e2e-durations.txt")

validate_durations() {
  printf '%s\n' "$1" | awk '
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
  '
}

# tagged prints the manifest and the discovered tests as "recorded <seconds>
# <test>" and "discovered <test>" lines for the awk programs below.
tagged() {
  printf '%s\n' "$2" | awk 'NF == 2 { print "recorded", $1, $2 }'
  printf '%s\n' "$1" | awk 'NF == 1 && $1 ~ /^Test/ { print "discovered", $1 }'
}

# estimate prints "<seconds> <test>" for every discovered test: its recorded
# duration, or default_seconds when it has none.
estimate() {
  tagged "$1" "$2" | awk -v fallback="$default_seconds" '
    $1 == "recorded" { seconds[$3] = $2; next }
    { print ($2 in seconds ? seconds[$2] : fallback), $2 }'
}

# warn_unrecorded prints a CI warning for every discovered test without a
# recorded duration and every recorded test that no longer exists.
warn_unrecorded() {
  tagged "$1" "$2" | awk -v fallback="$default_seconds" '
    $1 == "recorded" { recorded[$3] = 1; next }
    { discovered[$2] = 1 }
    END {
      for (test in discovered) if (!(test in recorded))
        printf "::warning::E2E test %s has no recorded duration and is planned at %ds; refresh scripts/e2e-durations.txt with scripts/refresh-e2e-durations.sh once it runs on main\n", test, fallback
      for (test in recorded) if (!(test in discovered))
        printf "::warning::scripts/e2e-durations.txt records %s, which is not an E2E test; scripts/refresh-e2e-durations.sh drops it\n", test
    }' | LC_ALL=C sort
}

# plan prints "<shard> <seconds> <test>" for every estimated test. Longest
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

# check_plan verifies that the plan runs every estimated test in exactly one
# shard. It warns when a shard plans over warn_percent of the budget and fails
# when a shard exceeds the budget.
check_plan() {
  estimated=$1
  planned=$(plan "$estimated" "$2")
  estimated_tests=$(printf '%s\n' "$estimated" | awk 'NF == 2 { print $2 }' | LC_ALL=C sort)
  planned_tests=$(printf '%s\n' "$planned" | awk 'NF == 3 && $1 >= 1 && $1 <= count { print $3 }' count="$2" | LC_ALL=C sort)
  if [ "$estimated_tests" != "$planned_tests" ]; then
    echo "E2E shard plan does not run every test exactly once." >&2
    return 1
  fi
  printf '%s\n' "$planned" | awk -v count="$2" -v budget="$3" -v warn="$warn_percent" '
    { total[$1] += $2 }
    END {
      for (s = 1; s <= count; s++) {
        printf "E2E shard %d/%d plans %ds of %ds budget\n", s, count, total[s], budget
        if (total[s] > budget) over = 1
        else if (total[s] * 100 > budget * warn) near = 1
      }
      if (over) {
        print "An E2E shard exceeds its budget; raise shard_count in scripts/run-e2e-shard.sh and every E2E matrix in .github/workflows/ci.yml." > "/dev/stderr"
        exit 1
      }
      if (near) printf "::warning::An E2E shard plans over %d%% of its budget; raise shard_count in scripts/run-e2e-shard.sh and every E2E matrix in .github/workflows/ci.yml before a shard exceeds it.\n", warn
    }'
}

run_self_check() {
  discovered=$(go test -tags=e2e -list '^Test' ./internal/e2e)
  validate_durations "$durations"
  warn_unrecorded "$discovered" "$durations"
  check_plan "$(estimate "$discovered" "$durations")" "$shard_count" "$budget_seconds"

  if ! validate_durations '2 TestΩRecorded'; then
    echo "self-check failed: a valid Unicode test duration was rejected" >&2
    return 1
  fi
  if validate_durations '0 TestZero' >/dev/null 2>&1; then
    echo "self-check failed: a zero duration was accepted" >&2
    return 1
  fi
  if validate_durations '1 TestRecorded
2 TestRecorded' >/dev/null 2>&1; then
    echo "self-check failed: a duplicate test duration was accepted" >&2
    return 1
  fi
  if [ "$(estimate 'TestRecorded
TestΩNew
ok  	example.com/e2e	0.01s' '5 TestRecorded
7 TestRemoved' | tr '\n' ' ')" != "5 TestRecorded $default_seconds TestΩNew " ]; then
    echo "self-check failed: a missing test is not planned at the default estimate" >&2
    return 1
  fi
  if [ "$(warn_unrecorded 'TestRecorded
TestNew' '5 TestRecorded
7 TestRemoved' | awk '{ print $1, $2, $3, $4 }')" != '::warning::E2E test TestNew has
::warning::scripts/e2e-durations.txt records TestRemoved, which' ]; then
    echo "self-check failed: missing and removed tests are not reported as warnings" >&2
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
  if ! near=$(check_plan '9 TestA' 1 10) || [ "${near#*::warning::}" = "$near" ]; then
    echo "self-check failed: a shard near its budget did not warn" >&2
    return 1
  fi
  if [ "$(check_plan '8 TestA' 1 10)" != 'E2E shard 1/1 plans 8s of 10s budget' ]; then
    echo "self-check failed: a shard within the warning threshold warned" >&2
    return 1
  fi
  printf 'E2E shard assignment self-check passed.\n'
}

if [ "$#" -eq 1 ] && [ "$1" = --self-test ]; then
  run_self_check
  exit 0
fi
if [ "$#" -eq 1 ] && [ "$1" = --plan ]; then
  validate_durations "$durations"
  listed=$(go test -tags=e2e -list '^Test' ./internal/e2e)
  plan "$(estimate "$listed" "$durations")" "$shard_count"
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
validate_durations "$durations"
pattern=$(plan "$(estimate "$listed" "$durations")" "$shard_count" | awk -v shard="$shard" '$1 == shard { if (n++) printf "|"; printf "%s", $3 } END { print "" }')
printf 'Running E2E shard %s/%s\n' "$shard" "$shard_count"
started=$(date +%s)
# -v prints each top-level test's duration for scripts/refresh-e2e-durations.sh.
go test -tags=e2e -v -count="$count" -timeout="$timeout" ./internal/e2e -run "^(${pattern})$"
elapsed=$(($(date +%s) - started))
limit=$((budget_seconds * 3 / 2))
printf 'E2E shard %s/%s took %ss\n' "$shard" "$shard_count" "$elapsed"
if [ "$count" -eq 1 ] && [ "$elapsed" -gt "$limit" ]; then
  echo "::warning::E2E shard $shard took ${elapsed}s, over ${limit}s (60% of the 15m timeout); refresh scripts/e2e-durations.txt with scripts/refresh-e2e-durations.sh"
fi
