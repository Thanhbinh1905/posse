package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestCodexIdleFailureAcceptsContainedRolloutPath(t *testing.T) {
	for _, test := range []struct {
		name    string
		escapes bool
		want    string
	}{
		{"absolute rollout", false, "codex_interrupt_rejected"},
		{"symlink escapes sessions", true, "worker_idle"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			db, project, task := createWorkingTask(t)
			defer db.Close()
			codexHome := t.TempDir()
			t.Setenv("CODEX_HOME", codexHome)
			sessions := filepath.Join(codexHome, "sessions")
			if err := os.Mkdir(sessions, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(sessions, "rollout.jsonl")
			writePath := path
			if test.escapes {
				outside := t.TempDir()
				writePath = filepath.Join(outside, "rollout.jsonl")
				linkedDir := filepath.Join(sessions, "linked")
				if err := os.Symlink(outside, linkedDir); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(linkedDir, "rollout.jsonl")
			}
			started := time.Now().Add(-time.Second)
			id, err := db.QueueMessage(ctx, task.ID, "Steer", false)
			if err != nil {
				t.Fatal(err)
			}
			if claimed, err := db.ClaimMessage(ctx, id, "fixture", started.UnixMilli(), 0); err != nil || !claimed {
				t.Fatalf("claim message: claimed=%t, err=%v", claimed, err)
			}
			if err := db.MarkMessageSubmitting(ctx, id, "fixture"); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if err := db.MarkClaimedMessageDelivered(ctx, id, "fixture", now.UnixMilli()); err != nil {
				t.Fatal(err)
			}
			rollout := fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"task_started","turn_id":"one"}}
{"timestamp":%q,"type":"event_msg","payload":{"type":"task_complete","turn_id":"one","error":{"message":"{\"type\":\"error\",\"status\":400,\"error\":{\"message\":\"unsupported websocket request type: response.interrupt\"}}"}}}
`, started.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano))
			if err := os.WriteFile(writePath, []byte(rollout), 0600); err != nil {
				t.Fatal(err)
			}
			reference, _ := json.Marshal(map[string]string{"session_path": path})
			snapshot := herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, Label: task.PaneLabel, WorkspaceID: task.HerdrWorkspaceID, Agent: "codex", AgentStatus: "idle", AgentSession: reference}}}
			if _, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, now, time.Minute); err != nil {
				t.Fatal(err)
			}
			notices, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, now.Add(time.Minute), time.Minute)
			if err != nil || len(notices) != 1 || notices[0].Kind != test.want {
				t.Fatalf("notices = %+v, err = %v; want %s", notices, err, test.want)
			}
		})
	}
}

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
