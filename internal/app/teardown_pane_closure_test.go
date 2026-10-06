package app

import (
	"context"
	"errors"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestTeardownRefusesSurvivingPaneWithoutConditionalClose(t *testing.T) {
	fixture := newPRLandingFixture(t, "local", store.StateWorking)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", Label: fixture.project.LeadLabel, Agent: "claude"},
		{PaneID: fixture.task.PaneID, WorkspaceID: fixture.task.HerdrWorkspaceID, TabID: "w2:t1", Label: fixture.task.PaneLabel, CWD: fixture.worktree, Agent: "claude"},
	}}
	fixture.service.Herdr = fake
	plan, err := fixture.service.planTaskPaneTeardown(context.Background(), fixture.project, fixture.task)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.verifyTaskPanesClosed(context.Background(), fixture.project, fixture.task, plan)
	var refusal *axi.Error
	if !errors.As(err, &refusal) || refusal.Code != "herdr_conditional_close_unavailable" {
		t.Fatalf("surviving Task pane refusal = %#v, want herdr_conditional_close_unavailable", err)
	}
	for _, method := range []string{"pane.close", "tab.close", "workspace.close"} {
		if fake.CallCount(method) != 0 {
			t.Fatalf("fail-closed teardown called %s: %#v", method, fake.Calls)
		}
	}
}
