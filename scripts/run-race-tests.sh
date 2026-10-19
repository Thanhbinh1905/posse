#!/usr/bin/env bash
set -euo pipefail

app_package=github.com/thanhbinh1905/posse/internal/app
scratch=${TMPDIR:-${TMP:-/tmp}}
log_dir=$(mktemp -d "$scratch/posse-race.XXXXXX")
trap 'rm -rf -- "$log_dir"' EXIT

listed=$(go test -list '^Test' ./internal/app)
mapfile -t tests < <(printf '%s\n' "$listed" | awk '$1 ~ /^Test/')
if [ "${#tests[@]}" -eq 0 ]; then
  echo "no internal/app tests were discovered" >&2
  exit 1
fi

patterns=( '' '' )
for index in "${!tests[@]}"; do
  shard=$((index % 2))
  if [ -n "${patterns[$shard]}" ]; then
    patterns[shard]+='|'
  fi
  patterns[shard]+="${tests[index]}"
done

other_package_list=$(go list ./... | awk -v app="$app_package" '$0 != app')
mapfile -t other_packages <<< "$other_package_list"
if [ "${#other_packages[@]}" -eq 0 ] || [ -z "${other_packages[0]}" ]; then
  echo "no non-app packages were discovered" >&2
  exit 1
fi

printf 'Running %s internal/app tests in two race-enabled workers and all other packages once.\n' "${#tests[@]}"
start=$SECONDS
GOMAXPROCS=2 go test -race -count=1 -timeout=35m -run "^(${patterns[0]})$" ./internal/app >"$log_dir/app-0.log" 2>&1 &
pid0=$!
GOMAXPROCS=2 go test -race -count=1 -timeout=35m -run "^(${patterns[1]})$" ./internal/app >"$log_dir/app-1.log" 2>&1 &
pid1=$!
GOMAXPROCS=2 go test -race -count=1 -timeout=35m -p=1 "${other_packages[@]}" >"$log_dir/other.log" 2>&1 &
pid_other=$!

status=0
for entry in "app-0:$pid0" "app-1:$pid1" "other:$pid_other"; do
  name=${entry%%:*}
  pid=${entry#*:}
  if ! wait "$pid"; then
    status=1
    printf '\n--- %s race test output ---\n' "$name" >&2
    cat "$log_dir/$name.log" >&2
  else
    printf '\n--- %s race test output ---\n' "$name"
    cat "$log_dir/$name.log"
  fi
done

printf '\nRace suite elapsed: %s seconds\n' "$((SECONDS - start))"
exit "$status"
