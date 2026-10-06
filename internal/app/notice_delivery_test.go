package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLookoutExposesAmbiguousNoticeDelivery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
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
	noticeID, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "Worker finished"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	t.Chdir(repo)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{ServerStartedAt: "server-generation-1", Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "pi", AgentStatus: "idle"}}}
	service := testService(home, fake)
	code, output := runCLI(t, service, "lookout", "--json", "--quiet-routine", "--handoff", "--destination", "pi:session-1")
	if code != 0 {
		t.Fatalf("claim Notice handoff: %s", output)
	}
	var batch struct {
		Notices []struct {
			ID int64 `json:"id"`
		} `json:"notices"`
		Delivery struct {
			DeliveryID string `json:"delivery_id"`
			BatchID    string `json:"batch_id"`
			OwnerToken string `json:"owner_token"`
		} `json:"delivery"`
	}
	if err := json.Unmarshal([]byte(output), &batch); err != nil {
		t.Fatalf("decode handoff: %s: %v", output, err)
	}
	if len(batch.Notices) != 1 || batch.Notices[0].ID != noticeID || batch.Delivery.DeliveryID == "" || batch.Delivery.OwnerToken == "" {
		t.Fatalf("handoff result = %+v", batch)
	}

	code, output = runCLI(t, service, "lookout", "--json", "--receipt", batch.Delivery.DeliveryID, "--receipt-outcome", "uncertain", "--receipt-token", batch.Delivery.OwnerToken)
	if code != 0 {
		t.Fatalf("record ambiguous receipt: %s", output)
	}
	code, output = runCLI(t, service, "lookout", "--json", "--quiet-routine", "--handoff", "--destination", "pi:session-1")
	if code != 0 || !strings.Contains(output, `"state":"uncertain"`) || !strings.Contains(output, batch.Delivery.BatchID) || !strings.Contains(output, "Inspect that Lead session before retrying") {
		t.Fatalf("ambiguous receipt was not exposed: code=%d output=%s", code, output)
	}
	observer, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	notices, err := observer.UndeliveredNotices(ctx, project.ID)
	if err != nil || len(notices) != 1 || notices[0].Kind != "notice_delivery_uncertain" {
		t.Fatalf("uncertainty Notice = %#v, %v", notices, err)
	}
	code, output = runCLI(t, service, "lookout", "--json", "--receipt", batch.Delivery.DeliveryID, "--receipt-outcome", "rejected")
	if code != 0 {
		t.Fatalf("explicitly resolve uncertain receipt: %s", output)
	}
	code, output = runCLI(t, service, "lookout", "--json", "--quiet-routine", "--handoff", "--destination", "pi:session-1")
	if code != 0 {
		t.Fatalf("retry manually rejected batch: %s", output)
	}
	var retried struct {
		Notices []struct {
			ID int64 `json:"id"`
		} `json:"notices"`
		Delivery struct {
			DeliveryID string `json:"delivery_id"`
			BatchID    string `json:"batch_id"`
		} `json:"delivery"`
	}
	if err := json.Unmarshal([]byte(output), &retried); err != nil {
		t.Fatalf("decode retried batch: %s: %v", output, err)
	}
	if len(retried.Notices) != 1 || retried.Notices[0].ID != noticeID || retried.Delivery.DeliveryID != batch.Delivery.DeliveryID || retried.Delivery.BatchID != batch.Delivery.BatchID {
		t.Fatalf("manual rejection did not retry the stable exact batch: %+v", retried)
	}
}
