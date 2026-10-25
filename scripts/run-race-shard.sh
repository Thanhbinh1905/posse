#!/bin/sh
set -eu

# Race-enabled unit tests are balanced across shards by the durations recorded
# in race-durations.txt, shared with the plain Go test in internal/e2e
# (TestRaceDurationsCoverEveryTest). A line is "<seconds> <package>" for a
# package that runs whole, or "<seconds> <package> <test>" for each top-level
# test of a package split across shards. Packages are module-relative, such as
# internal/store. Refresh the file from a CI run with
# scripts/refresh-race-durations.sh.
#
# shard_count must match the test-shard matrix in .github/workflows/ci.yml.
# budget_seconds caps each shard's planned test time at 40% of the 15m go test
# timeout, leaving room for compilation and runner variance so a shard stays
# under about 60% of the timeout. When the self-check reports a shard over
# budget, add a shard here and to the matrix.
shard_count=8
budget_seconds=360
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
  packages=$1
  manifest=$2

  if ! printf '%s\n' "$manifest" | awk '
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
  '; then
    return 1
  fi

  recorded_packages=$(printf '%s\n' "$manifest" | awk 'NF >= 2 { print $2 }' | LC_ALL=C sort -u)
  if [ "$packages" != "$recorded_packages" ]; then
    echo "race-durations.txt must record every package of the module." >&2
    printf 'Discovered packages:\n%s\nRecorded packages:\n%s\n' "$packages" "$recorded_packages" >&2
    return 1
  fi
}

# validate_split_tests checks that each split package records exactly the
# top-level tests that go test discovers in it.
validate_split_tests() {
  for pkg in $(split_packages "$2"); do
    discovered=$(go test -list '^Test' "./$pkg" | awk 'NF == 1 && $1 ~ /^Test/ { print $1 }' | LC_ALL=C sort)
    recorded=$(printf '%s\n' "$2" | awk -v pkg="$pkg" 'NF == 3 && $2 == pkg { print $3 }' | LC_ALL=C sort)
    if [ "$discovered" != "$recorded" ]; then
      printf 'race-durations.txt must record every top-level test of %s exactly once.\n' "$pkg" >&2
      printf 'Discovered tests:\n%s\nRecorded tests:\n%s\n' "$discovered" "$recorded" >&2
      return 1
    fi
  done
}

# plan prints "<shard> <seconds> <package> [<test>]" for every recorded unit.
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

# check_plan verifies that the plan runs every recorded unit in exactly one
# shard and that no shard exceeds the budget.
check_plan() {
  manifest=$1
  planned=$(plan "$manifest" "$2")
  recorded_units=$(printf '%s\n' "$manifest" | awk 'NF == 2 || NF == 3 { print $2, $3 }' | LC_ALL=C sort)
  planned_units=$(printf '%s\n' "$planned" | awk -v count="$2" '$1 >= 1 && $1 <= count { print $3, $4 }' | LC_ALL=C sort)
  if [ "$recorded_units" != "$planned_units" ]; then
    echo "Race shard plan does not run every recorded unit exactly once." >&2
    return 1
  fi
  printf '%s\n' "$planned" | awk -v count="$2" -v budget="$3" '
    { total[$1] += $2 }
    END {
      for (s = 1; s <= count; s++) {
        printf "Race shard %d/%d plans %ds of %ds budget\n", s, count, total[s], budget
        if (total[s] > budget) over = 1
      }
      if (over) {
        print "A race shard exceeds its budget; raise shard_count in scripts/run-race-shard.sh and the test-shard matrix in .github/workflows/ci.yml." > "/dev/stderr"
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
  packages=$(relative_packages)
  validate_durations "$packages" "$durations"
  validate_split_tests "$packages" "$durations"
  check_plan "$durations" "$shard_count" "$budget_seconds"

  if validate_durations 'pkg/a
pkg/b' '1 pkg/a' >/dev/null 2>&1; then
    echo "self-check failed: an unrecorded package was accepted" >&2
    return 1
  fi
  if validate_durations 'pkg/a' '1 pkg/a
2 pkg/a TestA' >/dev/null 2>&1; then
    echo "self-check failed: a package recorded whole and per test was accepted" >&2
    return 1
  fi
  if validate_durations 'pkg/a' '0 pkg/a' >/dev/null 2>&1; then
    echo "self-check failed: a zero duration was accepted" >&2
    return 1
  fi
  if validate_durations 'pkg/a' '1 pkg/a TestA
2 pkg/a TestA' >/dev/null 2>&1; then
    echo "self-check failed: a duplicate test duration was accepted" >&2
    return 1
  fi
  if ! validate_durations 'pkg/a
pkg/b' '1 pkg/a TestΩ
2 pkg/a TestB
3 pkg/b'; then
    echo "self-check failed: a valid manifest was rejected" >&2
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
    echo "self-check failed: a shard over budget was accepted" >&2
    return 1
  fi
  printf 'Race shard assignment self-check passed.\n'
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
shard=$1
if [ "$2" != "$shard_count" ]; then
  echo "race shard total $2 does not match shard_count $shard_count in $0; update the CI matrix or shard_count" >&2
  exit 2
fi
case "$shard" in
  ''|*[!0-9]*|0) echo "unknown race shard: $shard" >&2; exit 2 ;;
esac
if [ "$shard" -gt "$shard_count" ]; then
  echo "unknown race shard: $shard" >&2
  exit 2
fi

validate_durations "$(relative_packages)" "$durations"
shard_plan=$(plan "$durations" "$shard_count" | awk -v shard="$shard" '$1 == shard')
printf 'Running race shard %s/%s\n' "$shard" "$shard_count"
started=$(date +%s)

# -v prints each top-level test's duration for scripts/refresh-race-durations.sh.
shard_commands "$shard_plan" | while IFS= read -r command; do
  eval "go test -race -v -count=1 -timeout=15m $command" || exit 1
done

elapsed=$(($(date +%s) - started))
limit=$((budget_seconds * 3 / 2))
printf 'Race shard %s/%s took %ss\n' "$shard" "$shard_count" "$elapsed"
if [ "$elapsed" -gt "$limit" ]; then
  echo "::warning::Race shard $shard took ${elapsed}s, over ${limit}s (60% of the 15m timeout); refresh scripts/race-durations.txt with scripts/refresh-race-durations.sh <run-id>"
fi
