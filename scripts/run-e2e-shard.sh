#!/bin/sh
set -eu

# The shard manifest lives in e2e-shards.txt, shared with the plain Go test in
# internal/e2e (TestShardManifestCoversEveryE2ETest). Keep it complete so newly
# added tests cannot silently go unsharded.
assignments=$(cat "$(dirname "$0")/e2e-shards.txt")

validate_assignments() {
  discovered=$1
  manifest=$2

  if ! printf '%s\n' "$manifest" | awk '
    NF == 0 { next }
    NF != 2 {
      printf "invalid E2E shard assignment on manifest line %d: expected <shard> <test>\n", NR > "/dev/stderr"
      invalid = 1
      next
    }
    {
      if ($1 != "1" && $1 != "2" && $1 != "3" && $1 != "4") {
        printf "invalid E2E shard ID %s on manifest line %d; expected 1..4\n", $1, NR > "/dev/stderr"
        invalid = 1
      }
      if ($2 !~ /^Test/) {
        printf "invalid top-level E2E test name %s on manifest line %d\n", $2, NR > "/dev/stderr"
        invalid = 1
      }
      if (++seen[$2] > 1) {
        printf "duplicate E2E test assignment: %s\n", $2 > "/dev/stderr"
        invalid = 1
      }
    }
    END { if (invalid) exit 1 }
  '; then
    return 1
  fi

  discovered_tests=$(printf '%s\n' "$discovered" | awk 'NF == 1 && $1 ~ /^Test/ { print $1 }' | sort)
  assigned_tests=$(printf '%s\n' "$manifest" | awk 'NF == 2 { print $2 }' | sort)
  if [ "$discovered_tests" != "$assigned_tests" ]; then
    echo "E2E shard assignments must include every discovered top-level test exactly once." >&2
    printf 'Discovered tests:\n%s\nAssigned tests:\n%s\n' "$discovered_tests" "$assigned_tests" >&2
    return 1
  fi
}

run_self_check() {
  discovered=$(go test -tags=e2e -list '^Test' ./internal/e2e)
  validate_assignments "$discovered" "$assignments"

  if validate_assignments 'TestAssigned
TestΩUnassigned' '1 TestAssigned' >/dev/null 2>&1; then
    echo "self-check failed: an unassigned Unicode test was accepted" >&2
    return 1
  fi
  if ! validate_assignments 'TestΩUnassigned' '2 TestΩUnassigned'; then
    echo "self-check failed: a valid Unicode test assignment was rejected" >&2
    return 1
  fi
  if validate_assignments 'TestFixtureRootRemovesReadOnlyGoModuleDirectories' '5 TestFixtureRootRemovesReadOnlyGoModuleDirectories' >/dev/null 2>&1; then
    echo "self-check failed: an invalid manifest shard ID was accepted" >&2
    return 1
  fi
  if validate_assignments 'TestAssigned' '1 TestAssigned
2 TestAssigned' >/dev/null 2>&1; then
    echo "self-check failed: a duplicate test assignment was accepted" >&2
    return 1
  fi
  printf 'E2E shard assignment self-check passed.\n'
}

if [ "$#" -eq 1 ] && [ "$1" = --self-test ]; then
  run_self_check
  exit 0
fi
if [ "$#" -ne 1 ]; then
  echo "usage: $0 <1|2|3|4>|--self-test" >&2
  exit 2
fi
count=${POSSE_E2E_COUNT:-1}
case "$count" in
  ''|*[!0-9]*|0) echo "POSSE_E2E_COUNT must be a positive integer" >&2; exit 2 ;;
esac
timeout=${POSSE_E2E_TIMEOUT:-15m}
shard=$1
case "$shard" in
  1|2|3|4) ;;
  *) echo "unknown E2E shard: $shard" >&2; exit 2 ;;
esac

listed=$(go test -tags=e2e -list '^Test' ./internal/e2e)
validate_assignments "$listed" "$assignments"
pattern=$(printf '%s\n' "$assignments" | awk -v shard="$shard" '$1 == shard { if (count++) printf "|"; printf "%s", $2 } END { print "" }')
printf 'Running E2E shard %s/4\n' "$shard"
# Recovery-heavy shards take up to eleven minutes locally; allow bounded runner variance.
go test -tags=e2e -count="$count" -timeout="$timeout" ./internal/e2e -run "^(${pattern})$"
