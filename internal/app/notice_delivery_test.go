package app

import (
	"context"
	"encoding/json"
	"os"
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

func TestLookoutReplacementSessionSurfacesPrintedUncertainty(t *testing.T) {
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
	noticeID, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "old session delivery"})
	if err != nil {
		t.Fatal(err)
	}
	delivery, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{noticeID}, "pi:old-session", "server-generation-1", "old-owner", 1000, 100)
	if err != nil || !claimed {
		t.Fatalf("old-session claim = %#v, %v, %v", delivery, claimed, err)
	}
	if err := db.MarkNoticeDeliveryPrinted(ctx, delivery.DeliveryID, "old-owner", 1010); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE notice_delivery_receipts SET lease_until=0 WHERE delivery_id=?`, delivery.DeliveryID); err != nil {
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
	fake.SnapshotValue = herdr.Snapshot{ServerStartedAt: "server-generation-2", Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "pi", AgentStatus: "idle"}}}
	service := testService(home, fake)
	code, output := runCLI(t, service, "lookout", "--json", "--quiet-routine", "--handoff", "--destination", "pi:new-session")
	if code != 0 || !strings.Contains(output, `"state":"uncertain"`) || !strings.Contains(output, delivery.BatchID) || !strings.Contains(output, "pi:old-session") || !strings.Contains(output, strings.Join(int64Strings([]int64{noticeID}), ",")) {
		t.Fatalf("replacement session did not expose old printed batch uncertainty: code=%d output=%s", code, output)
	}
	observer, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	current, err := observer.NoticeDelivery(ctx, delivery.DeliveryID)
	if err != nil || current.State != "uncertain" || current.BatchID != delivery.BatchID {
		t.Fatalf("old receipt after replacement = %#v, %v", current, err)
	}
	notices, err := observer.NoticesByIDs(ctx, project.ID, []int64{noticeID})
	if err != nil || notices[0].DeliveredAt != 0 {
		t.Fatalf("replacement marked unconfirmed Notice delivered: %#v, %v", notices, err)
	}
}

func TestLookoutReceiptResolutionCannotCrossProjects(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	shop, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, shop.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	foreignRoot := filepath.Join(root, "foreign")
	if err := os.Mkdir(foreignRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign, err := db.CreateProject(ctx, "foreign", foreignRoot, "main")
	if err != nil {
		t.Fatal(err)
	}
	foreignNoticeID, err := db.CreateNotice(ctx, store.Notice{ProjectID: foreign.ID, Kind: "task_done", Summary: "foreign receipt"})
	if err != nil {
		t.Fatal(err)
	}
	delivery, claimed, err := db.ClaimNoticeDelivery(ctx, foreign.ID, []int64{foreignNoticeID}, "pi:foreign-session", "server-generation-1", "foreign-owner", 1000, 1000)
	if err != nil || !claimed {
		t.Fatalf("foreign receipt claim = %#v, %v, %v", delivery, claimed, err)
	}
	if err := db.MarkNoticeDeliveryPrinted(ctx, delivery.DeliveryID, "foreign-owner", 1010); err != nil {
		t.Fatal(err)
	}
	if err := db.ResolveNoticeDelivery(ctx, foreign.ID, delivery.DeliveryID, "foreign-owner", "uncertain", 1020); err != nil {
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
	code, output := runCLI(t, service, "lookout", "--json", "--receipt", delivery.DeliveryID, "--receipt-outcome", "accepted")
	if code == 0 {
		t.Fatalf("tokenless cross-Project resolution succeeded: %s", output)
	}
	code, output = runCLI(t, service, "lookout", "--json", "--receipt", delivery.DeliveryID, "--receipt-outcome", "accepted", "--receipt-token", "foreign-owner")
	if code == 0 {
		t.Fatalf("token-backed cross-Project resolution succeeded: %s", output)
	}
	observer, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	current, err := observer.NoticeDelivery(ctx, delivery.DeliveryID)
	if err != nil || current.State != "uncertain" {
		t.Fatalf("foreign receipt after unauthorized attempts = %#v, %v", current, err)
	}
	notices, err := observer.NoticesByIDs(ctx, foreign.ID, []int64{foreignNoticeID})
	if err != nil || notices[0].DeliveredAt != 0 {
		t.Fatalf("foreign Notice changed after unauthorized attempts: %#v, %v", notices, err)
	}
}
