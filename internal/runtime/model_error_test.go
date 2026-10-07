package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestHandledModelErrorSuppressesDelayedGenericIdleNotice(t *testing.T) {
	ctx := context.Background()
	db, project, task := createWorkingTask(t)
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET launches=1 WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, observed, err := db.ObserveModelErrorEpisode(ctx, task.ID, 1, "pi", "model_stream_error", "fingerprint", time.Now().UnixMilli()); err != nil || !observed {
		t.Fatalf("record handled model-error episode: observed=%t err=%v", observed, err)
	}
	pane := herdr.Pane{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "pi", AgentStatus: "idle"}
	now := time.Now()
	for _, at := range []time.Time{now, now.Add(time.Hour)} {
		if _, err := ReconcileSnapshot(ctx, db, project.ID, herdr.Snapshot{Panes: []herdr.Pane{pane}}, at, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "worker_idle" {
			t.Fatalf("handled model-error episode fell back to generic idle Notice: %#v", notices)
		}
	}
}

func TestResolvedModelErrorAllowsOrdinaryIdleNotice(t *testing.T) {
	ctx := context.Background()
	db, project, task := createWorkingTask(t)
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET launches=1 WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	episode, observed, err := db.ObserveModelErrorEpisode(ctx, task.ID, 1, "pi", "model_stream_error", "fingerprint", time.Now().UnixMilli())
	if err != nil || !observed {
		t.Fatalf("record model-error episode: observed=%t err=%v", observed, err)
	}
	if _, err := db.FinishModelErrorEpisode(ctx, task.ID, episode.Episode, "resolved", "Rider turn completed successfully", nil, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	pane := herdr.Pane{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "pi", AgentStatus: "idle"}
	now := time.Now()
	for _, at := range []time.Time{now, now.Add(time.Hour)} {
		if _, err := ReconcileSnapshot(ctx, db, project.ID, herdr.Snapshot{Panes: []herdr.Pane{pane}}, at, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "worker_idle" {
			return
		}
	}
	t.Fatalf("resolved model-error episode did not restore ordinary idle Notice: %#v", notices)
}
