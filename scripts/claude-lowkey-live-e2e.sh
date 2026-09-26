#!/usr/bin/env bash
# Opt-in credentialed live TUI test. Only this script's tmux server and worktree lab
# are touched. Requires an authenticated isolated Claude Code environment.
set -euo pipefail
if [[ ${POSSE_CLAUDE_LOWKEY_LIVE_E2E:-} != 1 ]]; then
  echo 'skip: set POSSE_CLAUDE_LOWKEY_LIVE_E2E=1 for the live Claude test'
  exit 0
fi
command -v tmux >/dev/null
command -v claude >/dev/null
: "${ANTHROPIC_API_KEY:?Set ANTHROPIC_API_KEY for the isolated Claude session}"
root=$(git rev-parse --show-toplevel)
lab=$(mktemp -d "$root/.claude-lowkey-e2e.XXXXXX")
socket="posse-lowkey-e2e-$$"
trap 'tmux -L "$socket" kill-server 2>/dev/null || true; rm -rf "$lab"' EXIT
mkdir -p "$lab/project" "$lab/config" "$lab/claude" "$lab/posse"
printf '[lowkey]\nlead = true\n' > "$lab/posse/config.toml"
printf 'alpha beta gamma\n' > "$lab/project/notes.txt"
# Drop all inherited Herdr routing and Claude session markers. Authentication, if
# supplied by the test runner, is the only inherited Claude state.
for name in ${!HERDR_@} ${!CLAUDE_CODE_@} CLAUDECODE; do unset "$name"; done
export XDG_CONFIG_HOME="$lab/config" POSSE_HOME="$lab/posse" CLAUDE_CONFIG_DIR="$lab/claude"
export POSSE_LOWKEY_CONFIG="$lab/posse/config.toml" POSSE_LOWKEY_GLOBAL_CONFIG="$lab/posse/config.toml"
export CLAUDE_CODE_ENABLE_FUNCTION_HOOKS=1
export CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false
export CLAUDE_CODE_SEND_FEEDBACK=0
plugin="$root/internal/app/claude_lowkey"
tmux -L "$socket" new-session -d -s test -x 140 -y 44 -c "$lab/project" \
  "claude --model haiku --plugin-dir '$plugin' --dangerously-skip-permissions --settings '{\"feedbackDrafts\":\"off\"}' --debug-file '$lab/debug.log'"
screen() { tmux -L "$socket" capture-pane -p -S -100 -t test; }
wait_for() {
  local needle=$1 attempt shot selected
  for ((attempt=0;attempt<240;attempt++)); do
    shot=$(screen)
    if [[ $shot == *'Yes, I trust this folder'* ]]; then
      selected=$(printf '%s\n' "$shot" | grep -F '❯' | head -1 || true)
      if [[ $selected == *'Yes, I trust this folder'* ]]; then
        tmux -L "$socket" send-keys -t test Enter
      else
        tmux -L "$socket" send-keys -t test Down
      fi
    elif [[ $shot == *"$needle"* ]]; then
      return 0
    fi
    sleep .25
  done
  screen >&2
  echo "missing Claude TUI text: $needle" >&2
  exit 1
}
wait_for '❯'
# The test runner must pre-trust the isolated project, or handle Claude's trust prompt.
tmux -L "$socket" send-keys -t test -l 'Run Bash: sleep 3; cat notes.txt. Reply with the three words in reverse order.'
tmux -L "$socket" send-keys -t test Enter
wait_for '░▒▓█'
wait_for 'gamma beta alpha'
if screen | grep -Eq 'Bash\(|⏺ Bash'; then
  echo 'tool row leaked into the lowkey transcript' >&2; exit 1
fi
tmux -L "$socket" send-keys -t test -l '/lowkey off'
tmux -L "$socket" send-keys -t test Enter
wait_for 'Bash('
grep -Eq 'lead = false' "$lab/posse/config.toml"
tmux -L "$socket" send-keys -t test -l '/lowkey on'
tmux -L "$socket" send-keys -t test Enter
wait_for 'Lowkey on'
grep -Eq 'lead = true' "$lab/posse/config.toml"
tmux -L "$socket" send-keys -t test C-c C-c
# The restored transcript must use the preference before the first row draws.
tmux -L "$socket" kill-session -t test
tmux -L "$socket" new-session -d -s test -x 140 -y 44 -c "$lab/project" \
  "claude --continue --plugin-dir '$plugin' --dangerously-skip-permissions --settings '{\"feedbackDrafts\":\"off\"}'"
wait_for 'gamma beta alpha'
if screen | grep -Eq 'Bash\(|⏺ Bash'; then echo 'continued tool row leaked' >&2; exit 1; fi
echo 'Claude lowkey live E2E passed'
