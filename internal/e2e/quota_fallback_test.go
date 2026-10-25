//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	posseRuntime "github.com/thanhbinh1905/posse/internal/runtime"
	"github.com/thanhbinh1905/posse/internal/store"
)

// quotaScreens is what each supported harness renders when its account can no
// longer serve requests. The Claude line is the one that stalled Workers for
// about two hours on 2026-10-25.
var quotaScreens = map[string]string{
	"claude":   "⏺ You've hit your session limit · resets 7pm\n\n────────────────────────────────────────\n> \n────────────────────────────────────────\n  ? for shortcuts\n",
	"codex":    "■ You've hit your usage limit. Upgrade to Pro (https://openai.com/chatgpt/pricing) or try again in 2 hours 13 minutes.\n\n› Implement {feature}\n\n  100% context left · ? for shortcuts\n",
	"pi":       " Error: You have hit your ChatGPT usage limit (plus plan). Try again in ~158 min.\n" + e2ePiErrorHelpLine + "\n\n~/work\n↑1.2k ↓3.4k 1.0%/272k (auto)   (openai-codex) gpt-5.5\n",
	"opencode": "  Error: You exceeded your current quota, please check your plan and billing details.\n\n  ┃  Ask anything...\n  ctrl+p commands\n",
}

func TestClaudeSessionLimitFallsBackToAnotherProfile(t *testing.T) {
	runQuotaFallback(t, "claude", "codex", "You've hit your session limit · resets 7pm", "7pm")
}

func TestCodexUsageLimitFallsBackToAnotherProfile(t *testing.T) {
	runQuotaFallback(t, "codex", "claude", "You've hit your usage limit. Upgrade to Pro (https://openai.com/chatgpt/pricing) or try again in 2 hours 13 minutes.", "in 2 hours 13 minutes")
}

func TestPiUsageLimitFallsBackToAnotherProfile(t *testing.T) {
	runQuotaFallback(t, "pi", "claude", "You have hit your ChatGPT usage limit (plus plan). Try again in ~158 min.", "in ~158 min")
}

func TestOpenCodeQuotaFallsBackToAnotherProfile(t *testing.T) {
	runQuotaFallback(t, "opencode", "claude", "You exceeded your current quota, please check your plan and billing details.", "")
}

func runQuotaFallback(t *testing.T, primaryKind, backupKind, line, resets string) {
	fixture := newQuotaFixture(t, [][2]string{
		{"profiles.primary.kind", primaryKind},
		{"profiles.primary.model", "quota-primary"},
		{"profiles.backup.kind", backupKind},
		{"profiles.backup.model", "quota-backup"},
		{"profiles.primary.fallback", "backup"},
	})
	task := fixture.start(t)
	notice := fixture.waitNotice(t, "quota_fallback")

	relaunched := fixture.base.mustTask(t, "t1")
	if relaunched.Launches != 2 || relaunched.Profile != "backup" || relaunched.State != store.StateWorking {
		t.Fatalf("fallback relaunch: launches=%d profile=%q state=%q, want 2 backup working", relaunched.Launches, relaunched.Profile, relaunched.State)
	}
	if relaunched.WorktreePath != task.WorktreePath || relaunched.SandboxID != task.SandboxID || relaunched.Branch != task.Branch {
		t.Fatalf("fallback moved the Task: sandbox %d %q branch %q -> %d %q %q", task.SandboxID, task.WorktreePath, task.Branch, relaunched.SandboxID, relaunched.WorktreePath, relaunched.Branch)
	}
	fixture.waitLog(t, "backup "+backupKind+" "+filepath.Clean(task.WorktreePath)+" found work")
	if work, err := os.ReadFile(filepath.Join(task.WorktreePath, "quota-work.txt")); err != nil || string(work) != "work before the quota ran out\n" {
		t.Fatalf("work before the quota is not in the Sandbox: %q %v", work, err)
	}

	var data map[string]any
	if err := json.Unmarshal([]byte(notice.DataJSON), &data); err != nil {
		t.Fatal(err)
	}
	wantLimit := "hard"
	if resets != "" {
		wantLimit = "resetting"
	}
	if data["line"] != line || data["resets"] != resets || data["limit"] != wantLimit || data["profile"] != "primary" || data["fallback"] != "backup" || data["agent"] != primaryKind {
		t.Fatalf("quota_fallback data = %#v, want line=%q resets=%q limit=%q", data, line, resets, wantLimit)
	}
	if !strings.Contains(notice.Summary, "relaunched with fallback Profile backup in the same Sandbox") {
		t.Fatalf("quota_fallback summary = %q", notice.Summary)
	}
	decisions, err := fixture.base.db.Decisions(context.Background(), fixture.base.project.ID, true)
	if err != nil || len(decisions) != 0 {
		t.Fatalf("a fallback must not ask the User: decisions=%#v err=%v", decisions, err)
	}
	show := runPosse(t, fixture.base.binary, fixture.base.repo, fixture.base.leadEnv, "show", "--full", "t1")
	for _, want := range []string{"quota_exhausted", "quota_fallback"} {
		if !strings.Contains(show, want) {
			t.Fatalf("posse show --full omits %s: %s", want, show)
		}
	}
	// The relaunched Worker's screen still holds the old quota message above
	// the relaunch prompt; it must not trigger a second fallback.
	time.Sleep(2 * time.Second)
	runPosse(t, fixture.base.binary, fixture.base.repo, fixture.base.leadEnv)
	if current := fixture.base.mustTask(t, "t1"); current.Launches != 2 {
		t.Fatalf("stale quota screen relaunched the fallback Worker again: launches=%d", current.Launches)
	}
}

func TestQuotaWhileHerdrReportsWorkingFallsBackOnceStalled(t *testing.T) {
	fixture := newQuotaFixture(t, [][2]string{
		{"defaults.stall_after", "1s"},
		{"profiles.primary.kind", "claude"},
		{"profiles.primary.model", "quota-silent"},
		{"profiles.backup.kind", "codex"},
		{"profiles.backup.model", "quota-backup"},
		{"profiles.primary.fallback", "backup"},
	})
	task := fixture.start(t)
	// No turn-end event arrives, and the Lead may share the exhausted account;
	// the Project Watch's periodic pass alone must notice the quota screen.
	watch := exec.Command(fixture.base.binary, "watch", "--poll-only", "--timeout", "30000")
	watch.Dir, watch.Env = fixture.base.repo, fixture.base.leadEnv
	if err := watch.Start(); err != nil {
		t.Fatalf("start Project Watch: %v", err)
	}
	defer func() {
		_ = watch.Process.Kill()
		_ = watch.Wait()
	}()
	if !waitForCondition(20*time.Second, func() bool { return fixture.hasNotice(t, "quota_fallback") }) {
		current := fixture.base.mustTask(t, "t1")
		t.Fatalf("a Worker stuck at its quota message while reported working never fell back: state=%s launches=%d", current.State, current.Launches)
	}
	fixture.waitLog(t, "backup codex "+filepath.Clean(task.WorktreePath)+" found work")
	if current := fixture.base.mustTask(t, "t1"); current.Launches != 2 || current.Profile != "backup" || current.State != store.StateWorking {
		t.Fatalf("fallback relaunch: launches=%d profile=%q state=%q", current.Launches, current.Profile, current.State)
	}
}

func TestQuotaMessageThenAgentExitFallsBack(t *testing.T) {
	fixture := newQuotaFixture(t, [][2]string{
		{"profiles.primary.kind", "codex"},
		{"profiles.primary.model", "quota-exit"},
		{"profiles.backup.kind", "claude"},
		{"profiles.backup.model", "quota-backup"},
		{"profiles.primary.fallback", "backup"},
	})
	task := fixture.start(t)
	watch := exec.Command(fixture.base.binary, "watch", "--poll-only", "--timeout", "30000")
	watch.Dir, watch.Env = fixture.base.repo, fixture.base.leadEnv
	if err := watch.Start(); err != nil {
		t.Fatalf("start Project Watch: %v", err)
	}
	defer func() {
		_ = watch.Process.Kill()
		_ = watch.Wait()
	}()
	fixture.waitNotice(t, "quota_fallback")
	fixture.waitLog(t, "backup claude "+filepath.Clean(task.WorktreePath)+" found work")
	current := fixture.base.mustTask(t, "t1")
	if current.Launches != 2 || current.Profile != "backup" || current.State != store.StateWorking || fixture.hasNotice(t, "task_lost") {
		t.Fatalf("an agent that exited at its quota was not relaunched with its fallback: launches=%d profile=%q state=%q", current.Launches, current.Profile, current.State)
	}
}

func TestQuotaExhaustionWithoutFallbackRaisesDecision(t *testing.T) {
	// The backup Profile also runs out, and it names no fallback.
	fixture := newQuotaFixture(t, [][2]string{
		{"profiles.primary.kind", "claude"},
		{"profiles.primary.model", "quota-primary"},
		{"profiles.backup.kind", "claude"},
		{"profiles.backup.model", "quota-primary"},
		{"profiles.primary.fallback", "backup"},
	})
	task := fixture.start(t)
	fixture.waitNotice(t, "quota_fallback")
	// The fallback Claude Worker starts below the first launch's quota
	// message; that stale message must not count against it.
	for range 3 {
		runPosse(t, fixture.base.binary, fixture.base.repo, fixture.base.leadEnv)
	}
	if current := fixture.base.mustTask(t, "t1"); current.Launches != 2 || fixture.hasNotice(t, "quota_exhausted") {
		t.Fatalf("stale quota screen was attributed to the fallback launch: launches=%d", current.Launches)
	}
	fixture.emitQuota(t, 2)
	notice := fixture.waitNotice(t, "quota_exhausted")
	if !strings.Contains(notice.Summary, "Profile backup hit a usage limit that resets 7pm") || !strings.Contains(notice.Summary, "it has no fallback Profile") {
		t.Fatalf("quota_exhausted summary = %q", notice.Summary)
	}

	var decision store.Decision
	if !waitForCondition(10*time.Second, func() bool {
		runPosse(t, fixture.base.binary, fixture.base.repo, fixture.base.leadEnv, "decisions")
		decisions, err := fixture.base.db.Decisions(context.Background(), fixture.base.project.ID, true)
		if err != nil || len(decisions) != 1 {
			return false
		}
		decision = decisions[0]
		return true
	}) {
		t.Fatal("no quota Decision was raised when no fallback remained")
	}
	if decision.Kind != "quota" || strings.Join(decision.Options, ",") != "relaunch,discard" || decision.TaskLaunches != 2 {
		t.Fatalf("decision = %#v, want a quota Decision for launch 2 with relaunch and discard", decision)
	}
	for _, want := range []string{"once the limit resets 7pm", "posse relaunch t1 --profile <name>"} {
		if !strings.Contains(decision.Question, want) {
			t.Fatalf("decision question %q omits %q", decision.Question, want)
		}
	}
	if current := fixture.base.mustTask(t, "t1"); current.Launches != 2 || current.Profile != "backup" {
		t.Fatalf("Task changed without a User answer: launches=%d profile=%q", current.Launches, current.Profile)
	}

	runPosse(t, fixture.base.binary, fixture.base.repo, fixture.base.leadEnv, "decide", fmt.Sprint(decision.ID), "relaunch", "--user-approved", "relaunch it after the reset")
	output := runPosse(t, fixture.base.binary, fixture.base.repo, fixture.base.leadEnv, "apply", fmt.Sprint(decision.ID))
	if !strings.Contains(output, "working") {
		t.Fatalf("apply quota Decision: %s", output)
	}
	current := fixture.base.mustTask(t, "t1")
	if current.Launches != 3 || current.Profile != "backup" || current.WorktreePath != task.WorktreePath {
		t.Fatalf("relaunch after the Decision: launches=%d profile=%q sandbox=%q, want 3 backup %q", current.Launches, current.Profile, current.WorktreePath, task.WorktreePath)
	}
}

type quotaFixture struct {
	base *prLifecycleFixture
	log  string
}

func newQuotaFixture(t *testing.T, settings [][2]string) *quotaFixture {
	t.Helper()
	base := newPRLifecycleFixture(t)
	t.Cleanup(func() { _ = base.db.Close() })
	if err := os.MkdirAll(filepath.Join(base.root, "pi", "extensions"), 0o700); err != nil {
		t.Fatal(err)
	}
	client := herdr.NewWithEnv("herdr", base.env)
	if _, err := client.Run(context.Background(), "integration", "install", "pi"); err != nil {
		t.Fatalf("install Pi integration in isolated Herdr: %v", err)
	}
	for _, setting := range append([][2]string{{"defaults.merge_mode", "local"}, {"defaults.auto_teardown", "never"}}, settings...) {
		runPosse(t, base.binary, base.repo, base.env, "config", "set", setting[0], setting[1])
	}
	for kind, screen := range quotaScreens {
		// A launch whose Profile model is quota-primary, quota-silent or
		// quota-exit does some work, then prints its harness's quota screen. Any other launch
		// records whether that work is still in its Sandbox.
		script := fmt.Sprintf(`#!/bin/sh
set -eu
if [ "${1:-}" = "--version" ]; then echo '%[1]s 1.0.0'; exit 0; fi
report() { herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent %[1]s --state "$1" >/dev/null 2>&1; }
report idle
# Codex and OpenCode take the launch prompt as an argument; others read it.
case "$*" in
  *" and follow it."*) ;;
  *) IFS= read -r prompt || exit 0 ;;
esac
report working
case " $* " in
  *" quota-primary "*|*" quota-silent "*|*" quota-exit "*)
    printf 'work before the quota ran out\n' > quota-work.txt
    printf 'primary %[1]s %%s\n' "$PWD" >> "$POSSE_TEST_ROOT/quota.log"
    attempt=$(grep -c '^primary ' "$POSSE_TEST_ROOT/quota.log")
    while [ ! -e "$POSSE_TEST_ROOT/emit-quota-$attempt" ]; do sleep 0.02; done
    printf '%%s' %[2]s
    # A quota-silent harness never reports the end of its turn, so Herdr keeps
    # showing it working, as Claude did during the 2026-10-25 stall.
    case " $* " in *" quota-silent "*) continue_silently=1 ;; *) continue_silently=0 ;; esac
    if [ "$continue_silently" = 1 ]; then while IFS= read -r line; do :; done; exit 0; fi
    # A quota-exit harness quits after its message, leaving the shell behind.
    case " $* " in *" quota-exit "*) herdr pane release-agent "$HERDR_PANE_ID" --source posse.fake --agent %[1]s >/dev/null 2>&1; exit 0 ;; esac
    report idle
    # Herdr runs plugin hooks outside the Worker's process group.
    HERDR_PLUGIN_EVENT_JSON="{\"event\":\"pane.agent_status_changed\",\"data\":{\"pane_id\":\"$HERDR_PANE_ID\",\"agent_status\":\"idle\"}}" setsid env -u POSSE_TASK_ID "$POSSE_E2E_POSSE_BIN" _ingest >> "$POSSE_TEST_ROOT/ingest.log" 2>&1 </dev/null &
    ;;
  *)
    if [ -f quota-work.txt ]; then found='found work'; else found='missing work'; fi
    printf 'backup %[1]s %%s %%s\n' "$PWD" "$found" >> "$POSSE_TEST_ROOT/quota.log"
    printf 'Continuing the Task.\n'
    ;;
esac
while IFS= read -r line; do :; done
`, kind, shellQuote(screen))
		if err := os.WriteFile(filepath.Join(base.root, "bin", kind), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return &quotaFixture{base: base, log: filepath.Join(base.root, "quota.log")}
}

func (fixture *quotaFixture) start(t *testing.T) store.Task {
	t.Helper()
	brief := filepath.Join(fixture.base.root, "quota.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Survive a quota\ndone_when: the work continues\n---\nContinue the task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.base.binary, fixture.base.repo, fixture.base.leadEnv, "start", "--brief", brief, "--name", "survive-quota", "--profile", "primary"); !strings.Contains(output, "t1") {
		t.Fatalf("start did not return t1: %s", output)
	}
	fixture.emitQuota(t, 1)
	return fixture.base.mustTask(t, "t1")
}

// emitQuota lets the quota-primary launch numbered attempt print its quota
// screen once Posse records that launch as working, as a real harness would
// only exhaust its quota mid-turn.
func (fixture *quotaFixture) emitQuota(t *testing.T, attempt int) {
	t.Helper()
	if !waitForCondition(15*time.Second, func() bool {
		contents, _ := os.ReadFile(fixture.log)
		task, err := fixture.base.db.Task(context.Background(), fixture.base.project.ID, "t1")
		return err == nil && task.State == store.StateWorking && task.Launches == attempt && strings.Count(string(contents), "primary ") >= attempt
	}) {
		contents, _ := os.ReadFile(fixture.log)
		task, _ := fixture.base.db.Task(context.Background(), fixture.base.project.ID, "t1")
		screen, _ := (posseRuntime.SystemProgress{}).ReadPane(context.Background(), herdr.NewWithEnv("herdr", fixture.base.env), task.PaneID, 60)
		t.Fatalf("quota-primary launch %d never started working: state=%s launches=%d log=%s screen=%q", attempt, task.State, task.Launches, contents, screen)
	}
	if err := os.WriteFile(filepath.Join(fixture.base.root, fmt.Sprintf("emit-quota-%d", attempt)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fixture *quotaFixture) waitLog(t *testing.T, want string) {
	t.Helper()
	if !waitForCondition(15*time.Second, func() bool {
		contents, _ := os.ReadFile(fixture.log)
		return strings.Contains(string(contents), want)
	}) {
		contents, _ := os.ReadFile(fixture.log)
		t.Fatalf("fake harness log never contained %q: %s", want, contents)
	}
}

func (fixture *quotaFixture) hasNotice(t *testing.T, kind string) bool {
	t.Helper()
	notices, err := fixture.base.db.Notices(context.Background(), fixture.base.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == kind {
			return true
		}
	}
	return false
}

func (fixture *quotaFixture) waitNotice(t *testing.T, kind string) store.Notice {
	t.Helper()
	var found store.Notice
	if !waitForCondition(20*time.Second, func() bool {
		notices, err := fixture.base.db.Notices(context.Background(), fixture.base.project.ID, false)
		if err != nil {
			return false
		}
		for _, notice := range notices {
			if notice.Kind == kind {
				found = notice
				return true
			}
		}
		return false
	}) {
		task := fixture.base.mustTask(t, "t1")
		screen, _ := (posseRuntime.SystemProgress{}).ReadPane(context.Background(), herdr.NewWithEnv("herdr", fixture.base.env), task.PaneID, 60)
		notices, _ := fixture.base.db.Notices(context.Background(), fixture.base.project.ID, false)
		ingest, _ := os.ReadFile(filepath.Join(fixture.base.root, "ingest.log"))
		t.Fatalf("no %s Notice: state=%s notices=%#v screen=%q ingest=%s", kind, task.State, notices, screen, ingest)
	}
	return found
}
