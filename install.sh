#!/bin/sh

set -u

usage() {
  cat <<'USAGE'
Install posse.

Usage: install.sh [options]

Options:
  --yes                 Prepare your agents without a terminal to confirm
  --no-setup            Install the binary only; change nothing else
  --home <directory>    Keep posse's data in this folder
  --version <version>   Install a release such as v1.2.3
  --prefix <directory>  Install into this directory
  --uninstall           Remove posse and its setup integration
  --help                Show this help

Environment:
  POSSE_HOME            Data folder for this run; it overrides the saved one
  POSSE_REPO, POSSE_VERSION, POSSE_PREFIX, POSSE_RELEASE_BASE
  GH_TOKEN or GITHUB_TOKEN for private release downloads
  NO_COLOR to disable colors and animation
USAGE
}

# ---------------------------------------------------------------------------
# Terminal UI. Live mode redraws a stage list in place while work runs; plain
# mode prints one line per event. Only live mode moves the cursor.

stage_title() {
  case $1 in
    1) title='Check your system' ;;
    2) title='Choose a version' ;;
    3) title='Download and verify' ;;
    4) title='Install' ;;
    5) title='Choose where posse keeps its data' ;;
    *) title='Prepare your agents for posse' ;;
  esac
}

# vlen prints the display width of $1. Under a C locale wc counts bytes,
# which only overestimates and never makes a line wrap.
vlen() {
  printf '%s' "$1" | wc -m | tr -d ' '
}

# fit prints $1 when it is at most $2 columns wide. A longer ASCII text is cut
# with an ellipsis; a longer non-ASCII text is dropped rather than cut inside
# a character.
fit() {
  fit_len=$(vlen "$1")
  if [ "$fit_len" -le "$2" ]; then
    printf '%s' "$1"
    return
  fi
  case $1 in
    *[!\ -~]*) return ;;
  esac
  [ "$2" -gt 1 ] || return
  printf '%s…' "$(printf '%s' "$1" | cut -c "1-$(($2 - 1))")"
}

# pad sets $pad to $1 spaces without starting a process.
pad() {
  pad=
  k=0
  while [ "$k" -lt "$1" ]; do
    pad="$pad "
    k=$((k + 1))
  done
}

display_path() {
  case $1 in
    "$HOME") printf '~' ;;
    "$HOME"/*) printf '~%s' "${1#"$HOME"}" ;;
    *) printf '%s' "$1" ;;
  esac
}

ui_init() {
  ui_live=0
  ui_color=0
  ui_truecolor=0
  ui_cols=80
  ui_ticker=
  ui_cursor_up=0
  ui_frame_lines=0
  ui_spin=0
  ui_op=
  ui_op_input=0
  ui_err=
  ui_input_row=0
  ui_final=0
  ui_hidden_cursor=0
  current_stage=0
  sub_count=0
  setup_notes=
  crew_required=
  crew_agents=
  crew_addons=
  version_label=
  i=1
  while [ "$i" -le 6 ]; do
    eval "st_$i=pending sd_$i= sdl_$i=0"
    i=$((i + 1))
  done

  if [ -t 1 ] && [ -z "${NO_COLOR:-}" ] && [ "${TERM:-}" != dumb ]; then
    ui_color=1
  fi
  if [ "$ui_color" -eq 1 ]; then
    size=$(stty size 2>/dev/null < /dev/tty) || size=
    cols=${size#* }
    case $cols in
      ''|*[!0-9]*) cols=$(tput cols 2>/dev/null) || cols= ;;
    esac
    case $cols in
      ''|*[!0-9]*) cols=${COLUMNS:-80} ;;
    esac
    case $cols in
      ''|*[!0-9]*) cols=80 ;;
    esac
    ui_cols=$cols
    if [ -z "${CI:-}" ] && [ "$ui_cols" -ge 60 ]; then
      ui_live=1
    fi
    case ${COLORTERM:-} in
      truecolor|24bit) ui_truecolor=1 ;;
    esac
  fi
  # Content width, one column short of the terminal so a line never wraps.
  ui_w=$((ui_cols - 1))
  [ "$ui_w" -le 74 ] || ui_w=74

  esc=$(printf '\033')
  c_reset='' c_bold='' c_faint='' c_text='' c_ok='' c_warn='' c_err='' c_lead=''
  c_claude='' c_codex='' c_pi='' c_opencode=''
  if [ "$ui_color" -eq 1 ]; then
    c_reset="${esc}[0m" c_bold="${esc}[1m"
    if [ "$ui_truecolor" -eq 1 ]; then
      c_text="${esc}[39m" c_faint="${esc}[38;2;110;119;140m" c_ok="${esc}[38;2;120;228;211m"
      c_warn="${esc}[38;2;255;200;87m" c_err="${esc}[38;2;255;93;115m"
      c_lead="${esc}[38;2;120;228;211m"
      c_claude="${esc}[38;2;255;158;100m" c_codex="${esc}[38;2;158;206;106m"
      c_pi="${esc}[38;2;177;138;255m" c_opencode="${esc}[38;2;120;228;211m"
    else
      c_text="${esc}[39m" c_faint="${esc}[90m" c_ok="${esc}[36m" c_warn="${esc}[33m" c_err="${esc}[31m"
      c_lead="${esc}[96m"
      c_claude="${esc}[33m" c_codex="${esc}[32m" c_pi="${esc}[35m" c_opencode="${esc}[36m"
    fi
  fi
  tick=1
  if [ "$ui_live" -eq 1 ] && sleep 0.1 2>/dev/null; then
    tick=0.1
  fi
}

# The C1 "Open orbit" mark: the Lead in the centre, three Workers on an
# orbit open at the bottom. L = Lead, C = Worker, a = orbit.
logo_pixels='......CCC......
....aaCCCaa....
...aa.CCC.aa...
..aa.......aa..
..a...LLL...a..
.aa..LLLLL..aa.
.aa..LLLLL..aa.
.aa..LLLLL..aa.
.CCC..LLL..CCC.
.CCC.......CCC.
.CCC.......CCC.
...............'

# logo_rows prints the mark as half-block rows: each character cell shows two
# pixels, the top one in the foreground and the bottom one in the background.
logo_rows() {
  printf '%s\n' "$logo_pixels" | awk -v e="$esc" -v t="$ui_truecolor" '
    function fg(p) { if (t) return (p == "L" ? "38;2;120;228;211" : p == "C" ? "38;2;148;170;251" : "38;2;89;105;160"); return (p == "L" ? "96" : p == "C" ? "94" : "34") }
    function bg(p) { if (t) return (p == "L" ? "48;2;120;228;211" : p == "C" ? "48;2;148;170;251" : "48;2;89;105;160"); return (p == "L" ? "106" : p == "C" ? "104" : "44") }
    NR % 2 == 1 { top = $0; next }
    {
      line = ""
      for (i = 1; i <= length(top); i++) {
        a = substr(top, i, 1); b = substr($0, i, 1)
        if (a == "." && b == ".") line = line e "[0m "
        else if (b == ".") line = line e "[0m" e "[" fg(a) "m▀"
        else if (a == ".") line = line e "[0m" e "[" fg(b) "m▄"
        else if (a == b) line = line e "[0m" e "[" fg(a) "m█"
        else line = line e "[0m" e "[" fg(a) ";" bg(b) "m▀"
      }
      print line e "[0m"
    }'
}

wordmark() {
  if [ "$ui_truecolor" -eq 1 ]; then
    printf '%s' "$c_bold"
    printf '%s' "${esc}[38;2;120;228;211mp${esc}[38;2;125;214;223mo${esc}[38;2;134;199;233ms${esc}[38;2;141;185;242ms${esc}[38;2;148;170;251me"
    printf '%s' "$c_reset"
  else
    printf '%s%sposse%s' "$c_bold" "$c_lead" "$c_reset"
  fi
}

ui_header() {
  tagline='One Lead. A whole crew. Running on Herdr.'
  if [ "$ui_color" -eq 1 ] && [ "$ui_cols" -ge 64 ]; then
    printf '\n'
    row=0
    logo_rows | while IFS= read -r logo_line; do
      row=$((row + 1))
      case $row in
        3) printf '  %s    %s\n' "$logo_line" "$(wordmark)" ;;
        4) printf '  %s    %s%s%s\n' "$logo_line" "$c_faint" "$tagline" "$c_reset" ;;
        *) printf '  %s\n' "$logo_line" ;;
      esac
    done
    printf '\n'
    return
  fi
  if [ "$ui_color" -eq 1 ]; then
    printf '\n  %s●%s %s  %s%s%s\n\n' "$c_lead" "$c_reset" "$(wordmark)" "$c_faint" "$tagline" "$c_reset"
    return
  fi
  printf '\n'
  printf '%s\n' "   .--o--." "  /       \\     posse" " |    @    |    $tagline" "  o       o"
}

# mark_for sets $mark to the colored mark of a stage state.
mark_for() {
  case $1 in
    done) mark="$c_ok$c_bold✓$c_reset" ;;
    active)
      case $((ui_spin % 10)) in
        0) mark='⠋' ;; 1) mark='⠙' ;; 2) mark='⠹' ;; 3) mark='⠸' ;; 4) mark='⠼' ;;
        5) mark='⠴' ;; 6) mark='⠦' ;; 7) mark='⠧' ;; 8) mark='⠇' ;; *) mark='⠏' ;;
      esac
      mark="$c_lead$mark$c_reset"
      ;;
    fail) mark="$c_err$c_bold✗$c_reset" ;;
    warn) mark="$c_warn$c_bold!$c_reset" ;;
    skip) mark="$c_faint–$c_reset" ;;
    *) mark="$c_faint○$c_reset" ;;
  esac
}

# fl appends one line to the frame being composed.
fl() {
  ui_frame="$ui_frame$1${esc}[K
"
  ui_lines=$((ui_lines + 1))
}

# set_head refreshes the progress box title after the version or prefix
# changes. It runs outside the animation, so it may start processes.
set_head() {
  head_text="Installing posse${version_label:+ $version_label}"
  head_plain="$head_text → $(display_path "$prefix")"
  if [ "$(vlen "$head_plain")" -le $((ui_w - 17)) ]; then
    head_text=$head_plain
  fi
  head_len=$(vlen "$head_text")
  ui_rule_box=$(rule $((ui_w - 4)) ─)
  ui_rule_crew=$(rule $((ui_w - 34)) ─)
}

# set_detail stores a stage detail already fitted to its row.
set_detail() {
  stage_title "$1"
  fitted=$(fit "$2" $((ui_w - 4 - ${#title} - 2)))
  eval "sd_$1=\$fitted sdl_$1=$(vlen "$fitted")"
}

ui_compose() {
  ui_frame=
  ui_lines=0
  done_count=0
  state=pending detail='' detail_len=0 sub_state='' sub_text=''
  i=1
  while [ "$i" -le 6 ]; do
    eval "state=\$st_$i"
    case $state in done|skip|warn) done_count=$((done_count + 1)) ;; esac
    i=$((i + 1))
  done
  fl ''
  if [ "$ui_final" -eq 0 ]; then
    bar=
    j=1
    while [ "$j" -le 6 ]; do
      if [ "$j" -le "$done_count" ]; then bar="$bar$c_lead▰"; else bar="$bar$c_faint▱"; fi
      j=$((j + 1))
    done
    pad $((ui_w - 16 - head_len))
    fl "  $c_faint╭$ui_rule_box╮$c_reset"
    fl "  $c_faint│$c_reset $c_bold$head_text$c_reset$pad$bar$c_reset $c_faint$done_count/6$c_reset $c_faint│$c_reset"
    fl "  $c_faint╰$ui_rule_box╯$c_reset"
  fi
  ui_input_row=0
  i=1
  while [ "$i" -le 6 ]; do
    eval "state=\$st_$i detail=\$sd_$i detail_len=\${sdl_$i:-0}"
    stage_title "$i"
    title_color=$c_text
    case $state in pending|skip) title_color=$c_faint ;; esac
    mark_for "$state"
    pad $((ui_w - 4 - ${#title} - detail_len))
    fl "  $mark $title_color$title$c_reset$pad$c_faint$detail$c_reset"
    if [ "$i" -eq 6 ] && [ "$sub_count" -gt 0 ]; then
      j=1
      while [ "$j" -le "$sub_count" ]; do
        eval "sub_state=\$sub_state_$j sub_text=\$sub_text_$j"
        if [ "$sub_state" = note ]; then
          fl "        $c_faint$sub_text$c_reset"
          j=$((j + 1))
          continue
        fi
        text_color=$c_text
        case $sub_state in pending|kept|skip) text_color=$c_faint ;; esac
        mark_state=$sub_state
        [ "$sub_state" = kept ] && mark_state="done"
        mark_for "$mark_state"
        fl "      $mark $text_color$sub_text$c_reset"
        j=$((j + 1))
      done
    fi
    if [ "$i" -eq "$current_stage" ] && [ -n "$ui_err" ] && [ "$ui_final" -eq 0 ]; then
      while IFS= read -r err_line; do
        fl "      $c_warn$err_line$c_reset"
      done <<ERRORS
$ui_err
ERRORS
    fi
    if [ "$i" -eq "$current_stage" ] && [ -n "$ui_op" ] && [ "$ui_final" -eq 0 ]; then
      if [ "$ui_op_input" -eq 1 ]; then
        ui_input_row=$((ui_lines + 1))
        fl "      $c_bold$ui_op$c_reset"
      else
        fl "      $c_faint$ui_op$c_reset"
      fi
    fi
    i=$((i + 1))
  done
  if [ "$ui_final" -eq 0 ] && [ -n "$crew_required" ]; then
    fl ''
    fl "  $c_faint── $c_reset${c_bold}Crew$c_reset$c_faint · what posse works with $ui_rule_crew$c_reset"
    fl "  ${c_faint}Required$c_reset  $crew_required"
    fl "  ${c_faint}Agents$c_reset    $crew_agents"
    fl "  ${c_faint}Add-ons$c_reset   $crew_addons"
    fl "            $c_faint○ not found · posse doctor checks sign-in$c_reset"
  fi
  if [ "$ui_final" -eq 0 ]; then
    fl ''
    fl "  ${c_faint}ctrl+c cancel$c_reset"
  fi
}

rule() {
  r=
  k=0
  while [ "$k" -lt "$1" ]; do
    r="$r$2"
    k=$((k + 1))
  done
  printf '%s' "$r"
}

ui_paint() {
  [ "$ui_live" -eq 1 ] || return 0
  ui_compose
  out=
  if [ "$ui_cursor_up" -gt 0 ]; then
    out="${esc}[${ui_cursor_up}A"
  fi
  printf '%s\r%s%s[J' "$out" "$ui_frame" "$esc"
  ui_frame_lines=$ui_lines
  ui_cursor_up=$ui_lines
}

ui_hide_cursor() {
  if [ "$ui_live" -eq 1 ] && [ "$ui_hidden_cursor" -eq 0 ]; then
    printf '%s[?25l' "$esc"
    ui_hidden_cursor=1
  fi
}

ui_show_cursor() {
  if [ "$ui_hidden_cursor" -eq 1 ]; then
    printf '%s[?25h' "$esc"
    ui_hidden_cursor=0
  fi
}

# busy_start animates the current stage until busy_stop. The ticker paints a
# snapshot of the state; the main shell never paints while it runs.
busy_start() {
  ui_op=$(fit "$1" $((ui_w - 6)))
  ui_op_input=0
  if [ "$ui_live" -eq 0 ]; then
    return
  fi
  ui_paint
  (
    stop=0
    trap 'stop=1' TERM
    trap '' INT
    while [ "$stop" -eq 0 ]; do
      sleep "$tick"
      [ "$stop" -eq 0 ] || break
      ui_spin=$((ui_spin + 1))
      ui_paint
    done
  ) &
  ui_ticker=$!
}

busy_stop() {
  if [ -n "$ui_ticker" ]; then
    kill -TERM "$ui_ticker" 2>/dev/null
    wait "$ui_ticker" 2>/dev/null
    ui_ticker=
  fi
  ui_op=
}

stage_begin() {
  current_stage=$1
  eval "st_$1=active"
  if [ "$ui_live" -eq 1 ]; then
    ui_paint
  else
    stage_title "$1"
    printf '\n[%s/6] %s\n' "$1" "$title"
  fi
}

# stage_end marks the current stage done, skip or warn with a short detail.
stage_end() {
  busy_stop
  eval "st_$current_stage=\$1"
  set_detail "$current_stage" "$2"
  ui_paint
}

status_line() {
  mark=$1
  shift
  if [ "$ui_live" -eq 1 ]; then
    # The ticker owns the screen while it runs.
    if [ -z "$ui_ticker" ] && [ "$mark" != fail ]; then
      ui_op=$(fit "$*" $((ui_w - 6)))
      ui_paint
    fi
    return
  fi
  case $mark in
    ok) printf '  %s✓%s  %s\n' "$c_ok" "$c_reset" "$*" ;;
    fail) printf '  %s✗%s  %s\n' "$c_err" "$c_reset" "$*" ;;
    skip) printf '  %s-%s  %s\n' "$c_faint" "$c_reset" "$*" ;;
  esac
}

# fail stops the run: the stage list shows where it stopped and the reason
# stays in scrollback.
fail() {
  busy_stop
  if [ "$ui_live" -eq 1 ]; then
    [ "$current_stage" -gt 0 ] && eval "st_$current_stage=fail"
    ui_final=1
    ui_paint
    ui_show_cursor
    printf '\n'
    say "$*" "$c_err✗$c_reset"
    if [ -n "${fail_hint:-}" ]; then
      printf '%s\n' "$fail_hint"
    fi
  else
    status_line fail "$*"
    if [ -n "${fail_hint:-}" ]; then
      printf '%s\n' "$fail_hint"
    fi
  fi
  exit 1
}

on_signal() {
  busy_stop
  if [ "$ui_live" -eq 1 ]; then
    if [ "$current_stage" -gt 0 ]; then
      eval "st_$current_stage=fail"
      set_detail "$current_stage" cancelled
    fi
    ui_final=1
    ui_paint
  fi
  ui_show_cursor
  if [ "$current_stage" -ge 5 ]; then
    message="posse is installed, but its setup was interrupted. Run the installer again to finish."
  elif [ "$current_stage" -eq 4 ] && [ "${st_4:-}" = "done" ]; then
    message="posse is installed, but its setup was not run. Run the installer again to finish."
  else
    message="Nothing was installed."
  fi
  printf '\n'
  say "Installation cancelled during step $current_stage. $message"
  exit 130
}

# say prints $1 folded to the content width, after mark $2 when given.
say() {
  printf '%s\n' "$1" | fold -s -w $((ui_w - 5)) | sed "s/ *$//" | {
    first=1
    while IFS= read -r say_line; do
      if [ "$first" -eq 1 ] && [ -n "${2:-}" ]; then
        printf '  %s  %s\n' "$2" "$say_line"
      else
        printf '     %s\n' "$say_line"
      fi
      first=0
    done
  }
}

on_exit() {
  if [ -n "${ui_ticker:-}" ]; then
    kill -TERM "$ui_ticker" 2>/dev/null
  fi
  ui_show_cursor
  if [ -n "${temp_dir:-}" ]; then
    rm -rf "$temp_dir"
  fi
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

# read_answer shows prompt $1 and reads one line from the terminal into
# PROMPT_REPLY, even when the script itself arrives on stdin.
read_answer() {
  PROMPT_REPLY=
  if [ "$ui_live" -eq 1 ]; then
    ui_op=$1
    ui_op_input=1
    ui_paint
    # Put the cursor at the end of the prompt line inside the frame.
    up=$((ui_frame_lines - ui_input_row + 1))
    column=$((6 + $(vlen "$1")))
    printf '%s[%sA\r%s[%sC' "$esc" "$up" "$esc" "$column"
    ui_cursor_up=$((ui_input_row - 1))
    ui_show_cursor
    if IFS= read -r PROMPT_REPLY <&3; then :; else PROMPT_REPLY=; fi
    ui_hide_cursor
    wraps=$(((column + $(vlen "$PROMPT_REPLY")) / ui_cols))
    ui_cursor_up=$((ui_input_row + wraps))
    ui_op=
    ui_op_input=0
    return
  fi
  if [ "$tty_is_real" -eq 1 ]; then
    printf '  %s' "$1" >&3
  else
    printf '  %s' "$1"
  fi
  if IFS= read -r PROMPT_REPLY <&3; then :; else PROMPT_REPLY=; fi
  if [ "$tty_is_real" -eq 0 ]; then
    printf '\n'
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
  read_answer "$question [$choices] "
  case $PROMPT_REPLY in
    '') PROMPT_REPLY=$default_answer ;;
    [Yy]*) PROMPT_REPLY=y ;;
    *) PROMPT_REPLY=n ;;
  esac
}

# ---------------------------------------------------------------------------
# Step 1: tools posse needs, agents it can run and optional add-ons.

crew_item() {
  # crew_item <found:0|1> <color> <label>
  if [ "$1" -eq 1 ]; then
    printf '%s●%s %s' "$2" "$c_reset" "$3"
  else
    printf '%s○ %s%s' "$c_faint" "$3" "$c_reset"
  fi
}

addon_item() {
  if [ "$1" -eq 1 ]; then
    printf '%s✓%s %s' "$c_ok" "$c_reset" "$2"
  else
    printf '%s○ %s%s' "$c_faint" "$2" "$c_reset"
  fi
}

detect_crew() {
  agents_found=
  agents_missing=
  crew_agents=
  for agent in claude:Claude\ Code:"$c_claude" codex:Codex:"$c_codex" pi:Pi:"$c_pi" opencode:OpenCode:"$c_opencode"; do
    command_name=${agent%%:*}
    rest=${agent#*:}
    label=${rest%%:*}
    color=${rest#*:}
    if command -v "$command_name" >/dev/null 2>&1; then
      found=1
      agents_found="${agents_found:+$agents_found, }$label"
    else
      found=0
      agents_missing="${agents_missing:+$agents_missing, }$label"
    fi
    crew_agents="${crew_agents:+$crew_agents  }$(crew_item "$found" "$color" "$label")"
  done
  addons_found=
  addons_missing=
  crew_addons=
  for addon in gh glab lavish-axi quota-axi; do
    if command -v "$addon" >/dev/null 2>&1; then
      found=1
      addons_found="${addons_found:+$addons_found, }$addon"
    else
      found=0
      addons_missing="${addons_missing:+$addons_missing, }$addon"
    fi
    crew_addons="${crew_addons:+$crew_addons  }$(addon_item "$found" "$addon")"
  done
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
  busy_stop
  if [ "$ui_live" -eq 0 ]; then
    printf '\n  No release archive is available for %s/%s.\n' "$target_os" "$target_arch"
    printf '  Build from source with: go install github.com/thanhbinh1905/posse/cmd/posse@%s\n' "$version"
  fi
  ask_yes_no "No $target_os/$target_arch archive. Build from source with go install?" n yes
  if [ "$PROMPT_REPLY" != y ]; then
    fail "Source installation was skipped."
  fi
  mkdir -p "$temp_dir/gobin" || fail "Could not create a temporary Go install directory."
  busy_start "Building posse $version with go install"
  if ! GOBIN="$temp_dir/gobin" go install "github.com/thanhbinh1905/posse/cmd/posse@$version" > "$temp_dir/go.log" 2>&1; then
    fail_hint=$(sed 's/^/  /' "$temp_dir/go.log")
    fail "The source build failed."
  fi
  busy_stop
  [ -x "$temp_dir/gobin/posse" ] || fail "The source build did not produce a posse binary."
  install_binary=$temp_dir/gobin/posse
  verify_detail="built from source"
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

# path_hint prints how to put the prefix on PATH, or nothing when it is there.
path_hint() {
  case :$PATH: in
    *:"$prefix":*) return ;;
  esac
  shell_path=${SHELL:-}
  shell_name=${shell_path##*/}
  case $shell_name in
    fish) printf 'Add this to fish: fish_add_path "%s"' "$prefix" ;;
    bash|zsh) printf "Add this to your shell config: export PATH=\"%s:\$PATH\"" "$prefix" ;;
    *) printf 'Add %s to your PATH.' "$prefix" ;;
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
  git_version=$(git --version 2>/dev/null | sed -n 's/^[^0-9]*\([0-9][0-9.]*\).*/\1/p')
  status_line ok "Git ${git_version:-is installed}"

  herdr_hint='  Install or upgrade Herdr: curl -fsSL https://herdr.dev/install.sh | sh
  Official instructions: https://herdr.dev/docs/install/'
  if ! command -v herdr >/dev/null 2>&1; then
    fail_hint=$herdr_hint
    fail "Herdr 0.9.0 or newer is required."
  fi
  herdr_raw=$(herdr --version 2>/dev/null || true)
  herdr_version=$(printf '%s\n' "$herdr_raw" | sed -n 's/^[^0-9]*\([0-9][0-9]*\)\.\([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\1.\2.\3/p' | sed -n '1p')
  if [ -z "$herdr_version" ] || ! printf '%s\n' "$herdr_version" | awk -F. '($1 + 0) > 0 || (($1 + 0) == 0 && ($2 + 0) > 9) || (($1 + 0) == 0 && ($2 + 0) == 9 && ($3 + 0) >= 0) { exit 0 } { exit 1 }'; then
    fail_hint=$herdr_hint
    fail "Herdr 0.9.0 or newer is required. Detected: ${herdr_raw:-unknown version}."
  fi
  status_line ok "Herdr $herdr_version"

  detect_crew
  crew_required="$c_ok✓$c_reset git${git_version:+ $git_version}  $c_ok✓$c_reset Herdr $herdr_version"
  if [ "$ui_live" -eq 0 ]; then
    if [ -n "$agents_found" ]; then
      status_line ok "Agents found: $agents_found (sign-in is checked by posse doctor)"
    else
      status_line skip "No supported agent found: install Claude Code, Codex, Pi or OpenCode."
    fi
    if [ -n "$addons_found" ]; then
      status_line ok "Optional add-ons found: $addons_found"
    fi
    if [ -n "$addons_missing" ]; then
      status_line skip "Optional add-ons not found: $addons_missing"
    fi
  fi
  stage_end "done" "$target_os/$target_arch"
}

# ---------------------------------------------------------------------------
# Step 5: the data folder. POSSE_HOME wins for this run; otherwise posse
# saves the choice in its home pointer file, never in a shell rc file.

choose_home() {
  binary=$prefix/posse
  if [ "$no_setup" -eq 1 ]; then
    status_line skip "Skipped by --no-setup."
    stage_end skip "skipped"
    return
  fi
  if ! home_info=$("$binary" _home 2>&1); then
    case $home_info in
      *unknown\ command*)
        status_line skip "This posse version keeps its data in POSSE_HOME or ~/.posse."
        stage_end skip "$(display_path "${POSSE_HOME:-$HOME/.posse}")"
        return
        ;;
    esac
    fail "$home_info"
  fi
  read_home_info "$home_info"
  if [ "$home_source" = env ]; then
    if [ -n "$home_choice" ]; then
      fail "--home cannot be combined with POSSE_HOME. Unset POSSE_HOME to save a data folder."
    fi
    status_line ok "Using POSSE_HOME=$home_path for this run; it is not saved."
    stage_end "done" "$(display_path "$home_path") (POSSE_HOME)"
    return
  fi
  if [ -n "$home_choice" ]; then
    save_home "$home_choice" || fail "$home_error"
  elif [ "$tty_available" -eq 1 ]; then
    while :; do
      read_answer "Data folder [$(display_path "$home_path")]: "
      [ -n "$PROMPT_REPLY" ] || break
      if save_home "$PROMPT_REPLY"; then
        break
      fi
      if [ "$ui_live" -eq 1 ]; then
        ui_err=$(printf '%s\n' "$home_error" | fold -s -w $((ui_w - 8)) | sed "s/ *$//")
      else
        status_line fail "$home_error"
      fi
    done
  else
    status_line skip "No TTY available; keeping the current data folder."
  fi
  ui_err=
  status_line ok "posse keeps its data in $home_path"
  stage_end "done" "$(display_path "$home_path")"
}

read_home_info() {
  home_path=$(printf '%s\n' "$1" | sed -n '1p')
  home_source=$(printf '%s\n' "$1" | sed -n '2p')
  home_pointer=$(printf '%s\n' "$1" | sed -n '3p')
}

save_home() {
  if home_info=$("$binary" _home --set "$1" 2>&1); then
    read_home_info "$home_info"
    return 0
  fi
  home_error=$home_info
  return 1
}

# ---------------------------------------------------------------------------
# Step 6: machine setup. posse setup --human prints one line per area; this
# step shows them as rows and keeps their notes for the final summary.

# trim_sub drops suffix $1 from the last setup row; its mark already says it.
trim_sub() {
  eval "trim_text=\$sub_text_$sub_count"
  eval "sub_text_$sub_count=\${trim_text%\"\$1\"}"
}

# add_note adds an indented note under the last setup row, folded to fit.
add_note() {
  note=$(printf '%s\n' "$1" | sed 's/^ *//')
  printf '%s\n' "$note" | fold -s -w $((ui_w - 8)) | sed "s/ *$//" > "$temp_dir/note"
  while IFS= read -r note_line; do
    add_sub note "$note_line"
  done < "$temp_dir/note"
}

add_sub() {
  sub_count=$((sub_count + 1))
  fitted=$(fit "$2" $((ui_w - 8)))
  eval "sub_state_$sub_count=\$1 sub_text_$sub_count=\$fitted"
}

# show_setup_lines turns posse setup --human output into stage rows, or
# prints it as is in plain mode.
show_setup_lines() {
  sub_count=0
  setup_notes=
  setup_changes=0
  while IFS= read -r line; do
    case $line in
      '') ;;
      '✓  '*) add_sub "done" "${line#✓  }"; setup_changes=$((setup_changes + 1)) ;;
      '+  '*) add_sub pending "${line#+  }"; trim_sub ': will be added' ;;
      '-  '*)
        case $line in
          *': already set') add_sub kept "${line#-  }" ;;
          *) add_sub skip "${line#-  }" ;;
        esac
        ;;
      '!  '*) add_sub warn "${line#!  }" ;;
      '✗  '*) add_sub fail "${line#✗  }" ;;
      *)
        setup_notes="$setup_notes$line
"
        add_note "$line"
        ;;
    esac
  done <<EOF
$1
EOF
}

# print_setup_lines prints posse setup --human output under a plain step.
print_setup_lines() {
  if [ "$ui_live" -eq 0 ]; then
    printf '%s\n' "$1" | sed \
      -e '/^$/d' \
      -e 's/^/  /' \
      -e "s/^  ✓/  $c_ok✓$c_reset/" \
      -e "s/^  ✗/  $c_err✗$c_reset/" \
      -e "s/^  !/  $c_warn!$c_reset/" \
      -e "s/^  -  /  $c_faint-$c_reset  /"
  fi
}

setup_after_install() {
  binary=$prefix/posse
  setup_state=skipped
  if [ "$no_setup" -eq 1 ]; then
    status_line skip "Machine setup skipped by --no-setup."
    stage_end skip "skipped"
    return
  fi
  help_output=$("$binary" --help 2>&1 || true)
  case $help_output in
    *setup*) ;;
    *)
      status_line skip "This posse build has no setup command."
      stage_end skip "not available"
      return
      ;;
  esac
  busy_start "Checking hooks, Herdr plugin and skills"
  # --exit-code exits 0 when setup is complete and 3 when changes are pending.
  setup_status=0
  setup_plan=$("$binary" setup --check --exit-code --human 2>&1) || setup_status=$?
  busy_stop
  case $setup_plan in
    error\{*|*"
error{"*)
      status_line skip "This posse version has no readable setup output."
      stage_end skip "not available"
      return
      ;;
  esac
  show_setup_lines "$setup_plan"
  if [ "$setup_status" -ne 3 ] || { [ "$tty_available" -eq 0 ] && [ "$auto_yes" -eq 0 ]; }; then
    print_setup_lines "$setup_plan"
  fi
  case $setup_status in
    0)
      setup_state=ready
      status_line ok "Your agents are already prepared."
      stage_end "done" "already set"
      ;;
    3)
      if [ "$tty_available" -eq 0 ] && [ "$auto_yes" -eq 0 ]; then
        setup_state=pending
        status_line skip "No TTY available; nothing was changed. Run posse setup, or rerun with --yes."
        stage_end warn "not applied"
        return
      fi
      ui_paint
      busy_start "Adding hooks, the Herdr plugin and skills"
      if setup_result=$("$binary" setup --human 2>&1); then
        busy_stop
        show_setup_lines "$setup_result"
        print_setup_lines "$setup_result"
        setup_state=ready
        if [ "$setup_changes" -eq 1 ]; then
          stage_end "done" "1 change"
        else
          stage_end "done" "$setup_changes changes"
        fi
      else
        busy_stop
        show_setup_lines "$setup_result"
        print_setup_lines "$setup_result"
        [ "$ui_live" -eq 1 ] && fail_hint=$setup_notes
        fail "Machine setup failed. Fix the issue above, then run the installer again."
      fi
      ;;
    *)
      setup_state=pending
      status_line skip "Machine setup was not applied."
      stage_end warn "not applied"
      ;;
  esac
}

print_box() {
  # print_box <title> <line>...
  box_title=$1
  shift
  if [ "$ui_color" -eq 1 ]; then
    width=$((ui_w - 2))
    [ "$width" -le 64 ] || width=64
    printf '\n  %s╭─ %s%s%s %s╮%s\n' "$c_faint" "$c_reset$c_bold" "$box_title" "$c_reset$c_faint" "$(rule $((width - 5 - ${#box_title})) ─)" "$c_reset"
    for box_line in "$@"; do
      box_len=$(vlen "$box_line")
      pad $((width - 4 - box_len))
      box_line=$(printf '%s' "$box_line" | sed "s/posse up/$c_lead${c_bold}posse up$c_reset/; s/posse setup/$c_lead${c_bold}posse setup$c_reset/")
      printf '  %s│%s %s%s %s│%s\n' "$c_faint" "$c_reset" "$box_line" "$pad" "$c_faint" "$c_reset"
    done
    printf '  %s╰%s╯%s\n' "$c_faint" "$(rule $((width - 2)) ─)" "$c_reset"
    return
  fi
  printf '\n  +%s+\n' '------------------------------------------------------------'
  printf '  | %-58s |\n' "$box_title"
  for box_line in "$@"; do
    printf '  | %-58s |\n' "$box_line"
  done
  printf '  +%s+\n' '------------------------------------------------------------'
}

finish() {
  busy_stop
  if [ "$ui_live" -eq 1 ]; then
    ui_final=1
    ui_paint
    ui_show_cursor
  fi
  hint=$(path_hint)
  if [ -n "$hint" ]; then
    printf '\n'
    printf '%s\n' "$hint" | fold -s -w $((ui_w - 4)) | sed "s/ *$//" | {
      IFS= read -r first
      printf '  %s!%s %s\n' "$c_warn" "$c_reset" "$first"
      while IFS= read -r rest; do
        printf '    %s\n' "$rest"
      done
    }
  fi
  if [ "$setup_state" = ready ]; then
    print_box 'posse is ready' \
      'Next: open a new Herdr workspace in your repository' \
      '      (Ctrl+B, then Shift+N) and run posse up there.' \
      '      The Lead can walk you through configuration.'
  else
    print_box 'posse is installed' \
      'Next: run posse setup to prepare your agents.' \
      'Then: open a new Herdr workspace in your repository' \
      '      (Ctrl+B, then Shift+N) and run posse up there.'
  fi
}

uninstall_posse() {
  binary=$prefix/posse
  data_dir=${POSSE_HOME:-$HOME/.posse}
  data_pointer=
  if [ -x "$binary" ]; then
    if home_info=$("$binary" _home 2>/dev/null); then
      read_home_info "$home_info"
      data_dir=$home_path
      [ "$home_source" = pointer ] && data_pointer=$home_pointer
    fi
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
  case $data_dir in
    ''|/|"$HOME"|"$HOME"/) fail "Refusing to delete $data_dir." ;;
  esac
  ask_yes_no "Delete $data_dir (Task history and config)?" n no
  if [ "$PROMPT_REPLY" = y ]; then
    if rm -rf "$data_dir"; then
      status_line ok "Removed $data_dir"
      if [ -n "$data_pointer" ] && rm -f "$data_pointer"; then
        status_line ok "Removed $data_pointer"
      fi
    else
      fail "Could not remove $data_dir."
    fi
  else
    status_line skip "Kept $data_dir (Task history and config)."
  fi
  print_box 'posse has been uninstalled' 'Next: install again with install.sh'
}

main() {
  auto_yes=0
  no_setup=0
  uninstall=0
  requested_version=
  home_choice=
  temp_dir=
  prefix=${POSSE_PREFIX:-}
  custom_release_base=0
  if [ -n "${POSSE_RELEASE_BASE:-}" ]; then
    custom_release_base=1
    release_base=${POSSE_RELEASE_BASE%/}
  else
    release_base=
  fi
  ui_live=0
  ui_ticker=
  ui_err=
  ui_hidden_cursor=0
  c_ok='' c_err='' c_faint='' c_reset=''
  current_stage=0

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
      --home)
        [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--home requires a directory."
        home_choice=$2
        shift 2
        ;;
      --help|-h) usage; return 0 ;;
      *) fail "Unknown option: $1. Run install.sh --help for usage." ;;
    esac
  done
  if [ "$no_setup" -eq 1 ] && [ -n "$home_choice" ]; then
    fail "--home cannot be combined with --no-setup, which changes nothing but the binary."
  fi

  [ -n "${HOME:-}" ] || fail "HOME is not set."
  [ -n "$prefix" ] || prefix=$HOME/.local/bin
  case $prefix in
    /*) ;;
    *) prefix=$(pwd)/$prefix ;;
  esac

  ui_init
  if [ "$uninstall" -eq 1 ]; then
    ui_live=0
  fi
  trap on_exit 0
  trap on_signal INT TERM HUP
  ui_header
  open_prompt_input

  if [ "$uninstall" -eq 1 ]; then
    uninstall_posse
    return 0
  fi

  set_head
  ui_hide_cursor
  stage_begin 1
  check_tools

  stage_begin 2
  POSSE_REPO=${POSSE_REPO:-Thanhbinh1905/posse}
  version=${requested_version:-${POSSE_VERSION:-}}
  temp_dir=${TMPDIR:-/tmp}/posse-install.$$
  if [ -e "$temp_dir" ]; then
    bad_temp=$temp_dir
    temp_dir=
    fail "Temporary path already exists: $bad_temp"
  fi
  (umask 077 && mkdir "$temp_dir") || fail "Could not create a temporary work directory."
  choose_auth
  version_note=
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
    busy_start "Asking GitHub for the latest release"
    api_url=https://api.github.com/repos/$POSSE_REPO/releases/latest
    api_download "$api_url" "$release_json" || fail "Could not read the latest release. For a private repository, set GH_TOKEN or GITHUB_TOKEN, or authenticate gh."
    busy_stop
    version=$(sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' "$release_json" | sed -n '1p')
    [ -n "$version" ] || fail "The latest release response did not include a tag."
    version_note=latest
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
      busy_start "Reading release $version"
      if api_download "$api_url" "$release_json"; then
        :
      else
        rm -f "$release_json"
      fi
      busy_stop
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
  version_label=$version
  set_head
  stage_end "done" "$version${version_note:+ $version_note}"

  stage_begin 3
  if [ "$auth_enabled" -eq 1 ] && [ "$custom_release_base" -eq 0 ]; then
    status_line ok "Release downloads use the authenticated GitHub API."
  elif [ "$custom_release_base" -eq 1 ]; then
    status_line skip "Using the configured release base."
  fi
  asset_version=${version#v}
  archive_name=posse_${asset_version}_${target_os}_${target_arch}.tar.gz
  archive_file=$temp_dir/$archive_name
  checksum_file=$temp_dir/checksums.txt
  verify_detail="SHA-256 ok"
  busy_start "Downloading $archive_name"
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
    busy_stop
    busy_start "Verifying the SHA-256 checksum"
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
  stage_end "done" "$verify_detail"

  stage_begin 4
  mkdir -p "$prefix" || fail "Could not create install prefix $prefix."
  prefix=$(cd "$prefix" && pwd) || fail "Could not resolve install prefix."
  atomic_install "$install_binary"
  status_line ok "Installed $prefix/posse"
  stage_end "done" "$(display_path "$prefix/posse")"

  stage_begin 5
  choose_home

  stage_begin 6
  setup_after_install

  finish
}

main "$@"
