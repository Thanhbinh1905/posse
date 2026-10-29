package app

import (
	"strings"
	"testing"
)

const claudePromptFooter = "\n\n────────────────────────────────────────\n> \n────────────────────────────────────────\n  ? for shortcuts\n"

func TestClassifyQuotaExhaustionRecognizesEachHarness(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		kind   string
		output string
		line   string
		resets string
	}{
		{
			name:   "claude session limit from the 2026-10-25 stall",
			kind:   "claude",
			output: "⏺ Running the shard self-check.\n\n⏺ You've hit your session limit · resets 7pm" + claudePromptFooter,
			line:   "You've hit your session limit · resets 7pm",
			resets: "7pm",
		},
		{
			name:   "claude session limit with time zone",
			kind:   "claude",
			output: "  ⎿  You've hit your session limit · resets 1:20pm (Asia/Bangkok)\n     /upgrade to increase your usage limit." + claudePromptFooter,
			line:   "You've hit your session limit · resets 1:20pm (Asia/Bangkok)",
			resets: "1:20pm (Asia/Bangkok)",
		},
		{
			name:   "claude weekly limit",
			kind:   "claude",
			output: "⏺ You’ve hit your weekly limit · resets Oct 9, 6pm (Asia/Bangkok)" + claudePromptFooter,
			line:   "You've hit your weekly limit · resets Oct 9, 6pm (Asia/Bangkok)",
			resets: "Oct 9, 6pm (Asia/Bangkok)",
		},
		{
			name:   "claude limit options menu",
			kind:   "claude",
			output: "⏺ You've hit your session limit · resets 7pm (Asia/Bangkok)\n\n  What do you want to do?\n\n  ❯ 1. Stop and wait for limit to reset\n    2. Upgrade your plan\n\n  Enter to confirm · Esc to cancel\n",
			line:   "You've hit your session limit · resets 7pm (Asia/Bangkok)",
			resets: "7pm (Asia/Bangkok)",
		},
		{
			name:   "claude auto-continue usage limit screen from #574",
			kind:   "claude",
			output: "  ⎿  You've hit your session limit · resets 1:30pm (Asia/Bangkok)\n● Usage limit reached · continuing automatically at 1:30pm · esc to cancel\n\n  ⚠ Usage limit reached · limit resets 1:30pm\n    Continuing shortly · esc to cancel\n" + claudePromptFooter,
			line:   "Usage limit reached · limit resets 1:30pm",
			resets: "1:30pm",
		},
		{
			name:   "claude legacy epoch",
			kind:   "claude",
			output: "⏺ Claude AI usage limit reached|1792731600" + claudePromptFooter,
			line:   "Claude AI usage limit reached|1792731600",
			resets: "at unix time 1792731600",
		},
		{
			name:   "claude credit balance",
			kind:   "claude",
			output: `  ⎿  API Error: 400 {"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."}}` + claudePromptFooter,
			line:   "Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits.",
		},
		{
			name:   "codex usage limit",
			kind:   "codex",
			output: "• Ran go test ./...\n\n■ You've hit your usage limit. Upgrade to Pro (https://openai.com/chatgpt/pricing) or try again in 2 hours 13 minutes.\n\n› Implement {feature}\n\n  100% context left · ? for shortcuts\n",
			line:   "You've hit your usage limit. Upgrade to Pro (https://openai.com/chatgpt/pricing) or try again in 2 hours 13 minutes.",
			resets: "in 2 hours 13 minutes",
		},
		{
			name:   "codex hard quota",
			kind:   "codex",
			output: "■ Quota exceeded. Check your plan and billing details.\n\n› Implement {feature}\n",
			line:   "Quota exceeded. Check your plan and billing details.",
		},
		{
			name:   "pi codex subscription limit",
			kind:   "pi",
			output: "Working on it\n\n Error: You have hit your ChatGPT usage limit (plus plan). Try again in ~158 min.\n" + quotaPiHelp + "\n\n~/work/posse\n↑445k ↓86k R18M $0.263 21.2%/272k (auto)   (openai-codex) gpt-5.5\n",
			line:   "You have hit your ChatGPT usage limit (plus plan). Try again in ~158 min.",
			resets: "in ~158 min",
		},
		{
			name:   "pi openai insufficient quota",
			kind:   "pi",
			output: " Error: 429 You exceeded your current quota, please check your plan and billing details.\n" + quotaPiHelp + "\n",
			line:   "You exceeded your current quota, please check your plan and billing details.",
		},
		{
			name:   "pi model cooldown usage limit",
			kind:   "pi",
			output: " Error: OpenAI API error (429): " + piModelCooldownBody + "\n" + quotaPiHelp + "\n",
			line:   "OpenAI API error (429): " + piModelCooldownBody,
			resets: "450h2m10s",
		},
		{
			name:   "pi model cooldown after retries",
			kind:   "pi",
			output: " Error: Retry failed after 3 attempts: OpenAI API error (429): " + piModelCooldownBody + "\n" + quotaPiHelp + "\n",
			line:   "Retry failed after 3 attempts: OpenAI API error (429): " + piModelCooldownBody,
			resets: "450h2m10s",
		},
		{
			name:   "pi all credentials cooling down",
			kind:   "pi",
			output: " Error: All credentials for model gpt-5.6-luna are cooling down via provider codex (last error: usage_limit_reached: The usage limit has been reached)\n" + quotaPiHelp + "\n",
			line:   "All credentials for model gpt-5.6-luna are cooling down via provider codex (last error: usage_limit_reached: The usage limit has been reached)",
		},
		{
			name:   "opencode provider quota",
			kind:   "opencode",
			output: "  Build  gpt-5.5\n\n  Error: You exceeded your current quota, please check your plan and billing details.\n\n  ┃  Ask anything...\n  ctrl+p commands\n",
			line:   "You exceeded your current quota, please check your plan and billing details.",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			found, ok := classifyQuotaExhaustion(testCase.kind, testCase.output)
			if !ok {
				t.Fatalf("quota message not recognized in %q", testCase.output)
			}
			if found.Line != testCase.line || found.Resets != testCase.resets {
				t.Fatalf("quota = %#v, want line=%q resets=%q", found, testCase.line, testCase.resets)
			}
			wantLimit := "hard"
			if testCase.resets != "" {
				wantLimit = "resetting"
			}
			if found.limit() != wantLimit {
				t.Fatalf("limit = %q, want %q", found.limit(), wantLimit)
			}
		})
	}
}

const quotaPiHelp = piErrorHelpLine

const piModelCooldownBody = `{"code":"model_cooldown","last_upstream_error":"usage_limit_reached: The usage limit has been reached","message":"All credentials for model gpt-5.6-luna are cooling down via provider codex (last error: usage_limit_reached: The usage limit has been reached)","model":"gpt-5.6-luna","provider":"codex","reset_seconds":1620130,"reset_time":"450h2m10s"}`

func TestClassifyQuotaExhaustionIgnoresQuotaTextTheWorkerMovedPast(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		kind   string
		output string
	}{
		{name: "claude answered after a fixture", kind: "claude", output: "  ⎿  You've hit your session limit · resets 7pm\n\n⏺ The fixture prints the session-limit text." + claudePromptFooter},
		{name: "claude relaunched after the limit", kind: "claude", output: "⏺ You've hit your session limit · resets 7pm\n\n> Read /home/u/.posse/projects/p/tasks/t1/relaunch.md and follow it." + claudePromptFooter},
		{name: "claude quota far above the end", kind: "claude", output: "⏺ You've hit your session limit · resets 7pm\n" + strings.Repeat("     test output line\n", quotaTailLines) + claudePromptFooter},
		{name: "claude quota text mid-line", kind: "claude", output: "     want \"You've hit your session limit · resets 7pm\"" + claudePromptFooter},
		{name: "codex continued", kind: "codex", output: "■ You've hit your usage limit. Try again in 5 minutes.\n\n• Ran go test ./...\n\n› Implement {feature}\n"},
		{name: "codex rate limit is not quota", kind: "codex", output: "■ stream error: exceeded retry limit, last status: 429 Too Many Requests\n\n› Implement {feature}\n"},
		{name: "claude phrase in another harness", kind: "codex", output: "■ Your credit balance is too low\n"},
		{name: "pi quota in tool output", kind: "pi", output: "────────────────────\n$ cat fixture\nError: You have hit your ChatGPT usage limit (plus plan).\n" + quotaPiHelp + "\n────────────────────\n\n Done.\n"},
		{name: "pi stream error", kind: "pi", output: " Error: stream error: stream disconnected before completion: stream closed before response.completed\n" + quotaPiHelp + "\n"},
		{name: "unknown kind", kind: "custom", output: "You've hit your session limit · resets 7pm\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if found, ok := classifyQuotaExhaustion(testCase.kind, testCase.output); ok {
				t.Fatalf("quota = %#v recognized in %q", found, testCase.output)
			}
		})
	}
}
