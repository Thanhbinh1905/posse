#!/bin/sh
set -eu

script=$(cd "$(dirname "$0")" && pwd)/select-e2e-tests.sh
output=$("$script" --self-test)
[ "$output" = 'E2E change selector self-test passed.' ]
