#!/bin/sh
set -eu

# Race-enabled unit tests are balanced across shards by the durations recorded
# in race-durations.txt. A line is "<seconds> <package>" for a package that
# runs whole, or "<seconds> <package> <test>" for each top-level test of a
# package split across shards. Packages are module-relative, such as
# internal/store. A discovered package or split test without a recorded
# duration is planned at default_seconds and a recorded one that no longer
# exists is ignored, so parallel PRs that add or remove tests never fail the
# check. Refresh the file from CI timing data with
# scripts/refresh-race-durations.sh.
#
# The shard count follows the plan: it is the smallest count whose plan keeps
# every shard within target_percent of budget_seconds, and --matrix prints it
# as the CI matrix. budget_seconds caps each shard's planned test time at 40%
# of the 25m go test timeout, leaving room for compilation and runner variance
# so a shard stays under about 60% of the timeout.
budget_seconds=600
target_percent=85
target_seconds=$((budget_seconds * target_percent / 100))
# default_seconds is above the 95th percentile of recorded split tests.
default_seconds=5
module=$(go list -m)
durations=$(cat "$(dirname "$0")/race-durations.txt")

# relative_packages prints every package of the module, module-relative.
relative_packages() {
  go list ./... | sed "s|^${module}/||" | LC_ALL=C sort
}

# split_packages prints the packages the manifest records test by test.
split_packages() {
  printf '%s\n' "$1" | awk 'NF == 3 { print $2 }' | LC_ALL=C sort -u
}

validate_durations() {
  printf '%s\n' "$1" | awk '
    NF == 0 { next }
    NF != 2 && NF != 3 {
      printf "invalid race duration on line %d: expected <seconds> <package> [<test>]\n", NR > "/dev/stderr"
      invalid = 1
      next
    }
    {
      if ($1 !~ /^[1-9][0-9]*$/) {
        printf "invalid race duration %s on line %d; expected whole seconds of at least 1\n", $1, NR > "/dev/stderr"
        invalid = 1
      }
      if (NF == 3 && $3 !~ /^Test/) {
        printf "invalid top-level test name %s on line %d\n", $3, NR > "/dev/stderr"
        invalid = 1
      }
      if (++seen[$2 " " $3] > 1) {
        printf "duplicate race duration: %s %s\n", $2, $3 > "/dev/stderr"
        invalid = 1
      }
      if (NF == 2) whole[$2] = 1; else split_pkg[$2] = 1
    }
    END {
      for (pkg in whole) if (pkg in split_pkg) {
        printf "package %s is recorded both whole and per test\n", pkg > "/dev/stderr"
        invalid = 1
      }
      if (invalid) exit 1
    }
  '
}

# units prints every unit to plan: "<package>" for a package that runs whole,
# and "<package> <test>" for each top-level test of a package the manifest
# splits. Listing with -race reuses the build for the shard's own go test run.
units() {
  packages=$(relative_packages)
  split=$({
    printf '%s\n' "$packages" | sed 's/^/package /'
    split_packages "$1" | sed 's/^/split /'
  } | awk '$1 == "package" { known[$2] = 1; next } $2 in known { print $2 }')
  {
    printf '%s\n' "$split" | sed 's/^/split /'
    printf '%s\n' "$packages" | sed 's/^/package /'
  } | awk '$1 == "split" { split_pkg[$2] = 1; next } NF == 2 && !($2 in split_pkg) { print $2 }'
  if [ -n "$split" ]; then
    # shellcheck disable=SC2046 # package paths have no spaces
    listed=$(go test -race -list '^Test' $(printf '%s\n' "$split" | sed 's|^|./|'))
    printf '%s\n' "$listed" | awk -v module="$module" '
      NF == 1 && $1 ~ /^Test/ { pending[++n] = $1; next }
      /^ok[ \t]/ {
        pkg = substr($2, length(module) + 2)
        for (i = 1; i <= n; i++) print pkg, pending[i]
        n = 0
      }'
  fi
}

# tagged prints the manifest and the units as "recorded <seconds> <package>
# [<test>]" and "unit <package> [<test>]" lines for the awk programs below.
tagged() {
  printf '%s\n' "$2" | awk 'NF == 2 || NF == 3 { print "recorded", $0 }'
  printf '%s\n' "$1" | awk 'NF == 1 || NF == 2 { print "unit", $0 }'
}

# estimate prints "<seconds> <package> [<test>]" for every unit: its recorded
# duration, or default_seconds when it has none.
estimate() {
  tagged "$1" "$2" | awk -v fallback="$default_seconds" '
    $1 == "recorded" { seconds[$3 " " $4] = $2; next }
    {
      key = $2 " " $3
      line = (key in seconds ? seconds[key] : fallback) " " key
      sub(/ $/, "", line)
      print line
    }'
}

# warn_unrecorded prints a CI warning for every unit without a recorded
# duration and every recorded unit that no longer exists.
warn_unrecorded() {
  tagged "$1" "$2" | awk -v fallback="$default_seconds" '
    $1 == "recorded" { key = $3 " " $4; sub(/ $/, "", key); recorded[key] = 1; next }
    { key = $2 " " $3; sub(/ $/, "", key); unit[key] = 1 }
    END {
      for (key in unit) if (!(key in recorded))
        printf "::warning::%s has no recorded race duration and is planned at %ds; refresh scripts/race-durations.txt with scripts/refresh-race-durations.sh once it runs on main\n", key, fallback
      for (key in recorded) if (!(key in unit))
        printf "::warning::scripts/race-durations.txt records %s, which no longer exists; scripts/refresh-race-durations.sh drops it\n", key
    }' | LC_ALL=C sort
}

# plan prints "<shard> <seconds> <package> [<test>]" for every estimated unit.
# Longest units go first, each to the shard with the least planned time so far.
plan() {
  printf '%s\n' "$1" |
    awk 'NF == 2 || NF == 3' |
    LC_ALL=C sort -k1,1nr -k2,2 -k3,3 |
    awk -v count="$2" '{
      best = 1
      for (s = 2; s <= count; s++) if (total[s] < total[best]) best = s
      total[best] += $1
      print best, $0
    }'
}

# shards prints the smallest shard count whose plan keeps every shard within
# the target seconds. It fails when one unit alone exceeds the target.
shards() {
  count=$(printf '%s\n' "$1" | awk -v target="$2" '
    NF == 2 || NF == 3 {
      if ($1 > target) {
        key = $2 " " $3
        sub(/ $/, "", key)
        printf "Race unit %s plans %ds, over the %ds shard target\n", key, $1, target > "/dev/stderr"
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

# check_plan verifies that the plan runs every estimated unit in exactly one
# shard and fails when a shard exceeds the target seconds.
check_plan() {
  estimated=$1
  planned=$(plan "$estimated" "$2")
  estimated_units=$(printf '%s\n' "$estimated" | awk 'NF == 2 || NF == 3 { print $2, $3 }' | LC_ALL=C sort)
  planned_units=$(printf '%s\n' "$planned" | awk -v count="$2" '$1 >= 1 && $1 <= count { print $3, $4 }' | LC_ALL=C sort)
  if [ "$estimated_units" != "$planned_units" ]; then
    echo "Race shard plan does not run every unit exactly once." >&2
    return 1
  fi
  printf '%s\n' "$planned" | awk -v count="$2" -v target="$3" '
    { total[$1] += $2 }
    END {
      for (s = 1; s <= count; s++) {
        printf "Race shard %d/%d plans %ds of %ds target\n", s, count, total[s], target
        if (total[s] > target) over = 1
      }
      if (over) {
        print "A race shard exceeds its target." > "/dev/stderr"
        exit 1
      }
    }'
}

# shard_commands prints the arguments of each go test invocation of a shard's
# plan: one for all whole packages, then one -run per split package.
shard_commands() {
  whole=$(printf '%s\n' "$1" | awk 'NF == 3 { printf " ./%s", $3 }')
  if [ -n "$whole" ]; then
    printf '%s\n' "${whole# }"
  fi
  for pkg in $(printf '%s\n' "$1" | awk 'NF == 4 { print $3 }' | LC_ALL=C sort -u); do
    pattern=$(printf '%s\n' "$1" | awk -v pkg="$pkg" 'NF == 4 && $3 == pkg { if (n++) printf "|"; printf "%s", $4 }')
    printf './%s -run '"'"'^(%s)$'"'"'\n' "$pkg" "$pattern"
  done
}

run_self_check() {
  validate_durations "$durations"
  discovered=$(units "$durations")
  warn_unrecorded "$discovered" "$durations"
  estimated=$(estimate "$discovered" "$durations")
  check_plan "$estimated" "$(shards "$estimated" "$target_seconds")" "$target_seconds"

  if validate_durations '1 pkg/a
2 pkg/a TestA' >/dev/null 2>&1; then
    echo "self-check failed: a package recorded whole and per test was accepted" >&2
    return 1
  fi
  if validate_durations '0 pkg/a' >/dev/null 2>&1; then
    echo "self-check failed: a zero duration was accepted" >&2
    return 1
  fi
  if validate_durations '1 pkg/a TestA
2 pkg/a TestA' >/dev/null 2>&1; then
    echo "self-check failed: a duplicate test duration was accepted" >&2
    return 1
  fi
  if ! validate_durations '1 pkg/a TestΩ
2 pkg/a TestB
3 pkg/b'; then
    echo "self-check failed: a valid manifest was rejected" >&2
    return 1
  fi
  if [ "$(estimate 'pkg/a TestA
pkg/a TestNew
pkg/b
pkg/new' '2 pkg/a TestA
3 pkg/a TestRemoved
4 pkg/b
6 pkg/removed' | tr '\n' ' ')" != "2 pkg/a TestA $default_seconds pkg/a TestNew 4 pkg/b $default_seconds pkg/new " ]; then
    echo "self-check failed: a missing unit is not planned at the default estimate" >&2
    return 1
  fi
  if [ "$(warn_unrecorded 'pkg/a TestA
pkg/a TestNew
pkg/new' '2 pkg/a TestA
3 pkg/a TestRemoved
6 pkg/removed' | awk '{ print $1, $2, $3 }')" != '::warning::pkg/a TestNew has
::warning::pkg/new has no
::warning::scripts/race-durations.txt records pkg/a
::warning::scripts/race-durations.txt records pkg/removed,' ]; then
    echo "self-check failed: missing and removed units are not reported as warnings" >&2
    return 1
  fi
  if [ "$(plan '5 pkg/a TestA
4 pkg/b
3 pkg/c
3 pkg/a TestD' 2 | awk '{ print $1 ":" $3 $4 }' | tr '\n' ' ')" != '1:pkg/aTestA 2:pkg/b 2:pkg/aTestD 1:pkg/c ' ]; then
    echo "self-check failed: the plan does not balance by duration" >&2
    return 1
  fi
  if [ "$(shard_commands '1 5 pkg/a TestA
1 3 pkg/a TestD
1 4 pkg/b
1 3 pkg/c')" != './pkg/b ./pkg/c
./pkg/a -run '"'"'^(TestA|TestD)$'"'"'' ]; then
    echo "self-check failed: a shard does not run its whole and split units" >&2
    return 1
  fi
  if check_plan '3 pkg/a
3 pkg/b' 1 5 >/dev/null 2>&1; then
    echo "self-check failed: a shard over its target was accepted" >&2
    return 1
  fi
  if [ "$(check_plan '5 pkg/a' 1 5)" != 'Race shard 1/1 plans 5s of 5s target' ]; then
    echo "self-check failed: a shard at its target was rejected" >&2
    return 1
  fi
  if [ "$(shards "200 pkg/a
$((100 - default_seconds)) pkg/b TestB
$default_seconds pkg/b TestC" 300)" != 1 ] || [ "$(shards "200 pkg/a
$((100 - default_seconds)) pkg/b TestB
$default_seconds pkg/b TestC
$default_seconds pkg/b TestNew" 300)" != 2 ]; then
    echo "self-check failed: the shard count does not grow with an unrecorded unit" >&2
    return 1
  fi
  if [ "$(shards '4 pkg/a
4 pkg/b
4 pkg/c TestC' 6)" != 3 ]; then
    echo "self-check failed: the shard count does not cover a plan the total alone underestimates" >&2
    return 1
  fi
  if shards '7 pkg/a TestA' 6 >/dev/null 2>&1; then
    echo "self-check failed: a unit over the shard target was accepted" >&2
    return 1
  fi
  printf 'Race shard assignment self-check passed.\n'
}

if [ "$#" -eq 1 ] && [ "$1" = --self-test ]; then
  run_self_check
  exit 0
fi
if [ "$#" -eq 1 ] && { [ "$1" = --plan ] || [ "$1" = --matrix ]; }; then
  validate_durations "$durations"
  estimated=$(estimate "$(units "$durations")" "$durations")
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
shard=$1
validate_durations "$durations"
estimated=$(estimate "$(units "$durations")" "$durations")
shard_count=$(shards "$estimated" "$target_seconds")
if [ "$2" != "$shard_count" ]; then
  echo "race shard total $2 does not match the planned $shard_count shards; build the CI matrix from $0 --matrix" >&2
  exit 2
fi
case "$shard" in
  ''|*[!0-9]*|0) echo "unknown race shard: $shard" >&2; exit 2 ;;
esac
if [ "$shard" -gt "$shard_count" ]; then
  echo "unknown race shard: $shard" >&2
  exit 2
fi

shard_plan=$(plan "$estimated" "$shard_count" | awk -v shard="$shard" '$1 == shard')
printf 'Running race shard %s/%s\n' "$shard" "$shard_count"
started=$(date +%s)

# -v prints each top-level test's duration for scripts/refresh-race-durations.sh.
shard_commands "$shard_plan" | while IFS= read -r command; do
  eval "go test -race -v -count=1 -timeout=25m $command" || exit 1
done

elapsed=$(($(date +%s) - started))
limit=$((budget_seconds * 3 / 2))
printf 'Race shard %s/%s took %ss\n' "$shard" "$shard_count" "$elapsed"
if [ "$elapsed" -gt "$limit" ]; then
  echo "::warning::Race shard $shard took ${elapsed}s, over ${limit}s (60% of the 25m timeout); refresh scripts/race-durations.txt with scripts/refresh-race-durations.sh"
fi
