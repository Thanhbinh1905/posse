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
			output: "$ grep stream-error rider.log\n2026-10-06 log entry: Error: stream error: stream disconnected before completion: stream closed before response.completed\n" + piErrorHelpLine,
		},
		{
			name:   "successful shell result copying complete Pi error block",
			agent:  "pi",
			output: "────────────────\n$ grep -A1 'stream error' rider.log\nError: stream error: stream disconnected before completion: stream closed before response.completed\n" + piErrorHelpLine + "\n────────────────",
		},
		{
			name:   "tool result copying complete Pi error block",
			agent:  "pi",
			output: "────────────────\nRead rider.log\nError: stream error: stream disconnected before completion: stream closed before response.completed\n" + piErrorHelpLine + "\n────────────────",
		},
		{
			name:   "Pi failure after a completed tool result",
			agent:  "pi",
			output: "────────────────\n$ grep stream-error rider.log\nno match\n────────────────\nError: stream error: stream disconnected before completion: stream closed before response.completed\n" + piErrorHelpLine,
			want:   "model_stream_error",
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
