package store

import (
	"context"
	"errors"
	"testing"
)

func TestAdmissionTransfersBranchAndDeliveryCustody(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	occupied := func(int) (bool, error) { return false, nil }
	source, _, err := db.CreateTaskWithSequence(ctx, p.ID, p.Name, Task{Type: "change", ShortName: "source", Title: "Source"}, occupied)
	if err != nil {
		t.Fatal(err)
	}
	const url = "https://github.com/acme/shop/pull/17"
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET state='done',pr_url=?,gated_sha=? WHERE id=?`, url, branchAuthorTestSHA, source); err != nil {
		t.Fatal(err)
	}
	delivery, err := db.EnsureDelivery(ctx, source, "", url, "publish")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimBranchAuthor(ctx, "shop", "posse/t1", source, 1, branchAuthorTestSHA, branchAuthorTestSHA, false); err != nil {
		t.Fatal(err)
	}
	task := Task{Type: "change", ShortName: "successor", Title: "Successor", Branch: "posse/t1", Adoption: &TaskAdoptionRequest{SourceTaskID: source, Repository: "shop", Branch: "posse/t1", PRURL: url, ExpectedHead: branchAuthorTestSHA, ExpectedGeneration: 1, PreviousWriterStopped: true}}
	id, _, err := db.CreateTaskWithSequenceAndIntentWithinLimit(ctx, p.ID, p.Name, task, "start", 1, 1, occupied)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := db.TaskByID(ctx, p.ID, source)
	next, _ := db.TaskByID(ctx, p.ID, id)
	if old.PRURL != "" || old.State != StateTornDown || old.GatedSHA != "" || next.PRURL != url {
		t.Fatalf("source=%+v successor=%+v", old, next)
	}
	claim, err := db.BranchAuthor(ctx, "shop", "posse/t1")
	if err != nil || claim.TaskID != id || claim.Generation != 2 {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	history, err := db.TaskDeliveries(ctx, source)
	if err != nil || len(history) != 1 || history[0].ID != delivery.ID {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	if _, err := db.ClaimBranchAuthor(ctx, "shop", "posse/t1", source, 2, branchAuthorTestSHA, branchAuthorTestSHA, true); !errors.Is(err, ErrBranchAuthorStale) {
		t.Fatalf("superseded source reclaimed branch: %v", err)
	}
	if _, _, err := db.CreateTaskWithSequenceAndIntentWithinLimit(ctx, p.ID, p.Name, task, "start", 1, 3, occupied); err == nil {
		t.Fatal("source adopted twice")
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("refusal changed Task count: %d, %v", count, err)
	}
	if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
		t.Fatal(err)
	}
	lineage, err := db.TaskAdoptions(ctx, id)
	if err != nil || len(lineage) != 1 || lineage[0].SourceTaskID != source {
		t.Fatalf("lineage=%+v err=%v", lineage, err)
	}
}
