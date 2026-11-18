//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// claudeAutoContinueScreen is the usage-limit screen Claude Code renders when it
// plans to continue by itself, captured in #574.
const claudeAutoContinueScreen = "  ⎿  You've hit your session limit · resets 1:30pm (Asia/Bangkok)\n● Usage limit reached · continuing automatically at 1:30pm · esc to cancel\n\n  ⚠ Usage limit reached · limit resets 1:30pm\n    Continuing shortly · esc to cancel\n\n────────────────────────────────────────\n> \n────────────────────────────────────────\n  ? for shortcuts\n"

func TestClaudeSessionLimitFallsBackToAnotherProfile(t *testing.T) {
	runQuotaFallback(t, "claude", "quota-primary", "codex", "You've hit your session limit · resets 7pm", "7pm")
}

func TestClaudeAutoContinueScreenFallsBackToAnotherProfile(t *testing.T) {
	runQuotaFallback(t, "claude", "quota-autocontinue", "codex", "Usage limit reached · limit resets 1:30pm", "1:30pm")
}

func TestCodexUsageLimitFallsBackToAnotherProfile(t *testing.T) {
	runQuotaFallback(t, "codex", "quota-primary", "claude", "You've hit your usage limit. Upgrade to Pro (https://openai.com/chatgpt/pricing) or try again in 2 hours 13 minutes.", "in 2 hours 13 minutes")
}

func TestPiUsageLimitFallsBackToAnotherProfile(t *testing.T) {
	runQuotaFallback(t, "pi", "quota-primary", "claude", "You have hit your ChatGPT usage limit (plus plan). Try again in ~158 min.", "in ~158 min")
}

func TestRealPiModelCooldownFallsBackThroughWatch(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("real Pi executable is not installed")
	}
	base := newPRLifecycleFixture(t)
	t.Cleanup(func() { _ = base.db.Close() })
	piHome := filepath.Join(base.root, "pi")
	if err := os.MkdirAll(filepath.Join(piHome, "extensions"), 0700); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"model_cooldown","last_upstream_error":"usage_limit_reached: The usage limit has been reached","message":"All credentials for model gpt-6-luna are cooling down via provider codex (last error: usage_limit_reached: The usage limit has been reached)","model":"gpt-6-luna","provider":"codex","reset_seconds":6003,"reset_time":"1h40m3s"}`))
	}))
	t.Cleanup(api.Close)
	models := fmt.Sprintf(`{"providers":{"openai":{"baseUrl":%q,"api":"openai-responses","apiKey":"dummy-local-only","models":[{"id":"gpt-6-luna","contextWindow":200000,"maxTokens":1024}]}}}`, api.URL)
	if err := os.WriteFile(filepath.Join(piHome, "models.json"), []byte(models), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(piHome, "settings.json"), []byte(`{"retry":{"enabled":true,"maxRetries":3},"showImages":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	client := herdr.NewWithEnv("herdr", base.env)
	if _, err := client.Run(context.Background(), "integration", "install", "pi"); err != nil {
		t.Fatal(err)
	}
	for _, setting := range [][2]string{
		{"defaults.stall_after", "1s"},
		{"profiles.primary.kind", "pi"},
		{"profiles.primary.model", "gpt-6-luna"},
		{"profiles.primary.args", `["--provider", "openai", "--no-skills", "--no-prompt-templates"]`},
		{"profiles.backup.kind", "claude"},
		{"profiles.backup.model", "quota-backup"},
		{"profiles.primary.fallback", "backup"},
	} {
		runPosse(t, base.binary, base.repo, base.env, "config", "set", setting[0], setting[1])
	}
	brief := filepath.Join(base.root, "real-pi-quota.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Real Pi quota\ndone_when: fallback launches\n---\nContinue the task.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, base.binary, base.repo, base.leadEnv, "start", "--brief", brief, "--name", "real-pi-quota", "--profile", "primary"); !strings.Contains(output, "t1") {
		t.Fatalf("start did not return t1: %s", output)
	}
	task := base.mustTask(t, "t1")
	watch := exec.Command(base.binary, "watch", "--poll-only", "--timeout", "120000")
	watch.Dir, watch.Env = base.repo, base.leadEnv
	if err := watch.Start(); err != nil {
		t.Fatalf("start Project Watch: %v", err)
	}
	defer func() {
		if watch.Process.Pid > 1 {
			_ = watch.Process.Kill()
		}
		_ = watch.Wait()
	}()
	if !waitForCondition(30*time.Second, func() bool { return requests.Load() >= 1 }) {
		t.Fatal("real Pi did not call the local 429 endpoint")
	}
	var screen string
	if !waitForCondition(30*time.Second, func() bool {
		screen, _ = (posseRuntime.SystemProgress{}).ReadScreen(context.Background(), client, task.PaneID)
		return strings.Contains(screen, "usage_limit_reached") && strings.Contains(screen, "Retry failed after 3 attempts:")
	}) {
		t.Fatalf("real Pi did not render the quota error and retry line: requests=%d screen=%q", requests.Load(), screen)
	}
	t.Logf("real Pi quota pane after %d provider requests:\n%s", requests.Load(), screen)
	notice := waitForQuotaNotice(t, base, "quota_fallback", 30*time.Second, screen, client, task)
	if !strings.Contains(notice.Summary, "relaunched with fallback Profile backup") {
		t.Fatalf("quota_fallback notice = %#v", notice)
	}
	current := base.mustTask(t, "t1")
	if current.Launches != 2 || current.Profile != "backup" || current.State != store.StateWorking {
		t.Fatalf("fallback relaunch: launches=%d profile=%q state=%q", current.Launches, current.Profile, current.State)
	}

	runPosse(t, base.binary, base.repo, base.env, "config", "unset", "profiles.primary.fallback")
	if output := runPosse(t, base.binary, base.repo, base.leadEnv, "start", "--brief", brief, "--name", "real-pi-no-fallback", "--profile", "primary"); !strings.Contains(output, "t2") {
		t.Fatalf("start without fallback did not return t2: %s", output)
	}
	withoutFallback := base.mustTask(t, "t2")
	if !waitForCondition(30*time.Second, func() bool { return requests.Load() >= 5 }) {
		t.Fatal("second real Pi did not call the local 429 endpoint")
	}
	noFallbackNotice := waitForQuotaNotice(t, base, "quota_exhausted", 30*time.Second, "", client, withoutFallback)
	var data map[string]any
	if err := json.Unmarshal([]byte(noFallbackNotice.DataJSON), &data); err != nil {
		t.Fatal(err)
	}
	line, _ := data["line"].(string)
	if data["resets"] != "1h40m3s" || data["fallback"] != "" || !strings.Contains(noFallbackNotice.Summary, "1h40m3s") || !strings.Contains(line, "Retry failed after 3 attempts:") {
		t.Fatalf("quota_exhausted notice = %#v data=%#v", noFallbackNotice, data)
	}
	for _, notice := range noticesForTask(t, base, withoutFallback.ID) {
		if notice.Kind == "worker_idle" {
			t.Fatalf("real Pi quota raised worker_idle instead of quota_exhausted: %#v", notice)
		}
	}
}

func noticesForTask(t *testing.T, base *prLifecycleFixture, taskID int64) []store.Notice {
	t.Helper()
	notices, err := base.db.Notices(context.Background(), base.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var selected []store.Notice
	for _, notice := range notices {
		if notice.TaskID == taskID {
			selected = append(selected, notice)
		}
	}
	return selected
}

func waitForQuotaNotice(t *testing.T, base *prLifecycleFixture, kind string, timeout time.Duration, screen string, client *herdr.Client, task store.Task) store.Notice {
	t.Helper()
	var found store.Notice
	if waitForCondition(timeout, func() bool {
		notices, err := base.db.Notices(context.Background(), base.project.ID, false)
		if err != nil {
			return false
		}
		for _, notice := range notices {
			if notice.Kind == kind {
				found = notice
				return true
			}
		}
		screen, _ = (posseRuntime.SystemProgress{}).ReadScreen(context.Background(), client, task.PaneID)
		return false
	}) {
		return found
	}
	t.Fatalf("no %s Notice: state=%s launches=%d screen=%q", kind, task.State, task.Launches, screen)
	return store.Notice{}
}

func TestClaudeQuotaFallbackPreparesSandboxBeforeRelaunch(t *testing.T) {
	fixture := newQuotaFixture(t, [][2]string{
		{"profiles.primary.kind", "claude"},
		{"profiles.primary.model", "quota-primary"},
		{"profiles.backup.kind", "claude"},
		{"profiles.backup.model", "quota-dialog"},
		{"profiles.primary.fallback", "backup"},
	})
	task := fixture.startWithoutEmittingQuota(t)
	if err := os.Remove(filepath.Join(fixture.base.root, "claude", ".claude.json")); err != nil {
		t.Fatal(err)
	}
	fixture.emitQuota(t, 1)
	fixture.waitNotice(t, "quota_fallback")
	fixture.waitLog(t, "backup claude "+filepath.Clean(task.WorktreePath)+" found work")
	current := fixture.base.mustTask(t, "t1")
	if current.Launches != 2 || current.Profile != "backup" || current.State != store.StateWorking || fixture.hasNotice(t, "quota_exhausted") {
		t.Fatalf("fallback did not prepare the Sandbox before launch: launches=%d profile=%q state=%q", current.Launches, current.Profile, current.State)
	}
}

func TestOpenCodeQuotaFallsBackToAnotherProfile(t *testing.T) {
	runQuotaFallback(t, "opencode", "quota-primary", "claude", "You exceeded your current quota, please check your plan and billing details.", "")
}

func runQuotaFallback(t *testing.T, primaryKind, primaryModel, backupKind, line, resets string) {
	fixture := newQuotaFixture(t, [][2]string{
		{"profiles.primary.kind", primaryKind},
		{"profiles.primary.model", primaryModel},
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
	if !waitForCondition(60*time.Second, func() bool { return fixture.hasNotice(t, "quota_fallback") }) {
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
case "$*" in
  *" quota-dialog "*)
    if ! grep -Fq "$PWD" "$POSSE_TEST_ROOT/claude/.claude.json" 2>/dev/null; then
      printf 'Trust this folder?\n'
      report blocked
      while IFS= read -r line; do :; done
      exit 0
    fi
    ;;
esac
report idle
# Codex and OpenCode take the launch prompt as an argument; others read it.
case "$*" in
  *" and follow it."*) ;;
  *) IFS= read -r prompt || exit 0 ;;
esac
report working
case " $* " in
  *" quota-primary "*|*" quota-silent "*|*" quota-exit "*|*" quota-autocontinue "*|*" quota-background "*)
    printf 'work before the quota ran out\n' > quota-work.txt
    printf 'primary %[1]s %%s\n' "$PWD" >> "$POSSE_TEST_ROOT/quota.log"
    attempt=$(grep -c '^primary ' "$POSSE_TEST_ROOT/quota.log")
    while [ ! -e "$POSSE_TEST_ROOT/emit-quota-$attempt" ]; do sleep 0.02; done
    case " $* " in
      *" quota-background "*)
        printf 'Background command completed\nYou\047ve hit your weekly limit · resets Nov 7, 4am (Asia/Bangkok)\n'
        report blocked
        while IFS= read -r line; do :; done
        exit 0
        ;;
      *" quota-autocontinue "*) printf '%%s' %[3]s ;;
      *) printf '%%s' %[2]s ;;
    esac
    # A quota-silent harness never reports the end of its turn, so Herdr keeps
    # showing it working, as Claude did during the 2026-10-25 stall.
    case " $* " in *" quota-silent "*) continue_silently=1 ;; *) continue_silently=0 ;; esac
    if [ "$continue_silently" = 1 ]; then while IFS= read -r line; do :; done; exit 0; fi
    # A quota-exit harness quits after its message once Herdr has detected it,
    # as a real agent would, leaving the shell behind.
    case " $* " in *" quota-exit "*) i=0; while herdr agent get "$HERDR_PANE_ID" 2>/dev/null | grep -q '"launch_pending":true' && [ $i -lt 1000 ]; do sleep 0.02; i=$((i+1)); done; herdr pane release-agent "$HERDR_PANE_ID" --source posse.fake --agent %[1]s >/dev/null 2>&1; exit 0 ;; esac
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
`, kind, shellQuote(screen), shellQuote(claudeAutoContinueScreen))
		if err := os.WriteFile(filepath.Join(base.root, "bin", kind), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return &quotaFixture{base: base, log: filepath.Join(base.root, "quota.log")}
}

func (fixture *quotaFixture) start(t *testing.T) store.Task {
	t.Helper()
	task := fixture.startWithoutEmittingQuota(t)
	fixture.emitQuota(t, 1)
	return task
}

func (fixture *quotaFixture) startWithoutEmittingQuota(t *testing.T) store.Task {
	t.Helper()
	brief := filepath.Join(fixture.base.root, "quota.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Survive a quota\ndone_when: the work continues\n---\nContinue the task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.base.binary, fixture.base.repo, fixture.base.leadEnv, "start", "--brief", brief, "--name", "survive-quota", "--profile", "primary"); !strings.Contains(output, "t1") {
		t.Fatalf("start did not return t1: %s", output)
	}
	return fixture.base.mustTask(t, "t1")
}

// emitQuota lets the quota-primary launch numbered attempt print its quota
// screen once Posse records that launch as working, as a real harness would
// only exhaust its quota mid-turn.
func (fixture *quotaFixture) emitQuota(t *testing.T, attempt int) {
	t.Helper()
	if !waitForCondition(45*time.Second, func() bool {
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
	if !waitForCondition(45*time.Second, func() bool {
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
	if !waitForCondition(60*time.Second, func() bool {
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
		intents, _ := fixture.base.db.Intents(context.Background(), fixture.base.project.ID)
		signals, _ := fixture.base.db.TaskSignals(context.Background(), task.ID, 30)
		t.Fatalf("no %s Notice: state=%s launches=%d intents=%#v signals=%#v notices=%#v screen=%q ingest=%s", kind, task.State, task.Launches, intents, signals, notices, screen, ingest)
	}
	return found
}
