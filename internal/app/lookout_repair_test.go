package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLookoutRepairDoesNotStartLeadOrChangeHeldRecoveryPolicy(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	root := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", root, "main")
	if err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	service := testService(home, fake)
	t.Setenv("HERDR_PANE_ID", "")
	output := &bytes.Buffer{}
	commandContext := &axi.Context{Context: ctx, Out: output, ErrOut: &bytes.Buffer{}}
	configPath := filepath.Join(home, "config.toml")
	configBefore := []byte("[defaults]\nauto_recover = false\n")
	if err := os.WriteFile(configPath, configBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	beforeRecovery, err := db.ProjectRecovery(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}

	err = service.repairLookout(commandContext, db, home, project)
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "lead_workspace_missing" {
		t.Fatalf("repair without an active Lead = %v, want lead_workspace_missing", err)
	}
	afterRecovery, err := db.ProjectRecovery(ctx, project.ID)
	if err != nil || afterRecovery != beforeRecovery {
		t.Fatalf("repair changed recovery policy: before=%#v after=%#v err=%v", beforeRecovery, afterRecovery, err)
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(configAfter, configBefore) {
		t.Fatalf("repair changed config: err=%v", err)
	}
	if fake.CallCount("tab.create") != 0 {
		t.Fatal("repair created a Lead or Lookout tab")
	}
}

func TestLookoutRepairRefusesDownProject(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkProjectDown(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	service := testService(home, nil)
	commandContext := &axi.Context{Context: ctx, Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}
	err = service.repairLookout(commandContext, db, home, project)
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "project_down" {
		t.Fatalf("repair of down Project = %v, want project_down", err)
	}
}

func TestPollerSupervisionDoesNotCreateAnEmptyHomeDatabase(t *testing.T) {
	home := t.TempDir()
	previousVerifier := lookoutExecutableVerifier
	lookoutExecutableVerifier = func(string) bool { return true }
	t.Cleanup(func() { lookoutExecutableVerifier = previousVerifier })
	service := New(home, nil)
	commandContext := &axi.Context{Context: context.Background(), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}
	service.superviseLookoutPollers(commandContext, "doctor", nil)
	if _, err := os.Stat(filepath.Join(home, "posse.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("poller supervision created a database in an empty home: %v", err)
	}
}

func TestLookoutPollerRowReportsStoppedAndNotStartedStates(t *testing.T) {
	ctx := context.Background()
	db, project, home := newLookoutTestProject(t)
	defer db.Close()
	service := testService(home, nil)
	project.HerdrWorkspaceID = ""
	if got := service.lookoutPollerRow(ctx, db, home, project)["state"]; got != "not_started" {
		t.Fatalf("unstarted Project Lookout state = %v", got)
	}
	if err := db.MarkProjectDown(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	project, err := db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := service.lookoutPollerRow(ctx, db, home, project)["state"]; got != "stopped" {
		t.Fatalf("down Project Lookout state = %v", got)
	}
}
