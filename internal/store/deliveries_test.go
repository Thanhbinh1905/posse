package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func deliveryTestTask(t *testing.T) (*DB, string, Project, int64) {
	t.Helper()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	project, err := db.CreateProject(context.Background(), "shop", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(context.Background(), project.ID, Task{Seq: 1, Type: "change", Title: "Change", Branch: "posse/t1"})
	if err != nil {
		t.Fatal(err)
	}
	return db, home, project, taskID
}

func TestEnsureDeliveryKeepsTheFirstOriginAndSeparatesStewardFromTask(t *testing.T) {
	ctx := context.Background()
	db, _, _, taskID := deliveryTestTask(t)
	prURL := "https://github.com/acme/shop/pull/17"
	created, err := db.EnsureDelivery(ctx, taskID, "", prURL, "publish")
	if err != nil {
		t.Fatal(err)
	}
	if created.Steward != "lead" || created.Origin != "publish" || created.TaskID != taskID {
		t.Fatalf("delivery = %#v", created)
	}
	again, err := db.EnsureDelivery(ctx, taskID, "", prURL, "legacy")
	if err != nil || again.ID != created.ID || again.Origin != "publish" {
		t.Fatalf("repeat ensure = %#v, %v", again, err)
	}
	member, err := db.EnsureDelivery(ctx, taskID, "api", prURL, "legacy")
	if err != nil || member.ID == created.ID || member.Origin != "legacy" {
		t.Fatalf("member delivery = %#v, %v", member, err)
	}
	if _, err := db.EnsureDelivery(ctx, taskID, "", "", "publish"); err == nil {
		t.Fatal("delivery without a PR URL was accepted")
	}
}

func TestDeliveryOperationsAreKeyedLimitedAndObservedBeforeRepeat(t *testing.T) {
	ctx := context.Background()
	db, _, _, taskID := deliveryTestTask(t)
	delivery, err := db.EnsureDelivery(ctx, taskID, "", "https://github.com/acme/shop/pull/17", "publish")
	if err != nil {
		t.Fatal(err)
	}
	headA, headB := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rerun := DeliveryOperation{DeliveryID: delivery.ID, Operation: "rerun", Actor: "lead", ActorPane: "p1", HeadSHA: headA, Subject: "run:42", Baseline: "1"}

	first, err := db.BeginDeliveryOperation(ctx, rerun, headA, 2)
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != "pending" || !first.Running(time.Now()) {
		t.Fatalf("new operation = %#v", first)
	}
	if _, err := db.BeginDeliveryOperation(ctx, rerun, headA, 2); !errors.Is(err, ErrDeliveryOperationUnresolved) {
		t.Fatalf("pending operation allowed a repeat: %v", err)
	}
	if err := db.ResolveDeliveryOperation(ctx, first.ID, "uncertain", "connection reset"); err != nil {
		t.Fatal(err)
	}
	unresolved, err := db.UnresolvedDeliveryOperation(ctx, delivery.ID, "rerun", headA, "run:42")
	if err != nil || unresolved.ID != first.ID || unresolved.Running(time.Now()) {
		t.Fatalf("unresolved = %#v, %v", unresolved, err)
	}
	if _, err := db.BeginDeliveryOperation(ctx, rerun, headA, 2); !errors.Is(err, ErrDeliveryOperationUnresolved) {
		t.Fatalf("uncertain operation allowed a repeat: %v", err)
	}
	if err := db.ResolveDeliveryOperation(ctx, first.ID, "succeeded", "observed on the forge"); err != nil {
		t.Fatal(err)
	}
	if err := db.ResolveDeliveryOperation(ctx, first.ID, "failed", "late"); !errors.Is(err, ErrStateRace) {
		t.Fatalf("a settled operation changed again: %v", err)
	}
	second, err := db.BeginDeliveryOperation(ctx, rerun, headA, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ResolveDeliveryOperation(ctx, second.ID, "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginDeliveryOperation(ctx, rerun, headA, 2); !errors.Is(err, ErrDeliveryOperationLimit) {
		t.Fatalf("third rerun on one head was allowed: %v", err)
	}
	// A failed attempt does not count against the limit; a new head has its own.
	onB := rerun
	onB.HeadSHA = headB
	if _, err := db.BeginDeliveryOperation(ctx, onB, headB, 2); err != nil {
		t.Fatalf("rerun on a new head: %v", err)
	}

	// A reply is keyed across heads: the same text on a new head is a duplicate.
	reply := DeliveryOperation{DeliveryID: delivery.ID, Operation: "reply", Actor: "user", HeadSHA: headA, Subject: "body:1"}
	posted, err := db.BeginDeliveryOperation(ctx, reply, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ResolveDeliveryOperation(ctx, posted.ID, "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	reply.HeadSHA = headB
	if _, err := db.BeginDeliveryOperation(ctx, reply, "", 1); !errors.Is(err, ErrDeliveryOperationLimit) {
		t.Fatalf("identical reply on a new head was allowed: %v", err)
	}

	if _, err := db.BeginDeliveryOperation(ctx, DeliveryOperation{DeliveryID: delivery.ID, Operation: "rerun", Actor: "worker"}, "", 0); err == nil {
		t.Fatal("an operation by a Worker actor was accepted")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO delivery_operations(delivery_id,operation,actor,outcome,created_at,updated_at) VALUES(?,?,?,?,?,?)`, delivery.ID, "merge", "lead", "pending", 1, 1); err == nil {
		t.Fatal("the schema accepted a merge steward operation")
	}
}

func TestPendingDeliveryOperationPastItsLeaseIsNotRunning(t *testing.T) {
	stale := DeliveryOperation{Outcome: "pending", UpdatedAt: time.Now().Add(-DeliveryOperationLease - time.Second).UnixMilli()}
	if stale.Running(time.Now()) || !stale.Unresolved() {
		t.Fatalf("an interrupted pending operation must be unresolved but not running: %#v", stale)
	}
	if (DeliveryOperation{Outcome: "uncertain", UpdatedAt: time.Now().UnixMilli()}).Running(time.Now()) {
		t.Fatal("an uncertain operation is not running")
	}
}

func TestDeliveriesAndOperationsSurviveSnapshotRebuild(t *testing.T) {
	ctx := context.Background()
	db, home, _, taskID := deliveryTestTask(t)
	delivery, err := db.EnsureDelivery(ctx, taskID, "", "https://github.com/acme/shop/pull/17", "publish")
	if err != nil {
		t.Fatal(err)
	}
	operation, err := db.BeginDeliveryOperation(ctx, DeliveryOperation{DeliveryID: delivery.ID, Operation: "rerun", Actor: "lead", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Subject: "run:42", Baseline: "1"}, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ResolveDeliveryOperation(ctx, operation.ID, "uncertain", "connection reset"); err != nil {
		t.Fatal(err)
	}
	if count, err := db.RebuildFromSnapshots(ctx, home); err != nil || count != 1 {
		t.Fatalf("rebuild count=%d err=%v", count, err)
	}
	deliveries, err := db.TaskDeliveries(ctx, taskID)
	if err != nil || len(deliveries) != 1 || deliveries[0] != delivery {
		t.Fatalf("deliveries after rebuild = %#v, %v", deliveries, err)
	}
	restored, err := db.UnresolvedDeliveryOperation(ctx, delivery.ID, "rerun", operation.HeadSHA, "run:42")
	if err != nil || restored.ID != operation.ID || restored.Outcome != "uncertain" || restored.Baseline != "1" {
		t.Fatalf("an unknown outcome was lost on rebuild: %#v, %v", restored, err)
	}
}

func TestMigrationRecordsExistingPRURLsAsLegacyDeliveries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Old", Branch: "posse/t1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(ctx, project.ID, Task{Seq: 2, Type: "change", Title: "No PR", Branch: "posse/t2"}); err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/acme/shop/pull/17"
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET pr_url=? WHERE id=?`, prURL, taskID); err != nil {
		t.Fatal(err)
	}
	// Return the store to its state before the delivery migration.
	for _, statement := range []string{
		`DROP TABLE delivery_operations`,
		`DROP TABLE deliveries`,
		`ALTER TABLE pr_observations DROP COLUMN confirmed_at`,
		`DELETE FROM goose_db_version WHERE version_id=73`,
		`DELETE FROM posse_migration_checksums WHERE version=73`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	deliveries, err := db.TaskDeliveries(ctx, taskID)
	if err != nil || len(deliveries) != 1 || deliveries[0].PRURL != prURL || deliveries[0].Origin != "legacy" || deliveries[0].Steward != "lead" {
		t.Fatalf("legacy deliveries = %#v, %v", deliveries, err)
	}
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries`).Scan(&total); err != nil || total != 1 {
		t.Fatalf("deliveries = %d, %v; want only the Task with a PR URL", total, err)
	}
}
