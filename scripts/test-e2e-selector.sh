#!/bin/sh
set -eu

script=$(cd "$(dirname "$0")" && pwd)/select-e2e-tests.sh
map_file=$(mktemp)
stale_map=$(mktemp)
invalid_map=$(mktemp)
missing_map=$(mktemp)
mock_bin=$(mktemp -d)
rm -f -- "$missing_map"
trap 'rm -f -- "$map_file" "$stale_map" "$invalid_map" "$missing_map"; rm -rf -- "$mock_bin"' EXIT
real_git=$(command -v git)
cat > "$mock_bin/git" <<'EOF'
#!/bin/sh
if [ "$1" = diff ]; then
  printf '%s\n' "${POSSE_E2E_TEST_CHANGED:-}"
  exit 0
fi
exec "$POSSE_E2E_TEST_REAL_GIT" "$@"
EOF
chmod +x "$mock_bin/git"
export POSSE_E2E_TEST_REAL_GIT="$real_git"
head=$(git rev-parse HEAD)
tests=$(go test -tags=e2e -list '^Test' ./internal/e2e | awk '/^Test/ { print $1 }')
all_sorted=$(printf '%s\n' "$tests" | sort -u)
{
  printf '#commit\t%s\n' "$head"
  printf '%s\n' "$tests" | while IFS= read -r test; do printf '#test\t%s\t1\n' "$test"; done
  printf '#shard\t1\tinternal/axi/cli.go\n'
  first_test=$(printf '%s\n' "$tests" | head -n 1)
  printf '%s\tinternal/app/args.go\n' "$first_test"
} > "$map_file"
"$script" --self-test "$map_file"
[ -z "$("$script" --list "$head" "$map_file")" ]
[ "$first_test" = "$(PATH="$mock_bin:$PATH" POSSE_E2E_TEST_CHANGED=internal/app/args.go "$script" --list "$head" "$map_file")" ]
[ "$all_sorted" = "$(PATH="$mock_bin:$PATH" POSSE_E2E_TEST_CHANGED=internal/axi/cli.go "$script" --list "$head" "$map_file")" ]
[ "$all_sorted" = "$(PATH="$mock_bin:$PATH" POSSE_E2E_TEST_CHANGED=internal/store/store.go "$script" --list "$head" "$map_file")" ]
[ "$all_sorted" = "$(PATH="$mock_bin:$PATH" POSSE_E2E_TEST_CHANGED=internal/app/app.go "$script" --list "$head" "$map_file")" ]
[ -z "$(PATH="$mock_bin:$PATH" POSSE_E2E_TEST_CHANGED=docs/unused.md "$script" --list "$head" "$map_file")" ]
[ "$all_sorted" = "$(PATH="$mock_bin:$PATH" POSSE_E2E_TEST_CHANGED=.github/workflows/ci.yml "$script" --list "$head" "$map_file")" ]
[ "$all_sorted" = "$(PATH="$mock_bin:$PATH" POSSE_E2E_TEST_CHANGED=scripts/select-e2e-tests.sh "$script" --list "$head" "$map_file")" ]
[ "$all_sorted" = "$("$script" --list "$head" "$missing_map")" ]
printf '#commit\t%s\n' "$head" > "$invalid_map"
[ "$all_sorted" = "$(PATH="$mock_bin:$PATH" POSSE_E2E_TEST_CHANGED=internal/app/args.go "$script" --list "$head" "$invalid_map")" ]
printf '#commit\t%s\n' "$(git rev-parse HEAD^ 2>/dev/null || git rev-parse HEAD)" > "$stale_map"
[ "$all_sorted" = "$("$script" --list "$head" "$stale_map")" ]
