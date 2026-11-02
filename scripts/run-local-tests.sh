#!/bin/sh
set -eu

# Runs the slow suites the way CI does, with the same shard plan, flags and
# 15m go test timeout:
#   scripts/run-local-tests.sh app [<race-shard>]   internal/app tests of one race
#                                                   shard, or of every shard
#   scripts/run-local-tests.sh e2e <e2e-shard>      one E2E shard
# Shard numbers come from `scripts/run-race-shard.sh --plan` and
# `scripts/run-e2e-shard.sh --plan`.
dir=$(dirname "$0")

shard_total() {
  "$dir/$1" --matrix | tr -d '[]' | awk -F, '{ print $NF }'
}

run_app_shard() {
  pattern=$("$dir/run-race-shard.sh" --plan | awk -v shard="$1" '$1 == shard && $3 == "internal/app" && $4 != "" { if (n++) printf "|"; printf "%s", $4 } END { print "" }')
  if [ -z "$pattern" ]; then
    echo "race shard $1 holds no internal/app tests" >&2
    return 0
  fi
  go test -race -v -count=1 -timeout=15m ./internal/app -run "^(${pattern})\$"
}

case "${1:-}" in
app)
  total=$(shard_total run-race-shard.sh)
  if [ "$#" -eq 2 ]; then
    run_app_shard "$2"
  elif [ "$#" -eq 1 ]; then
    shard=1
    while [ "$shard" -le "$total" ]; do
      run_app_shard "$shard"
      shard=$((shard + 1))
    done
  else
    echo "usage: $0 app [<race-shard>]|e2e <e2e-shard>" >&2
    exit 2
  fi
  ;;
e2e)
  if [ "$#" -ne 2 ]; then
    echo "usage: $0 app [<race-shard>]|e2e <e2e-shard>" >&2
    exit 2
  fi
  exec "$dir/run-e2e-shard.sh" "$2" "$(shard_total run-e2e-shard.sh)"
  ;;
*)
  echo "usage: $0 app [<race-shard>]|e2e <e2e-shard>" >&2
  exit 2
  ;;
esac
