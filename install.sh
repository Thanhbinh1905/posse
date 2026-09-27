#!/bin/sh

set -u

usage() {
  cat <<'USAGE'
Install posse.

Usage: install.sh [options]

Options:
  --yes                 Apply setup without prompting
  --no-setup            Install the binary without setup
  --version <version>   Install a release such as v1.2.3
  --prefix <directory>  Install into this directory
  --uninstall           Remove posse and its setup integration
  --help                Show this help

Environment:
  POSSE_REPO, POSSE_VERSION, POSSE_PREFIX, POSSE_RELEASE_BASE
  GH_TOKEN or GITHUB_TOKEN for private release downloads
  NO_COLOR to disable terminal colors
USAGE
}

status_line() {
  mark=$1
  shift
  case $mark in
    ok) printf '  %s✓%s  %s\n' "$color_green" "$color_reset" "$*" ;;
    fail) printf '  %s✗%s  %s\n' "$color_red" "$color_reset" "$*" ;;
    skip) printf '  %s-%s  %s\n' "$color_dim" "$color_reset" "$*" ;;
  esac
}

fail() {
  status_line fail "$*"
  exit 1
}

print_banner() {
  printf '\n'
  printf '  %s\n' '####    ###    ####   ####  #####'
  printf '  %s\n' '#   #  #   #  #      #      #'
  printf '  %s\n' '####   #   #   ###    ###   ####'
  printf '  %s\n' '#      #   #      #      #  #'
  printf '  %s\n' '#       ###   ####   ####   #####'
  printf '\n'
  printf '  %sOne Lead. A whole crew. Running on Herdr.%s\n\n' "$color_bold" "$color_reset"
}

open_prompt_input() {
  tty_available=0
  tty_is_real=0
  if [ -n "${POSSE_TTY:-}" ]; then
    if exec 3< "$POSSE_TTY"; then
      tty_available=1
    fi
  elif ( : <> /dev/tty ) 2>/dev/null; then
    exec 3<> /dev/tty
    tty_available=1
    tty_is_real=1
  fi
}

ask_yes_no() {
  question=$1
  default_answer=$2
  allow_auto_yes=$3
  PROMPT_REPLY=

  if [ "$allow_auto_yes" = yes ] && [ "$auto_yes" -eq 1 ]; then
    PROMPT_REPLY=y
    status_line skip "--yes selected; applying the requested change."
    return
  fi

  if [ "$tty_available" -eq 0 ]; then
    PROMPT_REPLY=n
    status_line skip "No TTY available; using safe default [N]."
    return
  fi

  if [ "$default_answer" = y ]; then
    choices=Y/n
  else
    choices=y/N
  fi
  if [ "$tty_is_real" -eq 1 ]; then
    printf '  %s [%s] ' "$question" "$choices" >&3
  else
    printf '  %s [%s] ' "$question" "$choices"
  fi
  if IFS= read -r PROMPT_REPLY <&3; then
    :
  else
    PROMPT_REPLY=
  fi
  case $PROMPT_REPLY in
    '') PROMPT_REPLY=$default_answer ;;
    [Yy]*) PROMPT_REPLY=y ;;
    *) PROMPT_REPLY=n ;;
  esac
}

check_tools() {
  system_name=$(uname -s 2>/dev/null) || fail "Could not determine the operating system."
  machine_name=$(uname -m 2>/dev/null) || fail "Could not determine the CPU architecture."
  case $system_name in
    Linux) target_os=linux ;;
    Darwin) target_os=darwin ;;
    *) fail "Unsupported operating system: $system_name. Supported systems are Linux and macOS." ;;
  esac
  case $machine_name in
    x86_64|amd64) target_arch=amd64 ;;
    aarch64|arm64) target_arch=arm64 ;;
    *) fail "Unsupported architecture: $machine_name. Supported architectures are amd64 and arm64." ;;
  esac
  status_line ok "System: $target_os / $target_arch"

  if command -v curl >/dev/null 2>&1; then
    downloader=curl
  elif command -v wget >/dev/null 2>&1; then
    downloader=wget
  else
    fail "Install curl or wget, then run this installer again."
  fi
  status_line ok "Download tool: $downloader"

  command -v tar >/dev/null 2>&1 || fail "Install tar, then run this installer again."
  status_line ok "Archive tool: tar"

  if command -v sha256sum >/dev/null 2>&1; then
    checksum_tool=sha256sum
  elif command -v shasum >/dev/null 2>&1; then
    checksum_tool=shasum
  else
    fail "Install sha256sum or shasum, then run this installer again."
  fi
  status_line ok "Checksum tool: $checksum_tool"

  command -v git >/dev/null 2>&1 || fail "Install git, then run this installer again."
  status_line ok "Git is installed."

  if command -v gh >/dev/null 2>&1; then
    status_line ok "Optional: gh is installed."
  else
    status_line skip "Optional: gh is not installed."
  fi
  if command -v no-mistakes >/dev/null 2>&1; then
    status_line ok "Optional: no-mistakes is installed."
  else
    status_line skip "Optional: no-mistakes is not installed."
  fi

  if ! command -v herdr >/dev/null 2>&1; then
    status_line fail "Herdr 0.9.0 or newer is required."
    printf '  Install or upgrade Herdr: curl -fsSL https://herdr.dev/install.sh | sh\n'
    printf '  Official instructions: https://herdr.dev/docs/install/\n'
    exit 1
  fi
  herdr_raw=$(herdr --version 2>/dev/null || true)
  herdr_version=$(printf '%s\n' "$herdr_raw" | sed -n 's/^[^0-9]*\([0-9][0-9]*\)\.\([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\1.\2.\3/p' | sed -n '1p')
  if [ -z "$herdr_version" ] || ! printf '%s\n' "$herdr_version" | awk -F. '($1 + 0) > 0 || (($1 + 0) == 0 && ($2 + 0) > 9) || (($1 + 0) == 0 && ($2 + 0) == 9 && ($3 + 0) >= 0) { exit 0 } { exit 1 }'; then
    status_line fail "Herdr 0.9.0 or newer is required. Detected: ${herdr_raw:-unknown version}."
    printf '  Install or upgrade Herdr: curl -fsSL https://herdr.dev/install.sh | sh\n'
    printf '  Official instructions: https://herdr.dev/docs/install/\n'
    exit 1
  fi
  status_line ok "Herdr $herdr_version"
}

choose_auth() {
  auth_token=
  if [ -n "${GH_TOKEN:-}" ]; then
    auth_token=$GH_TOKEN
  elif [ -n "${GITHUB_TOKEN:-}" ]; then
    auth_token=$GITHUB_TOKEN
  elif command -v gh >/dev/null 2>&1; then
    auth_token=$(gh auth token 2>/dev/null || true)
  fi
  if [ -n "$auth_token" ]; then
    auth_rc=$temp_dir/wgetrc
    umask 077
    printf 'header = Authorization: Bearer %s\n' "$auth_token" > "$auth_rc"
    auth_enabled=1
  else
    auth_enabled=0
  fi
}

api_download() {
  api_url=$1
  output_file=$2
  DOWNLOAD_STATUS=000
  if [ "$downloader" = curl ]; then
    if [ "$auth_enabled" -eq 1 ]; then
      DOWNLOAD_STATUS=$(curl -sS --output "$output_file" --write-out '%{http_code}' --config - "$api_url" <<EOF
header = "Authorization: Bearer $auth_token"
header = "Accept: application/vnd.github+json"
EOF
)
    else
      DOWNLOAD_STATUS=$(curl -sS --output "$output_file" --write-out '%{http_code}' "$api_url") || return 1
    fi
    case $DOWNLOAD_STATUS in
      2??) return 0 ;;
      *) return 1 ;;
    esac
  fi
  if [ "$auth_enabled" -eq 1 ]; then
    if WGETRC=$auth_rc wget -q -O "$output_file" "$api_url"; then
      DOWNLOAD_STATUS=200
      return 0
    fi
    DOWNLOAD_STATUS=000
    return 1
  fi
  if wget -q -O "$output_file" "$api_url"; then
    DOWNLOAD_STATUS=200
    return 0
  fi
  DOWNLOAD_STATUS=000
  return 1
}

plain_download() {
  download_url=$1
  output_file=$2
  DOWNLOAD_STATUS=000
  case $download_url in
    file://*) is_file_url=1 ;;
    *) is_file_url=0 ;;
  esac
  if [ "$downloader" = curl ]; then
    DOWNLOAD_STATUS=$(curl -sS -L --output "$output_file" --write-out '%{http_code}' "$download_url") || return 1
    case $DOWNLOAD_STATUS in
      2??) return 0 ;;
      000)
        if [ "$is_file_url" -eq 1 ]; then
          return 0
        fi
        return 1
        ;;
      *) return 1 ;;
    esac
  fi
  if wget -q -O "$output_file" "$download_url"; then
    DOWNLOAD_STATUS=200
    return 0
  fi
  DOWNLOAD_STATUS=000
  return 1
}

api_asset_download() {
  api_url=$1
  output_file=$2
  headers_file=$temp_dir/asset-headers
  DOWNLOAD_STATUS=000
  if [ "$downloader" = curl ]; then
    DOWNLOAD_STATUS=$(curl -sS --dump-header "$headers_file" --output "$output_file" --write-out '%{http_code}' --config - "$api_url" <<EOF
header = "Authorization: Bearer $auth_token"
header = "Accept: application/octet-stream"
EOF
)
    case $DOWNLOAD_STATUS in
      2??) return 0 ;;
      3??)
        redirect_url=$(awk 'tolower(substr($0, 1, 9)) == "location:" { sub(/^[^:]*:[[:space:]]*/, ""); sub(/\r$/, ""); print; exit }' "$headers_file")
        case $redirect_url in
          https://release-assets.githubusercontent.com/*|https://objects.githubusercontent.com/*) ;;
          *) return 1 ;;
        esac
        plain_download "$redirect_url" "$output_file"
        return $?
        ;;
      *) return 1 ;;
    esac
  fi

  if WGETRC=$auth_rc wget --server-response --max-redirect=0 -O "$output_file" "$api_url" 2> "$headers_file"; then
    DOWNLOAD_STATUS=200
    return 0
  fi
  redirect_url=$(awk 'tolower($1) == "location:" { sub(/^[^:]*:[[:space:]]*/, ""); sub(/\r$/, ""); print; exit }' "$headers_file")
  case $redirect_url in
    https://release-assets.githubusercontent.com/*|https://objects.githubusercontent.com/*)
      plain_download "$redirect_url" "$output_file"
      return $?
      ;;
  esac
  DOWNLOAD_STATUS=000
  return 1
}

asset_id_for_name() {
  json_file=$1
  asset_wanted=$2
  awk -v wanted="$asset_wanted" '
    /"assets"[[:space:]]*:/ { in_assets = 1 }
    in_assets && /^[[:space:]]*\][,]?[[:space:]]*$/ { exit }
    in_assets && /^[[:space:]]*\{/ { current = 1; asset_id = ""; asset_name = "" }
    in_assets && current && /"id"[[:space:]]*:/ {
      line = $0
      sub(/^.*"id"[[:space:]]*:[[:space:]]*/, "", line)
      sub(/[^0-9].*$/, "", line)
      asset_id = line
    }
    in_assets && current && /"name"[[:space:]]*:/ {
      line = $0
      sub(/^.*"name"[[:space:]]*:[[:space:]]*"/, "", line)
      sub(/".*$/, "", line)
      asset_name = line
      if (asset_name == wanted) {
        print asset_id
        exit
      }
      current = 0
    }
  ' "$json_file"
}

release_asset_download() {
  asset_name=$1
  output_file=$2
  if [ "$custom_release_base" -eq 1 ]; then
    plain_download "$release_base/$version/$asset_name" "$output_file" 2>/dev/null && return 0
    if [ "$DOWNLOAD_STATUS" = 404 ] || [ "$DOWNLOAD_STATUS" = 000 ]; then
      plain_download "$release_base/$asset_name" "$output_file" 2>/dev/null && return 0
    fi
    return 1
  fi
  if [ "$auth_enabled" -eq 1 ]; then
    asset_id=$(asset_id_for_name "$release_json" "$asset_name")
    if [ -z "$asset_id" ]; then
      DOWNLOAD_STATUS=404
      return 1
    fi
    api_asset_download "https://api.github.com/repos/$POSSE_REPO/releases/assets/$asset_id" "$output_file"
  else
    plain_download "$release_base/$version/$asset_name" "$output_file"
  fi
}

checksum_value() {
  awk -v wanted="$1" '$2 == wanted || $2 == "*" wanted { print $1; found = 1; exit } END { if (!found) exit 1 }' "$2"
}

digest_file() {
  if [ "$checksum_tool" = sha256sum ]; then
    sha256sum "$1" | awk '{ print $1 }'
  else
    shasum -a 256 "$1" | awk '{ print $1 }'
  fi
}

build_from_source() {
  if ! command -v go >/dev/null 2>&1; then
    fail "No release archive is available for $target_os/$target_arch, and Go is not installed."
  fi
  printf '\n  No release archive is available for %s/%s.\n' "$target_os" "$target_arch"
  printf '  Build from source with: go install github.com/thanhbinh1905/posse/cmd/posse@%s\n' "$version"
  ask_yes_no "Build from source and install now?" n yes
  if [ "$PROMPT_REPLY" != y ]; then
    fail "Source installation was skipped."
  fi
  mkdir -p "$temp_dir/gobin" || fail "Could not create a temporary Go install directory."
  if ! GOBIN="$temp_dir/gobin" go install "github.com/thanhbinh1905/posse/cmd/posse@$version"; then
    fail "The source build failed."
  fi
  [ -x "$temp_dir/gobin/posse" ] || fail "The source build did not produce a posse binary."
  install_binary=$temp_dir/gobin/posse
}

atomic_install() {
  source_binary=$1
  install_temp=$prefix/.posse.tmp.$$
  if [ -e "$install_temp" ]; then
    fail "Temporary install path already exists: $install_temp"
  fi
  if ! cp "$source_binary" "$install_temp"; then
    rm -f "$install_temp"
    fail "Could not copy posse into $prefix."
  fi
  if ! chmod 755 "$install_temp"; then
    rm -f "$install_temp"
    fail "Could not mark the posse binary executable."
  fi
  if ! mv -f "$install_temp" "$prefix/posse"; then
    rm -f "$install_temp"
    fail "Could not atomically install posse into $prefix."
  fi
}

path_setup_hint() {
  case :$PATH: in
    *:"$prefix":*) return ;;
  esac
  shell_path=${SHELL:-}
  shell_name=${shell_path##*/}
  case $shell_name in
    fish)
      printf '  Add this to fish: fish_add_path "%s"\n' "$prefix"
      ;;
    bash|zsh)
      printf "  Add this to your shell config: export PATH=\"%s:\$PATH\"\n" "$prefix"
      ;;
    *)
      printf '  Add %s to your PATH.\n' "$prefix"
      ;;
  esac
}

# print_setup_lines indents posse setup --human output under the current step
# and colors its marks. Older builds print TOON instead, which is not shown.
print_setup_lines() {
  case $1 in
    error\{*|*"
error{"*)
      status_line skip "This posse version has no readable setup output."
      return
      ;;
  esac
  printf '%s\n' "$1" | sed \
    -e 's/^/  /' \
    -e "s/^  ✓/  $color_green✓$color_reset/" \
    -e "s/^  ✗/  $color_red✗$color_reset/" \
    -e "s/^  -  /  $color_dim-$color_reset  /"
}

setup_after_install() {
  binary=$prefix/posse
  if [ "$no_setup" -eq 1 ]; then
    status_line skip "Machine setup skipped by --no-setup."
    return
  fi
  help_output=$("$binary" --help 2>&1 || true)
  case $help_output in
    *setup*) ;;
    *)
      status_line skip "This posse build has no setup command."
      return
      ;;
  esac
  # --exit-code exits 0 when setup is complete and 3 when changes are pending.
  setup_status=0
  setup_plan=$("$binary" setup --check --exit-code --human 2>&1) || setup_status=$?
  print_setup_lines "$setup_plan"
  case $setup_status in
    0) status_line ok "Machine setup is already complete." ;;
    3)
      ask_yes_no "Apply these changes now?" y yes
      if [ "$PROMPT_REPLY" = y ]; then
        if setup_result=$("$binary" setup --human 2>&1); then
          printf '\n'
          print_setup_lines "$setup_result"
        else
          print_setup_lines "$setup_result"
          fail "Machine setup failed. Fix the issue above, then run the installer again."
        fi
      else
        status_line skip "Machine setup was not applied."
      fi
      ;;
    *)
      status_line skip "Machine setup was not applied."
      return
      ;;
  esac
}

print_box() {
  printf '\n  +%s+\n' '------------------------------------------------------------'
  for box_line in "$@"; do
    printf '  | %-58s |\n' "$box_line"
  done
  printf '  +%s+\n' '------------------------------------------------------------'
}

uninstall_posse() {
  binary=$prefix/posse
  if [ -x "$binary" ]; then
    help_output=$("$binary" --help 2>&1 || true)
    case $help_output in
      *setup*)
        if "$binary" setup --uninstall >/dev/null 2>&1; then
          status_line ok "posse setup integration removed."
        else
          status_line skip "Could not remove the setup integration. Run posse setup --uninstall to see why."
        fi
        ;;
      *) status_line skip "No posse setup command is available." ;;
    esac
    if rm -f "$binary"; then
      status_line ok "Removed $binary"
    else
      fail "Could not remove $binary."
    fi
  else
    status_line skip "No binary found at $binary."
  fi
  ask_yes_no "Delete $HOME/.posse (Task history and config)?" n no
  if [ "$PROMPT_REPLY" = y ]; then
    if rm -rf "$HOME/.posse"; then
      status_line ok "Removed $HOME/.posse"
    else
      fail "Could not remove $HOME/.posse."
    fi
  else
    status_line skip "Kept $HOME/.posse (Task history and config)."
  fi
  print_box 'posse has been uninstalled' 'Next: install again with install.sh'
}

main() {
  color_green=
  color_red=
  color_dim=
  color_bold=
  color_reset=
  auto_yes=0
  no_setup=0
  uninstall=0
  requested_version=
  prefix=${POSSE_PREFIX:-}
  custom_release_base=0
  if [ -n "${POSSE_RELEASE_BASE:-}" ]; then
    custom_release_base=1
    release_base=${POSSE_RELEASE_BASE%/}
  else
    release_base=
  fi

  while [ "$#" -gt 0 ]; do
    case $1 in
      --yes) auto_yes=1; shift ;;
      --no-setup) no_setup=1; shift ;;
      --uninstall) uninstall=1; shift ;;
      --version)
        [ "$#" -ge 2 ] || fail "--version requires a value."
        requested_version=$2
        shift 2
        ;;
      --prefix)
        [ "$#" -ge 2 ] || fail "--prefix requires a directory."
        prefix=$2
        shift 2
        ;;
      --help|-h) usage; return 0 ;;
      *) fail "Unknown option: $1. Run install.sh --help for usage." ;;
    esac
  done

  [ -n "${HOME:-}" ] || fail "HOME is not set."
  [ -n "$prefix" ] || prefix=$HOME/.local/bin
  case $prefix in
    /*) ;;
    *) prefix=$(pwd)/$prefix ;;
  esac

  color_green=
  color_red=
  color_dim=
  color_bold=
  color_reset=
  if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
    color_green=$(printf '\033[32m')
    color_red=$(printf '\033[31m')
    color_dim=$(printf '\033[2m')
    color_bold=$(printf '\033[1m')
    color_reset=$(printf '\033[0m')
  fi
  print_banner
  open_prompt_input

  if [ "$uninstall" -eq 1 ]; then
    uninstall_posse
    return 0
  fi

  printf '[1/5] Check your system\n'
  check_tools

  printf '\n[2/5] Choose a version\n'
  POSSE_REPO=${POSSE_REPO:-Thanhbinh1905/posse}
  version=${requested_version:-${POSSE_VERSION:-}}
  temp_dir=${TMPDIR:-/tmp}/posse-install.$$
  if [ -e "$temp_dir" ]; then
    fail "Temporary path already exists: $temp_dir"
  fi
  (umask 077 && mkdir "$temp_dir") || fail "Could not create a temporary work directory."
  trap 'rm -rf "$temp_dir"' 0
  choose_auth
  if [ -z "$version" ]; then
    if [ "$custom_release_base" -eq 1 ]; then
      fail "Set POSSE_VERSION or pass --version when using POSSE_RELEASE_BASE."
    fi
    release_json=$temp_dir/release.json
    if [ "$auth_enabled" -eq 1 ]; then
      status_line ok "Checking the latest release through the authenticated GitHub API."
    else
      status_line ok "Checking the latest public release."
    fi
    api_url=https://api.github.com/repos/$POSSE_REPO/releases/latest
    api_download "$api_url" "$release_json" || fail "Could not read the latest release. For a private repository, set GH_TOKEN or GITHUB_TOKEN, or authenticate gh."
    version=$(sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' "$release_json" | sed -n '1p')
    [ -n "$version" ] || fail "The latest release response did not include a tag."
  else
    status_line ok "Using version $version"
    if [ "$custom_release_base" -eq 0 ]; then
      release_json=$temp_dir/release.json
      api_url=https://api.github.com/repos/$POSSE_REPO/releases/tags/$version
      if [ -n "$auth_token" ]; then
        auth_enabled=1
      else
        auth_enabled=0
      fi
      if api_download "$api_url" "$release_json"; then
        :
      else
        rm -f "$release_json"
      fi
    fi
  fi
  case $version in
    v[0-9]* ) ;;
    *) fail "Version must start with v and identify a release, for example v1.2.3." ;;
  esac
  if [ "$custom_release_base" -eq 0 ]; then
    release_base=https://github.com/$POSSE_REPO/releases/download
  fi
  status_line ok "Selected $version"

  printf '\n[3/5] Download and verify\n'
  if [ "$auth_enabled" -eq 1 ] && [ "$custom_release_base" -eq 0 ]; then
    status_line ok "Release downloads use the authenticated GitHub API."
  elif [ "$custom_release_base" -eq 1 ]; then
    status_line skip "Using the configured release base."
  fi
  asset_version=${version#v}
  archive_name=posse_${asset_version}_${target_os}_${target_arch}.tar.gz
  archive_file=$temp_dir/$archive_name
  checksum_file=$temp_dir/checksums.txt
  if [ "$custom_release_base" -eq 0 ] && [ "$auth_enabled" -eq 1 ] && { [ -z "${release_json:-}" ] || [ ! -s "$release_json" ]; }; then
    release_json=$temp_dir/release.json
    api_url=https://api.github.com/repos/$POSSE_REPO/releases/tags/$version
    api_download "$api_url" "$release_json" || fail "Could not read release $version through the GitHub API."
  fi
  if ! release_asset_download "$archive_name" "$archive_file"; then
    if [ "$DOWNLOAD_STATUS" = 404 ] || [ "$DOWNLOAD_STATUS" = 000 ]; then
      build_from_source
    else
      fail "Could not download $archive_name (HTTP $DOWNLOAD_STATUS)."
    fi
  else
    if ! release_asset_download checksums.txt "$checksum_file"; then
      fail "Could not download checksums.txt (HTTP $DOWNLOAD_STATUS)."
    fi
    expected_checksum=$(checksum_value "$archive_name" "$checksum_file") || fail "checksums.txt has no entry for $archive_name."
    case $expected_checksum in
      *[!0123456789abcdefABCDEF]*|'') fail "The archive checksum is not a SHA-256 value." ;;
    esac
    [ "${#expected_checksum}" -eq 64 ] || fail "The archive checksum is not a SHA-256 value."
    actual_checksum=$(digest_file "$archive_file") || fail "Could not calculate the archive checksum."
    if [ "$actual_checksum" != "$expected_checksum" ]; then
      fail "Checksum mismatch for $archive_name. Nothing was installed."
    fi
    status_line ok "SHA-256 verified for $archive_name"
    extract_dir=$temp_dir/extracted
    mkdir "$extract_dir" || fail "Could not create a temporary extraction directory."
    if ! tar -xzf "$archive_file" -C "$extract_dir"; then
      fail "Could not unpack $archive_name."
    fi
    [ -f "$extract_dir/posse" ] || fail "The archive does not contain a posse binary."
    chmod 755 "$extract_dir/posse" || fail "Could not prepare the posse binary."
    install_binary=$extract_dir/posse
  fi

  printf '\n[4/5] Install\n'
  mkdir -p "$prefix" || fail "Could not create install prefix $prefix."
  prefix=$(cd "$prefix" && pwd) || fail "Could not resolve install prefix."
  atomic_install "$install_binary"
  status_line ok "Installed $prefix/posse"
  path_setup_hint

  printf '\n[5/5] Set up this machine\n'
  setup_after_install

  print_box 'posse is installed' '' \
    'Next: open your agent and run the posse-setup skill' \
    "      (Claude Code: /posse-setup, Codex: \$posse-setup)" \
    '      to finish configuration and register your Projects.' \
    'Then: run posse up from a Herdr pane in your repository.'
}

main "$@"
