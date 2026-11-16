//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestCodexLaunchSuppressesUpdatePromptAndInterruptRefuses(t *testing.T) {
	fixture := newWorkerTabsFixture(t)
	if _, err := fixture.client.Run(t.Context(), "integration", "install", "codex"); err != nil {
		t.Fatalf("install Codex integration in isolated Herdr: %v", err)
	}
	configPath := filepath.Join(fixture.home, "config.toml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config = []byte(strings.Replace(string(config), "[profiles.deep]\nkind = \"claude\"", "[profiles.deep]\nkind = \"codex\"", 1))
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	argsLog := filepath.Join(fixture.root, "codex-args.log")
	codex := `#!/bin/sh
set -eu
printf '%s\n' "$@" > "$POSSE_TEST_ROOT/codex-args.log"
if ! printf '%s\n' "$@" | grep -Fq 'check_for_update_on_startup=false'; then
  printf 'A new version of Codex is available. Install update? [Y/n]\n'
  : > "$POSSE_TEST_ROOT/codex-update-visible"
  while IFS= read -r line; do :; done
  exit 0
fi
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent codex --state idle >/dev/null 2>&1
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent codex --state working >/dev/null 2>&1
while IFS= read -r line; do :; done
`
	if err := os.WriteFile(filepath.Join(fixture.root, "bin", "codex"), []byte(codex), 0o700); err != nil {
		t.Fatal(err)
	}
	task := fixture.start(t, "t1", "Codex update prompt", "codex-update-prompt")
	if task.State != store.StateWorking {
		t.Fatalf("Codex Task state = %s, want working", task.State)
	}
	args, err := os.ReadFile(argsLog)
	if err != nil || !strings.Contains(string(args), "check_for_update_on_startup=false") {
		t.Fatalf("fake Codex launch args omitted update suppression: %s, %v", args, err)
	}
	if !strings.Contains(string(args), "Read ") || !strings.Contains(string(args), "launch.md and follow it.") {
		t.Fatalf("fake Codex launch omitted its opening instruction: %s", args)
	}

	output, err := runPosseAllowFailure(fixture, fixture.repo, fixture.leadEnv, "send", "t1", "--interrupt", "Stop and redirect")
	if err == nil || !strings.Contains(output, "Codex does not support verified turn interruption") {
		t.Fatalf("posse send --interrupt output=%s err=%v; want a clear refusal", output, err)
	}
	if task := fixture.task(t, "t1"); task.State != store.StateWorking {
		t.Fatalf("Codex Task changed state after refused interrupt: %s", task.State)
	}

	if _, err := os.Stat(filepath.Join(fixture.root, "codex-update-visible")); !os.IsNotExist(err) {
		t.Fatalf("fake Codex displayed the update prompt despite launch suppression: %v", err)
	}
}
