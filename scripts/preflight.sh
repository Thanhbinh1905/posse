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
    parsed=$(date -u -d "$(printf '%s\n' "$version" | awk '{ print substr($0, 1, 8) " " substr($0, 9, 2) ":" substr($0, 11, 2) ":" substr($0, 13, 2) }')" +%Y%m%d%H%M%S 2>/dev/null || true)
    if [ "$parsed" != "$version" ]; then
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
