package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestIntentPayloadUpdateRequiresCurrentOwner(t *testing.T) {
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "discard identities", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	owner := os.Getpid()
	if err := db.StartIntent(ctx, project.ID, taskID, "unsaddle", "started", `{}`, owner); err != nil {
		t.Fatal(err)
	}
	intent, err := db.IntentByTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"discard":true,"user_quote":"destroy t3 now"}`
	if err := db.UpdateIntentPayload(ctx, intent.ID, owner, payload); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateIntentPayload(ctx, intent.ID, owner+1, `{"rider":"replacement"}`); !errors.Is(err, ErrIntentOwned) {
		t.Fatalf("non-owner payload update = %v, want ErrIntentOwned", err)
	}
	current, err := db.IntentByTask(ctx, taskID)
	if err != nil || current.PayloadJSON != payload || current.Step != "started" {
		t.Fatalf("retained payload changed: %#v %v", current, err)
	}
	if err := db.FinishIntent(ctx, intent.ID, owner); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateIntentPayload(ctx, intent.ID, owner, payload); !errors.Is(err, ErrIntentOwned) {
		t.Fatalf("finished intent payload update = %v, want ErrIntentOwned", err)
	}
}
