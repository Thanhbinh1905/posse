//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLeadMessageUsesTargetNoticeReceiver(t *testing.T) {
	f := newWorkerTabsFixtureWithManualWatchOwner(t)
	receiverRepo := filepath.Join(f.root, "receiver")
	initRepository(t, receiverRepo, filepath.Join(f.root, "receiver-origin.git"), f.env)
	receiver := createLabeledWorkspace(t, f.client, receiverRepo, "receiver")
	receiverEnv := f.userPaneEnv(receiver)
	runPosse(t, f.binary, receiverRepo, receiverEnv, "up", "--name", "receiver", "--yes")
	if _, err := f.client.Call(context.Background(), "pane.focus", map[string]any{"pane_id": receiver.RootPane.PaneID}); err != nil {
		t.Fatal(err)
	}

	message := "The fix is ready.\n" + strings.Repeat("Read the linked issue. ", 8) + "Last line."
	wantSummary := "Message from the shop Lead: " + message
	var sent struct {
		NoticeID int64  `json:"notice_id"`
		State    string `json:"state"`
	}
	output := runPosse(t, f.binary, f.repo, f.leadEnv, "lead", "send", "receiver", message, "--json")
	if err := json.Unmarshal([]byte(output), &sent); err != nil || sent.NoticeID == 0 || sent.State != "queued" {
		t.Fatalf("send result = %s, err = %v", output, err)
	}

	var batch struct {
		Project string `json:"project"`
		Notices []struct {
			ID      int64  `json:"id"`
			Kind    string `json:"kind"`
			Summary string `json:"summary"`
		} `json:"notices"`
		Wake     string `json:"wake"`
		Delivery struct {
			ID    string `json:"delivery_id"`
			Token string `json:"owner_token"`
		} `json:"delivery"`
	}
	output = runPosse(t, f.binary, receiverRepo, receiverEnv, "watch", "--json", "--handoff", "--destination", "fake:receiver", "--timeout", "2000")
	if err := json.Unmarshal([]byte(output), &batch); err != nil || batch.Project != "receiver" || batch.Delivery.ID == "" || batch.Delivery.Token == "" {
		t.Fatalf("receiver batch = %s, err = %v", output, err)
	}
	found := false
	for _, notice := range batch.Notices {
		if notice.ID == sent.NoticeID {
			found = true
			if notice.Kind != "lead_message" || notice.Summary != wantSummary {
				t.Fatalf("received Notice = %+v, want attributed message %q", notice, wantSummary)
			}
		}
	}
	if !found {
		t.Fatalf("Notice %d missing from %s", sent.NoticeID, output)
	}
	_, encoded, _ := strings.Cut(batch.Wake, "\nbody: ")
	var wakeBody string
	if err := json.Unmarshal([]byte(encoded), &wakeBody); err != nil || !strings.Contains(wakeBody, strconv.Quote(wantSummary)) {
		t.Fatalf("wake did not include the full attributed message: %q (%v)", batch.Wake, err)
	}

	accept := []string{"watch", "--receipt", batch.Delivery.ID, "--receipt-token", batch.Delivery.Token, "--receipt-outcome", "accepted"}
	runPosseExpectingFailure(t, f, f.repo, f.leadEnv, "no rows", accept...)
	runPosse(t, f.binary, receiverRepo, receiverEnv, accept...)
	runPosse(t, f.binary, receiverRepo, receiverEnv, "ack", strconv.FormatInt(sent.NoticeID, 10))

	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	receipt, err := db.NoticeDelivery(context.Background(), batch.Delivery.ID)
	if err != nil || receipt.State != "accepted" || receipt.ProjectID == f.projectID {
		t.Fatalf("stored Notice receipt = %+v, err = %v", receipt, err)
	}
}

func TestLeadMessageRequiresLeadAndQueuesWithoutReceiver(t *testing.T) {
	f := newWorkerTabsFixtureWithManualWatchOwner(t)
	if err := os.WriteFile(filepath.Join(f.root, "quiet-agent"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	worker := f.start(t, "t1", "Worker cannot send Lead messages", "worker-denied")
	offline := filepath.Join(f.root, "offline")
	initRepository(t, offline, filepath.Join(f.root, "offline-origin.git"), f.env)
	runPosse(t, f.binary, offline, f.env, "project", "add", "--name", "offline")

	runPosseExpectingFailure(t, f, worker.WorktreePath, workerSignalEnv(f, worker), "worker_forbidden", "lead", "send", "offline", "Unapproved Worker message")
	runPosseExpectingFailure(t, f, f.repo, f.env, "lead_only", "lead", "send", "offline", "Unbound sender")
	runPosseExpectingFailure(t, f, offline, f.leadEnv, "lead_only", "lead", "send", "shop", "Wrong sender Project")
	runPosseExpectingFailure(t, f, f.repo, f.leadEnv, "project_unknown", "lead", "send", "unknown", "No such Project")
	runPosseExpectingFailure(t, f, f.repo, f.leadEnv, "requires another Project", "lead", "send", "shop", "No self-send")

	output := runPosse(t, f.binary, f.repo, f.leadEnv, "lead", "send", "offline", "Keep this pending")
	if !strings.Contains(output, "state: queued") {
		t.Fatalf("send to an offline Lead: %s", output)
	}
	ctx := context.Background()
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.ProjectByName(ctx, "offline")
	if err != nil || project.LeadPaneID != "" {
		t.Fatalf("send should not start the target Lead: %+v, err = %v", project, err)
	}
	notices, err := db.UndeliveredNotices(ctx, project.ID)
	if err != nil || len(notices) != 1 || notices[0].Kind != "lead_message" || notices[0].Summary != "Message from the shop Lead: Keep this pending" {
		t.Fatalf("pending target Notices = %+v, err = %v", notices, err)
	}
	sourceNotices, err := db.Notices(ctx, f.projectID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range sourceNotices {
		if notice.Kind == "lead_message" {
			t.Fatalf("send wrote a Lead message to the source Project: %+v", notice)
		}
	}
}
