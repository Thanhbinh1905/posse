package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestClaudeStopPluginIsLeadOnlyAndBounded(t *testing.T) {
	home := t.TempDir()
	project := store.Project{Name: "shop", Root: t.TempDir()}
	service := testService(home, nil)
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	launch, err := service.prepareLeadLaunch(home, project, cfg, "claude")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "projects", "shop", "lead-claude-lookout")
	if !strings.Contains(strings.Join(launch.Args, " "), "--plugin-dir "+root) {
		t.Fatalf("Claude Lead missing Stop plugin: %#v", launch.Args)
	}
	contents, err := os.ReadFile(filepath.Join(root, "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plugin struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command     string `json:"command"`
				AsyncRewake bool   `json:"asyncRewake"`
				Timeout     int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(contents, &plugin); err != nil {
		t.Fatal(err)
	}
	hook := plugin.Hooks["Stop"][0].Hooks[0]
	if !hook.AsyncRewake || time.Duration(hook.Timeout)*time.Second <= claudeStopWait+5*time.Second || !strings.Contains(hook.Command, " _claude-stop") || !strings.Contains(hook.Command, shellQuote(home)) || !strings.Contains(hook.Command, shellQuote(project.Root)) {
		t.Fatalf("unbounded or unowned Stop hook: %s", contents)
	}
	cfg.Kinds["claude"] = config.Kind{NoticeDelivery: config.NoticeDeliveryPrompt}
	launch, err = service.prepareLeadLaunch(home, project, cfg, "claude")
	if err != nil || strings.Contains(strings.Join(launch.Args, " "), root) {
		t.Fatalf("non-Lookout Claude got Stop hook: %#v %v", launch, err)
	}
}

func TestClaudeTranscriptReceiptRequiresUserText(t *testing.T) {
	for _, tc := range []struct {
		name, entry string
		want        bool
	}{
		{"reminder", `{"type":"user","message":{"content":[{"type":"text","text":"<system-reminder>Posse batch receipt: receipt-1\nnotice</system-reminder>"}]}}`, true},
		{"string", `{"type":"user","message":{"content":"Posse batch receipt: receipt-1\nnotice"}}`, true},
		{"assistant quote", `{"type":"assistant","message":{"content":[{"type":"text","text":"Posse batch receipt: receipt-1\nnotice"}]}}`, false},
		{"tool quote", `{"type":"user","message":{"content":[{"type":"tool_result","text":"Posse batch receipt: receipt-1\nnotice"}]}}`, false},
		{"prefix", `{"type":"user","message":{"content":"Posse batch receipt: receipt-10\nnotice"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "transcript.jsonl")
			if err := os.WriteFile(path, []byte(tc.entry+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := claudeTranscriptHasReceipt(context.Background(), path, claudeReceiptMarker("receipt-1"))
			if err != nil || got != tc.want {
				t.Fatalf("receipt = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestClaudeReceiptRestartPreservesExactBatchAndAcknowledgment(t *testing.T) {
	for _, recorded := range []bool{true, false} {
		t.Run(map[bool]string{true: "accepted after lease", false: "missing held"}[recorded], func(t *testing.T) {
			db, project, _ := newLookoutTestProject(t)
			defer db.Close()
			ctx := context.Background()
			id, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "pending", DataJSON: `{}`})
			if err != nil {
				t.Fatal(err)
			}
			at := currentTime() - time.Minute.Milliseconds()
			delivery, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{id}, "claude:session", "generation", "owner", at, 1000)
			if err != nil || !claimed {
				t.Fatalf("claim: %v %v", claimed, err)
			}
			if err := db.MarkNoticeDeliveryPrinted(ctx, delivery.DeliveryID, "owner", at+1); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `UPDATE notices SET acked_at=? WHERE id=?`, at+2, id); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "transcript.jsonl")
			if recorded {
				entry, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": claudeReceiptMarker(delivery.DeliveryID) + "\npending"}})
				if err := os.WriteFile(path, append(entry, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := reconcileClaudeReceipt(ctx, db, project.ID, "claude:session", path); err != nil {
				t.Fatal(err)
			}
			got, err := db.NoticeDelivery(ctx, delivery.DeliveryID)
			want := map[bool]string{true: "accepted", false: "uncertain"}[recorded]
			if err != nil || got.State != want || len(got.NoticeIDs) != 1 || got.NoticeIDs[0] != id || got.BatchID != delivery.BatchID {
				t.Fatalf("restart receipt: %#v %v", got, err)
			}
			var acked int64
			if err := db.QueryRowContext(ctx, `SELECT acked_at FROM notices WHERE id=?`, id).Scan(&acked); err != nil || acked != at+2 {
				t.Fatalf("ack lost: %d %v", acked, err)
			}
		})
	}
}

func TestClaudeStopLeavesDownProjectStopped(t *testing.T) {
	db, project, home := newLookoutTestProject(t)
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), `UPDATE projects SET status='down' WHERE id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	initRepo(t, project.Root)
	t.Chdir(project.Root)
	service := testService(home, nil)
	service.herdrContext = func() bool { return false }
	var output bytes.Buffer
	input := `{"hook_event_name":"Stop","session_id":"session","transcript_path":"unused"}`
	if code := service.RunClaudeStop(strings.NewReader(input), &output, nil); code != 0 || output.Len() != 0 {
		t.Fatalf("down Project rearmed its Stop hook: %d %s", code, output.String())
	}
	rows := lookoutRows([]lookoutProcess{{PID: 1, Kind: "lead", StopHook: true}}, true)
	if !strings.Contains(rows[0][len(rows[0])-1].Value.(string), "next Claude Stop hook") {
		t.Fatalf("update did not identify the hook restart owner: %#v", rows)
	}
}

func TestClaudeStopIgnoresSubagentsAndRejectsUnboundedWait(t *testing.T) {
	service := testService(t.TempDir(), nil)
	var output bytes.Buffer
	if code := service.RunClaudeStop(strings.NewReader(`{"hook_event_name":"Stop","agent_id":"child"}`), &output, nil); code != 0 || output.Len() != 0 {
		t.Fatalf("subagent owned a wait: %d %s", code, output.String())
	}
	for _, timeout := range []string{"0", "-1", "240001", "9223372036854775807"} {
		output.Reset()
		if code := service.RunClaudeStop(strings.NewReader(`{}`), &output, []string{"--timeout", timeout}); code != 2 || !strings.Contains(output.String(), "timeout must be between") {
			t.Fatalf("timeout %s: %d %s", timeout, code, output.String())
		}
	}
}
