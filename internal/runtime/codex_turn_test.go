package runtime

import (
	"strings"
	"testing"
	"time"
)

func TestParseCodexInterruptFailureUsesLatestRootTurn(t *testing.T) {
	const (
		start   = `{"timestamp":"2026-11-26T18:25:21.438Z","type":"event_msg","payload":{"type":"task_started","turn_id":"one","root_turn_id":"one"}}` + "\n"
		failure = `{"timestamp":"2026-11-26T18:25:38.541Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"one","root_turn_id":"one","error":{"message":"{\"type\":\"error\",\"status\":400,\"error\":{\"message\":\"unsupported websocket request type: response.interrupt\",\"type\":\"invalid_request_error\"}}","codex_error_info":"other"}}}` + "\n"
		next    = `{"timestamp":"2026-11-26T18:28:49.753Z","type":"event_msg","payload":{"type":"task_started","turn_id":"two","root_turn_id":"two"}}` + "\n"
		success = `{"timestamp":"2026-11-26T18:29:49.260Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"two","root_turn_id":"two","last_agent_message":"Done"}}` + "\n"
	)
	for _, test := range []struct {
		name, rollout, want string
	}{
		{"terminal failure", start + failure, "one"},
		{"newer turn started", start + failure + next, ""},
		{"newer turn succeeded", start + failure + next + success, ""},
		{"user or tool text", start + strings.Replace(failure, `"event_msg"`, `"response_item"`, 1), ""},
		{"child turn", start + strings.Replace(failure, `"root_turn_id":"one"`, `"root_turn_id":"parent"`, 1), ""},
		{"unbound completion", failure, ""},
		{"transient error", start + strings.Replace(failure, `"task_complete"`, `"stream_error"`, 1), ""},
		{"other provider status", start + strings.Replace(failure, "400", "500", 1), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			turn := parseCodexInterruptFailure(strings.NewReader(test.rollout))
			if turn.id != test.want {
				t.Fatalf("turn = %+v, want %q", turn, test.want)
			}
			if test.want != "" && (!turn.started.Equal(time.Date(2026, 11, 26, 18, 25, 21, 438000000, time.UTC)) || !turn.completed.Equal(time.Date(2026, 11, 26, 18, 25, 38, 541000000, time.UTC))) {
				t.Fatalf("failure turn lost its start or completion timestamp: %+v", turn)
			}
		})
	}
}
