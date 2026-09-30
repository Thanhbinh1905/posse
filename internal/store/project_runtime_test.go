package store

import (
	"context"
	"reflect"
	"testing"
)

func TestRememberProjectServerStartedAtFillsGenerationAlongsideLookoutRecovery(t *testing.T) {
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
	lookout := LookoutRecovery{PaneID: "w1:p2", RetryAt: 123456, FailedStarts: 2, NoticeRaised: true}
	if err := db.SetLookoutRecovery(ctx, project.ID, lookout); err != nil {
		t.Fatal(err)
	}
	if err := db.RememberProjectServerStartedAt(ctx, project.ID, "server-1"); err != nil {
		t.Fatal(err)
	}
	generation, err := db.ProjectServerStartedAt(ctx, project.ID)
	if err != nil || generation != "server-1" {
		t.Fatalf("remembered Project generation = %q, want server-1: %v", generation, err)
	}
	gotLookout, err := db.LookoutRecovery(ctx, project.ID)
	if err != nil || gotLookout != lookout {
		t.Fatalf("remembering Project generation changed Lookout recovery: %#v, want %#v: %v", gotLookout, lookout, err)
	}
	if err := db.RememberProjectServerStartedAt(ctx, project.ID, "server-2"); err != nil {
		t.Fatal(err)
	}
	generation, err = db.ProjectServerStartedAt(ctx, project.ID)
	if err != nil || generation != "server-1" {
		t.Fatalf("remembering a later Project generation = %q, want the original server-1: %v", generation, err)
	}
}

func TestLookoutRecoveryPersistsAcrossStoreReopen(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	want := LookoutRecovery{PaneID: "w1:p2", RetryAt: 123456, FailedStarts: 4, NoticeRaised: true}
	if err := db.SetLookoutRecovery(ctx, project.ID, want); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.LookoutRecovery(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened Lookout recovery = %#v, want %#v", got, want)
	}
	if err := db.ResetLookoutRecovery(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	got, err = db.LookoutRecovery(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != (LookoutRecovery{}) {
		t.Fatalf("reset Lookout recovery = %#v, want empty state", got)
	}
}
