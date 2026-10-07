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
			output: "task output\nError: stream error: stream disconnected before completion: stream closed before response.completed",
			want:   "model_stream_error",
		},
		{
			name:   "Pi refusal with bounded provider details",
			agent:  "pi",
			output: "This content was flagged for possible cybersecurity risk\nprovider details",
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
			output: "Error: stream error: stream disconnected before completion: stream closed before response.completed\nThe model resumed successfully and continued working.",
		},
		{
			name:   "error is outside the terminal tail",
			agent:  "pi",
			output: "This content was flagged for possible cybersecurity risk\n" + strings.Repeat("x", 2050),
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
