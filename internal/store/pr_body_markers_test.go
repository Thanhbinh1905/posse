package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestEnsurePRBodyMarkerPersistsAndBindsTokenPerTaskMember(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Ship"})
	if err != nil {
		t.Fatal(err)
	}

	pending, err := db.EnsurePRBodyMarker(ctx, taskID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 64 {
		t.Fatalf("marker token length = %d, want 64 hexadecimal characters", len(pending))
	}
	if retry, err := db.EnsurePRBodyMarker(ctx, taskID, "", ""); err != nil || retry != pending {
		t.Fatalf("pending retry token=%q err=%v, want %q", retry, err, pending)
	}

	prURL := "https://github.com/acme/shop/pull/17"
	adopted, err := db.EnsurePRBodyMarker(ctx, taskID, "", prURL)
	if err != nil || adopted != pending {
		t.Fatalf("adopted token=%q err=%v, want pending token %q", adopted, err, pending)
	}
	marker, err := db.GetPRBodyMarker(ctx, taskID, "")
	if err != nil || marker.PRURL != prURL || marker.Token != pending {
		t.Fatalf("bound marker=%+v err=%v", marker, err)
	}
	if err := db.BindPRBodyMarker(ctx, taskID, "", prURL, pending); err != nil {
		t.Fatalf("idempotent bind: %v", err)
	}

	memberToken, err := db.EnsurePRBodyMarker(ctx, taskID, "api", prURL)
	if err != nil || memberToken == pending {
		t.Fatalf("workspace member token=%q err=%v, want distinct from main repository token", memberToken, err)
	}

	nextDelivery, err := db.EnsurePRBodyMarker(ctx, taskID, "", "")
	if err != nil || nextDelivery == pending {
		t.Fatalf("new delivery token=%q err=%v, want a new token", nextDelivery, err)
	}
	newURL := "https://github.com/acme/shop/pull/18"
	if err := db.BindPRBodyMarker(ctx, taskID, "", newURL, nextDelivery); err != nil {
		t.Fatal(err)
	}
	marker, err = db.GetPRBodyMarker(ctx, taskID, "")
	if err != nil || marker.PRURL != newURL || marker.Token != nextDelivery {
		t.Fatalf("new delivery marker=%+v err=%v", marker, err)
	}
}

func TestPersistTaskPreservesMarkersMissingFromStore(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Published"})
	if err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/acme/shop/pull/17"
	token, err := db.EnsurePRBodyMarker(ctx, taskID, "", prURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM pr_body_markers WHERE task_id = ?", taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.PersistTask(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(db.TaskSnapshotPath(project.Name, 1))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot TaskSnapshot
	if _, err := toml.Decode(string(data), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.PRBodyMarkers) != 1 || snapshot.PRBodyMarkers[0].Token != token || snapshot.PRBodyMarkers[0].PRURL != prURL {
		t.Fatalf("Task update dropped the authoritative marker: %#v", snapshot.PRBodyMarkers)
	}
	if count, err := db.RebuildFromSnapshots(ctx, home); err != nil || count != 1 {
		t.Fatalf("rebuild count=%d err=%v", count, err)
	}
	marker, err := db.GetPRBodyMarker(ctx, taskID, "")
	if err != nil || marker.Token != token || marker.PRURL != prURL {
		t.Fatalf("restored marker=%+v err=%v", marker, err)
	}
}

func TestPRBodyMarkersSurviveSnapshotRebuild(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	pendingTask, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Pending", Branch: "posse/pending"})
	if err != nil {
		t.Fatal(err)
	}
	pendingToken, err := db.EnsurePRBodyMarker(ctx, pendingTask, "", "")
	if err != nil {
		t.Fatal(err)
	}
	requestTask, err := db.CreateTask(ctx, project.ID, Task{Seq: 2, Type: "ship", Title: "Published", Branch: "posse/published"})
	if err != nil {
		t.Fatal(err)
	}
	requestURL := "https://github.com/acme/shop/pull/22"
	requestToken, err := db.EnsurePRBodyMarker(ctx, requestTask, "", requestURL)
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(db.TaskSnapshotPath(project.Name, 1))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot TaskSnapshot
	if _, err := toml.Decode(string(data), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.PRBodyMarkers) != 1 || snapshot.PRBodyMarkers[0].Token != pendingToken || snapshot.PRBodyMarkers[0].PRURL != "" {
		t.Fatalf("pending marker snapshot = %#v", snapshot.PRBodyMarkers)
	}

	if count, err := db.RebuildFromSnapshots(ctx, home); err != nil || count != 2 {
		t.Fatalf("rebuild count=%d err=%v", count, err)
	}
	pending, err := db.GetPRBodyMarker(ctx, pendingTask, "")
	if err != nil || pending.Token != pendingToken || pending.PRURL != "" {
		t.Fatalf("pending reservation after rebuild = %+v, %v", pending, err)
	}
	request, err := db.GetPRBodyMarker(ctx, requestTask, "")
	if err != nil || request.Token != requestToken || request.PRURL != requestURL {
		t.Fatalf("published PR marker after rebuild = %+v, %v", request, err)
	}
	markers, err := db.PRBodyMarkersForTask(ctx, requestTask)
	if err != nil || len(markers) != 1 || markers[0].TaskID != requestTask || markers[0].PRURL != requestURL || markers[0].Token != requestToken {
		t.Fatalf("restored marker rows = %#v, %v", markers, err)
	}
}
