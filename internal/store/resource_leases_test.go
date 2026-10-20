package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureSandboxResourceLeaseReusesExistingLeaseDuringWriterContention(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Lease contention", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := db.AcquireSandbox(ctx, project.ID, taskID, filepath.Join(t.TempDir(), "pool"))
	if err != nil {
		t.Fatal(err)
	}
	request := ResourceLeaseRequest{
		ResourceType: ResourcePath, ScopePath: sandbox.Path, OwnerProjectID: project.ID,
		OwnerTaskID: taskID, Owner: "shop sandbox", SourceKey: fmt.Sprintf("sandbox:%d", sandbox.ID), SandboxID: sandbox.ID,
	}
	if _, err := db.EnsureSandboxResourceLease(ctx, request); err != nil {
		t.Fatalf("create Sandbox lease: %v", err)
	}
	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(ctx, `UPDATE projects SET name=name WHERE id=?`, project.ID); err != nil {
		_ = writer.Rollback()
		t.Fatal(err)
	}
	defer writer.Rollback()

	readCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	lease, err := db.EnsureSandboxResourceLease(readCtx, request)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("reuse existing Sandbox lease under writer contention: %v", err)
	}
	if lease.SandboxID != sandbox.ID {
		t.Fatalf("reused lease Sandbox id = %d, want %d", lease.SandboxID, sandbox.ID)
	}
	if elapsed >= 300*time.Millisecond {
		t.Fatalf("read-only Sandbox lease reuse took %s under writer contention", elapsed)
	}
}
