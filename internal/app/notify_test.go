package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestNotifyCommandReceivesOnlyUserActionableNoticePayload(t *testing.T) {
	home := t.TempDir()
	command := filepath.Join(t.TempDir(), "notify.sh")
	payloadPath := filepath.Join(t.TempDir(), "payload.json")
	script := "#!/bin/sh\ncat > " + payloadPath + "\n"
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := config.SetFile(config.ConfigPath(home, "shop"), "notify.command", command, true); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(context.Background(), "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByID(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range []store.Notice{
		{ProjectID: project.ID, Kind: "task_failed", Summary: "failure"},
		{ProjectID: project.ID, Kind: "pr_opened", Summary: "routine"},
	} {
		if _, err := db.CreateNotice(context.Background(), notice); err != nil {
			t.Fatal(err)
		}
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"}}}
	service := testService(home, fake)
	if err := service.deliverNotices(context.Background(), db, project); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(payloadPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatal("notify command did not receive a payload")
	}
	var payload []map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("invalid JSON payload %q: %v", data, err)
	}
	if len(payload) != 1 || payload[0]["kind"] != "task_failed" {
		t.Fatalf("notify payload = %s", data)
	}
}

func TestNotifyCommandFailureDoesNotBlockNoticeDelivery(t *testing.T) {
	home := t.TempDir()
	command := filepath.Join(t.TempDir(), "notify.sh")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nexit 17\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := config.SetFile(config.ConfigPath(home, "shop"), "notify.command", command, true); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(context.Background(), "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByID(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateNotice(context.Background(), store.Notice{ProjectID: project.ID, Kind: "task_failed", Summary: "failure"}); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"}}}
	service := testService(home, fake)
	started := time.Now()
	if err := service.deliverNotices(context.Background(), db, project); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("failed notify command delayed Notice delivery")
	}
	remaining, err := db.UndeliveredNotices(context.Background(), project.ID)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("Notice delivery blocked by notify command: %d, %v; Herdr calls: %#v", len(remaining), err, fake.Calls)
	}
}

func TestNotifyCommandFailureIsLogged(t *testing.T) {
	messages := make(chan string, 1)
	started := time.Now()
	runNoticeCommandWithLogger("exit 17", []store.Notice{{Kind: "task_failed", DataJSON: `{}`}}, "ask", func(format string, args ...any) {
		messages <- fmt.Sprintf(format, args...)
	})
	if time.Since(started) > time.Second {
		t.Fatal("failed notify command blocked its caller")
	}
	select {
	case message := <-messages:
		if !strings.Contains(message, "notify command failed") || !strings.Contains(message, "exit status 17") {
			t.Fatalf("notify command log = %q", message)
		}
	case <-time.After(time.Second):
		t.Fatal("notify command failure was not logged")
	}
}

func TestNotifyCommandTimeoutIsLoggedWithoutWaiting(t *testing.T) {
	messages := make(chan string, 1)
	started := time.Now()
	runNoticeCommandWithLogger("exec sleep 30", []store.Notice{{Kind: "task_failed", DataJSON: `{}`}}, "ask", func(format string, args ...any) {
		messages <- fmt.Sprintf(format, args...)
	})
	if time.Since(started) > time.Second {
		t.Fatal("slow notify command blocked its caller")
	}
	select {
	case message := <-messages:
		if !strings.Contains(message, "notify command failed") {
			t.Fatalf("notify timeout log = %q", message)
		}
	case <-time.After(noticeCommandTimeout + 2*time.Second):
		t.Fatal("notify command timeout was not logged")
	}
}

func TestNoticeNeedsUserUsesLowkeyReportingKinds(t *testing.T) {
	for _, testCase := range []struct {
		kind           string
		dataJSON       string
		mergeAutonomy  string
		wantUserAction bool
	}{
		{kind: "task_failed", wantUserAction: true},
		{kind: "pr_merged", wantUserAction: true},
		{kind: "merge_ready", mergeAutonomy: "ask", wantUserAction: true},
		{kind: "merge_ready", mergeAutonomy: "auto", wantUserAction: false},
		{kind: "pr_opened", wantUserAction: false},
		{kind: "quota_fallback", wantUserAction: false},
		{kind: "pr_watch_failing", dataJSON: `{"consecutive_failures":1}`, wantUserAction: false},
		{kind: "pr_watch_failing", dataJSON: `{"consecutive_failures":3}`, wantUserAction: true},
		{kind: "pr_watch_failing", dataJSON: `{"watch_episode":"stale"}`, wantUserAction: false},
		{kind: "root_fetch_failing", dataJSON: `{"consecutive_failures":2}`, wantUserAction: false},
		{kind: "root_fetch_failing", dataJSON: `{"consecutive_failures":3}`, wantUserAction: true},
	} {
		if got := noticeNeedsUser(store.Notice{Kind: testCase.kind, DataJSON: testCase.dataJSON}, testCase.mergeAutonomy); got != testCase.wantUserAction {
			t.Errorf("noticeNeedsUser(%q, %q) = %v, want %v", testCase.kind, testCase.mergeAutonomy, got, testCase.wantUserAction)
		}
	}
}

func TestNotifyCommandSchemaAndHelpDescribeTheKey(t *testing.T) {
	service := testService(t.TempDir(), nil)
	output := &bytes.Buffer{}
	cli := service.CLI()
	cli.Out = output
	if code := cli.Run([]string{"config", "schema", "--json"}); code != 0 {
		t.Fatalf("config schema exit=%d output=%s", code, output)
	}
	for _, want := range []string{"notify.command", "JSON User-actionable Notices", `"user_only":true`} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("config schema omitted %q: %s", want, output)
		}
	}
	if !strings.Contains(reportingRule(true), "notify.command") {
		t.Fatal("Lead help does not describe notify.command")
	}
}

func TestNotifyCommandDispatchesNewNoticesDuringHerdrThrottle(t *testing.T) {
	home := t.TempDir()
	command := filepath.Join(t.TempDir(), "notify.sh")
	payloadPath := filepath.Join(t.TempDir(), "payload.jsonl")
	script := "#!/bin/sh\ncat >> " + payloadPath + "\nprintf '\\n' >> " + payloadPath + "\n"
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := config.SetFile(config.ConfigPath(home, "shop"), "notify.command", command, true); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(context.Background(), "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByID(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := db.CreateNotice(context.Background(), store.Notice{ProjectID: project.ID, Kind: "task_failed", Summary: "first"})
	if err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{FocusedPaneID: "w1:p1", Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle", Focused: true}}}
	service := testService(home, fake)
	if err := service.deliverNotices(context.Background(), db, project); err != nil {
		t.Fatal(err)
	}
	waitForNotifyLines(t, payloadPath, 1)
	secondID, err := db.CreateNotice(context.Background(), store.Notice{ProjectID: project.ID, Kind: "task_lost", Summary: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.deliverNotices(context.Background(), db, project); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("notification.show") != 1 {
		t.Fatalf("Herdr popup count during throttle = %d, want 1", fake.CallCount("notification.show"))
	}
	if err := service.deliverNotices(context.Background(), db, project); err != nil {
		t.Fatal(err)
	}
	lines := waitForNotifyLines(t, payloadPath, 2)
	var payloads [][]noticeCommandItem
	for _, line := range lines {
		var payload []noticeCommandItem
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatalf("invalid JSON payload %q: %v", line, err)
		}
		payloads = append(payloads, payload)
	}
	if len(payloads) != 2 || len(payloads[0]) != 1 || len(payloads[1]) != 1 ||
		payloads[0][0].ID != firstID || payloads[1][0].ID != secondID {
		t.Fatalf("notification payloads = %#v, want Notice ids %d and %d exactly once", payloads, firstID, secondID)
	}
	if _, err := db.RebuildFromSnapshots(context.Background(), home); err != nil {
		t.Fatal(err)
	}
	notices, err := db.UndeliveredNotices(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimNoticeCommandNotifications(context.Background(), project.ID, notices, currentTime())
	if err != nil || len(claimed) != 0 {
		t.Fatalf("command notification state after snapshot rebuild = %#v, %v", claimed, err)
	}
}

func waitForNotifyLines(t *testing.T, path string, count int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Fields(string(data))
			if len(lines) >= count {
				return lines
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("received fewer than %d notify payloads: %q", count, data)
	return nil
}
