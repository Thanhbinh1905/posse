package store

import (
	"context"
	"encoding/json"
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

func TestDiscardApprovalForIntentPinsFirstBranchTip(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "discard approval", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	owner := os.Getpid()
	quote := "User approved the stopped Rider's final tip"
	if err := db.StartIntent(ctx, project.ID, taskID, "unsaddle", "started", `{"discard":true,"user_quote":"`+quote+`"}`, owner); err != nil {
		t.Fatal(err)
	}
	intent, err := db.IntentByTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := db.RecordDiscardApprovalForIntent(ctx, intent.ID, owner, taskID, quote, "approved-tip")
	if err != nil || first != "approved-tip" {
		t.Fatalf("initial discard approval = %q, %v", first, err)
	}
	retried, err := db.RecordDiscardApprovalForIntent(ctx, intent.ID, owner, taskID, quote, "advanced-tip")
	if err != nil || retried != "approved-tip" {
		t.Fatalf("retried discard approval = %q, %v; want original tip", retried, err)
	}
	var count int
	var savedQuote, savedTip string
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),MAX(user_quote),MAX(branch_sha) FROM approvals WHERE task_id=? AND action='discard'`, taskID).Scan(&count, &savedQuote, &savedTip); err != nil {
		t.Fatal(err)
	}
	if count != 1 || savedQuote != quote || savedTip != "approved-tip" {
		t.Fatalf("discard approval rows = count %d, quote %q, tip %q", count, savedQuote, savedTip)
	}
	current, err := db.IntentByTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Recorded  bool   `json:"discard_approval_recorded"`
		BranchSHA string `json:"discard_approval_branch_sha"`
	}
	if err := json.Unmarshal([]byte(current.PayloadJSON), &payload); err != nil || !payload.Recorded || payload.BranchSHA != "approved-tip" {
		t.Fatalf("intent discard approval marker = %#v, payload=%s, err=%v", payload, current.PayloadJSON, err)
	}
}
