#!/bin/sh
set -eu

# E2E tests are balanced across shards by the per-test durations recorded in
# e2e-durations.txt. A discovered test without a recorded duration is planned
# at default_seconds and a recorded test that no longer exists is ignored, so
# parallel PRs that add or remove tests never fail the check. Refresh the file
# from CI timing data with scripts/refresh-e2e-durations.sh.
#
# The shard count follows the plan: it is the smallest count whose plan keeps
# every shard within target_percent of budget_seconds, and --matrix prints it
# as the CI matrix. budget_seconds caps each shard's planned test time at 40%
# of the 15m go test timeout, leaving room for compilation and runner variance
# so a shard stays under about 60% of the timeout. target_percent leaves
# headroom of at least default_seconds, so one newly added test with no
# recorded duration cannot push a planned shard over budget_seconds.
budget_seconds=360
target_percent=75
target_seconds=$((budget_seconds * target_percent / 100))
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

# shards prints the smallest shard count whose plan keeps every shard within
# the target seconds. It fails when one test alone exceeds the target.
shards() {
  count=$(printf '%s\n' "$1" | awk -v target="$2" '
    NF == 2 {
      if ($1 > target) {
        printf "E2E test %s plans %ds, over the %ds shard target\n", $2, $1, target > "/dev/stderr"
        exit 1
      }
      total += $1
    }
    END { print (total == 0 ? 1 : int((total + target - 1) / target)) }') || return 1
  while [ "$(plan "$1" "$count" | awk '{ total[$1] += $2 } END { for (s in total) if (total[s] > max) max = total[s]; print max + 0 }')" -gt "$2" ]; do
    count=$((count + 1))
  done
  echo "$count"
}

# check_plan verifies that the plan runs every estimated test in exactly one
# shard and fails when a shard exceeds the target seconds.
check_plan() {
  estimated=$1
  planned=$(plan "$estimated" "$2")
  estimated_tests=$(printf '%s\n' "$estimated" | awk 'NF == 2 { print $2 }' | LC_ALL=C sort)
  planned_tests=$(printf '%s\n' "$planned" | awk 'NF == 3 && $1 >= 1 && $1 <= count { print $3 }' count="$2" | LC_ALL=C sort)
  if [ "$estimated_tests" != "$planned_tests" ]; then
    echo "E2E shard plan does not run every test exactly once." >&2
    return 1
  fi
  printf '%s\n' "$planned" | awk -v count="$2" -v target="$3" '
    { total[$1] += $2 }
    END {
      for (s = 1; s <= count; s++) {
        printf "E2E shard %d/%d plans %ds of %ds target\n", s, count, total[s], target
        if (total[s] > target) over = 1
      }
      if (over) {
        print "An E2E shard exceeds its target." > "/dev/stderr"
        exit 1
      }
    }'
}

run_self_check() {
  discovered=$(go test -tags=e2e -list '^Test' ./internal/e2e)
  validate_durations "$durations"
  warn_unrecorded "$discovered" "$durations"
  estimated=$(estimate "$discovered" "$durations")
  check_plan "$estimated" "$(shards "$estimated" "$target_seconds")" "$target_seconds"

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
    echo "self-check failed: a shard over its target was accepted" >&2
    return 1
  fi
  if [ "$(check_plan '5 TestA' 1 5)" != 'E2E shard 1/1 plans 5s of 5s target' ]; then
    echo "self-check failed: a shard at its target was rejected" >&2
    return 1
  fi
  if [ "$(shards '200 TestA
100 TestB' 306)" != 1 ] || [ "$(shards "200 TestA
100 TestB
$default_seconds TestNew" 306)" != 2 ]; then
    echo "self-check failed: the shard count does not grow with an unrecorded test" >&2
    return 1
  fi
  if [ "$(shards '4 TestA
4 TestB
4 TestC' 6)" != 3 ]; then
    echo "self-check failed: the shard count does not cover a plan the total alone underestimates" >&2
    return 1
  fi
  if shards '7 TestA' 6 >/dev/null 2>&1; then
    echo "self-check failed: a test over the shard target was accepted" >&2
    return 1
  fi
  printf 'E2E shard assignment self-check passed.\n'
}

if [ "$#" -eq 1 ] && [ "$1" = --self-test ]; then
  run_self_check
  exit 0
fi
if [ "$#" -eq 1 ] && { [ "$1" = --plan ] || [ "$1" = --matrix ]; }; then
  validate_durations "$durations"
  listed=$(go test -tags=e2e -list '^Test' ./internal/e2e)
  estimated=$(estimate "$listed" "$durations")
  shard_count=$(shards "$estimated" "$target_seconds")
  if [ "$1" = --matrix ]; then
    seq "$shard_count" | paste -sd, | sed 's/.*/[&]/'
  else
    plan "$estimated" "$shard_count"
  fi
  exit 0
fi
if [ "$#" -ne 2 ]; then
  echo "usage: $0 <shard> <shard-total>|--self-test|--plan|--matrix" >&2
  exit 2
fi
count=${POSSE_E2E_COUNT:-1}
case "$count" in
  ''|*[!0-9]*|0) echo "POSSE_E2E_COUNT must be a positive integer" >&2; exit 2 ;;
esac
timeout=${POSSE_E2E_TIMEOUT:-15m}
shard=$1
listed=$(go test -tags=e2e -list '^Test' ./internal/e2e)
validate_durations "$durations"
estimated=$(estimate "$listed" "$durations")
shard_count=$(shards "$estimated" "$target_seconds")
if [ "$2" != "$shard_count" ]; then
  echo "E2E shard total $2 does not match the planned $shard_count shards; build the CI matrix from $0 --matrix" >&2
  exit 2
fi
case "$shard" in
  ''|*[!0-9]*|0) echo "unknown E2E shard: $shard" >&2; exit 2 ;;
esac
if [ "$shard" -gt "$shard_count" ]; then
  echo "unknown E2E shard: $shard" >&2
  exit 2
fi

pattern=$(plan "$estimated" "$shard_count" | awk -v shard="$shard" '$1 == shard { if (n++) printf "|"; printf "%s", $3 } END { print "" }')
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
