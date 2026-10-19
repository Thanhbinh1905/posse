package app

import (
	"strings"
	"testing"
)

func TestClassifyModelErrorAtTurnEnd(t *testing.T) {
	for _, test := range []struct {
		name, agent, output, want string
	}{
		{
			name:   "Pi stream disconnect",
			agent:  "pi",
			output: "task output\nError: stream error: stream disconnected before completion: stream closed before response.completed\n" + piErrorHelpLine + strings.Repeat("\n", 6) + "────────────────",
			want:   "model_stream_error",
		},
		{
			name:   "Pi launch line is not a shell tool result",
			agent:  "pi",
			output: "$ pi\nRead the task brief and follow it.\nError: stream error: stream disconnected before completion: stream closed before response.completed\n" + piErrorHelpLine,
			want:   "model_stream_error",
		},
		{
			name:   "Pi refusal with bounded provider details",
			agent:  "pi",
			output: "Error: This content was flagged for possible cybersecurity risk\n" + piErrorHelpLine + "\n────────────────",
			want:   "model_refused",
		},
		{
			name:   "stream error prefix does not admit refusal markers",
			agent:  "pi",
			output: "stream error: This content was flagged for possible cybersecurity risk\n" + piErrorHelpLine,
		},
		{
			name:   "refusal after a truncated prior tool panel remains recognized",
			agent:  "pi",
			output: "truncated tool-panel output\n────────────────\nError: This content was flagged for possible cybersecurity risk\n" + piErrorHelpLine,
			want:   "model_refused",
		},
		{
			name:   "refusal on the first line of a closed tool panel is ignored",
			agent:  "pi",
			output: "agent text\n────────────────\nError: This content was flagged for possible cybersecurity risk\n" + piErrorHelpLine + "\n────────────────",
		},
		{
			name:   "unknown harness",
			agent:  "custom",
			output: "Error: stream error: stream disconnected before completion: stream closed before response.completed",
		},
		{
			name:   "old error followed by new output",
			agent:  "pi",
			output: "Error: stream error: stream disconnected before completion: stream closed before response.completed\n" + piErrorHelpLine + "\nThe model resumed successfully and continued working.",
		},
		{
			name:   "ordinary tool output quoting stream failure",
			agent:  "pi",
			output: "$ grep stream-error worker.log\n2026-10-06 log entry: Error: stream error: stream disconnected before completion: stream closed before response.completed\n" + piErrorHelpLine,
		},
		{
			name:   "successful shell result copying complete Pi error block",
			agent:  "pi",
			output: "────────────────\n$ grep -A1 'stream error' worker.log\nError: stream error: stream disconnected before completion: stream closed before response.completed\n" + piErrorHelpLine + "\n────────────────",
		},
		{
			name:   "tool result copying complete Pi error block",
			agent:  "pi",
			output: "────────────────\nRead worker.log\nError: stream error: stream disconnected before completion: stream closed before response.completed\n" + piErrorHelpLine + "\n────────────────",
		},
		{
			name:   "Pi failure after a completed tool result",
			agent:  "pi",
			output: "────────────────\n$ grep stream-error worker.log\nno match\n────────────────\nError: stream error: stream disconnected before completion: stream closed before response.completed\n" + piErrorHelpLine,
			want:   "model_stream_error",
		},
		{
			name:  "issue 228 captured tail",
			agent: "pi",
			output: ` Error: stream error: stream disconnected before completion: stream closed before response.completed
 If this looks like a pi bug, /bug sends a report to the developers.

────────────────────────────────────────────────
────────────────────────────────────────────────
~/.posse/pool/posse/sandbox-17 (posse/accept-findings-report)
↑445k ↓86k R18M $0.263 21.2%/272k (auto)                       (openai) gpt-6-luna • xhigh`,
			want: "model_stream_error",
		},
		{
			name:   "issue 228 tail begins inside an earlier tool panel",
			agent:  "pi",
			output: "truncated tool-panel output\n────────────────────────────────────────────────\nError: stream error: stream disconnected before completion: stream closed before response.completed\n" + piErrorHelpLine,
			want:   "model_stream_error",
		},
		{
			name:   "error on the first line of a closed tool panel is ignored",
			agent:  "pi",
			output: "agent text\n────────────────\nError: stream error: stream disconnected before completion: stream closed before response.completed\n" + piErrorHelpLine + "\n────────────────",
		},
		{
			name:   "Pi working-directory footer without status line",
			agent:  "pi",
			output: "Error: stream error: stream disconnected before completion: stream closed before response.completed\n" + piErrorHelpLine + "\n~/worktree",
			want:   "model_stream_error",
		},
		{
			name:   "working-directory-looking path after Pi status is not a footer",
			agent:  "pi",
			output: "Error: stream error: stream disconnected before completion: stream closed before response.completed\n" + piErrorHelpLine + "\n↑445k ↓86k R18M $0.263 21.2%/272k\n~/not-the-working-directory",
		},
		{
			name:  "issue 228 unprefixed stream error after edit tool line",
			agent: "pi",
			output: ` edit ~/.../busy_worker_steer_interrupt_test.go


 stream error: stream disconnected before completion: stream closed before response.completed
 If this looks like a pi bug, /bug sends a report to the developers.

─────

─────
~/... footer`,
			want: "model_stream_error",
		},
		{
			name:   "unprefixed stream error inside tool output is ignored",
			agent:  "pi",
			output: "─────\nedit ~/.../busy_worker_steer_interrupt_test.go\nstream error: stream disconnected before completion: stream closed before response.completed\n" + piErrorHelpLine + "\n─────",
		},
		{
			name:   "quoted refusal is not a harness error result",
			agent:  "pi",
			output: "The User asked about: Error: This content was flagged for possible cybersecurity risk\n" + piErrorHelpLine,
		},
		{
			name:   "error is outside the terminal tail",
			agent:  "pi",
			output: "Error: This content was flagged for possible cybersecurity risk\n" + piErrorHelpLine + "\n" + strings.Repeat("x", 2050),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			kind, _, found := classifyModelError(test.agent, test.output)
			if found != (test.want != "") || found && kind != test.want {
				t.Fatalf("classifyModelError(%q)=(%q,%t), want %q", test.agent, kind, found, test.want)
			}
		})
	}
}
