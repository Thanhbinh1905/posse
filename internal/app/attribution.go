package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// claudeNoAttributionSettings turns off the commit trailer and PR footer Claude
// Code adds by default. includeCoAuthoredBy covers releases before attribution.
const claudeNoAttributionSettings = `{"attribution":{"commit":"","pr":""},"includeCoAuthoredBy":false}`

// prependAttributionArgs puts the settings first so a Profile's own
// --settings, which Claude Code lets replace earlier ones, still wins.
func prependAttributionArgs(kind string, profile config.Profile, args []string) []string {
	if kind != "claude" || profile.Attribution {
		return args
	}
	return append([]string{"--settings", claudeNoAttributionSettings}, args...)
}

var attributionTrailerKeys = []string{"co-authored-by", "assisted-by", "generated-by"}

// Agent identities are matched on the trailer's name or email domain, never on
// arbitrary text, so human co-authors are unaffected.
var (
	attributionAgentNames   = []string{"claude", "codex", "copilot", "gemini", "opencode", "cursor", "aider", "devin", "jules"}
	attributionAgentDomains = []string{"anthropic.com", "openai.com", "aider.chat", "cursor.com", "cursor.sh", "devin.ai"}
)

func agentAttributionTrailer(line string) bool {
	key, value, ok := strings.Cut(line, ":")
	if !ok || !oneOfString(strings.ToLower(strings.TrimSpace(key)), attributionTrailerKeys...) {
		return false
	}
	value = strings.ToLower(strings.TrimSpace(value))
	name, email, _ := strings.Cut(value, "<")
	if fields := strings.FieldsFunc(name, func(r rune) bool { return r == ' ' || r == '(' || r == '-' }); len(fields) != 0 && oneOfString(fields[0], attributionAgentNames...) {
		return true
	}
	email = strings.TrimSuffix(strings.TrimSpace(email), ">")
	_, domain, found := strings.Cut(email, "@")
	if !found {
		return false
	}
	for _, agent := range attributionAgentDomains {
		if domain == agent || strings.HasSuffix(domain, "."+agent) {
			return true
		}
	}
	return false
}

// refuseAgentAttribution checks every commit a push would add above the Task's
// bases. It never rewrites history; the Worker amends its own commits.
func refuseAgentAttribution(ctx context.Context, worktree, head string, task store.Task, bases ...string) error {
	if plan, err := task.AdmittedPlan(); err == nil && plan != nil && plan.Launch.Profile.Attribution {
		return nil
	}
	bases = append(bases, task.BaseRef)
	if task.TargetBranch != "" {
		bases = append(bases, "refs/remotes/origin/"+task.TargetBranch)
	}
	args := []string{"log", "--format=%H%x00%(trailers:only,unfold)%x1e", head, "--not"}
	resolved := 0
	for _, base := range bases {
		if base == "" {
			continue
		}
		if _, err := gitOutput(ctx, worktree, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err == nil {
			args = append(args, base)
			resolved++
		}
	}
	if resolved == 0 {
		return axi.Failure("attribution_check_failed", "could not resolve the Task base to check commits for agent attribution trailers", false, strings.Join(bases, ", "))
	}
	output, err := gitOutputRaw(ctx, worktree, args...)
	if err != nil {
		return axi.Failure("attribution_check_failed", "could not read Task commit trailers before push", true, err.Error())
	}
	var found []string
	for _, record := range strings.Split(output, "\x1e") {
		sha, trailers, ok := strings.Cut(strings.TrimLeft(record, "\n"), "\x00")
		if !ok {
			continue
		}
		for _, line := range strings.Split(trailers, "\n") {
			if agentAttributionTrailer(line) {
				found = append(found, fmt.Sprintf("%s %s", shortSHA(sha), strings.TrimSpace(line)))
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	help := append([]string{"Reword each listed commit without the trailer, then retry", "Set `attribution = true` on the Task's Profile to allow agent attribution"}, found...)
	return axi.Failure("agent_attribution_refused", fmt.Sprintf("refusing to push: %d Task commit trailer(s) attribute an agent", len(found)), false, help...)
}
