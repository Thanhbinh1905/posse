package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/runtime"
	"github.com/thanhbinh1905/posse/internal/store"
)

// quotaExhaustion is a harness's rendered report that its account can no
// longer serve requests. Resets is the harness's own reset text; a limit
// without one is a hard quota that only the User can restore.
type quotaExhaustion struct {
	Line   string
	Resets string
}

func (q quotaExhaustion) limit() string {
	if q.Resets != "" {
		return "resetting"
	}
	return "hard"
}

type quotaPattern struct {
	kinds  []string
	phrase *regexp.Regexp
}

// Phrases match at the start of a harness message line after its bullet and
// error prefixes are removed. Rate limits that clear within a retry are not
// quota exhaustion and are left to the harness.
var quotaPatterns = []quotaPattern{
	// Claude Code: "You've hit your session limit · resets 7pm (Asia/Bangkok)".
	{kinds: []string{"claude"}, phrase: regexp.MustCompile(`(?i)^you've hit your (?:[a-z0-9-]+ ){0,2}limit\b`)},
	{kinds: []string{"claude"}, phrase: regexp.MustCompile(`(?i)^(?:claude (?:ai )?usage|(?:5-hour|session|weekly|opus weekly|sonnet weekly|usage)) limit reached\b`)},
	// Codex: "You've hit your usage limit. ... try again in 2 hours 13 minutes."
	{kinds: []string{"codex", "opencode"}, phrase: regexp.MustCompile(`(?i)^you've hit your usage limit\b`)},
	{kinds: []string{"codex", "opencode"}, phrase: regexp.MustCompile(`(?i)^quota exceeded\. check your plan and billing details\b`)},
	// Provider messages relayed by Pi and OpenCode.
	{kinds: []string{"pi", "opencode"}, phrase: regexp.MustCompile(`(?i)^you (?:have|'ve) hit your (?:[a-z0-9-]+ ){0,2}(?:usage|session|weekly) limit\b`)},
	{kinds: []string{"claude", "codex", "pi", "opencode"}, phrase: regexp.MustCompile(`(?i)^you exceeded your current quota\b`)},
	{kinds: []string{"claude", "pi", "opencode"}, phrase: regexp.MustCompile(`(?i)^(?:your )?credit balance is too low\b`)},
	{kinds: []string{"pi", "opencode"}, phrase: regexp.MustCompile(`(?i)^insufficient_quota\b`)},
	// Pi via a proxy provider: `OpenAI API error (429): {"code":"model_cooldown","last_upstream_error":"usage_limit_reached: ...","reset_time":"450h2m10s"}`.
	{kinds: []string{"pi"}, phrase: regexp.MustCompile(`(?i)^(?:retry failed after \d+ attempts: )?openai api error \(429\):.*\busage_limit_reached\b`)},
}

var (
	quotaResetPattern = regexp.MustCompile(`(?i)(?:\bresets?|\breset at|\bwill reset at|\btry again)\s+((?:in|at)\s+)?(.+?)\.?$`)
	// Bullets and error prefixes the harnesses put before a message line.
	quotaLinePrefix   = regexp.MustCompile(`^(?:[⏺●⎿■•✗✘×│┃▌>⚠]\s*)+`)
	quotaErrorPrefix  = regexp.MustCompile(`(?i)^(?:api error:\s*|error:\s*|\[\w+\]\s*)?(?:\d{3}\s+)?`)
	quotaJSONMessage  = regexp.MustCompile(`"message"\s*:\s*"([^"]+)"`)
	quotaResetTime    = regexp.MustCompile(`"reset_time"\s*:\s*"([^"]+)"`)
	quotaEpochSuffix  = regexp.MustCompile(`\|(\d{9,})$`)
	quotaWhitespaceRE = regexp.MustCompile(`\s+`)
)

// quotaTailLines bounds how far above the end of the screen a quota message
// may sit. A harness prints its prompt and footer below it, never more output.
const quotaTailLines = 12

// classifyQuotaExhaustion recognizes a harness's quota message only as the
// last thing the Worker reported: within the final lines of the screen and
// with no later assistant output or Posse prompt. Quota text inside earlier
// tool output or a file the Worker displayed is ignored.
func classifyQuotaExhaustion(kind, output string) (quotaExhaustion, bool) {
	kind = strings.ToLower(kind)
	if kind == "pi" {
		rendered := piRenderedErrors(output)
		if len(rendered) == 0 {
			return quotaExhaustion{}, false
		}
		// Only the latest error result describes the Worker's current state.
		return matchQuotaMessage(kind, rendered[len(rendered)-1].text)
	}
	lines := strings.Split(normalizeModelTerminalOutput(output), "\n")
	var tail []string
	for i := len(lines) - 1; i >= 0 && len(tail) < quotaTailLines; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			tail = append(tail, line)
		}
	}
	// tail runs from the last line upward; stop at the first later line that
	// shows the Worker continued after the message.
	for _, line := range tail {
		if found, ok := matchQuotaMessage(kind, line); ok {
			return found, true
		}
		if quotaLaterActivity(kind, line) {
			return quotaExhaustion{}, false
		}
	}
	return quotaExhaustion{}, false
}

// quotaLaterActivity reports a line that a Worker prints only when it keeps
// working: a new assistant message, or a Posse launch or relaunch prompt.
func quotaLaterActivity(kind, line string) bool {
	if strings.Contains(line, "and follow it.") {
		return true
	}
	switch kind {
	case "claude":
		return strings.HasPrefix(line, "⏺") || strings.HasPrefix(line, "●")
	case "codex":
		return strings.HasPrefix(line, "•")
	}
	return false
}

func matchQuotaMessage(kind, line string) (quotaExhaustion, bool) {
	text := strings.ReplaceAll(strings.TrimSpace(line), "’", "'")
	text = strings.TrimSpace(quotaLinePrefix.ReplaceAllString(text, ""))
	text = strings.TrimSpace(quotaErrorPrefix.ReplaceAllString(text, ""))
	if match := quotaJSONMessage.FindStringSubmatch(text); match != nil {
		text = match[1]
	}
	if len(text) > 2048 {
		return quotaExhaustion{}, false
	}
	for _, pattern := range quotaPatterns {
		if !oneOfString(kind, pattern.kinds...) || !pattern.phrase.MatchString(text) {
			continue
		}
		text = quotaWhitespaceRE.ReplaceAllString(text, " ")
		found := quotaExhaustion{Line: text}
		if epoch := quotaEpochSuffix.FindStringSubmatch(text); epoch != nil {
			found.Resets = "at unix time " + epoch[1]
		} else if reset := quotaResetTime.FindStringSubmatch(text); reset != nil {
			found.Resets = reset[1]
		} else if reset := quotaResetPattern.FindStringSubmatch(text); reset != nil {
			found.Resets = strings.TrimSpace(reset[1] + reset[2])
		}
		return found, true
	}
	return quotaExhaustion{}, false
}

// quotaSettle is how long a Worker that Herdr still reports working must show
// an unchanged screen before its quota message counts. A harness that keeps
// working repaints its spinner well within this window.
const quotaSettle = 30 * time.Second

// handleQuotaExhaustion relaunches each Worker whose harness reports an
// exhausted quota with its Profile's fallback in the same Sandbox, or asks the
// User when no fallback remains.
func (s *Service) handleQuotaExhaustion(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, snapshot herdr.Snapshot) ([]store.Notice, error) {
	tasks, err := db.LiveTasks(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	var notices []store.Notice
	var failures []error
	for _, task := range tasks {
		notice, err := s.handleTaskQuotaExhaustion(ctx, db, project, cfg, task, snapshot)
		if err != nil {
			failures = append(failures, fmt.Errorf("quota check for %s: %w", taskIDString(task.Seq), err))
		}
		if notice != nil {
			notices = append(notices, *notice)
		}
	}
	return notices, errors.Join(failures...)
}

func (s *Service) handleTaskQuotaExhaustion(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, snapshot herdr.Snapshot) (*store.Notice, error) {
	if task.Launches < 1 || !store.QuotaDecisionState(task.State) || task.State == store.StateNeedsDecision {
		return nil, nil
	}
	pane, found := findTaskPane(snapshot.Panes, task)
	if !found || pane.PaneID == "" {
		return nil, nil
	}
	agentWorking := pane.Agent != "" && pane.AgentStatus == "working" && task.State != store.StateStalled
	if agentWorking && (task.LastProgressAt == 0 || time.Since(time.UnixMilli(task.LastProgressAt)) < quotaSettle) {
		return nil, nil
	}
	kind := taskKind(cfg, task)
	if !quotaKindSupported(kind) {
		return nil, nil
	}
	if _, err := db.TaskQuotaExhaustion(ctx, task.ID, task.Launches); err == nil {
		return nil, nil
	} else if !store.IsNotFound(err) {
		return nil, err
	}
	// A relaunch or teardown in flight owns the pane; its old screen may still
	// show the previous launch's message.
	if _, err := db.IntentByTask(ctx, task.ID); err == nil {
		return nil, nil
	} else if !store.IsNotFound(err) {
		return nil, err
	}
	output, err := (runtime.SystemProgress{}).ReadScreen(ctx, s.Herdr, pane.PaneID)
	if err != nil {
		return nil, nil
	}
	if agentWorking {
		sum := sha256.Sum256([]byte(output))
		if hex.EncodeToString(sum[:]) != task.LastOutputHash {
			return nil, nil
		}
	}
	quota, ok := classifyQuotaExhaustion(kind, output)
	if !ok {
		return nil, nil
	}
	record := store.QuotaExhaustion{TaskID: task.ID, Launch: task.Launches, Agent: kind, Profile: task.Profile, Line: quota.Line, Resets: quota.Resets}
	claimed, err := db.ClaimQuotaExhaustion(ctx, record, os.Getpid(), processAlive)
	if err != nil || !claimed {
		return nil, err
	}
	home, err := s.homePath()
	if err != nil {
		return nil, err
	}
	fallback := ""
	if profile, ok := cfg.Profiles[task.Profile]; ok {
		fallback = profile.Fallback
	}
	data := map[string]any{"launch": task.Launches, "agent": kind, "profile": task.Profile, "limit": quota.limit(), "line": quota.Line, "resets": quota.Resets, "fallback": fallback}
	reached := quotaReachedText(task.Profile, quota)
	// The outcome is recorded even when the relaunch consumed or canceled ctx.
	finish := func(status, detail string, notice store.Notice) (*store.Notice, error) {
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return db.FinishQuotaExhaustion(finishCtx, task.ID, task.Launches, os.Getpid(), status, fallback, detail, notice)
	}
	if fallback != "" {
		relaunched, relaunchErr := s.relaunchTaskAttempt(ctx, db, home, project, cfg, task, fallback, true)
		if relaunchErr == nil {
			data["worker"] = relaunched.Worker
			detail := fmt.Sprintf("%s; relaunched with fallback Profile %s in the same Sandbox", reached, fallback)
			notice := store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "quota_fallback", Summary: task.Title + ": " + detail, DataJSON: marshalJSON(data)}
			return finish("fallback", detail, notice)
		}
		data["relaunch_error"] = relaunchErr.Error()
		reached += fmt.Sprintf("; relaunch with fallback Profile %s failed: %v", fallback, relaunchErr)
	} else {
		reached += "; it has no fallback Profile"
	}
	notice := store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "quota_exhausted", Summary: task.Title + ": " + reached, DataJSON: marshalJSON(data)}
	return finish("decision", reached, notice)
}

func quotaKindSupported(kind string) bool {
	for _, pattern := range quotaPatterns {
		if oneOfString(kind, pattern.kinds...) {
			return true
		}
	}
	return false
}

func quotaReachedText(profile string, quota quotaExhaustion) string {
	if quota.Resets != "" {
		return fmt.Sprintf("Profile %s hit a usage limit that resets %s (%q)", profile, quota.Resets, quota.Line)
	}
	return fmt.Sprintf("Profile %s ran out of quota (%q)", profile, quota.Line)
}
