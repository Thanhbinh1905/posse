#!/bin/sh
set -eu

awk '
  /^\.github\// || /^scripts\/.*e2e/ {
    required = 1
  }
  END { print required ? "true" : "false" }
'
