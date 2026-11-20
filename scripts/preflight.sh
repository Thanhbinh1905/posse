#!/bin/sh
set -eu

# Runs the deterministic checks that have broken main after a merge: the E2E
# shard assignment self-test, go generate leaving no diff, and timestamp versions
# for new migrations. Run it before posse publish.
cd "$(git rev-parse --show-toplevel)"

migrations_dir=internal/store/migrations
failed=0

step() {
  printf '== %s\n' "$1"
}

fail() {
  printf 'preflight: %s\n' "$1" >&2
  failed=1
}

step "E2E shard assignments"
scripts/run-e2e-shard.sh --self-test || fail "E2E shard self-test failed; record durations with scripts/refresh-e2e-durations.sh"

step "staticcheck"
scripts/staticcheck.sh || fail "staticcheck failed"

step "generated code"
if ! go generate ./...; then
  fail "go generate failed"
fi
if ! git diff --exit-code; then
  fail "go generate changed files; commit the regenerated output"
fi

step "new migration timestamp versions"
git fetch --quiet origin main || printf 'preflight: could not fetch origin/main; using the local ref\n' >&2
base=origin/main
if git rev-parse --verify --quiet "$base" >/dev/null; then
  version_of() {
    name=${1##*/}
    printf '%s\n' "${name%%_*}"
  }
  git ls-tree -r --name-only "$base" "$migrations_dir/" | while read -r path; do
    printf '%s %s\n' "$(version_of "$path")" "$path"
  done >"${TMPDIR:?TMPDIR must point to the Task scratch directory}/preflight-main-migrations.$$"
  for path in "$migrations_dir"/*.sql; do
    git cat-file -e "$base:$path" 2>/dev/null && continue
    version=$(version_of "$path")
    if ! printf '%s\n' "$version" | grep -Eq '^[0-9]{14}$'; then
      fail "$path is a new migration; use a 14-digit goose timestamp version (goose create without -s)"
      continue
    fi
    if ! awk -v version="$version" 'BEGIN {
      year = substr(version, 1, 4) + 0
      month = substr(version, 5, 2) + 0
      day = substr(version, 7, 2) + 0
      hour = substr(version, 9, 2) + 0
      minute = substr(version, 11, 2) + 0
      second = substr(version, 13, 2) + 0
      days[1] = 31; days[2] = 28; days[3] = 31; days[4] = 30
      days[5] = 31; days[6] = 30; days[7] = 31; days[8] = 31
      days[9] = 30; days[10] = 31; days[11] = 30; days[12] = 31
      if (year % 400 == 0 || year % 4 == 0 && year % 100 != 0) days[2] = 29
      valid = year > 0 && month >= 1 && month <= 12 && day >= 1 && day <= days[month] && hour <= 23 && minute <= 59 && second <= 59
      exit !valid
    }'; then
      fail "$path has an invalid goose timestamp version"
      continue
    fi
    other=$(awk -v v="$version" '$1 == v { print $2 }' "${TMPDIR:?TMPDIR must point to the Task scratch directory}/preflight-main-migrations.$$")
    if [ -n "$other" ]; then
      fail "$path reuses timestamp version $version of $other on $base; create a new timestamped migration"
    fi
  done
  rm -f "${TMPDIR:?TMPDIR must point to the Task scratch directory}/preflight-main-migrations.$$"
else
  fail "$base not found; cannot check migration numbers"
fi

if [ "$failed" -ne 0 ]; then
  exit 1
fi
printf 'preflight passed.\n'
