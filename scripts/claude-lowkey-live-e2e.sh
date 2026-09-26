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
if [[ -z ${ANTHROPIC_API_KEY:-} && ! -r $HOME/.claude/.credentials.json ]]; then
  echo 'Set ANTHROPIC_API_KEY or sign in to Claude Code with OAuth' >&2
  exit 1
fi
root=$(git rev-parse --show-toplevel)
lab=$(mktemp -d "$root/.claude-lowkey-e2e.XXXXXX")
socket="posse-lowkey-e2e-$$"
cleanup() {
  tmux -L "$socket" kill-server 2>/dev/null || true
  if [[ ${POSSE_LOWKEY_KEEP_LAB:-} == 1 ]]; then
    echo "isolated test lab: $lab"
    return
  fi
  # Claude can flush a debug log after its pane disappears. Wait for the writer
  # to settle, then check the absence again after removing the directory.
  local attempt
  sleep 1
  for ((attempt=0;attempt<40;attempt++)); do
    rm -rf "$lab" 2>/dev/null || true
    sleep .25
    if [[ ! -e $lab ]]; then return; fi
  done
  echo "could not remove isolated Claude lab: $lab" >&2
  return 1
}
trap cleanup EXIT
mkdir -p "$lab/project" "$lab/config" "$lab/claude" "$lab/posse"
chmod 700 "$lab" "$lab/claude"
if [[ -z ${ANTHROPIC_API_KEY:-} ]]; then
  cp "$HOME/.claude/.credentials.json" "$lab/claude/.credentials.json"
  chmod 600 "$lab/claude/.credentials.json"
  python3 - "$lab/claude/.claude.json" "$lab/project" <<'PY'
import json, os, sys
with open(os.path.expanduser('~/.claude.json')) as source:
    settings = json.load(source)
limited = {key: settings[key] for key in ('oauthAccount', 'userID', 'theme', 'lastOnboardingVersion') if key in settings}
limited.update(hasCompletedOnboarding=True, bypassPermissionsModeAccepted=True,
               projects={sys.argv[2]: {'hasTrustDialogAccepted': True}})
with open(sys.argv[1], 'w') as destination:
    json.dump(limited, destination)
os.chmod(sys.argv[1], 0o600)
PY
fi
printf '[lowkey]\nlead = true\n' > "$lab/posse/config.toml"
printf 'alpha beta gamma\n' > "$lab/project/notes.txt"
# Drop all inherited Herdr routing and Claude session markers. Authentication, if
# supplied by the test runner, is the only inherited Claude state.
for name in ${!HERDR_@} ${!CLAUDE_CODE_@} CLAUDECODE; do unset "$name"; done
export XDG_CONFIG_HOME="$lab/config" POSSE_HOME="$lab/posse" CLAUDE_CONFIG_DIR="$lab/claude"
export POSSE_LOWKEY_CONFIG="$lab/posse/config.toml" POSSE_LOWKEY_GLOBAL_CONFIG="$lab/posse/config.toml"
export POSSE_LOWKEY_NOTICES_DIR="$lab/posse/projects/shop/lead-claude-notices"
export CLAUDE_CODE_ENABLE_FUNCTION_HOOKS=1
export CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false
export CLAUDE_CODE_SEND_FEEDBACK=0
# Claude generates a types/ tree and tsconfig.json in --plugin-dir. Keep those
# writes in the isolated lab, not the source checkout.
cp -R "$root/internal/app/claude_lowkey" "$lab/plugin"
plugin="$lab/plugin"
tmux -L "$socket" new-session -d -s test -x 140 -y 44 -c "$lab/project" \
  "claude --model haiku --plugin-dir '$plugin' --dangerously-skip-permissions --settings '{\"feedbackDrafts\":\"off\"}' --debug-file '$lab/debug.log'"
# Current viewport only: a main-screen redraw leaves older copies in scrollback.
screen() { tmux -L "$socket" capture-pane -p -t test; }
wait_config() {
  local value=$1 attempt
  for ((attempt=0;attempt<120;attempt++)); do
    if grep -Eq "lead = $value" "$lab/posse/config.toml"; then return 0; fi
    sleep .25
  done
  echo "lowkey setting did not become $value" >&2; exit 1
}
wait_absent() {
  local needle=$1 attempt shot
  for ((attempt=0;attempt<120;attempt++)); do
    shot=$(screen)
    if [[ $shot != *"$needle"* ]]; then return 0; fi
    sleep .25
  done
  echo "unexpected transcript row: $needle" >&2; exit 1
}
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
sleep 1
tmux -L "$socket" send-keys -t test Enter
wait_for '░▒▓█'
wait_for 'gamma beta alpha'
if screen | grep -Eq 'Ran 1 shell command|Bash\(|⏺ Bash'; then
  echo 'tool row leaked into the lowkey transcript' >&2; exit 1
fi
tmux -L "$socket" send-keys -t test -l '/lowkey off'
sleep 1
tmux -L "$socket" send-keys -t test Enter
wait_config false
wait_for 'Ran 1 shell command'
tmux -L "$socket" send-keys -t test -l '/lowkey on'
sleep 1
tmux -L "$socket" send-keys -t test Enter
wait_config true
wait_absent 'Ran 1 shell command'
tmux -L "$socket" send-keys -t test C-c C-c
# The restored transcript must use the preference before the first row draws.
tmux -L "$socket" kill-session -t test
tmux -L "$socket" new-session -d -s test -x 140 -y 44 -c "$lab/project" \
  "claude --continue --plugin-dir '$plugin' --dangerously-skip-permissions --settings '{\"feedbackDrafts\":\"off\"}'"
wait_for 'gamma beta alpha'
if screen | grep -Eq 'Ran 1 shell command|Bash\(|⏺ Bash'; then echo 'continued tool row leaked' >&2; exit 1; fi
echo 'Claude lowkey live E2E passed'
