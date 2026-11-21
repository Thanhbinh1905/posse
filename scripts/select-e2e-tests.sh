#!/bin/sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
map_file=$root/scripts/e2e-change-map.txt

select_tests() {
  base=$1
  head=${2:-HEAD}
  changed=$(cd "$root" && git diff --name-only --diff-filter=ACMRTD "$base"..."$head") || return 1
  tests=$(cd "$root" && go test -tags=e2e -list '^Test' ./internal/e2e | awk '/^Test/ { print $1 }') || return 1
  full=0
  patterns=
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    matched=0
    while IFS=' ' read -r path_regex test_regex; do
      [ -n "${path_regex:-}" ] || continue
      case "$path_regex" in \#*) continue ;; esac
      if printf '%s\n' "$path" | grep -Eq "$path_regex"; then matched=1; fi
    done < "$map_file"
    [ "$matched" -eq 1 ] || full=1
  done <<EOF
$changed
EOF
  while IFS=' ' read -r path_regex test_regex; do
    [ -n "${path_regex:-}" ] || continue
    case "$path_regex" in \#*) continue ;; esac
    while IFS= read -r path; do
      [ -n "$path" ] || continue
      if printf '%s\n' "$path" | grep -Eq "$path_regex"; then
        case "$test_regex" in
          ALL) full=1 ;;
          IGNORE) : ;;
          TESTFILE)
            file=$root/$path
            if [ -f "$file" ]; then
              names=$(awk '/^func Test[^ (]*\(t \*testing\.T\)/ { name=$2; sub(/\(.*/, "", name); print name }' "$file")
              patterns=${patterns:+$patterns
}$names
            else
              full=1
            fi
            ;;
          *) patterns=${patterns:+$patterns
}$test_regex ;;
        esac
      fi
    done <<EOF
$changed
EOF
  done < "$map_file"
  [ "$full" -eq 1 ] && { printf '%s\n' "$tests"; return; }
  if [ -z "$patterns" ]; then printf '%s\n' "$tests"; return; fi
  selected=$(printf '%s\n' "$tests" | grep -E "$patterns" | sort -u || true)
  [ -n "$selected" ] && printf '%s\n' "$selected" || printf '%s\n' "$tests"
}

self_test() {
  if ! grep -Fqx '^internal/store/ ALL' "$map_file"; then return 1; fi
  if ! grep -Fqx '^internal/app/(app|service|main|cli|command)\.go$ ALL' "$map_file"; then return 1; fi
  if ! grep -Fqx '^internal/app/.+_test\.go$ IGNORE' "$map_file"; then return 1; fi
  if ! grep -Fqx '^internal/e2e/.+_test\.go$ TESTFILE' "$map_file"; then return 1; fi
  if ! grep -Fqx '^internal/(dispatch|config)/ Test(Dispatch|Config|Vocabulary|SelectedReviewPolicy)' "$map_file"; then return 1; fi
  printf 'E2E change selector self-test passed.\n'
}

case "${1:-}" in
  --self-test) self_test ;;
  --list)
    [ "$#" -ge 2 ] && [ "$#" -le 3 ] || { echo "usage: $0 --list <base> [head]" >&2; exit 2; }
    select_tests "$2" "${3:-HEAD}"
    ;;
  *) echo "usage: $0 --list <base>|--self-test" >&2; exit 2 ;;
esac
