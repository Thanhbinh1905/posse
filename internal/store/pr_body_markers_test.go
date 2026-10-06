package store

import (
	"context"
	"regexp"
	"testing"
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
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(pending) {
		t.Fatalf("marker token is not 256-bit lowercase hex: %q", pending)
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
