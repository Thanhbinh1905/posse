#!/bin/sh
set -eu

cd "$(git rev-parse --show-toplevel)"
version=$(cat scripts/staticcheck-version)
staticcheck="honnef.co/go/tools/cmd/staticcheck@$version"

if [ "${1-}" = "--version" ]; then
  exec go run "$staticcheck" -version
fi

exec go run "$staticcheck" -tags=e2e,perf ./...
