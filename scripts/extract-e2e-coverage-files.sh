#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <coverage-profile> <output>" >&2
  exit 2
fi
profile=$1
output=$2
[ -r "$profile" ] || { echo "coverage profile not readable: $profile" >&2; exit 1; }
awk '
  NF == 3 && $3 > 0 {
    file = $1
    sub(/:.*/, "", file)
    sub(/^github\.com\/thanhbinh1905\/posse\//, "", file)
    if (file ~ /^(internal|cmd)\/.+\.go$/ && file !~ /_test\.go$/)
      print file
  }' "$profile" | LC_ALL=C sort -u > "$output"
