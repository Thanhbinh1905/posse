//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestB05ConfigDispatchAndFrozenRelaunch(t *testing.T) {
	fixture := reviewIdentityFixture(t)
	defer fixture.db.Close()

	if code, output := runB05CLI(t, fixture, fixture.env, "config", "set", "remuda.keep_idle", "0"); code != 0 {
		t.Fatalf("keep_idle=0 was rejected: code=%d\n%s", code, output)
	}
	if code, output := runB05CLI(t, fixture, fixture.env, "config", "set", "defaults.max_workers", "0"); code == 0 || !strings.Contains(output, "defaults.max_workers") {
		t.Fatalf("non-positive execution budget was not rejected: code=%d\n%s", code, output)
	}
	if code, output := runB05CLI(t, fixture, fixture.env, "config", "unset", "dispatch.default.use"); code != 0 {
		t.Fatalf("clear fixture Dispatch default: code=%d\n%s", code, output)
	}
	configOutput := runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "show", "--effective", "--json")
	var effective struct {
		Effective struct {
			Defaults struct {
				MaxWorkers int `json:"max_workers"`
			} `json:"defaults"`
			Remuda struct {
				KeepIdle int `json:"keep_idle"`
			} `json:"remuda"`
		} `json:"effective"`
	}
	if err := json.Unmarshal([]byte(configOutput), &effective); err != nil {
		t.Fatalf("decode effective config: %v\n%s", err, configOutput)
	}
	if effective.Effective.Remuda.KeepIdle != 0 || effective.Effective.Defaults.MaxWorkers != 20 {
		t.Fatalf("effective capacity config = %#v, want keep_idle=0 and positive max_workers=20", effective.Effective)
	}

	for _, setting := range [][2]string{
		{"profiles.author.effort", "high"},
		{"lead.profiles.claude", "author"},
	} {
		if code, output := runB05CLI(t, fixture, fixture.env, "config", "set", setting[0], setting[1]); code != 0 {
			t.Fatalf("set %s: code=%d\n%s", setting[0], code, output)
		}
	}
	brief := filepath.Join(fixture.root, "b05-brief.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: Frozen launch contract\ndone_when: relaunch preserves the admitted Profile\n---\nExercise the admitted launch.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	kindOnly := b05Dispatch(t, fixture, brief)
	if kindOnly.Profile != "implicit:claude" || kindOnly.Model != "" || kindOnly.Effort != "" || kindOnly.ModelKnown {
		t.Fatalf("default compatibility inheritance = %#v, want kind-only with unknown model identity", kindOnly)
	}
	if code, output := runB05CLI(t, fixture, fixture.env, "config", "set", "defaults.dispatch_inheritance", "lead_profile"); code != 0 {
		t.Fatalf("select Lead Profile inheritance: code=%d\n%s", code, output)
	}
	leadProfile := b05Dispatch(t, fixture, brief)
	if leadProfile.Profile != "author" || leadProfile.Model != "model-a" || leadProfile.Effort != "high" || !leadProfile.ModelKnown {
		t.Fatalf("Lead Profile inheritance = %#v, want author/model-a/high with known model identity", leadProfile)
	}

	// Force the first Rider attempt to fail itself, so relaunch exercises the
	// Task's frozen plan after the current kind is disabled for new dispatch.
	agent := `#!/bin/sh
set -eu
printf '%s\t%s\n' "$PWD" "$*" >> "$POSSE_TEST_ROOT/b05-agent-args.log"
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
case "$PWD/" in
  "$POSSE_E2E_WORKTREES/"*)
    count_file="$POSSE_TEST_ROOT/b05-agent-starts"
    count=0
    if [ -f "$count_file" ]; then count=$(cat "$count_file"); fi
    count=$((count + 1))
    printf '%s\n' "$count" > "$count_file"
    IFS= read -r prompt || exit 0
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
    if [ "$count" -eq 1 ]; then
      while [ ! -f "$POSSE_TEST_ROOT/b05-fail-first" ]; do sleep 0.02; done
      "$POSSE_E2E_POSSE_BIN" holler failed 'Trigger frozen launch relaunch' > "$POSSE_TEST_ROOT/b05-first-signal.log" 2>&1
      exit 0
    fi
    while IFS= read -r line; do :; done
    ;;
  *)
    while IFS= read -r line; do :; done
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(fixture.root, "bin", "claude"), []byte(agent), 0o700); err != nil {
		t.Fatal(err)
	}
	rideCode, rideOutput := runB05CLI(t, fixture, fixture.leadEnv, "ride", "--brief", brief, "--name", "frozen-launch-contract", "--json")
	if rideCode != 0 {
		t.Fatalf("ride with inherited Lead Profile: code=%d\n%s", rideCode, rideOutput)
	}
	var rideResult b05DispatchResult
	if err := json.Unmarshal([]byte(rideOutput), &rideResult); err != nil || rideResult.Profile != "author" || rideResult.Model != "model-a" || rideResult.Effort != "high" || !rideResult.ModelKnown {
		t.Fatalf("ride did not show its resolved model identity: result=%#v err=%v output=%s", rideResult, err, rideOutput)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "b05-fail-first"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(10*time.Second, func() bool {
		task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
		return err == nil && task.State == "failed"
	}) {
		t.Fatalf("first Rider attempt did not fail; task=%#v", fixture.mustTask(t, "t1"))
	}

	if code, output := runB05CLI(t, fixture, fixture.env, "config", "set", "kinds.claude.disabled", "true"); code != 0 {
		t.Fatalf("disable Claude for new dispatch: code=%d\n%s", code, output)
	}
	disabledBrief := filepath.Join(fixture.root, "b05-disabled-brief.md")
	if err := os.WriteFile(disabledBrief, []byte("---\ntype: ship\ntitle: Disabled dispatch\ndone_when: disabled kind is refused\n---\nThis must not allocate resources.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"dispatch", "--brief", brief, "--json"},
		{"dispatch", "--brief", brief, "--profile", "author", "--json"},
		{"ride", "--brief", disabledBrief, "--name", "disabled-dispatch"},
	} {
		code, output := runB05CLI(t, fixture, fixture.leadEnv, args...)
		if code == 0 || !strings.Contains(output, "kind_disabled") {
			t.Fatalf("new dispatch silently selected disabled kind for %v: code=%d\n%s", args, code, output)
		}
	}
	tasks, err := fixture.db.Tasks(context.Background(), fixture.project.ID, true)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("disabled dispatch allocated a Task: tasks=%#v err=%v", tasks, err)
	}
	mounts, err := fixture.db.Mounts(context.Background(), fixture.project.ID)
	if err != nil || len(mounts) != 1 {
		t.Fatalf("disabled dispatch allocated a Mount: mounts=%#v err=%v", mounts, err)
	}
	if starts, err := os.ReadFile(filepath.Join(fixture.root, "b05-agent-starts")); err != nil || strings.TrimSpace(string(starts)) != "1" {
		t.Fatalf("disabled dispatch started a Rider: starts=%q err=%v", starts, err)
	}

	if code, output := runB05CLI(t, fixture, fixture.leadEnv, "relaunch", "t1"); code != 0 {
		t.Fatalf("frozen Task could not relaunch after kind disable: code=%d\n%s", code, output)
	}
	if !waitForCondition(10*time.Second, func() bool {
		starts, err := os.ReadFile(filepath.Join(fixture.root, "b05-agent-starts"))
		return err == nil && strings.TrimSpace(string(starts)) == "2"
	}) {
		t.Fatal("frozen Task was not relaunched")
	}
	task := fixture.mustTask(t, "t1")
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil || plan.Launch.ProfileName != "author" || plan.Launch.Profile.Model != "model-a" || plan.Launch.Profile.Effort != "high" {
		t.Fatalf("relaunch changed the admitted launch: plan=%#v err=%v", plan, err)
	}
	identities, err := fixture.db.TaskLaunchIdentities(context.Background(), task.ID)
	if err != nil || len(identities) != 2 || identities[0].ConfiguredModel != "model-a" || !identities[0].ModelKnown || identities[1].ConfiguredModel != "model-a" || !identities[1].ModelKnown {
		t.Fatalf("frozen launch identities = %#v err=%v, want both launches known as model-a", identities, err)
	}
	argsLog, err := os.ReadFile(filepath.Join(fixture.root, "b05-agent-args.log"))
	if err != nil || strings.Count(string(argsLog), "--model model-a") != 2 || strings.Count(string(argsLog), "--effort high") != 2 {
		t.Fatalf("launch arguments did not preserve model/effort across relaunch: %s err=%v", argsLog, err)
	}
}

type b05DispatchResult struct {
	Profile      string `json:"profile"`
	DispatchRule string `json:"dispatch_rule"`
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	ModelKnown   bool   `json:"model_known"`
}

func b05Dispatch(t *testing.T, fixture *prLifecycleFixture, brief string) b05DispatchResult {
	t.Helper()
	output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "dispatch", "--brief", brief, "--json")
	var result b05DispatchResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode dispatch result: %v\n%s", err, output)
	}
	return result
}

func runB05CLI(t *testing.T, fixture *prLifecycleFixture, env []string, args ...string) (int, string) {
	t.Helper()
	command := exec.Command(fixture.binary, args...)
	command.Dir, command.Env = fixture.repo, env
	output, err := command.CombinedOutput()
	if err == nil {
		return 0, string(output)
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), string(output)
	}
	t.Fatal(err)
	return 2, string(output)
}
