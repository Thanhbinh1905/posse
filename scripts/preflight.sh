#!/bin/sh
set -eu

# Runs the deterministic checks that have broken main after a merge: the E2E
# shard assignment self-test, go generate leaving no diff, and a migration
# number that origin/main already uses. Run it before posse publish.
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

step "go generate"
before=$(git status --porcelain)
go generate ./...
after=$(git status --porcelain)
if [ "$before" != "$after" ]; then
  git status --short >&2
  fail "go generate changed files; commit the regenerated output"
fi

step "migration numbers against origin/main"
git fetch --quiet origin main || printf 'preflight: could not fetch origin/main; using the local ref\n' >&2
base=origin/main
if git rev-parse --verify --quiet "$base" >/dev/null; then
  version_of() {
    name=${1##*/}
    printf '%s\n' "${name%%_*}"
  }
  git ls-tree --name-only "$base" "$migrations_dir/" | while read -r path; do
    printf '%s %s\n' "$(version_of "$path")" "$path"
  done >"${TMPDIR:-/tmp}/preflight-main-migrations.$$"
  for path in "$migrations_dir"/*.sql; do
    git cat-file -e "$base:$path" 2>/dev/null && continue
    version=$(version_of "$path")
    other=$(awk -v v="$version" '$1 == v { print $2 }' "${TMPDIR:-/tmp}/preflight-main-migrations.$$")
    if [ -n "$other" ]; then
      fail "$path reuses version $version of $other on $base; renumber it above the latest on $base"
    fi
  done
  rm -f "${TMPDIR:-/tmp}/preflight-main-migrations.$$"
else
  fail "$base not found; cannot check migration numbers"
fi

if [ "$failed" -ne 0 ]; then
  exit 1
fi
printf 'preflight passed.\n'
