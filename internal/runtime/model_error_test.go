package runtime

import (
	"context"
	"strings"
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
	if _, observed, err := db.ObserveModelErrorEpisode(ctx, task.ID, 1, "pi", "model_stream_error", "fingerprint", true, time.Now().UnixMilli()); err != nil || !observed {
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

func TestExpiredModelErrorNudgeRaisesSpecificInterruptionNotice(t *testing.T) {
	ctx := context.Background()
	db, project, task := createWorkingTask(t)
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET launches=1 WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	episode, observed, err := db.ObserveModelErrorEpisode(ctx, task.ID, 1, "pi", "model_stream_error", "fingerprint", true, now.UnixMilli())
	if err != nil || !observed {
		t.Fatalf("record model-error episode: observed=%t err=%v", observed, err)
	}
	if _, claimed, err := db.ClaimModelErrorNudge(ctx, task.ID, episode.Episode, episode.Fingerprint, 3, now.UnixMilli(), 250); err != nil || !claimed {
		t.Fatalf("claim retry: claimed=%t err=%v", claimed, err)
	}
	pane := herdr.Pane{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "pi", AgentStatus: "idle"}
	if _, err := ReconcileSnapshot(ctx, db, project.ID, herdr.Snapshot{Panes: []herdr.Pane{pane}}, now.Add(time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || notices[0].Kind != "model_stream_error" || !strings.Contains(notices[0].Summary, "uncertain") {
		t.Fatalf("expired claim did not raise specific interruption Notice: %#v", notices)
	}
	current, err := db.TaskModelErrorEpisode(ctx, task.ID)
	if err != nil || current.Status != "interrupted" || current.Attempts != 1 {
		t.Fatalf("interrupted episode=%#v err=%v", current, err)
	}
}

func TestIdleAwaitingModelErrorTurnRetainsGenericNoticeFallback(t *testing.T) {
	ctx := context.Background()
	db, project, task := createWorkingTask(t)
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET launches=1 WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	at := now.UnixMilli()
	episode, observed, err := db.ObserveModelErrorEpisode(ctx, task.ID, 1, "pi", "model_stream_error", "fingerprint", true, at)
	if err != nil || !observed {
		t.Fatalf("record model-error episode: observed=%t err=%v", observed, err)
	}
	if _, claimed, err := db.ClaimModelErrorNudge(ctx, task.ID, episode.Episode, episode.Fingerprint, 3, at, 250); err != nil || !claimed {
		t.Fatalf("claim retry: claimed=%t err=%v", claimed, err)
	}
	if err := db.CompleteModelErrorNudge(ctx, task.ID, episode.Episode, 250, "stream disconnect", at+250); err != nil {
		t.Fatal(err)
	}
	pane := herdr.Pane{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "pi", AgentStatus: "idle"}
	if _, err := ReconcileSnapshot(ctx, db, project.ID, herdr.Snapshot{Panes: []herdr.Pane{pane}}, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileSnapshot(ctx, db, project.ID, herdr.Snapshot{Panes: []herdr.Pane{pane}}, now.Add(time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "worker_idle" {
		t.Fatalf("orphaned completed attempt suppressed generic idle fallback: notices=%#v err=%v", notices, err)
	}
}

func TestResolvedModelErrorAllowsOrdinaryIdleNotice(t *testing.T) {
	ctx := context.Background()
	db, project, task := createWorkingTask(t)
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET launches=1 WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	episode, observed, err := db.ObserveModelErrorEpisode(ctx, task.ID, 1, "pi", "model_stream_error", "fingerprint", true, time.Now().UnixMilli())
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
