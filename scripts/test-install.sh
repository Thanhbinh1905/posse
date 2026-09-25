#!/bin/sh

set -eu

SCRIPT_DIR=$(CDPATH='' cd "$(dirname "$0")/.." && pwd)
INSTALLER=$SCRIPT_DIR/install.sh
TEST_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/posse-installer-test.XXXXXX")
trap 'rm -rf "$TEST_ROOT"' 0

if [ -n "${BASH_VERSION:-}" ]; then
  TEST_SHELL=bash
else
  TEST_SHELL=dash
fi

unset HERDR_ENV POSSE_TTY GH_TOKEN GITHUB_TOKEN

case $(uname -s) in
  Linux) TEST_OS=linux ;;
  Darwin) TEST_OS=darwin ;;
  *) printf 'Unsupported test operating system.\n' >&2; exit 1 ;;
esac
case $(uname -m) in
  x86_64|amd64) TEST_ARCH=amd64 ;;
  aarch64|arm64) TEST_ARCH=arm64 ;;
  *) printf 'Unsupported test architecture.\n' >&2; exit 1 ;;
esac

VERSION=v1.2.3
ASSET=posse_1.2.3_${TEST_OS}_${TEST_ARCH}.tar.gz
RELEASE_ROOT=$TEST_ROOT/releases
VERSION_ROOT=$RELEASE_ROOT/$VERSION
STUB_DIR=$TEST_ROOT/stub
FAKE_BIN=$TEST_ROOT/bin
mkdir -p "$VERSION_ROOT" "$STUB_DIR" "$FAKE_BIN"

cat > "$STUB_DIR/posse" <<'STUB'
#!/bin/sh
case $1 in
  --version) printf 'posse 1.2.3\n' ;;
  --help) printf 'commands:\n  setup\n' ;;
  setup)
    shift
    mode=apply
    human=0
    exit_code=0
    for arg in "$@"; do
      case $arg in
        --check) mode=check ;;
        --uninstall) mode=uninstall ;;
        --human) human=1 ;;
        --exit-code) exit_code=1 ;;
      esac
    done
    if [ "$human" -eq 1 ] && [ "${STUB_OLD_SETUP:-}" = 1 ]; then
      printf 'error{code,message,retryable,help}: "usage","unknown setup flag --human",false,[]\n'
      exit 2
    fi
    case $mode in
      check)
        printf 'setup-check\n' >> "$STUB_LOG"
        if [ "${STUB_SETUP_DONE:-}" = 1 ]; then
          printf '%s\n' '-  posse skill already installed'
          exit 0
        fi
        if [ "${STUB_UNLISTED_PENDING:-}" = 1 ]; then
          printf '%s\n' '-  posse skill already installed'
        else
          printf '%s\n' '+  Link the posse plugin into Herdr' '+  Install the posse skill' '-  Codex SessionStart hook already present'
        fi
        if [ "$exit_code" -eq 1 ]; then
          exit 3
        fi
        ;;
      uninstall)
        printf 'setup integration removed\n'
        printf 'setup-uninstall\n' >> "$STUB_LOG"
        ;;
      *)
        printf 'setup-run\n' >> "$STUB_LOG"
        if [ "${STUB_SETUP_FAIL:-}" = 1 ]; then
          printf '%s\n' '✗  setup would overwrite an unmanaged path' '   /home/user/.agents/skills/posse'
          exit 1
        fi
        printf '%s\n' '✓  Linked the posse plugin into Herdr' '✓  Installed the posse skill' '-  Codex SessionStart hook already present'
        ;;
    esac
    ;;
  *) printf 'unexpected posse command\n' >&2; exit 2 ;;
esac
STUB
chmod 755 "$STUB_DIR/posse"
tar -czf "$VERSION_ROOT/$ASSET" -C "$STUB_DIR" posse
(cd "$VERSION_ROOT" && sha256sum "$ASSET" > checksums.txt)

cat > "$FAKE_BIN/herdr" <<'HERDR'
#!/bin/sh
printf 'herdr 0.9.1\n'
HERDR
chmod 755 "$FAKE_BIN/herdr"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

pass() {
  printf 'PASS: %s\n' "$*"
}

contains() {
  case $1 in
    *"$2"*) return 0 ;;
    *) return 1 ;;
  esac
}

run_install() {
  test_home=$1
  test_prefix=$2
  test_log=$3
  shift 3
  HOME=$test_home \
    POSSE_PREFIX=$test_prefix \
    POSSE_VERSION=$VERSION \
    POSSE_RELEASE_BASE=file://$RELEASE_ROOT \
    STUB_LOG=$test_log \
    STUB_SOURCE=$STUB_DIR/posse \
    NO_COLOR=1 \
    PATH=$FAKE_BIN:$PATH \
    "$TEST_SHELL" "$INSTALLER" "$@"
}

assert_readable() {
  case $1 in
    *'error{'*|*'plan['*|*'{step,'*) fail "$2 printed TOON" ;;
  esac
}

assert_next_step() {
  contains "$1" "Next: open your agent and run the posse-setup skill" || fail "$2 omitted the posse-setup next step"
  contains "$1" "Claude Code: /posse-setup, Codex: \$posse-setup" || fail "$2 omitted how to run the skill"
  contains "$1" "Then: run posse up from a Herdr pane in your repository." || fail "$2 omitted posse up"
  box_widths=$(printf '%s\n' "$1" | awk '/^  [|] |^  [+]-/ { print length($0) }' | sort -u | wc -l)
  [ "$box_widths" -eq 1 ] || fail "$2 closing box lines have uneven widths"
}

mkdir -p "$TEST_ROOT/home-yes"
yes_log=$TEST_ROOT/yes.log
if output=$(run_install "$TEST_ROOT/home-yes" "$TEST_ROOT/prefix-yes" "$yes_log" --yes 2>&1); then
  :
else
  printf '%s\n' "$output" >&2
  fail "--yes installation failed"
fi
[ -x "$TEST_ROOT/prefix-yes/posse" ] || fail "--yes did not install the binary"
contains "$output" "[1/5]" || fail "numbered steps were not shown"
contains "$output" "  +  Link the posse plugin into Herdr" || fail "--yes did not show the setup plan"
contains "$output" "Apply these changes now" && fail "--yes should not prompt"
contains "$(cat "$yes_log")" "setup-run" || fail "--yes did not apply setup outside Herdr"
contains "$output" "  ✓  Installed the posse skill" || fail "--yes did not show the setup result"
contains "$output" "  -  Codex SessionStart hook already present" || fail "--yes did not show kept items"
assert_readable "$output" "--yes"
assert_next_step "$output" "--yes"
pass "$TEST_SHELL applies setup outside Herdr with --yes and a readable checklist"

mkdir -p "$TEST_ROOT/home-interactive-yes"
yes_tty=$TEST_ROOT/answers-yes
printf 'y\n' > "$yes_tty"
interactive_yes_log=$TEST_ROOT/interactive-yes.log
if output=$(POSSE_TTY=$yes_tty run_install "$TEST_ROOT/home-interactive-yes" "$TEST_ROOT/prefix-interactive-yes" "$interactive_yes_log" 2>&1); then
  :
else
  printf '%s\n' "$output" >&2
  fail "interactive yes installation failed"
fi
contains "$output" "Apply these changes now? [Y/n]" || fail "interactive setup prompt is missing"
contains "$(cat "$interactive_yes_log")" "setup-run" || fail "interactive yes did not apply setup"
pass "$TEST_SHELL reads a yes answer from POSSE_TTY"

mkdir -p "$TEST_ROOT/home-interactive-no"
no_tty=$TEST_ROOT/answers-no
printf 'n\n' > "$no_tty"
interactive_no_log=$TEST_ROOT/interactive-no.log
if output=$(POSSE_TTY=$no_tty run_install "$TEST_ROOT/home-interactive-no" "$TEST_ROOT/prefix-interactive-no" "$interactive_no_log" 2>&1); then
  :
else
  printf '%s\n' "$output" >&2
  fail "interactive no installation failed"
fi
contains "$output" "Machine setup was not applied." || fail "interactive no did not skip setup"
case $(cat "$interactive_no_log") in
  *setup-run*) fail "interactive no applied setup" ;;
esac
assert_next_step "$output" "interactive no"
pass "$TEST_SHELL reads a no answer from POSSE_TTY"

mkdir -p "$TEST_ROOT/home-no-tty"
no_tty_log=$TEST_ROOT/no-tty.log
if output=$(run_install "$TEST_ROOT/home-no-tty" "$TEST_ROOT/prefix-no-tty" "$no_tty_log" 2>&1); then
  :
else
  printf '%s\n' "$output" >&2
  fail "no-TTY installation failed"
fi
contains "$output" "No TTY available; using safe default [N]." || fail "no-TTY install did not state the safe default"
contains "$output" "Machine setup was not applied." || fail "no-TTY install did not skip setup"
case $(cat "$no_tty_log") in
  *setup-run*) fail "no-TTY install applied setup without --yes" ;;
esac
assert_readable "$output" "no-TTY install"
assert_next_step "$output" "no-TTY install"
pass "$TEST_SHELL previews setup without a TTY and keeps the safe default"

mkdir -p "$TEST_ROOT/home-no-setup"
no_setup_log=$TEST_ROOT/no-setup.log
: > "$no_setup_log"
if output=$(run_install "$TEST_ROOT/home-no-setup" "$TEST_ROOT/prefix-no-setup" "$no_setup_log" --yes --no-setup 2>&1); then
  :
else
  printf '%s\n' "$output" >&2
  fail "--no-setup installation failed"
fi
contains "$output" "Machine setup skipped by --no-setup." || fail "--no-setup did not report the skip"
[ -s "$no_setup_log" ] && fail "--no-setup ran posse setup"
assert_next_step "$output" "--no-setup"
pass "$TEST_SHELL skips setup with --no-setup"

mkdir -p "$TEST_ROOT/home-done"
done_log=$TEST_ROOT/done.log
if output=$(STUB_SETUP_DONE=1 run_install "$TEST_ROOT/home-done" "$TEST_ROOT/prefix-done" "$done_log" 2>&1); then
  :
else
  printf '%s\n' "$output" >&2
  fail "installation with complete setup failed"
fi
contains "$output" "Machine setup is already complete." || fail "complete setup was not reported"
contains "$output" "No TTY available" && fail "complete setup still asked to apply"
case $(cat "$done_log") in
  *setup-run*) fail "complete setup was applied again" ;;
esac
pass "$TEST_SHELL skips the prompt when setup is already complete"

mkdir -p "$TEST_ROOT/home-unlisted"
unlisted_log=$TEST_ROOT/unlisted.log
if output=$(STUB_UNLISTED_PENDING=1 run_install "$TEST_ROOT/home-unlisted" "$TEST_ROOT/prefix-unlisted" "$unlisted_log" --yes 2>&1); then
  :
else
  printf '%s\n' "$output" >&2
  fail "installation with pending setup failed"
fi
contains "$output" "Machine setup is already complete." && fail "pending setup was read from the checklist text"
contains "$(cat "$unlisted_log")" "setup-run" || fail "pending exit status did not lead to applying setup"
pass "$TEST_SHELL decides from the --exit-code status, not the checklist text"

mkdir -p "$TEST_ROOT/home-old-setup"
old_setup_log=$TEST_ROOT/old-setup.log
: > "$old_setup_log"
if output=$(STUB_OLD_SETUP=1 run_install "$TEST_ROOT/home-old-setup" "$TEST_ROOT/prefix-old-setup" "$old_setup_log" --yes 2>&1); then
  :
else
  printf '%s\n' "$output" >&2
  fail "installation of a build without human setup output failed"
fi
contains "$output" "This posse version has no readable setup output." || fail "old setup output was not replaced"
assert_readable "$output" "old setup build"
case $(cat "$old_setup_log") in
  *setup-run*) fail "old setup build was applied without a preview" ;;
esac
pass "$TEST_SHELL hides TOON from builds without human setup output"

mkdir -p "$TEST_ROOT/home-setup-fail"
if output=$(STUB_SETUP_FAIL=1 run_install "$TEST_ROOT/home-setup-fail" "$TEST_ROOT/prefix-setup-fail" "$TEST_ROOT/setup-fail.log" --yes 2>&1); then
  fail "failed setup unexpectedly succeeded"
fi
contains "$output" "  ✗  setup would overwrite an unmanaged path" || fail "setup failure was not shown"
contains "$output" "Machine setup failed." || fail "setup failure was not summarized"
assert_readable "$output" "failed setup"
pass "$TEST_SHELL reports a readable setup failure"

bad_root=$TEST_ROOT/bad-releases
mkdir -p "$bad_root/$VERSION"
cp "$VERSION_ROOT/$ASSET" "$bad_root/$VERSION/$ASSET"
printf '%064d  %s\n' 0 "$ASSET" > "$bad_root/$VERSION/checksums.txt"
mkdir -p "$TEST_ROOT/home-bad"
if output=$(HOME=$TEST_ROOT/home-bad POSSE_PREFIX=$TEST_ROOT/prefix-bad POSSE_VERSION=$VERSION POSSE_RELEASE_BASE=file://$bad_root NO_COLOR=1 PATH=$FAKE_BIN:$PATH "$TEST_SHELL" "$INSTALLER" --no-setup 2>&1); then
  fail "checksum mismatch unexpectedly succeeded"
fi
contains "$output" "Checksum mismatch" || fail "checksum mismatch was not reported"
[ ! -e "$TEST_ROOT/prefix-bad/posse" ] || fail "checksum mismatch installed a binary"
pass "$TEST_SHELL rejects a checksum mismatch before installation"

FAKE_GO=$FAKE_BIN/go
cat > "$FAKE_GO" <<'FAKE_GO_SCRIPT'
#!/bin/sh
[ "$1" = install ] || exit 2
mkdir -p "$GOBIN"
cp "$STUB_SOURCE" "$GOBIN/posse"
FAKE_GO_SCRIPT
chmod 755 "$FAKE_GO"
EMPTY_RELEASES=$TEST_ROOT/empty-releases
mkdir -p "$EMPTY_RELEASES" "$TEST_ROOT/home-source"
if output=$(HOME=$TEST_ROOT/home-source POSSE_PREFIX=$TEST_ROOT/prefix-source POSSE_VERSION=$VERSION POSSE_RELEASE_BASE=file://$EMPTY_RELEASES STUB_LOG=$TEST_ROOT/source.log STUB_SOURCE=$STUB_DIR/posse NO_COLOR=1 PATH=$FAKE_BIN:$PATH "$TEST_SHELL" "$INSTALLER" --yes --no-setup 2>&1); then
  :
else
  printf '%s\n' "$output" >&2
  fail "source fallback installation failed"
fi
[ -x "$TEST_ROOT/prefix-source/posse" ] || fail "source fallback did not install the binary"
contains "$output" "Build from source with: go install" || fail "source fallback did not offer the Go command"
pass "$TEST_SHELL offers and installs from source when the release asset is absent"

mkdir -p "$TEST_ROOT/home-missing"
if output=$(HOME=$TEST_ROOT/home-missing POSSE_PREFIX=$TEST_ROOT/prefix-missing POSSE_VERSION=$VERSION POSSE_RELEASE_BASE=file://$RELEASE_ROOT NO_COLOR=1 PATH=/usr/bin:/bin "$TEST_SHELL" "$INSTALLER" 2>&1); then
  fail "missing Herdr unexpectedly succeeded"
fi
contains "$output" "herdr.dev/docs/install/" || fail "missing Herdr output omitted the install hint"
pass "$TEST_SHELL stops with the Herdr install hint when Herdr is missing"

OLD_BIN=$TEST_ROOT/old-bin
mkdir -p "$OLD_BIN"
cat > "$OLD_BIN/herdr" <<'OLD_HERDR'
#!/bin/sh
printf 'herdr 0.8.9\n'
OLD_HERDR
chmod 755 "$OLD_BIN/herdr"
mkdir -p "$TEST_ROOT/home-old"
if output=$(HOME=$TEST_ROOT/home-old POSSE_PREFIX=$TEST_ROOT/prefix-old POSSE_VERSION=$VERSION POSSE_RELEASE_BASE=file://$RELEASE_ROOT NO_COLOR=1 PATH=$OLD_BIN:$FAKE_BIN:$PATH "$TEST_SHELL" "$INSTALLER" 2>&1); then
  fail "old Herdr unexpectedly succeeded"
fi
contains "$output" "Herdr 0.9.0 or newer is required" || fail "old Herdr version was not rejected"
pass "$TEST_SHELL rejects Herdr older than 0.9.0"

mkdir -p "$TEST_ROOT/home-no-color"
ESC=$(printf '\033')
if output=$(HOME=$TEST_ROOT/home-no-color POSSE_PREFIX=$TEST_ROOT/prefix-no-color POSSE_VERSION=$VERSION POSSE_RELEASE_BASE=file://$RELEASE_ROOT NO_COLOR=1 PATH=$FAKE_BIN:$PATH "$TEST_SHELL" "$INSTALLER" --no-setup 2>&1); then
  :
else
  printf '%s\n' "$output" >&2
  fail "NO_COLOR installation failed"
fi
case $output in *"$ESC"*) fail "NO_COLOR output contains an escape byte" ;; esac
pass "$TEST_SHELL keeps NO_COLOR output free of escape bytes"

mkdir -p "$TEST_ROOT/home-uninstall" "$TEST_ROOT/prefix-uninstall" "$TEST_ROOT/home-uninstall/.posse"
cp "$STUB_DIR/posse" "$TEST_ROOT/prefix-uninstall/posse"
printf 'keep\n' > "$TEST_ROOT/prefix-uninstall/keep.txt"
printf 'task data\n' > "$TEST_ROOT/home-uninstall/.posse/state"
printf 'home data\n' > "$TEST_ROOT/home-uninstall/keep.txt"
uninstall_log=$TEST_ROOT/uninstall.log
if output=$(HOME=$TEST_ROOT/home-uninstall POSSE_PREFIX=$TEST_ROOT/prefix-uninstall STUB_LOG=$uninstall_log NO_COLOR=1 PATH=$FAKE_BIN:$PATH "$TEST_SHELL" "$INSTALLER" --uninstall 2>&1); then
  :
else
  printf '%s\n' "$output" >&2
  fail "uninstall failed"
fi
[ ! -e "$TEST_ROOT/prefix-uninstall/posse" ] || fail "uninstall kept the binary"
[ -f "$TEST_ROOT/prefix-uninstall/keep.txt" ] || fail "uninstall removed an unrelated prefix file"
[ -f "$TEST_ROOT/home-uninstall/.posse/state" ] || fail "default uninstall removed ~/.posse"
[ -f "$TEST_ROOT/home-uninstall/keep.txt" ] || fail "uninstall removed unrelated home data"
contains "$(cat "$uninstall_log")" "setup-uninstall" || fail "uninstall did not call setup --uninstall"
contains "$output" "using safe default [N]" || fail "uninstall did not state the safe default"
pass "$TEST_SHELL removes only posse and preserves ~/.posse by default"

printf 'Installer scenarios passed under %s.\n' "$TEST_SHELL"
