package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPublishPrePushHeadsAreScopedAndNotVerified(t *testing.T) {
	ctx := context.Background()
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Publish", LandingMode: "pr"})
	if err != nil {
		t.Fatal(err)
	}
	const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := db.RecordPublishPrePushHead(ctx, taskID, "", head); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordPublishPrePushHead(ctx, taskID, "worker", head); err != nil {
		t.Fatal(err)
	}
	requestURL := "https://github.com/acme/shop/pull/17"
	for _, test := range []struct {
		repo string
		url  string
		want bool
	}{{"", requestURL, true}, {"worker", requestURL, true}, {"another", requestURL, false}} {
		got, err := db.WasPublishPrePushHead(ctx, taskID, test.repo, test.url, head)
		if err != nil || got != test.want {
			t.Fatalf("member %q request %q known=%t err=%v, want %t", test.repo, test.url, got, err, test.want)
		}
	}
	wrongRequest, err := db.WasPublishPrePushHead(ctx, taskID, "worker", "https://github.com/acme/shop/pull/18", head)
	if err != nil || wrongRequest {
		t.Fatalf("head for another request was accepted: known=%t err=%v", wrongRequest, err)
	}
	verified, err := db.WasVerifiedPRHead(ctx, taskID, requestURL, head)
	if err != nil || verified {
		t.Fatalf("pre-push head was incorrectly marked verified: verified=%t err=%v", verified, err)
	}
	if err := db.RecordPublishPrePushHead(ctx, taskID, "worker", ""); err != nil {
		t.Fatalf("empty remote head should be ignored: %v", err)
	}
}
