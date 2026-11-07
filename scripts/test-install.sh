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

unset HERDR_ENV POSSE_TTY GH_TOKEN GITHUB_TOKEN POSSE_HOME XDG_CONFIG_HOME CI

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
log=${STUB_LOG:-/dev/null}
case $1 in
  --version) printf 'posse 1.2.3\n' ;;
  --help) printf 'commands:\n  setup\n' ;;
  _home)
    if [ "${STUB_OLD_HOME:-}" = 1 ]; then
      printf 'error{code,message}: "usage","unknown command \\"_home\\""\n'
      exit 2
    fi
    if [ -n "${XDG_CONFIG_HOME:-}" ]; then pointer=$XDG_CONFIG_HOME/posse/home; else pointer=$HOME/.config/posse/home; fi
    if [ -n "${POSSE_HOME:-}" ]; then path=$POSSE_HOME source=env
    elif [ -f "$pointer" ]; then path=$(cat "$pointer") source=pointer
    else path=$HOME/.posse source=default
    fi
    if [ "$#" -ge 3 ] && [ "$2" = --set ]; then
      printf 'home-set %s\n' "$3" >> "$log"
      if [ "${STUB_HOME_SET_FAIL:-}" = 1 ] && [ ! -e "$log.failed" ]; then
        : > "$log.failed"
        printf 'cannot use "%s" as the posse data folder: it is not an absolute path\n' "$3" >&2
        exit 1
      fi
      case $3 in "~/"*) target=$HOME/${3#"~/"} ;; *) target=$3 ;; esac
      mkdir -p "$target" "${pointer%/*}"
      printf '%s\n' "$target" > "$pointer"
      path=$target source=pointer
    fi
    printf '%s\n%s\n%s\n' "$path" "$source" "$pointer"
    ;;
  setup)
    shift
    mode=apply
    human=0
    exit_code=0
    layout=0
    for arg in "$@"; do
      case $arg in
        --check) mode=check ;;
        --uninstall) mode=uninstall ;;
        --human) human=1 ;;
        --exit-code) exit_code=1 ;;
        --sidebar-layout) layout=1 ;;
      esac
    done
    if [ "$human" -eq 1 ] && [ "${STUB_OLD_SETUP:-}" = 1 ]; then
      printf 'error{code,message,retryable,help}: "usage","unknown setup flag --human",false,[]\n'
      exit 2
    fi
    case $mode in
      check)
        printf 'setup-check home=%s\n' "${POSSE_HOME:-}" >> "$log"
        if [ "${STUB_SETUP_DONE:-}" = 1 ]; then
          printf '%s\n' '-  Herdr plugin and agent integrations: already set' '-  SessionStart hook for Claude Code, Codex: already set'
          exit 0
        fi
        if [ "${STUB_UNLISTED_PENDING:-}" = 1 ]; then
          printf '%s\n' '-  Herdr plugin and agent integrations: already set'
        else
          printf '%s\n' '+  Herdr plugin and agent integrations: will be added' '-  Agents sidebar in Herdr: optional (posse setup --sidebar-layout)' '+  SessionStart hook for Claude Code, Codex: will be added' '     Codex asks once to trust its new hook.' '+  Recommended workflow skills: will be added'
        fi
        if [ "$exit_code" -eq 1 ]; then
          exit 3
        fi
        ;;
      uninstall)
        printf 'setup integration removed\n'
        printf 'setup-uninstall\n' >> "$log"
        ;;
      *)
        printf 'setup-run\n' >> "$log"
        if [ "$layout" -eq 1 ]; then printf 'setup-sidebar\n' >> "$log"; fi
        if [ "${STUB_SETUP_FAIL:-}" = 1 ]; then
          printf '%s\n' '✗  setup would overwrite an unmanaged path' '   /home/user/.claude/settings.json'
          exit 1
        fi
        printf '%s\n' '✓  Herdr plugin and agent integrations' '-  Agents sidebar in Herdr: optional (posse setup --sidebar-layout)' '✓  SessionStart hook for Claude Code, Codex' '     Codex asks once to trust its new hook.' '✓  Recommended workflow skills'
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

assert_box() {
  box_widths=$(printf '%s\n' "$1" | awk '/^  [|] |^  [+]-/ { print length($0) }' | sort -u | wc -l)
  [ "$box_widths" -eq 1 ] || fail "$2 closing box lines have uneven widths"
}

assert_ready() {
  contains "$1" "| posse is ready" || fail "$2 did not report posse as ready"
  contains "$1" "Next: open a new Herdr workspace in your repository" || fail "$2 omitted the new workspace step"
  contains "$1" "run posse up there." || fail "$2 omitted posse up"
  contains "$1" "posse-setup" && fail "$2 still sends the User to the posse-setup skill"
  assert_box "$1" "$2"
}

assert_installed_only() {
  contains "$1" "| posse is installed" || fail "$2 did not report posse as installed"
  contains "$1" "Next: run posse setup to prepare your agents." || fail "$2 omitted posse setup"
  contains "$1" "run posse up there." || fail "$2 omitted posse up"
  assert_box "$1" "$2"
}

run_ok() {
  label=$1
  shift
  if output=$("$@" 2>&1); then
    :
  else
    printf '%s\n' "$output" >&2
    fail "$label failed"
  fi
}

pointer_of() {
  printf '%s/.config/posse/home' "$1"
}

# --yes without a terminal: prepares the agents, keeps the default data folder.
mkdir -p "$TEST_ROOT/home-yes"
yes_log=$TEST_ROOT/yes.log
run_ok "--yes installation" run_install "$TEST_ROOT/home-yes" "$TEST_ROOT/prefix-yes" "$yes_log" --yes
[ -x "$TEST_ROOT/prefix-yes/posse" ] || fail "--yes did not install the binary"
for step in "[1/6] Check your system" "[2/6] Choose a version" "[3/6] Download and verify" "[4/6] Install" "[5/6] Choose where posse keeps its data" "[6/6] Prepare your agents for posse"; do
  contains "$output" "$step" || fail "--yes omitted $step"
done
contains "$output" "will be added" && fail "--yes printed the plan before the result"
contains "$(cat "$yes_log")" "setup-run" || fail "--yes did not apply setup"
contains "$(cat "$yes_log")" "setup-sidebar" && fail "--yes applied the retired sidebar layout"
contains "$(cat "$yes_log")" "home-set" && fail "--yes saved a data folder nobody chose"
contains "$output" "  ✓  SessionStart hook for Claude Code, Codex" || fail "--yes did not show the setup result"
contains "$output" "posse keeps its data in $TEST_ROOT/home-yes/.posse" || fail "--yes did not name the data folder"
contains "$output" "global-skills" && fail "--yes still mentions global skills"
contains "$output" "Apply these changes" && fail "--yes asked a setup question"
[ ! -e "$(pointer_of "$TEST_ROOT/home-yes")" ] || fail "--yes wrote a home pointer for the default folder"
assert_readable "$output" "--yes"
assert_ready "$output" "--yes"
pass "$TEST_SHELL prepares the agents with --yes and keeps the default data folder"

# No terminal and no --yes: the binary is installed, nothing else changes.
mkdir -p "$TEST_ROOT/home-no-tty"
no_tty_log=$TEST_ROOT/no-tty.log
run_ok "no-TTY installation" run_install "$TEST_ROOT/home-no-tty" "$TEST_ROOT/prefix-no-tty" "$no_tty_log"
contains "$output" "No TTY available; keeping the current data folder." || fail "no-TTY install did not explain the data folder"
contains "$output" "No TTY available; nothing was changed." || fail "no-TTY install did not explain the skipped setup"
contains "$output" "  +  SessionStart hook for Claude Code, Codex: will be added" || fail "no-TTY install did not preview the setup"
contains "$(cat "$no_tty_log")" "setup-run" && fail "no-TTY install applied setup without --yes"
contains "$(cat "$no_tty_log")" "home-set" && fail "no-TTY install saved a data folder"
assert_readable "$output" "no-TTY install"
assert_installed_only "$output" "no-TTY install"
pass "$TEST_SHELL changes nothing beyond the binary without a TTY or --yes"

# A terminal and Enter: the default folder stays, setup runs without a question.
mkdir -p "$TEST_ROOT/home-enter"
enter_tty=$TEST_ROOT/answers-enter
printf '\n' > "$enter_tty"
enter_log=$TEST_ROOT/enter.log
run_ok "interactive Enter installation" env POSSE_TTY="$enter_tty" HOME="$TEST_ROOT/home-enter" POSSE_PREFIX="$TEST_ROOT/prefix-enter" POSSE_VERSION=$VERSION POSSE_RELEASE_BASE="file://$RELEASE_ROOT" STUB_LOG="$enter_log" NO_COLOR=1 PATH="$FAKE_BIN:$PATH" "$TEST_SHELL" "$INSTALLER"
contains "$output" "Data folder [~/.posse]: " || fail "the data folder prompt is missing"
contains "$(cat "$enter_log")" "home-set" && fail "Enter saved a data folder"
contains "$(cat "$enter_log")" "setup-run" || fail "interactive install did not prepare the agents"
[ ! -e "$(pointer_of "$TEST_ROOT/home-enter")" ] || fail "Enter wrote a home pointer"
assert_ready "$output" "interactive Enter"
pass "$TEST_SHELL accepts the default data folder on Enter and asks nothing else"

# A terminal and a custom folder with spaces and Unicode.
mkdir -p "$TEST_ROOT/home-custom"
custom_tty=$TEST_ROOT/answers-custom
# shellcheck disable=SC2088 # the User types a literal ~
printf '%s\n' '~/Dữ liệu posse' > "$custom_tty"
custom_log=$TEST_ROOT/custom.log
run_ok "interactive custom installation" env POSSE_TTY="$custom_tty" HOME="$TEST_ROOT/home-custom" POSSE_PREFIX="$TEST_ROOT/prefix-custom" POSSE_VERSION=$VERSION POSSE_RELEASE_BASE="file://$RELEASE_ROOT" STUB_LOG="$custom_log" NO_COLOR=1 PATH="$FAKE_BIN:$PATH" "$TEST_SHELL" "$INSTALLER"
contains "$(cat "$custom_log")" "home-set ~/Dữ liệu posse" || fail "the custom folder was not passed to posse _home --set"
[ "$(cat "$(pointer_of "$TEST_ROOT/home-custom")")" = "$TEST_ROOT/home-custom/Dữ liệu posse" ] || fail "the home pointer does not name the custom folder"
contains "$output" "posse keeps its data in $TEST_ROOT/home-custom/Dữ liệu posse" || fail "custom install did not report the folder"
assert_ready "$output" "interactive custom"
pass "$TEST_SHELL saves a custom data folder with spaces and Unicode"

# A rejected folder is explained and asked again.
mkdir -p "$TEST_ROOT/home-retry"
retry_tty=$TEST_ROOT/answers-retry
printf 'relative\n\n' > "$retry_tty"
retry_log=$TEST_ROOT/retry.log
run_ok "retry installation" env STUB_HOME_SET_FAIL=1 POSSE_TTY="$retry_tty" HOME="$TEST_ROOT/home-retry" POSSE_PREFIX="$TEST_ROOT/prefix-retry" POSSE_VERSION=$VERSION POSSE_RELEASE_BASE="file://$RELEASE_ROOT" STUB_LOG="$retry_log" NO_COLOR=1 PATH="$FAKE_BIN:$PATH" "$TEST_SHELL" "$INSTALLER"
contains "$output" "it is not an absolute path" || fail "the rejected folder was not explained"
[ "$(printf '%s\n' "$output" | grep -c 'Data folder \[')" -eq 2 ] || fail "the installer did not ask again"
[ ! -e "$(pointer_of "$TEST_ROOT/home-retry")" ] || fail "a rejected folder wrote a pointer"
pass "$TEST_SHELL explains a rejected data folder and asks again"

# --home works without a terminal; a reinstall offers the saved folder.
mkdir -p "$TEST_ROOT/home-flag"
flag_log=$TEST_ROOT/flag.log
run_ok "--home installation" run_install "$TEST_ROOT/home-flag" "$TEST_ROOT/prefix-flag" "$flag_log" --yes --home "$TEST_ROOT/data flag"
[ "$(cat "$(pointer_of "$TEST_ROOT/home-flag")")" = "$TEST_ROOT/data flag" ] || fail "--home did not save the folder"
reinstall_tty=$TEST_ROOT/answers-reinstall
printf '\n' > "$reinstall_tty"
run_ok "reinstall" env POSSE_TTY="$reinstall_tty" HOME="$TEST_ROOT/home-flag" POSSE_PREFIX="$TEST_ROOT/prefix-flag" POSSE_VERSION=$VERSION POSSE_RELEASE_BASE="file://$RELEASE_ROOT" STUB_LOG="$flag_log.2" NO_COLOR=1 PATH="$FAKE_BIN:$PATH" "$TEST_SHELL" "$INSTALLER"
contains "$output" "Data folder [$TEST_ROOT/data flag]: " || fail "reinstall did not offer the saved folder"
contains "$(cat "$flag_log.2")" "home-set" && fail "reinstall rewrote the saved folder on Enter"
pass "$TEST_SHELL saves --home and offers the saved folder on reinstall"

# POSSE_HOME wins for the run and is never saved.
mkdir -p "$TEST_ROOT/home-env"
env_log=$TEST_ROOT/env.log
run_ok "POSSE_HOME installation" env POSSE_HOME="$TEST_ROOT/env home" HOME="$TEST_ROOT/home-env" POSSE_PREFIX="$TEST_ROOT/prefix-env" POSSE_VERSION=$VERSION POSSE_RELEASE_BASE="file://$RELEASE_ROOT" STUB_LOG="$env_log" NO_COLOR=1 PATH="$FAKE_BIN:$PATH" "$TEST_SHELL" "$INSTALLER" --yes
contains "$output" "Using POSSE_HOME=$TEST_ROOT/env home for this run; it is not saved." || fail "POSSE_HOME was not reported"
contains "$(cat "$env_log")" "home-set" && fail "POSSE_HOME was saved"
contains "$(cat "$env_log")" "setup-check home=$TEST_ROOT/env home" || fail "setup did not run against POSSE_HOME"
[ ! -e "$(pointer_of "$TEST_ROOT/home-env")" ] || fail "POSSE_HOME wrote the home pointer"
if output=$(env POSSE_HOME="$TEST_ROOT/env home" HOME="$TEST_ROOT/home-env" POSSE_PREFIX="$TEST_ROOT/prefix-env" POSSE_VERSION=$VERSION POSSE_RELEASE_BASE="file://$RELEASE_ROOT" STUB_LOG="$env_log" NO_COLOR=1 PATH="$FAKE_BIN:$PATH" "$TEST_SHELL" "$INSTALLER" --yes --home "$TEST_ROOT/other" 2>&1); then
  fail "--home with POSSE_HOME unexpectedly succeeded"
fi
contains "$output" "--home cannot be combined with POSSE_HOME" || fail "--home with POSSE_HOME was not explained"
pass "$TEST_SHELL uses POSSE_HOME for the run without saving it"

# --no-setup installs only the binary.
mkdir -p "$TEST_ROOT/home-no-setup"
no_setup_log=$TEST_ROOT/no-setup.log
: > "$no_setup_log"
run_ok "--no-setup installation" run_install "$TEST_ROOT/home-no-setup" "$TEST_ROOT/prefix-no-setup" "$no_setup_log" --yes --no-setup
contains "$output" "Machine setup skipped by --no-setup." || fail "--no-setup did not report the skip"
[ -s "$no_setup_log" ] && fail "--no-setup ran posse setup or _home: $(cat "$no_setup_log")"
[ ! -e "$TEST_ROOT/home-no-setup/.config" ] || fail "--no-setup wrote config"
[ ! -e "$TEST_ROOT/home-no-setup/.posse" ] || fail "--no-setup created the data folder"
assert_installed_only "$output" "--no-setup"
if output=$(run_install "$TEST_ROOT/home-no-setup" "$TEST_ROOT/prefix-no-setup" "$no_setup_log" --no-setup --home "$TEST_ROOT/x" 2>&1); then
  fail "--no-setup --home unexpectedly succeeded"
fi
contains "$output" "--home cannot be combined with --no-setup" || fail "--no-setup --home was not explained"
pass "$TEST_SHELL changes only the binary with --no-setup"

# A build without posse _home keeps its legacy data folder.
mkdir -p "$TEST_ROOT/home-old-home"
run_ok "old home build" env STUB_OLD_HOME=1 HOME="$TEST_ROOT/home-old-home" POSSE_PREFIX="$TEST_ROOT/prefix-old-home" POSSE_VERSION=$VERSION POSSE_RELEASE_BASE="file://$RELEASE_ROOT" STUB_LOG="$TEST_ROOT/old-home.log" NO_COLOR=1 PATH="$FAKE_BIN:$PATH" "$TEST_SHELL" "$INSTALLER" --yes
contains "$output" "This posse version keeps its data in POSSE_HOME or ~/.posse." || fail "old home build was not explained"
assert_readable "$output" "old home build"
pass "$TEST_SHELL keeps the legacy data folder for builds without posse _home"

mkdir -p "$TEST_ROOT/home-done"
done_log=$TEST_ROOT/done.log
run_ok "complete setup" env STUB_SETUP_DONE=1 HOME="$TEST_ROOT/home-done" POSSE_PREFIX="$TEST_ROOT/prefix-done" POSSE_VERSION=$VERSION POSSE_RELEASE_BASE="file://$RELEASE_ROOT" STUB_LOG="$done_log" NO_COLOR=1 PATH="$FAKE_BIN:$PATH" "$TEST_SHELL" "$INSTALLER"
contains "$output" "Your agents are already prepared." || fail "complete setup was not reported"
contains "$(cat "$done_log")" "setup-run" && fail "complete setup was applied again"
assert_ready "$output" "complete setup"
pass "$TEST_SHELL reports ready when setup is already complete"

mkdir -p "$TEST_ROOT/home-unlisted"
unlisted_log=$TEST_ROOT/unlisted.log
run_ok "pending setup" env STUB_UNLISTED_PENDING=1 HOME="$TEST_ROOT/home-unlisted" POSSE_PREFIX="$TEST_ROOT/prefix-unlisted" POSSE_VERSION=$VERSION POSSE_RELEASE_BASE="file://$RELEASE_ROOT" STUB_LOG="$unlisted_log" NO_COLOR=1 PATH="$FAKE_BIN:$PATH" "$TEST_SHELL" "$INSTALLER" --yes
contains "$output" "already prepared" && fail "pending setup was read from the checklist text"
contains "$(cat "$unlisted_log")" "setup-run" || fail "pending exit status did not lead to applying setup"
pass "$TEST_SHELL decides from the --exit-code status, not the checklist text"

mkdir -p "$TEST_ROOT/home-old-setup"
old_setup_log=$TEST_ROOT/old-setup.log
: > "$old_setup_log"
run_ok "old setup build" env STUB_OLD_SETUP=1 HOME="$TEST_ROOT/home-old-setup" POSSE_PREFIX="$TEST_ROOT/prefix-old-setup" POSSE_VERSION=$VERSION POSSE_RELEASE_BASE="file://$RELEASE_ROOT" STUB_LOG="$old_setup_log" NO_COLOR=1 PATH="$FAKE_BIN:$PATH" "$TEST_SHELL" "$INSTALLER" --yes
contains "$output" "This posse version has no readable setup output." || fail "old setup output was not replaced"
assert_readable "$output" "old setup build"
contains "$(cat "$old_setup_log")" "setup-run" && fail "old setup build was applied without a preview"
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
run_ok "source fallback installation" env HOME="$TEST_ROOT/home-source" POSSE_PREFIX="$TEST_ROOT/prefix-source" POSSE_VERSION=$VERSION POSSE_RELEASE_BASE="file://$EMPTY_RELEASES" STUB_LOG="$TEST_ROOT/source.log" STUB_SOURCE="$STUB_DIR/posse" NO_COLOR=1 PATH="$FAKE_BIN:$PATH" "$TEST_SHELL" "$INSTALLER" --yes --no-setup
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

ESC=$(printf '\033')
for mode in NO_COLOR=1 TERM=dumb; do
  mkdir -p "$TEST_ROOT/home-plain"
  run_ok "$mode installation" env "$mode" HOME="$TEST_ROOT/home-plain" POSSE_PREFIX="$TEST_ROOT/prefix-plain" POSSE_VERSION=$VERSION POSSE_RELEASE_BASE="file://$RELEASE_ROOT" STUB_LOG="$TEST_ROOT/plain.log" PATH="$FAKE_BIN:$PATH" "$TEST_SHELL" "$INSTALLER" --yes
  case $output in *"$ESC"*) fail "$mode output contains an escape byte" ;; esac
  contains "$output" "One Lead. A whole crew. Running on Herdr." || fail "$mode output lost the tagline"
done
pass "$TEST_SHELL keeps NO_COLOR and TERM=dumb output free of escape bytes"

mkdir -p "$TEST_ROOT/home-uninstall" "$TEST_ROOT/prefix-uninstall" "$TEST_ROOT/home-uninstall/.posse"
cp "$STUB_DIR/posse" "$TEST_ROOT/prefix-uninstall/posse"
printf 'keep\n' > "$TEST_ROOT/prefix-uninstall/keep.txt"
printf 'task data\n' > "$TEST_ROOT/home-uninstall/.posse/state"
printf 'home data\n' > "$TEST_ROOT/home-uninstall/keep.txt"
uninstall_log=$TEST_ROOT/uninstall.log
run_ok "uninstall" env HOME="$TEST_ROOT/home-uninstall" POSSE_PREFIX="$TEST_ROOT/prefix-uninstall" STUB_LOG="$uninstall_log" NO_COLOR=1 PATH="$FAKE_BIN:$PATH" "$TEST_SHELL" "$INSTALLER" --uninstall
[ ! -e "$TEST_ROOT/prefix-uninstall/posse" ] || fail "uninstall kept the binary"
[ -f "$TEST_ROOT/prefix-uninstall/keep.txt" ] || fail "uninstall removed an unrelated prefix file"
[ -f "$TEST_ROOT/home-uninstall/.posse/state" ] || fail "default uninstall removed ~/.posse"
[ -f "$TEST_ROOT/home-uninstall/keep.txt" ] || fail "uninstall removed unrelated home data"
contains "$(cat "$uninstall_log")" "setup-uninstall" || fail "uninstall did not call setup --uninstall"
contains "$output" "using safe default [N]" || fail "uninstall did not state the safe default"
pass "$TEST_SHELL removes only posse and preserves ~/.posse by default"

# Uninstall finds a saved data folder and, when asked, removes it and its pointer.
uhome=$TEST_ROOT/home-uninstall-saved
mkdir -p "$TEST_ROOT/prefix-uninstall-saved" "$uhome/.config/posse" "$uhome/.posse" "$TEST_ROOT/saved data"
cp "$STUB_DIR/posse" "$TEST_ROOT/prefix-uninstall-saved/posse"
printf '%s\n' "$TEST_ROOT/saved data" > "$uhome/.config/posse/home"
printf 'task data\n' > "$TEST_ROOT/saved data/state"
printf 'other home\n' > "$uhome/.posse/state"
yes_answer=$TEST_ROOT/answers-uninstall
printf 'y\n' > "$yes_answer"
run_ok "uninstall with a saved folder" env POSSE_TTY="$yes_answer" HOME="$uhome" POSSE_PREFIX="$TEST_ROOT/prefix-uninstall-saved" STUB_LOG="$TEST_ROOT/uninstall-saved.log" NO_COLOR=1 PATH="$FAKE_BIN:$PATH" "$TEST_SHELL" "$INSTALLER" --uninstall
contains "$output" "Delete $TEST_ROOT/saved data (Task history and config)?" || fail "uninstall did not offer the saved folder"
[ ! -e "$TEST_ROOT/saved data" ] || fail "uninstall kept the confirmed saved folder"
[ ! -e "$uhome/.config/posse/home" ] || fail "uninstall kept the pointer to a removed folder"
[ -f "$uhome/.posse/state" ] || fail "uninstall removed a folder it did not name"
pass "$TEST_SHELL uninstalls the saved data folder only when confirmed"

printf 'Installer scenarios passed under %s.\n' "$TEST_SHELL"
