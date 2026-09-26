#!/bin/sh
set -eu

workflow_dir=.github/workflows
release_workflow=$workflow_dir/release.yml
ci_workflow=$workflow_dir/ci.yml

fail() {
  printf 'release workflow policy failed: %s\n' "$1" >&2
  exit 1
}

[ ! -e "$workflow_dir/release-please.yml" ] || fail 'Release Please workflow must be absent'
[ ! -e "$workflow_dir/release-please-fixes.yml" ] || fail 'Merge Fix Releases workflow must be absent'
[ ! -e release-please-config.json ] || fail 'Release Please config must be absent'
[ ! -e .release-please-manifest.json ] || fail 'Release Please manifest must be absent'

if grep -R -n -E 'RELEASE_PLEASE_TOKEN|release-please|googleapis/release-please-action' "$workflow_dir"; then
  fail 'workflows must not depend on Release Please or its token'
fi

main_push_workflows=$(grep -lF 'branches: [main]' "$workflow_dir"/*.yml || true)
[ "$main_push_workflows" = "$ci_workflow" ] || fail 'only CI may run on pushes to main'
grep -Fq '  contents: read' "$ci_workflow" || fail 'main CI must have read-only contents permission'
if grep -Eq ':[[:space:]]*write([[:space:]]|$)|permissions:[[:space:]]*write-all|git[[:space:]]+tag|gh[[:space:]]+release|RELEASE_PLEASE_TOKEN' "$ci_workflow"; then
  fail 'main CI must not have write access or create releases or tags'
fi

[ -f "$release_workflow" ] || fail 'tag-triggered Release workflow must remain'
release_triggers=$(awk '
  /^on:$/ { in_triggers = 1; print; next }
  in_triggers && /^[^[:space:]]/ { exit }
  in_triggers { print }
' "$release_workflow")
expected_triggers=$(printf "on:\n  push:\n    tags:\n      - 'v*'")
[ "$release_triggers" = "$expected_triggers" ] || fail 'Release workflow must trigger only on version tags'
grep -Fq 'go run github.com/goreleaser/goreleaser/v2@v2.18.2 release --clean' "$release_workflow" || fail 'Release workflow must run GoReleaser'
grep -Fq "GITHUB_TOKEN: \${{ secrets.GITHUB_TOKEN }}" "$release_workflow" || fail 'Release workflow must use GITHUB_TOKEN'

printf 'release workflow policy passed\n'
