package app

import (
	"context"
	"strings"
	"testing"
	"time"

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
