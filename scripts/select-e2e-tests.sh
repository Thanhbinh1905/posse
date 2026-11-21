#!/bin/sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)

select_tests() {
  base=$1
  map_file=$2
  head=${3:-HEAD}
  all=$(cd "$root" && go test -tags=e2e -list '^Test' ./internal/e2e | awk '/^Test/ { print $1 }') || return 1
  full() { printf '%s\n' "$all" | sort -u; }
  coverage_tests_for_path() {
    path=$1
    matches=$(awk -F '\t' -v path="$path" '$1 !~ /^#/ && $2 == path { print $1 }' "$map_file")
    shards=$(awk -F '\t' -v path="$path" '$1 == "#shard" && $3 == path { print $2 }' "$map_file" | sort -u)
    for shard in $shards; do
      tests_in_shard=$(awk -F '\t' -v shard="$shard" '$1 == "#test" && $3 == shard { print $2 }' "$map_file")
      matches="$matches
$tests_in_shard"
    done
    printf '%s\n' "$matches" | grep '^Test' | sort -u || true
  }
  coverage_tests_for_package() {
    prefix=$1/
    paths=$(awk -F '\t' -v prefix="$prefix" '($1 == "#shard" && index($3, prefix) == 1) || ($1 !~ /^#/ && index($2, prefix) == 1) { print ($1 == "#shard" ? $3 : $2) }' "$map_file" | sort -u)
    for source in $paths; do coverage_tests_for_path "$source"; done | sort -u
  }
  changed=$(cd "$root" && git diff --name-only --no-renames --diff-filter=ACMRTD "$base"..."$head") || { full; return; }
  [ -r "$map_file" ] || { full; return; }
  commit=$(awk -F '\t' 'NR == 1 && $1 == "#commit" { print $2 }' "$map_file")
  case "$commit" in
    ''|*[!0-9a-f]*) full; return ;;
  esac
  [ "$commit" = "$base" ] || { full; return; }
  (cd "$root" && git cat-file -e "$commit^{commit}") 2>/dev/null || { full; return; }
  awk -F '\t' '
    NR == 1 { if ($1 != "#commit" || $2 !~ /^[0-9a-f]+$/ || NF != 2) invalid = 1; next }
    $1 == "#test" { if (NF != 3 || $2 !~ /^Test[A-Za-z0-9_]+$/ || $3 !~ /^[1-9][0-9]*$/) invalid = 1; testrows++; testnames[$2] = 1; testshards[$2] = $3; next }
    $1 == "#shard" { if (NF != 3 || $2 !~ /^[1-9][0-9]*$/ || $3 !~ /^(internal|cmd)\/.+\.go$/ || $3 ~ /_test\.go$/) invalid = 1; shardrows++; shardfiles[$2] = 1; next }
    NF != 2 || $1 !~ /^Test[A-Za-z0-9_]+$/ || $2 !~ /^(internal|cmd)\/.+\.go$/ || $2 ~ /_test\.go$/ { invalid = 1; next }
    { rows++; if (!($1 in testnames)) invalid = 1; external[$1] = 1 }
    END {
      for (test in testnames)
        if (!(test in external) && !(testshards[test] in shardfiles)) invalid = 1
      if (invalid || rows + shardrows == 0 || testrows == 0) exit 1
    }
  ' "$map_file" || { full; return; }
  expected_tests=$(printf '%s\n' "$all" | sort -u)
  mapped_tests=$(awk -F '\t' '$1 == "#test" { print $2 }' "$map_file" | sort -u)
  [ "$mapped_tests" = "$expected_tests" ] || { full; return; }
  selected=
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    case "$path" in
      .github/*|go.mod|go.sum|CONTEXT.md|scripts/*|internal/store/*|internal/app/app.go|internal/app/service.go|internal/app/main.go|internal/app/cli.go|internal/app/command.go)
        full
        return
        ;;
      *.md)
        case "$path" in internal/*) ;; *) continue ;; esac
        package_dir=${path%/*}
        while [ "$package_dir" != internal ]; do
          if (cd "$root" && go list "./$package_dir" >/dev/null 2>&1); then break; fi
          package_dir=${package_dir%/*}
        done
        [ "$package_dir" != internal ] || { full; return; }
        relative=${path#"$package_dir"/}
        embedded=$(cd "$root" && go list -f '{{range .EmbedFiles}}{{println .}}{{end}}' "./$package_dir") || { full; return; }
        if printf '%s\n' "$embedded" | grep -Fxq "$relative" || [ ! -e "$root/$path" ] || grep -R -lF --include='*_test.go' -- "$(basename "$path")" "$root/$package_dir" >/dev/null 2>&1; then
          package_tests=$(coverage_tests_for_package "$package_dir")
          [ -n "$package_tests" ] || { full; return; }
          selected="$selected
$package_tests"
        fi
        continue
        ;;
      *.go)
        matches=$(coverage_tests_for_path "$path")
        case "$path" in
          internal/e2e/*_test.go)
            if [ -f "$root/$path" ]; then
              names=$(awk '/^func Test[^ (]*\(t \*testing\.T\)/ { name=$2; sub(/\(.*/, "", name); print name }' "$root/$path")
              if [ -n "$names" ]; then
                matches=$(printf '%s\n%s\n' "$matches" "$names")
              else
                full
                return
              fi
            else
              full
              return
            fi
            ;;
          *_test.go) continue ;;
          *) [ -n "$matches" ] || { full; return; } ;;
        esac
        selected="$selected
$matches"
        ;;
      *) full; return ;;
    esac
  done <<EOF
$changed
EOF
  if [ -n "$selected" ]; then
    selected=$(printf '%s\n' "$selected" | grep '^Test' | sort -u)
    printf '%s\n' "$selected" | while IFS= read -r test; do
      if printf '%s\n' "$all" | grep -Fxq "$test"; then echo "$test"; fi
    done
  fi
}

case "${1:-}" in
  --self-test)
    [ "$#" -eq 2 ] || { echo "usage: $0 --self-test <coverage-map>" >&2; exit 2; }
    awk -F '\t' 'NR == 1 && $1 == "#commit" && $2 ~ /^[0-9a-f]+$/ { valid = 1 } END { exit !valid }' "$2"
    ;;
  --list)
    [ "$#" -ge 3 ] && [ "$#" -le 4 ] || { echo "usage: $0 --list <base> <coverage-map> [head]" >&2; exit 2; }
    select_tests "$2" "$3" "${4:-HEAD}"
    ;;
  *) echo "usage: $0 --list <base> <coverage-map> [head]|--self-test <coverage-map>" >&2; exit 2 ;;
esac
