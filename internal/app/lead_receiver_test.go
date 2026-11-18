package app

import (
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

func stubLeadReceiver(t *testing.T, live bool, grace time.Duration) {
	t.Helper()
	previousLive, previousGrace := leadReceiverLive, leadReceiverGrace
	leadReceiverLive = func(string, store.Project) (bool, error) { return live, nil }
	leadReceiverGrace = grace
	t.Cleanup(func() { leadReceiverLive, leadReceiverGrace = previousLive, previousGrace })
}

func (f codexLeadFixture) prompts() []string {
	var texts []string
	for _, call := range f.fake.Calls {
		if call.Method == "agent.prompt" {
			text, _ := call.Params["text"].(string)
			texts = append(texts, text)
		}
	}
	return texts
}

func (f codexLeadFixture) receiverMissingNotices(t *testing.T) []store.Notice {
	t.Helper()
	notices, err := f.db.Notices(context.Background(), f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	var missing []store.Notice
	for _, notice := range notices {
		if notice.Kind == "lead_receiver_missing" {
			missing = append(missing, notice)
		}
	}
	return missing
}

var idlePiLead = herdr.Pane{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "pi", AgentStatus: "idle"}

func TestPiLeadWithoutReceiverGetsTypedNoticesAndOneRecoveryNotice(t *testing.T) {
	stubLeadReceiver(t, false, 0)
	fixture := newCodexLeadFixture(t, idlePiLead, "", "0")
	ctx := context.Background()
	if err := fixture.service.deliverNotices(ctx, fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	prompts := fixture.prompts()
	if len(prompts) != 1 || !strings.Contains(prompts[0], "done") || !strings.Contains(prompts[0], "its Notice receiver is missing") || !strings.Contains(prompts[0], "Run `posse setup`, then `/reload` in the shop Lead pane") {
		t.Fatalf("a Pi Lead without a receiver did not get the Notice and the recovery step typed: %q", prompts)
	}
	if fixture.undelivered(t) != 0 {
		t.Fatalf("typed Notices stayed undelivered: %d", fixture.undelivered(t))
	}
	if _, err := fixture.db.CreateNotice(ctx, store.Notice{ProjectID: fixture.project.ID, Kind: "task_done", Summary: "second"}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.deliverNotices(ctx, fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	if prompts := fixture.prompts(); len(prompts) != 2 || !strings.Contains(prompts[1], "second") {
		t.Fatalf("a later Notice was not typed: %q", prompts)
	}
	if missing := fixture.receiverMissingNotices(t); len(missing) != 1 {
		t.Fatalf("receiver-missing Notices = %d, want one until it is acknowledged", len(missing))
	}
}

func TestClaudeLeadWithoutBackgroundWatchGetsReceiverMissingNoticeWhileBusy(t *testing.T) {
	stubLeadReceiver(t, false, 0)
	lead := idlePiLead
	lead.Agent = "claude"
	lead.AgentStatus = "working"
	fixture := newCodexLeadFixture(t, lead, "", "0")
	payloadPath := filepath.Join(t.TempDir(), "notify.json")
	command := filepath.Join(t.TempDir(), "notify.sh")
	if err := os.WriteFile(command, []byte("#!/bin/sh\ncat > "+payloadPath+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := config.SetFile(config.ConfigPath(fixture.home, "shop"), "notify.command", command, true); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.deliverNotices(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	if missing := fixture.receiverMissingNotices(t); len(missing) != 1 {
		t.Fatalf("receiver-missing Notices = %d, want one for a Claude Lead with no background watch", len(missing))
	}
	if fixture.fake.CallCount("notification.show") != 1 {
		t.Fatalf("out-of-band notification count = %d, want one", fixture.fake.CallCount("notification.show"))
	}
	waitForNotifyLines(t, payloadPath, 1)
	payload, err := os.ReadFile(payloadPath)
	if err != nil || !strings.Contains(string(payload), "lead_receiver_missing") {
		t.Fatalf("notify.command payload = %q, err=%v; want lead_receiver_missing", payload, err)
	}
}

func TestDeliveredHandoffDoesNotCountAsMissingReceiver(t *testing.T) {
	stubLeadReceiver(t, false, time.Second)
	fixture := newCodexLeadFixture(t, idlePiLead, "", "0")
	ctx := context.Background()
	fixture.fake.SnapshotValue.ServerStartedAt = "server-generation-1"
	t.Chdir(filepath.Dir(fixture.home))
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")

	code, output := runCLI(t, fixture.service, "watch", "--json", "--quiet-routine", "--handoff", "--destination", "pi:session-1")
	if code != 0 || !strings.Contains(output, `"state":"printed"`) {
		t.Fatalf("one-shot handoff did not deliver the actionable Notice: code=%d output=%s", code, output)
	}
	var batch struct {
		Delivery struct {
			DeliveryID string `json:"delivery_id"`
			OwnerToken string `json:"owner_token"`
		} `json:"delivery"`
	}
	if err := json.Unmarshal([]byte(output), &batch); err != nil {
		t.Fatalf("decode handoff: %s: %v", output, err)
	}
	code, output = runCLI(t, fixture.service, "watch", "--json", "--receipt", batch.Delivery.DeliveryID, "--receipt-outcome", "accepted", "--receipt-token", batch.Delivery.OwnerToken)
	if code != 0 {
		t.Fatalf("accept handoff receipt: %s", output)
	}
	notices, err := fixture.db.Notices(ctx, fixture.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || notices[0].DeliveredAt == 0 || notices[0].AckedAt != 0 {
		t.Fatalf("handoff Notice state = %#v, want delivered and unacknowledged", notices)
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE notices SET created_at=? WHERE id=?`, currentTime()-leadReceiverGrace.Milliseconds()-1, notices[0].ID); err != nil {
		t.Fatal(err)
	}
	if missing := fixture.receiverMissingNotices(t); len(missing) != 0 {
		t.Fatalf("one-shot handoff already raised lead_receiver_missing: %#v", missing)
	}
	if err := fixture.service.monitorLeadReceiver(ctx, fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	if missing := fixture.receiverMissingNotices(t); len(missing) != 0 {
		t.Fatalf("delivered handoff Notice raised lead_receiver_missing: %#v", missing)
	}
	pendingID, err := fixture.db.CreateNotice(ctx, store.Notice{ProjectID: fixture.project.ID, Kind: "task_done", Summary: "still undelivered", CreatedAt: currentTime() - leadReceiverGrace.Milliseconds() - 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.monitorLeadReceiver(ctx, fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	missing := fixture.receiverMissingNotices(t)
	if len(missing) != 1 || missing[0].ID == pendingID {
		t.Fatalf("old undelivered Notice did not raise lead_receiver_missing: pending=%d missing=%#v", pendingID, missing)
	}
}

func TestReceiverRecoveryClosesTheMissingEpisode(t *testing.T) {
	stubLeadReceiver(t, true, 0)
	fixture := newCodexLeadFixture(t, idlePiLead, "", "0")
	ctx := context.Background()
	if err := raiseLeadReceiverMissing(ctx, fixture.db, fixture.project, "pi"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.monitorLeadReceiver(ctx, fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var missing, recovered *store.Notice
	for i := range notices {
		if notices[i].Kind == "lead_receiver_missing" {
			missing = &notices[i]
		}
		if notices[i].Kind == "lead_receiver_recovered" {
			recovered = &notices[i]
		}
	}
	if missing == nil || missing.AckedAt == 0 || recovered == nil || recovered.AckedAt == 0 {
		t.Fatalf("receiver episode was not closed: missing=%#v recovered=%#v", missing, recovered)
	}
}

func TestDelayedActionableNoticeIgnoresAcknowledgedUndeliveredNotice(t *testing.T) {
	fixture := newCodexLeadFixture(t, idlePiLead, "", "0")
	ctx := context.Background()
	old := currentTime() - delayedNoticeAlertAfter.Milliseconds() - 1
	if _, err := fixture.db.ExecContext(ctx, `UPDATE notices SET created_at=? WHERE project_id=? AND kind='task_done'`, old, fixture.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.CreateNotice(ctx, store.Notice{ProjectID: fixture.project.ID, Kind: "task_done", Summary: "still pending", CreatedAt: old}); err != nil {
		t.Fatal(err)
	}
	notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var acknowledgedID, pendingID int64
	for _, notice := range notices {
		if notice.Kind != "task_done" {
			continue
		}
		if notice.Summary == "still pending" {
			pendingID = notice.ID
		} else {
			acknowledgedID = notice.ID
		}
	}
	if acknowledgedID == 0 || pendingID == 0 {
		t.Fatalf("task_done Notices = %#v, want acknowledged and pending Notices", notices)
	}
	if _, err := fixture.db.AckNotices(ctx, fixture.project.ID, []string{fmt.Sprint(acknowledgedID)}); err != nil {
		t.Fatal(err)
	}
	if err := raiseDelayedNoticeAlert(ctx, fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	open, err := fixture.db.Notices(ctx, fixture.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	var alerts []store.Notice
	for _, notice := range open {
		if notice.Kind != "notice_delivery_delayed" {
			continue
		}
		alerts = append(alerts, notice)
		if !strings.Contains(notice.Summary, fmt.Sprint(pendingID)) || strings.Contains(notice.Summary, fmt.Sprint(acknowledgedID)) {
			t.Fatalf("delayed alert includes wrong Notices: %q", notice.Summary)
		}
	}
	if len(alerts) != 1 {
		t.Fatalf("delayed alert count = %d, want one", len(alerts))
	}
}

func TestDelayedActionableNoticeRaisesAndClearsOneAlert(t *testing.T) {
	fixture := newCodexLeadFixture(t, idlePiLead, "", "0")
	ctx := context.Background()
	if _, err := fixture.db.ExecContext(ctx, `UPDATE notices SET created_at=? WHERE project_id=? AND kind='task_done'`, currentTime()-delayedNoticeAlertAfter.Milliseconds()-1, fixture.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := raiseDelayedNoticeAlert(ctx, fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	if err := raiseDelayedNoticeAlert(ctx, fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	open, err := fixture.db.Notices(ctx, fixture.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	var alerts []store.Notice
	for _, notice := range open {
		if notice.Kind == "notice_delivery_delayed" {
			alerts = append(alerts, notice)
		}
	}
	if len(alerts) != 1 || !noticeNeedsUser(alerts[0], "ask") {
		t.Fatalf("delayed Notice alerts = %#v, want one User-actionable alert", alerts)
	}
	if err := fixture.db.MarkNoticesDelivered(ctx, fixture.project.ID, []int64{1}, currentTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.AckNotices(ctx, fixture.project.ID, []string{"1"}); err != nil {
		t.Fatal(err)
	}
	if err := raiseDelayedNoticeAlert(ctx, fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	open, err = fixture.db.Notices(ctx, fixture.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range open {
		if notice.Kind == "notice_delivery_delayed" {
			t.Fatal("delayed Notice alert stayed open after its Notice was acknowledged")
		}
	}
}

func TestOpenCodeLeadWithoutReceiverNamesItsRecoveryStep(t *testing.T) {
	stubLeadReceiver(t, false, 0)
	lead := idlePiLead
	lead.Agent = "opencode"
	fixture := newCodexLeadFixture(t, lead, "", "0")
	if err := fixture.service.deliverNotices(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	if prompts := fixture.prompts(); len(prompts) != 1 || !strings.Contains(prompts[0], "Quit OpenCode in the shop Lead pane and run `posse up` there") {
		t.Fatalf("an OpenCode Lead without a receiver did not get its recovery step: %q", prompts)
	}
}

func TestPiLeadReceiverKeepsExtensionDelivery(t *testing.T) {
	for name, test := range map[string]struct {
		live  bool
		grace time.Duration
		lead  herdr.Pane
	}{
		"live receiver": {live: true, lead: idlePiLead},
		"within grace":  {grace: time.Hour, lead: idlePiLead},
		"busy Lead":     {lead: herdr.Pane{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "pi", AgentStatus: "working"}},
	} {
		t.Run(name, func(t *testing.T) {
			stubLeadReceiver(t, test.live, test.grace)
			fixture := newCodexLeadFixture(t, test.lead, "", "0")
			if err := fixture.service.deliverNotices(context.Background(), fixture.db, fixture.project); err != nil {
				t.Fatal(err)
			}
			if prompts := fixture.prompts(); len(prompts) != 0 || len(fixture.receiverMissingNotices(t)) != 0 {
				t.Fatalf("Posse took over the extension's Notices: prompts=%q missing=%d", prompts, len(fixture.receiverMissingNotices(t)))
			}
		})
	}
}

func TestAdoptionKeepsHoldWhileThePiLeadReceiverIsMissing(t *testing.T) {
	for _, live := range []bool{false, true} {
		stubLeadReceiver(t, live, 0)
		fixture := newCodexLeadFixture(t, idlePiLead, "", "0")
		ctx := context.Background()
		if err := fixture.db.SetProjectServerStartedAt(ctx, fixture.project.ID, "old-generation"); err != nil {
			t.Fatal(err)
		}
		snapshot := herdr.Snapshot{ServerStartedAt: "new-generation", Panes: []herdr.Pane{idlePiLead}}
		adopted, err := fixture.service.adoptCurrentGeneration(ctx, fixture.db, fixture.project, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if adopted != live {
			t.Fatalf("receiver live=%v: adopted=%v", live, adopted)
		}
	}
}
