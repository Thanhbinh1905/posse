#!/bin/sh
set -eu

# Stress the top-level Go tests a pull request adds or changes. CI runs every
# test once, so a new timing-sensitive test can pass once and flake on later
# pull requests; repeating it before merge exposes it at the PR that adds it.
#
#   --list <base>        print "<kind> <tags> <package> <test>" for every
#                        top-level test whose lines changed in <base>...HEAD
#   --run unit|e2e <base> run those tests with -count=$POSSE_STRESS_COUNT
#   --self-test          check the listing and a deliberately flaky test
#
# A test is changed when the diff touches a line between its "func Test" line
# and its closing brace. A change to a shared helper outside any test function
# lists no test. Files with an e2e build tag are kind "e2e" and run without
# -race, as CI runs them; every other file is kind "unit" and runs with -race.
# Each package runs in its own serial go test invocation.
#
# Runtime cap: one go test invocation stops after $POSSE_STRESS_TIMEOUT
# (default 20m) and the CI job stops at its timeout-minutes, which fails the
# pull request. The cost scales with the changed tests only; a pull request
# without a changed test runs nothing.
count=${POSSE_STRESS_COUNT:-10}
timeout=${POSSE_STRESS_TIMEOUT:-20m}

list_tests() {
  git diff -U0 --diff-filter=AMR "$1"...HEAD -- '*_test.go' |
    awk '
      /^\+\+\+ b\// { file = substr($0, 7); next }
      /^@@ / {
        split($3, plus, ",")
        start = substr(plus[1], 2) + 0
        len = (plus[2] == "" ? 1 : plus[2] + 0)
        files[file] = files[file] " " start " " (len == 0 ? start + 1 : start + len - 1)
      }
      END { for (f in files) print f files[f] }' |
    LC_ALL=C sort |
    while read -r file ranges; do
      tags=$(sed -n 's|^//go:build ||p' "$file" | head -n 1 | tr -c 'a-z0-9_' '\n' | awk '$1 == "e2e" || $1 == "perf"' | sort -u | paste -sd, -)
      kind=unit
      case ",$tags," in *,e2e,*) kind=e2e ;; esac
      awk -v ranges="$ranges" -v kind="$kind" -v tags="${tags:--}" -v pkg="$(dirname "$file")" '
        BEGIN { n = split(ranges, r, " ") }
        function changed(from, to,    i) {
          for (i = 1; i < n; i += 2) if (r[i] <= to && r[i + 1] >= from) return 1
          return 0
        }
        /^func Test[^(]*\(t \*testing\.T\)/ {
          name = $2
          sub(/\(.*/, "", name)
          start = FNR
        }
        /^}/ && name != "" {
          if (changed(start, FNR)) print kind, tags, pkg, name
          name = ""
        }' "$file"
    done
}

run_kind() {
  kind=$1
  tests=$(list_tests "$2" | awk -v kind="$kind" '$1 == kind')
  if [ -z "$tests" ]; then
    printf 'No %s tests changed against %s; nothing to stress.\n' "$kind" "$2"
    return 0
  fi
  printf '%s\n' "$tests" | awk '{ print $2, $3 }' | LC_ALL=C sort -u |
    while read -r tags pkg; do
      pattern=$(printf '%s\n' "$tests" | awk -v tags="$tags" -v pkg="$pkg" '$2 == tags && $3 == pkg { if (n++) printf "|"; printf "%s", $4 }')
      flags="-count=$count -timeout=$timeout -v"
      [ "$kind" = unit ] && flags="-race $flags"
      [ "$tags" != - ] && flags="-tags=$tags $flags"
      printf 'Stressing %s: %s\n' "$pkg" "$pattern"
      # shellcheck disable=SC2086 # flags hold no spaces inside a word
      go test $flags "./$pkg" -run "^($pattern)\$" || exit 1
    done
}

self_test() {
  scratch=$(mktemp -d)
  trap 'rm -rf "$scratch"' EXIT
  script=$(cd "$(dirname "$0")" && pwd)/$(basename "$0")
  cd "$scratch"
  git init -q -b main .
  git config user.email stress@example.invalid
  git config user.name stress
  cat > go.mod <<'EOF'
module scratch

go 1.26.0
EOF
  mkdir -p unit e2e
  cat > unit/a_test.go <<'EOF'
package unit

import "testing"

func helper() int { return 1 }

func TestKeep(t *testing.T) {
	t.Log("keep")
}

func TestEdit(t *testing.T) {
	t.Log("before")
}
EOF
  cat > e2e/b_test.go <<'EOF'
//go:build e2e && linux

package e2e

import "testing"

func TestE2EKeep(t *testing.T) {
	t.Log("keep")
}
EOF
  git add -A
  git commit -q -m base
  base=$(git rev-parse HEAD)

  git checkout -q -b change
  cat > unit/a_test.go <<'EOF'
package unit

import "testing"

func helper() int { return 2 }

func TestKeep(t *testing.T) {
	t.Log("keep")
}

func TestEdit(t *testing.T) {
	t.Log("after")
}

func TestNew(t *testing.T) {
	t.Log("new")
}
EOF
  cat > e2e/b_test.go <<'EOF'
//go:build e2e && linux

package e2e

import "testing"

func TestE2EKeep(t *testing.T) {
	t.Log("keep")
}

func TestE2ENew(t *testing.T) {
	t.Log("new")
}
EOF
  git add -A
  git commit -q -m change
  expected='e2e e2e e2e TestE2ENew
unit - unit TestEdit
unit - unit TestNew'
  if [ "$(list_tests "$base")" != "$expected" ]; then
    echo "self-check failed: the changed tests are not listed exactly" >&2
    list_tests "$base" >&2
    return 1
  fi

  git checkout -q -b doc "$base"
  printf '\n// note\n' >> unit/a_test.go
  git commit -q -am comment
  if [ -n "$(list_tests "$base")" ]; then
    echo "self-check failed: a change outside every test listed a test" >&2
    return 1
  fi

  git checkout -q -b stable "$base"
  cat >> unit/a_test.go <<'EOF'

func TestStable(t *testing.T) {
	t.Log("stable")
}
EOF
  git commit -q -am stable
  if ! POSSE_STRESS_COUNT=10 "$script" --run unit "$base" >/dev/null 2>&1; then
    echo "self-check failed: a stable new test failed the stress run" >&2
    return 1
  fi

  git checkout -q -b flaky "$base"
  cat >> unit/a_test.go <<'EOF'

var runs int

func TestFlaky(t *testing.T) {
	runs++
	if runs == 7 {
		t.Fatal("deliberately flaky")
	}
}
EOF
  git commit -q -am flaky
  if POSSE_STRESS_COUNT=10 "$script" --run unit "$base" >/dev/null 2>&1; then
    echo "self-check failed: a deliberately flaky new test passed the stress run" >&2
    return 1
  fi
  if ! POSSE_STRESS_COUNT=5 "$script" --run unit "$base" >/dev/null 2>&1; then
    echo "self-check failed: the flaky test failed below its failing run" >&2
    return 1
  fi
  if [ "$("$script" --run e2e "$base" 2>&1)" != "No e2e tests changed against $base; nothing to stress." ]; then
    echo "self-check failed: a pull request without an e2e test change ran e2e tests" >&2
    return 1
  fi
  printf 'Stress listing self-check passed.\n'
}

case "${1:-}" in
  --self-test) [ "$#" -eq 1 ] && { self_test; exit; } ;;
  --list) [ "$#" -eq 2 ] && { list_tests "$2"; exit; } ;;
  --run)
    case "${2:-}" in
      unit|e2e) [ "$#" -eq 3 ] && { run_kind "$2" "$3"; exit; } ;;
    esac ;;
esac
echo "usage: $0 --list <base>|--run unit|e2e <base>|--self-test" >&2
exit 2
