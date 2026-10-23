package app

import (
	"context"
	"errors"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestForeignMarkedForegroundScanSkipsSiblingPaneThatClosed(t *testing.T) {
	task := store.Task{Seq: 1, ContainmentMarker: "task-marker"}
	sibling := herdr.Pane{PaneID: "w1:p9", WorkspaceID: "w1"}
	for name, closeErr := range map[string]error{
		"pane_not_found":       &herdr.Error{Code: "pane_not_found", Message: "pane w1:p9 not found"},
		"invalid process info": errors.New("herdr did not return a valid process identity"),
	} {
		t.Run(name, func(t *testing.T) {
			fake := herdr.NewFake()
			fake.Errors["pane.process_info"] = closeErr
			service := testService(t.TempDir(), fake)
			if err := service.verifyNoForeignTaskMarkedForeground(context.Background(), task, sibling); err != nil {
				t.Fatalf("closed sibling pane failed Teardown: %v", err)
			}
		})
	}
}

func TestForeignMarkedForegroundScanStillFailsForLivePaneWithoutProcessInfo(t *testing.T) {
	task := store.Task{Seq: 1, ContainmentMarker: "task-marker"}
	sibling := herdr.Pane{PaneID: "w1:p9", WorkspaceID: "w1"}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{sibling}}
	fake.Errors["pane.process_info"] = errors.New("herdr did not return a valid process identity")
	err := testService(t.TempDir(), fake).verifyNoForeignTaskMarkedForeground(context.Background(), task, sibling)
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "sandbox_process_identity_unavailable" || !failure.Retryable {
		t.Fatalf("live pane without process info = %#v (%v)", failure, err)
	}
}
