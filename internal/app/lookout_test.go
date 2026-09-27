package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLeadLookoutReportsRestartAfterUpdateSignal(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	requestStarted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestStarted <- struct{}{}
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.1.0"})
	}))
	defer server.Close()
	t.Chdir(repo)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	service := testService(home, nil)
	service.Version, service.updateURL = "0.1.0", server.URL
	var output strings.Builder
	cli := service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	done := make(chan int, 1)
	go func() { done <- cli.Run([]string{"lookout", "--json"}) }()
	select {
	case <-requestStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("Lead lookout did not enter its wait loop")
	}
	marker := lookoutUpdateStopMarker(home, os.Getpid())
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("update\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("Lead lookout exit=%d: %s", code, output.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Lead lookout did not return after SIGTERM")
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(output.String()), &result); err != nil {
		t.Fatalf("Lead lookout output: %s: %v", output.String(), err)
	}
	if result["state"] != "stopped" || result["reason"] != "update" || !strings.Contains(output.String(), "restart `posse lookout`") {
		t.Fatalf("Lead lookout did not tell its Lead to restart: %s", output.String())
	}
}

func TestPiLookoutQuietRoutineAndMixedBatches(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := root + "/repo"
	initRepo(t, repo)
	home := root + "/posse"
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "pi", AgentStatus: "idle"}}}
	cli := testService(home, fake).CLI()
	var output strings.Builder
	cli.Out = &output
	cli.ErrOut = &output
	run := func(extra ...string) map[string]any {
		t.Helper()
		output.Reset()
		args := append([]string{"lookout", "--json", "--quiet-routine", "--timeout", "1"}, extra...)
		if code := cli.Run(args); code != 0 {
			t.Fatalf("lookout failed: %s", output.String())
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(output.String()), &result); err != nil {
			t.Fatalf("lookout JSON: %s: %v", output.String(), err)
		}
		return result
	}
	create := func(kind string) int64 {
		t.Helper()
		id, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: kind, Summary: kind})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	state := func(id int64) store.Notice {
		t.Helper()
		rows, err := db.Notices(ctx, project.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.ID == id {
				return row
			}
		}
		t.Fatalf("Notice %d not found", id)
		return store.Notice{}
	}
	if _, err := config.SetFile(config.ConfigPath(home, "shop"), "lowkey.lead", "true", true); err != nil {
		t.Fatal(err)
	}
	routine := create("pr_opened")
	if result := run(); result["state"] != "timeout" || state(routine).AckedAt == 0 || state(routine).DeliveredAt == 0 {
		t.Fatalf("routine Notice woke Lead or was not acknowledged: result=%v notice=%+v", result, state(routine))
	}
	routine = create("pr_opened")
	actionable := create("needs_decision")
	unknown := create("new_unclassified_kind")
	result := run()
	if result["lowkey"] != true || !strings.Contains(result["wake"].(string), "needs-decision") || !strings.Contains(result["wake"].(string), "new-unclassified-kind") || strings.Contains(result["wake"].(string), "pr-opened") || len(result["notices"].([]any)) != 2 {
		t.Fatalf("mixed batch: %v", result)
	}
	if state(routine).AckedAt == 0 || state(actionable).AckedAt != 0 || state(unknown).AckedAt != 0 || state(actionable).DeliveredAt == 0 {
		t.Fatalf("mixed states: routine=%+v actionable=%+v unknown=%+v", state(routine), state(actionable), state(unknown))
	}
	if _, err := config.SetFile(config.ConfigPath(home, "shop"), "lowkey.lead", "false", true); err != nil {
		t.Fatal(err)
	}
	visible := create("pr_opened")
	result = run()
	if result["lowkey"] != false || len(result["notices"].([]any)) != 1 || state(visible).AckedAt != 0 {
		t.Fatalf("lowkey off did not retain visible Lead delivery: result=%v notice=%+v", result, state(visible))
	}
	result = run("--requeue", stringID(visible))
	if result["lowkey"] != false || len(result["notices"].([]any)) != 1 || state(visible).AckedAt != 0 {
		t.Fatalf("failed Pi send did not retry the same Notice: result=%v notice=%+v", result, state(visible))
	}
}

type inspectNoticeWriter func([]byte) (int, error)

func (writer inspectNoticeWriter) Write(data []byte) (int, error) {
	return writer(data)
}

func TestLookoutReturnsExistingNoticeAndMarksDeliveredAfterPrinting(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := root + "/repo"
	initRepo(t, repo)
	home := root + "/posse"
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "Worker finished"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	fake := herdr.NewFake()
	fake.Results["session.snapshot"] = json.RawMessage(`{invalid`)
	service := testService(home, fake)
	var output strings.Builder
	printedBeforeDelivery := false
	snapshotCallsAtPrint := -1
	cli := service.CLI()
	cli.Out = inspectNoticeWriter(func(data []byte) (int, error) {
		observer, openErr := store.Open(home)
		if openErr != nil {
			return 0, openErr
		}
		allNotices, queryErr := observer.Notices(ctx, project.ID, false)
		closeErr := observer.Close()
		if queryErr != nil {
			return 0, queryErr
		}
		if closeErr != nil {
			return 0, closeErr
		}
		printedBeforeDelivery = len(allNotices) == 1 && allNotices[0].DeliveredAt == 0
		snapshotCallsAtPrint = fake.CallCount("session.snapshot")
		return output.Write(data)
	})
	if code := cli.Run([]string{"lookout", "--timeout", "10000"}); code != 0 {
		t.Fatalf("lookout failed: %s", output.String())
	}
	if snapshotCallsAtPrint != 0 {
		t.Fatalf("lookout reconciled before returning the existing Notice: %d snapshot calls", snapshotCallsAtPrint)
	}
	if !printedBeforeDelivery || !strings.Contains(output.String(), "Worker finished") {
		t.Fatalf("lookout output/delivery order is wrong: printedBeforeDelivery=%v output=%s", printedBeforeDelivery, output.String())
	}
	observer, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	pending, err := observer.UndeliveredNotices(ctx, project.ID)
	if err != nil || len(pending) != 0 {
		t.Fatalf("Notice remained undelivered after printing: %#v, %v", pending, err)
	}
}
