#!/bin/sh
set -eu

shard_count=4
app_package=github.com/thanhbinh1905/posse/internal/app
package_assignments='
1 github.com/thanhbinh1905/posse/cmd/posse
1 github.com/thanhbinh1905/posse/internal/atomicfile
1 github.com/thanhbinh1905/posse/internal/axi
1 github.com/thanhbinh1905/posse/internal/config
1 github.com/thanhbinh1905/posse/internal/dispatch
1 github.com/thanhbinh1905/posse/internal/execgroup
2 github.com/thanhbinh1905/posse/internal/execution
2 github.com/thanhbinh1905/posse/internal/herdr
2 github.com/thanhbinh1905/posse/internal/prepare
2 github.com/thanhbinh1905/posse/internal/sessionref
2 github.com/thanhbinh1905/posse/internal/setup
2 github.com/thanhbinh1905/posse/internal/taskplan
3 github.com/thanhbinh1905/posse/internal/runtime
3 github.com/thanhbinh1905/posse/internal/research
3 github.com/thanhbinh1905/posse/internal/usage
4 github.com/thanhbinh1905/posse/internal/store
4 github.com/thanhbinh1905/posse/internal/store/sqlc
'

app_tests() {
  go test -list '^Test' ./internal/app |
    awk 'NF == 1 && $1 ~ /^Test/ { print $1 }' |
    LC_ALL=C sort
}

app_tests_for_shard() {
  printf '%s\n' "$1" |
    awk -v shard="$2" -v count="$shard_count" 'NF && (NR - 1) % count + 1 == shard { print $0 }'
}

validate_assignments() {
  if ! printf '%s\n' "$package_assignments" | awk '
    NF == 0 { next }
    NF != 2 {
      printf "invalid race package assignment on manifest line %d: expected <shard> <package>\n", NR > "/dev/stderr"
      invalid = 1
      next
    }
    {
      if ($1 != "1" && $1 != "2" && $1 != "3" && $1 != "4") {
        printf "invalid race shard ID %s on manifest line %d; expected 1..4\n", $1, NR > "/dev/stderr"
        invalid = 1
      }
      if (++seen[$2] > 1) {
        printf "duplicate race package assignment: %s\n", $2 > "/dev/stderr"
        invalid = 1
      }
    }
    END { if (invalid) exit 1 }
  '; then
    return 1
  fi

  discovered_packages=$(go list ./... | LC_ALL=C sort)
  assigned_packages=$(printf '%s\n' "$package_assignments" | awk 'NF == 2 { print $2 }' | LC_ALL=C sort)
  expected_packages=$(printf '%s\n' "$discovered_packages" | grep -Fvx "$app_package" || true)
  if [ "$assigned_packages" != "$expected_packages" ]; then
    echo "Race shard package assignments must include every non-app package exactly once." >&2
    printf 'Discovered packages:\n%s\nAssigned packages:\n%s\n' "$expected_packages" "$assigned_packages" >&2
    return 1
  fi

  tests=$(app_tests)
  if [ -z "$tests" ]; then
    echo "No app tests were discovered for race sharding." >&2
    return 1
  fi
  assigned_tests=
  for shard_id in 1 2 3 4; do
    shard_tests=$(app_tests_for_shard "$tests" "$shard_id")
    if [ -z "$shard_tests" ]; then
      printf 'Race shard %s has no app tests.\n' "$shard_id" >&2
      return 1
    fi
    assigned_tests=$(printf '%s\n%s\n' "$assigned_tests" "$shard_tests")
  done
  assigned_tests=$(printf '%s\n' "$assigned_tests" | awk 'NF' | LC_ALL=C sort)
  if [ "$tests" != "$assigned_tests" ]; then
    echo "Race app shards must include every discovered top-level test exactly once." >&2
    printf 'Discovered tests:\n%s\nAssigned tests:\n%s\n' "$tests" "$assigned_tests" >&2
    return 1
  fi
}

if [ "$#" -eq 1 ] && [ "$1" = --self-test ]; then
  validate_assignments
  printf 'Race shard assignment self-check passed.\n'
  exit 0
fi
if [ "$#" -ne 1 ]; then
  echo "usage: $0 <1|2|3|4>|--self-test" >&2
  exit 2
fi
shard=$1
case "$shard" in
  1|2|3|4) ;;
  *) echo "unknown race shard: $shard" >&2; exit 2 ;;
esac

validate_assignments
tests=$(app_tests)
pattern=$(app_tests_for_shard "$tests" "$shard" | awk 'NR > 1 { printf "|" } { printf "%s", $0 }')
printf 'Running race shard %s/%s app tests\n' "$shard" "$shard_count"
go test -race -count=1 -timeout=15m ./internal/app -run "^(${pattern})$"

set --
while IFS=' ' read -r package_shard package; do
  [ -n "$package_shard" ] || continue
  if [ "$package_shard" = "$shard" ]; then
    set -- "$@" "$package"
  fi
done <<EOF
$package_assignments
EOF
if [ "$#" -gt 0 ]; then
  printf 'Running race shard %s/%s supporting packages\n' "$shard" "$shard_count"
  go test -race -count=1 -timeout=15m "$@"
fi
