package app

import (
	"context"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestAttachedPiLeadUsesFocusSafeNoticeFallback(t *testing.T) {
	lead := herdr.Pane{
		PaneID:      "w1:p1",
		WorkspaceID: "w1",
		Label:       "posse:shop:lead",
		Agent:       "pi",
		AgentStatus: "idle",
		Tokens:      map[string]string{"posse_attached": "true"},
	}
	fixture := newCodexLeadFixture(t, lead, "", "0")
	if err := fixture.service.deliverNotices(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	if fixture.fake.CallCount("agent.prompt") != 1 || fixture.undelivered(t) != 0 {
		t.Fatalf("attached Pi session did not receive its unfocused Notice: calls=%#v undelivered=%d", fixture.fake.Calls, fixture.undelivered(t))
	}
}
