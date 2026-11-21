#!/bin/sh
set -eu

if [ "$#" -lt 3 ]; then
  echo "usage: $0 <test-name> <output.tsv> <coverage-root>..." >&2
  exit 2
fi
test=$1
output=$2
shift 2
case "$test" in Test[A-Za-z0-9_]*) ;; *) echo "invalid test name: $test" >&2; exit 2 ;; esac
input=
for root do
  [ -d "$root" ] || continue
  dirs=$(find "$root" -type f -name 'covmeta.*' -exec dirname {} \; | LC_ALL=C sort -u)
  [ -n "$dirs" ] || continue
  root_input=$(printf '%s\n' "$dirs" | paste -sd, -)
  input=${input:+$input,}$root_input
done
: > "$output"
[ -n "$input" ] || exit 0
profile=$(mktemp "${TMPDIR:-/tmp}/e2e-coverage.XXXXXX")
trap 'rm -f -- "$profile"' EXIT
go tool covdata textfmt -i="$input" -o="$profile"
awk -v test="$test" '
  NR == 1 && $1 == "mode:" { next }
  NF == 3 && $3 > 0 {
    file = $1
    sub(/:.*/, "", file)
    sub(/^github\.com\/thanhbinh1905\/posse\//, "", file)
    if (file ~ /^(internal|cmd)\/.+\.go$/ && file !~ /_test\.go$/)
      print test "\t" file
  }' "$profile" | LC_ALL=C sort -u > "$output"
