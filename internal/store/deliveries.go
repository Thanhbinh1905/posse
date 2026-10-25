package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DeliveryOperationLease bounds how long a pending steward operation is
// treated as still running. Forge commands time out well inside it, so an
// older pending operation belongs to an interrupted process and its outcome is
// unknown.
const DeliveryOperationLease = 2 * time.Minute

var ErrDeliveryOperationInProgress = errors.New("the same steward operation is still running")
var ErrDeliveryOperationUnresolved = errors.New("an earlier steward operation has an unknown outcome")
var ErrDeliveryOperationLimit = errors.New("steward operation limit reached")

// Delivery is the canonical record of one forge PR/MR. The Lead is its
// operational steward; the authors are the Task's recorded launches.
type Delivery struct {
	ID        int64  `toml:"id"`
	TaskID    int64  `toml:"task_id"`
	Repo      string `toml:"repo,omitempty"`
	PRURL     string `toml:"pr_url"`
	Steward   string `toml:"steward"`
	Origin    string `toml:"origin"`
	CreatedAt int64  `toml:"created_at"`
}

// DeliveryOperation is one recorded steward forge operation.
type DeliveryOperation struct {
	ID         int64  `toml:"id"`
	DeliveryID int64  `toml:"delivery_id"`
	Operation  string `toml:"operation"`
	Actor      string `toml:"actor"`
	ActorPane  string `toml:"actor_pane,omitempty"`
	HeadSHA    string `toml:"head_sha,omitempty"`
	Subject    string `toml:"subject,omitempty"`
	Baseline   string `toml:"baseline,omitempty"`
	Outcome    string `toml:"outcome"`
	Detail     string `toml:"detail,omitempty"`
	CreatedAt  int64  `toml:"created_at"`
	UpdatedAt  int64  `toml:"updated_at"`
}

// Unresolved reports whether the operation's forge effect is still unknown.
func (operation DeliveryOperation) Unresolved() bool {
	return operation.Outcome == "pending" || operation.Outcome == "uncertain"
}

// Running reports whether a pending operation may still be in flight.
func (operation DeliveryOperation) Running(now time.Time) bool {
	return operation.Outcome == "pending" && now.UnixMilli()-operation.UpdatedAt < DeliveryOperationLease.Milliseconds()
}

const deliveryColumns = `id,task_id,repo,pr_url,steward,origin,created_at`
const deliveryOperationColumns = `id,delivery_id,operation,actor,actor_pane,head_sha,subject,baseline,outcome,detail,created_at,updated_at`

type rowScanner interface {
	Scan(...any) error
}

func scanDelivery(row rowScanner) (Delivery, error) {
	var delivery Delivery
	err := row.Scan(&delivery.ID, &delivery.TaskID, &delivery.Repo, &delivery.PRURL, &delivery.Steward, &delivery.Origin, &delivery.CreatedAt)
	return delivery, err
}

func scanDeliveryOperation(row rowScanner) (DeliveryOperation, error) {
	var operation DeliveryOperation
	err := row.Scan(&operation.ID, &operation.DeliveryID, &operation.Operation, &operation.Actor, &operation.ActorPane, &operation.HeadSHA, &operation.Subject, &operation.Baseline, &operation.Outcome, &operation.Detail, &operation.CreatedAt, &operation.UpdatedAt)
	return operation, err
}

// EnsureDelivery returns the delivery of a Task or Member PR URL, creating it
// with origin when it does not exist. An existing record keeps its origin.
func (db *DB) EnsureDelivery(ctx context.Context, taskID int64, repo, prURL, origin string) (Delivery, error) {
	if taskID < 1 || strings.TrimSpace(prURL) == "" || (origin != "publish" && origin != "legacy") {
		return Delivery{}, fmt.Errorf("invalid delivery for Task %d", taskID)
	}
	result, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO deliveries(task_id,repo,pr_url,steward,origin,created_at) VALUES(?,?,?,'lead',?,?)`, taskID, repo, prURL, origin, time.Now().UnixMilli())
	if err != nil {
		return Delivery{}, err
	}
	delivery, err := scanDelivery(db.QueryRowContext(ctx, `SELECT `+deliveryColumns+` FROM deliveries WHERE task_id=? AND repo=? AND pr_url=?`, taskID, repo, prURL))
	if err != nil {
		return Delivery{}, err
	}
	if rows, err := result.RowsAffected(); err == nil && rows > 0 {
		return delivery, db.PersistTask(ctx, taskID)
	}
	return delivery, nil
}

// TaskDeliveries lists a Task's deliveries in creation order.
func (db *DB) TaskDeliveries(ctx context.Context, taskID int64) ([]Delivery, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+deliveryColumns+` FROM deliveries WHERE task_id=? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deliveries := []Delivery{}
	for rows.Next() {
		delivery, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, rows.Err()
}

// DeliveryOperations lists a delivery's newest operations first.
func (db *DB) DeliveryOperations(ctx context.Context, deliveryID int64, limit int) ([]DeliveryOperation, error) {
	if limit < 1 {
		limit = -1
	}
	rows, err := db.QueryContext(ctx, `SELECT `+deliveryOperationColumns+` FROM delivery_operations WHERE delivery_id=? ORDER BY id DESC LIMIT ?`, deliveryID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	operations := []DeliveryOperation{}
	for rows.Next() {
		operation, err := scanDeliveryOperation(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, operation)
	}
	return operations, rows.Err()
}

func (db *DB) taskDeliveryOperations(ctx context.Context, taskID int64) ([]DeliveryOperation, error) {
	rows, err := db.QueryContext(ctx, `SELECT o.id,o.delivery_id,o.operation,o.actor,o.actor_pane,o.head_sha,o.subject,o.baseline,o.outcome,o.detail,o.created_at,o.updated_at FROM delivery_operations o JOIN deliveries d ON d.id=o.delivery_id WHERE d.task_id=? ORDER BY o.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	operations := []DeliveryOperation{}
	for rows.Next() {
		operation, err := scanDeliveryOperation(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, operation)
	}
	return operations, rows.Err()
}

// UnresolvedDeliveryOperation returns the newest pending or uncertain
// operation with the same delivery, operation and subject. A keyHead limits it
// to that PR head; an empty keyHead matches every head.
func (db *DB) UnresolvedDeliveryOperation(ctx context.Context, deliveryID int64, operation, keyHead, subject string) (DeliveryOperation, error) {
	return scanDeliveryOperation(db.QueryRowContext(ctx, `SELECT `+deliveryOperationColumns+` FROM delivery_operations WHERE delivery_id=? AND operation=? AND (?='' OR head_sha=?) AND subject=? AND outcome IN ('pending','uncertain') ORDER BY id DESC LIMIT 1`, deliveryID, operation, keyHead, keyHead, subject))
}

// BeginDeliveryOperation records a pending operation. Operations share a key
// when their delivery, operation and subject match and, with a keyHead, their
// head is keyHead. With limit > 0 it refuses once limit keyed operations
// succeeded or may have succeeded. It refuses while a keyed operation has an
// unknown outcome, so the caller observes that outcome first.
func (db *DB) BeginDeliveryOperation(ctx context.Context, operation DeliveryOperation, keyHead string, limit int) (DeliveryOperation, error) {
	if operation.DeliveryID < 1 || operation.Operation == "" || (operation.Actor != "lead" && operation.Actor != "user") {
		return DeliveryOperation{}, fmt.Errorf("invalid steward operation")
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return DeliveryOperation{}, err
	}
	defer tx.Rollback()
	var unresolved, applied int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(outcome IN ('pending','uncertain')),0), COALESCE(SUM(outcome IN ('pending','uncertain','succeeded')),0) FROM delivery_operations WHERE delivery_id=? AND operation=? AND (?='' OR head_sha=?) AND subject=?`, operation.DeliveryID, operation.Operation, keyHead, keyHead, operation.Subject).Scan(&unresolved, &applied); err != nil {
		return DeliveryOperation{}, err
	}
	if unresolved > 0 {
		return DeliveryOperation{}, ErrDeliveryOperationUnresolved
	}
	if limit > 0 && applied >= limit {
		return DeliveryOperation{}, ErrDeliveryOperationLimit
	}
	now := time.Now().UnixMilli()
	operation.Outcome, operation.CreatedAt, operation.UpdatedAt = "pending", now, now
	result, err := tx.ExecContext(ctx, `INSERT INTO delivery_operations(delivery_id,operation,actor,actor_pane,head_sha,subject,baseline,outcome,detail,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, operation.DeliveryID, operation.Operation, operation.Actor, operation.ActorPane, operation.HeadSHA, operation.Subject, operation.Baseline, operation.Outcome, operation.Detail, operation.CreatedAt, operation.UpdatedAt)
	if err != nil {
		return DeliveryOperation{}, err
	}
	if operation.ID, err = result.LastInsertId(); err != nil {
		return DeliveryOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveryOperation{}, err
	}
	return operation, db.persistDeliveryTask(ctx, operation.DeliveryID)
}

// RecordDeliveryOperation records an operation that has already completed,
// such as a read-only inspection.
func (db *DB) RecordDeliveryOperation(ctx context.Context, operation DeliveryOperation) (DeliveryOperation, error) {
	if operation.DeliveryID < 1 || operation.Operation == "" || (operation.Actor != "lead" && operation.Actor != "user") || operation.Outcome == "" || operation.Outcome == "pending" {
		return DeliveryOperation{}, fmt.Errorf("invalid steward operation")
	}
	now := time.Now().UnixMilli()
	operation.CreatedAt, operation.UpdatedAt = now, now
	result, err := db.ExecContext(ctx, `INSERT INTO delivery_operations(delivery_id,operation,actor,actor_pane,head_sha,subject,baseline,outcome,detail,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, operation.DeliveryID, operation.Operation, operation.Actor, operation.ActorPane, operation.HeadSHA, operation.Subject, operation.Baseline, operation.Outcome, operation.Detail, operation.CreatedAt, operation.UpdatedAt)
	if err != nil {
		return DeliveryOperation{}, err
	}
	if operation.ID, err = result.LastInsertId(); err != nil {
		return DeliveryOperation{}, err
	}
	return operation, db.persistDeliveryTask(ctx, operation.DeliveryID)
}

// ResolveDeliveryOperation settles a pending or uncertain operation. A
// settled operation is never changed again.
func (db *DB) ResolveDeliveryOperation(ctx context.Context, id int64, outcome, detail string) error {
	if outcome != "succeeded" && outcome != "failed" && outcome != "uncertain" {
		return fmt.Errorf("invalid steward operation outcome %q", outcome)
	}
	result, err := db.ExecContext(ctx, `UPDATE delivery_operations SET outcome=?,detail=?,updated_at=? WHERE id=? AND outcome IN ('pending','uncertain')`, outcome, detail, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil {
		return err
	} else if rows == 0 {
		return ErrStateRace
	}
	var deliveryID int64
	if err := db.QueryRowContext(ctx, `SELECT delivery_id FROM delivery_operations WHERE id=?`, id).Scan(&deliveryID); err != nil {
		return err
	}
	return db.persistDeliveryTask(ctx, deliveryID)
}

func (db *DB) persistDeliveryTask(ctx context.Context, deliveryID int64) error {
	var taskID int64
	if err := db.QueryRowContext(ctx, `SELECT task_id FROM deliveries WHERE id=?`, deliveryID).Scan(&taskID); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}
