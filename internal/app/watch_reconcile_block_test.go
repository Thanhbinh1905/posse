package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// heldReconcileFixture records Herdr generation 100:1 for the Project while the
// fake Herdr server reports 200:2, the state a missed `posse up --all` leaves.
func heldReconcileFixture(t *testing.T) (startFixture, *store.DB) {
	t.Helper()
	fixture := newStartFixture(t)
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.SetProjectServerStartedAt(context.Background(), fixture.project.ID, "100:1"); err != nil {
		t.Fatal(err)
	}
	fixture.fake.SnapshotValue.ServerStartedAt = "200:2"
	return fixture, db
}

func runOneWatchPass(t *testing.T, fixture startFixture, db *store.DB) string {
	t.Helper()
	output := &bytes.Buffer{}
	ctx := &axi.Context{Context: context.Background(), Out: output, ErrOut: output}
	if err := fixture.service.runWatchLoop(ctx, db, fixture.project, time.Millisecond, nil, nil); err != nil {
		t.Fatalf("watch pass: %v\n%s", err, output.String())
	}
	return output.String()
}

func reconcileBlockedNotices(t *testing.T, db *store.DB, projectID int64) []store.Notice {
	t.Helper()
	notices, err := db.Notices(context.Background(), projectID, false)
	if err != nil {
		t.Fatal(err)
	}
	var blocked []store.Notice
	for _, notice := range notices {
		if notice.Kind == "reconcile_blocked" {
			blocked = append(blocked, notice)
		}
	}
	return blocked
}

func TestWatchObservationReportsHeldHerdrGeneration(t *testing.T) {
	fixture, db := heldReconcileFixture(t)
	_, err := fixture.service.prepareProjectWatchObservation(context.Background(), db, fixture.project)
	if !errors.Is(err, errRecoveryHeld) {
		t.Fatalf("held Project observation returned %v, want errRecoveryHeld", err)
	}
	for _, want := range []string{"200:2", "100:1", "posse up --all"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("held cause %q omits %q", err.Error(), want)
		}
	}
}

func TestWatchRaisesOneReconcileBlockedNoticePerEpisode(t *testing.T) {
	fixture, db := heldReconcileFixture(t)
	previous := watchReconcileBlockNoticeAfter
	watchReconcileBlockNoticeAfter = time.Hour
	t.Cleanup(func() { watchReconcileBlockNoticeAfter = previous })

	runOneWatchPass(t, fixture, db)
	if notices := reconcileBlockedNotices(t, db, fixture.project.ID); len(notices) != 0 {
		t.Fatalf("a short block raised a Notice: %+v", notices)
	}
	block, err := db.ReconcileBlock(context.Background(), fixture.project.ID)
	if err != nil || block.Since == 0 || !strings.Contains(block.Cause, "posse up --all") {
		t.Fatalf("held pass recorded block=%+v err=%v", block, err)
	}

	watchReconcileBlockNoticeAfter = 0
	runOneWatchPass(t, fixture, db)
	runOneWatchPass(t, fixture, db)
	notices := reconcileBlockedNotices(t, db, fixture.project.ID)
	if len(notices) != 1 || !strings.Contains(notices[0].Summary, "queued Tasks are not admitted") || !strings.Contains(notices[0].Summary, "Herdr server 200:2 replaced the recorded 100:1") {
		t.Fatalf("persistent block raised Notices %+v, want one naming the cause", notices)
	}

	fixture.fake.SnapshotValue.ServerStartedAt = "100:1"
	runOneWatchPass(t, fixture, db)
	if block, err := db.ReconcileBlock(context.Background(), fixture.project.ID); err != nil || block.Since != 0 {
		t.Fatalf("a completed pass left block=%+v err=%v", block, err)
	}

	fixture.fake.SnapshotValue.ServerStartedAt = "300:3"
	runOneWatchPass(t, fixture, db)
	if notices := reconcileBlockedNotices(t, db, fixture.project.ID); len(notices) != 2 {
		t.Fatalf("a new block episode raised %d Notices, want 2", len(notices))
	}
}

func TestQueuedTaskWaitReasonNamesTheRealBlocker(t *testing.T) {
	fixture, db := heldReconcileFixture(t)
	ctx := context.Background()
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Defaults.MaxWorkers = 2
	queuedID, err := db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 2, Type: "change", Title: "Queued", ShortName: "queued", State: store.StateQueued})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := db.TaskByID(ctx, fixture.project.ID, queuedID)
	if err != nil {
		t.Fatal(err)
	}
	reason := func() string {
		t.Helper()
		reason, err := queuedTaskWaitReason(ctx, db, fixture.project, queued, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return reason
	}

	if got := reason(); got != "waiting for admission: 2 of 2 Worker slots are free" {
		t.Fatalf("unblocked free-slot wait_reason = %q", got)
	}
	if _, err := db.RecordReconcileBlock(ctx, fixture.project.ID, "restore is held until posse up: run `posse up --all`", time.Date(2026, 10, 28, 4, 0, 0, 0, time.UTC).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if got := reason(); got != "waiting for Project reconciliation, blocked since 2026-10-28T04:00:00Z: restore is held until posse up: run `posse up --all`" {
		t.Fatalf("held free-slot wait_reason = %q", got)
	}
	for seq := 3; seq <= 4; seq++ {
		if _, err := db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: seq, Type: "change", Title: "Busy", ShortName: "busy", State: store.StateSpawning}); err != nil {
			t.Fatal(err)
		}
	}
	if got := reason(); got != "waiting for a Worker slot" {
		t.Fatalf("full wait_reason = %q", got)
	}
}
