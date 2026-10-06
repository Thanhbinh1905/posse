package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/thanhbinh1905/posse/internal/atomicfile"
	"golang.org/x/sys/unix"
)

type NoticeDelivery struct {
	DeliveryID  string  `toml:"delivery_id" json:"delivery_id"`
	BatchID     string  `toml:"batch_id" json:"batch_id"`
	ProjectID   int64   `toml:"project_id" json:"project_id"`
	NoticeIDs   []int64 `toml:"notice_ids" json:"notice_ids"`
	Destination string  `toml:"destination" json:"destination"`
	Generation  string  `toml:"generation" json:"generation"`
	State       string  `toml:"state" json:"state"`
	OwnerToken  string  `toml:"owner_token" json:"owner_token"`
	ClaimedAt   int64   `toml:"claimed_at" json:"claimed_at"`
	LeaseUntil  int64   `toml:"lease_until" json:"lease_until"`
	AcceptedAt  int64   `toml:"accepted_at" json:"accepted_at"`
	CreatedAt   int64   `toml:"created_at" json:"created_at"`
	UpdatedAt   int64   `toml:"updated_at" json:"updated_at"`
}

type NoticeDeliverySnapshot struct {
	Version    int              `toml:"version"`
	Project    Project          `toml:"project"`
	Notices    []Notice         `toml:"notices,omitempty"`
	Deliveries []NoticeDelivery `toml:"deliveries,omitempty"`
}

func NoticeBatchID(projectID int64, ids []int64) (string, error) {
	ids, err := normalizeNoticeIDs(ids)
	if err != nil {
		return "", err
	}
	data, _ := json.Marshal(ids)
	sum := sha256.Sum256(append([]byte(fmt.Sprintf("posse-notice-batch-v1:%d:", projectID)), data...))
	return hex.EncodeToString(sum[:16]), nil
}

func NoticeDeliveryID(batchID, destination string) string {
	sum := sha256.Sum256([]byte("posse-notice-delivery-v1:" + batchID + ":" + destination))
	return hex.EncodeToString(sum[:16])
}

func normalizeNoticeIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, errors.New("notice delivery requires at least one Notice ID")
	}
	normalized := append([]int64(nil), ids...)
	sort.Slice(normalized, func(i, j int) bool { return normalized[i] < normalized[j] })
	for index, id := range normalized {
		if id < 1 || index > 0 && normalized[index-1] == id {
			return nil, errors.New("notice delivery IDs must be positive and unique")
		}
	}
	return normalized, nil
}

// ClaimNoticeDelivery records ownership before a batch is printed. The stable
// delivery ID names the same batch for one destination; a new owner can take
// over only after the previous lease expires.
func (db *DB) ClaimNoticeDelivery(ctx context.Context, projectID int64, ids []int64, destination, generation, token string, now, leaseMillis int64) (NoticeDelivery, bool, error) {
	ids, err := normalizeNoticeIDs(ids)
	if err != nil {
		return NoticeDelivery{}, false, err
	}
	if projectID < 1 || strings.TrimSpace(destination) == "" || strings.TrimSpace(generation) == "" || token == "" || leaseMillis < 1 {
		return NoticeDelivery{}, false, errors.New("notice delivery requires a Project, destination, generation and lease")
	}
	batchID, err := NoticeBatchID(projectID, ids)
	if err != nil {
		return NoticeDelivery{}, false, err
	}
	deliveryID := NoticeDeliveryID(batchID, destination)
	encodedIDs, err := json.Marshal(ids)
	if err != nil {
		return NoticeDelivery{}, false, err
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return NoticeDelivery{}, false, err
	}
	defer tx.Rollback()

	var delivery NoticeDelivery
	var rawIDs string
	var found bool
	err = tx.QueryRowContext(ctx, `SELECT delivery_id,batch_id,project_id,notice_ids_json,destination,generation,state,owner_token,claimed_at,lease_until,accepted_at,created_at,updated_at
		FROM notice_delivery_receipts WHERE delivery_id=?`, deliveryID).Scan(&delivery.DeliveryID, &delivery.BatchID, &delivery.ProjectID, &rawIDs, &delivery.Destination, &delivery.Generation, &delivery.State, &delivery.OwnerToken, &delivery.ClaimedAt, &delivery.LeaseUntil, &delivery.AcceptedAt, &delivery.CreatedAt, &delivery.UpdatedAt)
	if err == nil {
		found = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return NoticeDelivery{}, false, err
	}
	var competing int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notice_delivery_receipts WHERE project_id=? AND destination=? AND delivery_id<>? AND state IN ('claimed','printed'))`, projectID, destination, deliveryID).Scan(&competing); err != nil {
		return NoticeDelivery{}, false, err
	}
	if competing != 0 {
		return NoticeDelivery{}, false, nil
	}
	if found {
		if err := json.Unmarshal([]byte(rawIDs), &delivery.NoticeIDs); err != nil {
			return NoticeDelivery{}, false, fmt.Errorf("decode Notice delivery IDs: %w", err)
		}
		if !equalInt64s(delivery.NoticeIDs, ids) {
			return NoticeDelivery{}, false, errors.New("stable Notice delivery ID has different Notice IDs")
		}
		if delivery.State == "accepted" || delivery.State == "uncertain" {
			return delivery, false, nil
		}
		if delivery.OwnerToken != token && delivery.LeaseUntil > now {
			return delivery, false, nil
		}
		if err := claimDeliveryNotices(ctx, tx, projectID, ids, delivery.OwnerToken, token, now); err != nil {
			return NoticeDelivery{}, false, err
		}
		state := delivery.State
		if state == "rejected" {
			state = "claimed"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notice_delivery_receipts
			SET generation=?,state=?,owner_token=?,claimed_at=?,lease_until=?,updated_at=? WHERE delivery_id=?`, generation, state, token, now, now+leaseMillis, now, deliveryID); err != nil {
			return NoticeDelivery{}, false, err
		}
		delivery.Generation, delivery.State, delivery.OwnerToken = generation, state, token
		delivery.ClaimedAt, delivery.LeaseUntil, delivery.UpdatedAt = now, now+leaseMillis, now
	} else {
		if err := claimDeliveryNotices(ctx, tx, projectID, ids, "", token, now); err != nil {
			return NoticeDelivery{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notice_delivery_receipts
			(delivery_id,batch_id,project_id,notice_ids_json,destination,generation,state,owner_token,claimed_at,lease_until,created_at,updated_at)
			VALUES(?,?,?,?,?,?,'claimed',?,?,?,?,?)`, deliveryID, batchID, projectID, string(encodedIDs), destination, generation, token, now, now+leaseMillis, now, now); err != nil {
			return NoticeDelivery{}, false, err
		}
		delivery = NoticeDelivery{DeliveryID: deliveryID, BatchID: batchID, ProjectID: projectID, NoticeIDs: ids, Destination: destination, Generation: generation, State: "claimed", OwnerToken: token, ClaimedAt: now, LeaseUntil: now + leaseMillis, CreatedAt: now, UpdatedAt: now}
	}
	if err := tx.Commit(); err != nil {
		return NoticeDelivery{}, false, err
	}
	if err := db.PersistNoticeDeliverySnapshot(ctx, projectID); err != nil {
		return delivery, false, err
	}
	return delivery, true, nil
}

func claimDeliveryNotices(ctx context.Context, tx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, projectID int64, ids []int64, oldToken, token string, now int64) error {
	query := `UPDATE notices SET claim_token=?,claimed_at=? WHERE project_id=? AND delivered_at IS NULL AND acked_at IS NULL AND id IN (` + sqlPlaceholders(len(ids)) + `) AND (claim_token='' OR claim_token=?)`
	args := []any{token, now, projectID}
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, oldToken)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != int64(len(ids)) {
		return ErrStateRace
	}
	return nil
}

func equalInt64s(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (db *DB) NoticeDelivery(ctx context.Context, deliveryID string) (NoticeDelivery, error) {
	var delivery NoticeDelivery
	var rawIDs string
	err := db.QueryRowContext(ctx, `SELECT delivery_id,batch_id,project_id,notice_ids_json,destination,generation,state,owner_token,claimed_at,lease_until,accepted_at,created_at,updated_at
		FROM notice_delivery_receipts WHERE delivery_id=?`, deliveryID).Scan(&delivery.DeliveryID, &delivery.BatchID, &delivery.ProjectID, &rawIDs, &delivery.Destination, &delivery.Generation, &delivery.State, &delivery.OwnerToken, &delivery.ClaimedAt, &delivery.LeaseUntil, &delivery.AcceptedAt, &delivery.CreatedAt, &delivery.UpdatedAt)
	if err != nil {
		return NoticeDelivery{}, err
	}
	if err := json.Unmarshal([]byte(rawIDs), &delivery.NoticeIDs); err != nil {
		return NoticeDelivery{}, fmt.Errorf("decode Notice delivery IDs: %w", err)
	}
	return delivery, nil
}

func (db *DB) NoticesByIDs(ctx context.Context, projectID int64, ids []int64) ([]Notice, error) {
	ids, err := normalizeNoticeIDs(ids)
	if err != nil {
		return nil, err
	}
	query := `SELECT id,project_id,COALESCE(task_id,0),kind,summary,data_json,created_at,COALESCE(delivered_at,0),COALESCE(acked_at,0)
		FROM notices WHERE project_id=? AND id IN (` + sqlPlaceholders(len(ids)) + `) ORDER BY id`
	args := []any{projectID}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	notices := make([]Notice, 0, len(ids))
	for rows.Next() {
		var notice Notice
		if err := rows.Scan(&notice.ID, &notice.ProjectID, &notice.TaskID, &notice.Kind, &notice.Summary, &notice.DataJSON, &notice.CreatedAt, &notice.DeliveredAt, &notice.AckedAt); err != nil {
			return nil, err
		}
		notices = append(notices, notice)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(notices) != len(ids) {
		return nil, ErrStateRace
	}
	return notices, nil
}

func (db *DB) NoticeDeliveryForDestination(ctx context.Context, projectID int64, destination string) (NoticeDelivery, bool, error) {
	var deliveryID string
	err := db.QueryRowContext(ctx, `SELECT delivery_id FROM notice_delivery_receipts
		WHERE project_id=? AND destination=? AND state IN ('claimed','printed','rejected','uncertain')
		ORDER BY updated_at DESC,delivery_id LIMIT 1`, projectID, destination).Scan(&deliveryID)
	if errors.Is(err, sql.ErrNoRows) {
		return NoticeDelivery{}, false, nil
	}
	if err != nil {
		return NoticeDelivery{}, false, err
	}
	delivery, err := db.NoticeDelivery(ctx, deliveryID)
	return delivery, true, err
}

func (db *DB) MarkNoticeDeliveryPrinted(ctx context.Context, deliveryID, token string, at int64) error {
	result, err := db.ExecContext(ctx, `UPDATE notice_delivery_receipts SET state='printed',updated_at=?
		WHERE delivery_id=? AND owner_token=? AND state IN ('claimed','printed') AND lease_until>?`, at, deliveryID, token, at)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrStateRace
	}
	delivery, err := db.NoticeDelivery(ctx, deliveryID)
	if err != nil {
		return err
	}
	return db.PersistNoticeDeliverySnapshot(ctx, delivery.ProjectID)
}

// ResolveNoticeDelivery records the adapter's known outcome. An uncertain
// receipt remains held and creates a separate actionable Notice; it is never
// released for blind retry.
func (db *DB) ResolveNoticeDelivery(ctx context.Context, deliveryID, token, outcome string, at int64) error {
	if outcome != "accepted" && outcome != "rejected" && outcome != "uncertain" {
		return fmt.Errorf("invalid Notice delivery outcome %q", outcome)
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var delivery NoticeDelivery
	var rawIDs string
	if err := tx.QueryRowContext(ctx, `SELECT delivery_id,batch_id,project_id,notice_ids_json,destination,generation,state,owner_token,claimed_at,lease_until,accepted_at,created_at,updated_at
		FROM notice_delivery_receipts WHERE delivery_id=?`, deliveryID).Scan(&delivery.DeliveryID, &delivery.BatchID, &delivery.ProjectID, &rawIDs, &delivery.Destination, &delivery.Generation, &delivery.State, &delivery.OwnerToken, &delivery.ClaimedAt, &delivery.LeaseUntil, &delivery.AcceptedAt, &delivery.CreatedAt, &delivery.UpdatedAt); err != nil {
		return err
	}
	if delivery.OwnerToken != token || delivery.LeaseUntil <= at || delivery.State != "printed" && delivery.State != "claimed" {
		return ErrStateRace
	}
	if err := json.Unmarshal([]byte(rawIDs), &delivery.NoticeIDs); err != nil {
		return fmt.Errorf("decode Notice delivery IDs: %w", err)
	}
	var nextState string
	switch outcome {
	case "accepted":
		nextState = "accepted"
		query := `UPDATE notices SET delivered_at=?,claim_token='',claimed_at=0 WHERE project_id=? AND delivered_at IS NULL AND acked_at IS NULL AND claim_token=? AND id IN (` + sqlPlaceholders(len(delivery.NoticeIDs)) + `)`
		args := []any{at, delivery.ProjectID, token}
		for _, id := range delivery.NoticeIDs {
			args = append(args, id)
		}
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != int64(len(delivery.NoticeIDs)) {
			return ErrStateRace
		}
	case "rejected":
		nextState = "rejected"
		query := `UPDATE notices SET claim_token='',claimed_at=0 WHERE project_id=? AND claim_token=? AND id IN (` + sqlPlaceholders(len(delivery.NoticeIDs)) + `)`
		args := []any{delivery.ProjectID, token}
		for _, id := range delivery.NoticeIDs {
			args = append(args, id)
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	case "uncertain":
		nextState = "uncertain"
		uncertainToken := "uncertain:" + deliveryID
		query := `UPDATE notices SET claim_token=?,claimed_at=? WHERE project_id=? AND claim_token=? AND id IN (` + sqlPlaceholders(len(delivery.NoticeIDs)) + `)`
		args := []any{uncertainToken, at, delivery.ProjectID, token}
		for _, id := range delivery.NoticeIDs {
			args = append(args, id)
		}
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != int64(len(delivery.NoticeIDs)) {
			return ErrStateRace
		}
		data, _ := json.Marshal(map[string]any{"delivery_id": deliveryID, "batch_id": delivery.BatchID, "notice_ids": delivery.NoticeIDs})
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notices WHERE project_id=? AND kind='notice_delivery_uncertain' AND json_extract(data_json,'$.delivery_id')=?)`, delivery.ProjectID, deliveryID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO notices(project_id,kind,summary,data_json,created_at) VALUES(?,'notice_delivery_uncertain',?,?,?)`, delivery.ProjectID, fmt.Sprintf("Notice receipt %s for batch %s may have reached %s; inspect the Lead session before retrying IDs %s, then explicitly resolve the receipt as accepted or rejected", deliveryID, delivery.BatchID, delivery.Destination, formatNoticeIDs(delivery.NoticeIDs)), string(data), at); err != nil {
				return err
			}
		}
	}
	query := `UPDATE notice_delivery_receipts SET state=?,owner_token='',lease_until=0,accepted_at=CASE WHEN ?='accepted' THEN ? ELSE accepted_at END,updated_at=? WHERE delivery_id=? AND owner_token=?`
	result, err := tx.ExecContext(ctx, query, nextState, nextState, at, at, deliveryID, token)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return ErrStateRace
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistNoticeDeliverySnapshot(ctx, delivery.ProjectID)
}

func (db *DB) ResolveUncertainNoticeDelivery(ctx context.Context, deliveryID, outcome string, at int64) error {
	if outcome != "accepted" && outcome != "rejected" {
		return fmt.Errorf("uncertain Notice delivery must be resolved as accepted or rejected, got %q", outcome)
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var delivery NoticeDelivery
	var rawIDs string
	if err := tx.QueryRowContext(ctx, `SELECT delivery_id,batch_id,project_id,notice_ids_json,destination,generation,state,owner_token,claimed_at,lease_until,accepted_at,created_at,updated_at
		FROM notice_delivery_receipts WHERE delivery_id=?`, deliveryID).Scan(&delivery.DeliveryID, &delivery.BatchID, &delivery.ProjectID, &rawIDs, &delivery.Destination, &delivery.Generation, &delivery.State, &delivery.OwnerToken, &delivery.ClaimedAt, &delivery.LeaseUntil, &delivery.AcceptedAt, &delivery.CreatedAt, &delivery.UpdatedAt); err != nil {
		return err
	}
	if delivery.State != "uncertain" {
		return ErrStateRace
	}
	if err := json.Unmarshal([]byte(rawIDs), &delivery.NoticeIDs); err != nil {
		return fmt.Errorf("decode Notice delivery IDs: %w", err)
	}
	query := `UPDATE notices SET delivered_at=CASE WHEN ?='accepted' THEN ? ELSE delivered_at END,claim_token='',claimed_at=0
		WHERE project_id=? AND delivered_at IS NULL AND acked_at IS NULL AND claim_token=? AND id IN (` + sqlPlaceholders(len(delivery.NoticeIDs)) + `)`
	args := []any{outcome, at, delivery.ProjectID, "uncertain:" + deliveryID}
	for _, id := range delivery.NoticeIDs {
		args = append(args, id)
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != int64(len(delivery.NoticeIDs)) {
		return ErrStateRace
	}
	result, err = tx.ExecContext(ctx, `UPDATE notice_delivery_receipts SET state=?,accepted_at=CASE WHEN ?='accepted' THEN ? ELSE accepted_at END,updated_at=? WHERE delivery_id=? AND state='uncertain'`, outcome, outcome, at, at, deliveryID)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return ErrStateRace
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notices SET acked_at=COALESCE(acked_at,?) WHERE project_id=? AND kind='notice_delivery_uncertain' AND json_extract(data_json,'$.delivery_id')=?`, at, delivery.ProjectID, deliveryID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistNoticeDeliverySnapshot(ctx, delivery.ProjectID)
}

func formatNoticeIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return strings.Join(parts, ",")
}

func (db *DB) PersistNoticeDeliverySnapshotIfPresent(ctx context.Context, projectID int64) error {
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notice_delivery_receipts WHERE project_id=?)`, projectID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	return db.PersistNoticeDeliverySnapshot(ctx, projectID)
}

func (db *DB) PersistNoticeDeliverySnapshot(ctx context.Context, projectID int64) error {
	project, err := db.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	lockPath := db.NoticeDeliverySnapshotPath(project.Name) + ".lock"
	lock, err := acquireNoticeSnapshotLock(ctx, lockPath)
	if err != nil {
		return err
	}
	defer func() {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
	}()
	snapshot := NoticeDeliverySnapshot{Version: 1, Project: project}
	rows, err := db.QueryContext(ctx, `SELECT delivery_id,batch_id,project_id,notice_ids_json,destination,generation,state,owner_token,claimed_at,lease_until,accepted_at,created_at,updated_at
		FROM notice_delivery_receipts WHERE project_id=? ORDER BY created_at,delivery_id`, projectID)
	if err != nil {
		return err
	}
	noticeIDs := map[int64]bool{}
	for rows.Next() {
		var delivery NoticeDelivery
		var rawIDs string
		if err := rows.Scan(&delivery.DeliveryID, &delivery.BatchID, &delivery.ProjectID, &rawIDs, &delivery.Destination, &delivery.Generation, &delivery.State, &delivery.OwnerToken, &delivery.ClaimedAt, &delivery.LeaseUntil, &delivery.AcceptedAt, &delivery.CreatedAt, &delivery.UpdatedAt); err != nil {
			_ = rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(rawIDs), &delivery.NoticeIDs); err != nil {
			_ = rows.Close()
			return fmt.Errorf("decode Notice delivery IDs: %w", err)
		}
		for _, id := range delivery.NoticeIDs {
			noticeIDs[id] = true
		}
		snapshot.Deliveries = append(snapshot.Deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(noticeIDs) > 0 {
		all, err := db.Notices(ctx, projectID, false)
		if err != nil {
			return err
		}
		for _, notice := range all {
			if noticeIDs[notice.ID] || notice.Kind == "notice_delivery_uncertain" {
				snapshot.Notices = append(snapshot.Notices, notice)
			}
		}
	}
	var encoded strings.Builder
	if err := toml.NewEncoder(&encoded).Encode(snapshot); err != nil {
		return err
	}
	return atomicfile.Write(db.NoticeDeliverySnapshotPath(project.Name), []byte(encoded.String()), 0o600)
}

func acquireNoticeSnapshotLock(ctx context.Context, path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (db *DB) RestoreNoticeDeliverySnapshot(ctx context.Context, tx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, snapshot NoticeDeliverySnapshot) error {
	if snapshot.Version != 1 || snapshot.Project.ID == 0 || snapshot.Project.Name == "" {
		return errors.New("invalid Notice delivery snapshot")
	}
	for _, notice := range snapshot.Notices {
		if notice.ProjectID != snapshot.Project.ID || notice.ID < 1 {
			return errors.New("notice delivery snapshot contains a mismatched Notice")
		}
		var taskID any
		if notice.TaskID != 0 {
			taskID = notice.TaskID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notices(id,project_id,task_id,kind,summary,data_json,created_at,delivered_at,acked_at)
			VALUES(?,?,?,?,?,?,?,?,?)`, notice.ID, notice.ProjectID, taskID, notice.Kind, notice.Summary, notice.DataJSON, notice.CreatedAt, nullableInt64Value(notice.DeliveredAt), nullableInt64Value(notice.AckedAt)); err != nil {
			return err
		}
	}
	for _, delivery := range snapshot.Deliveries {
		batchID, batchErr := NoticeBatchID(delivery.ProjectID, delivery.NoticeIDs)
		if batchErr != nil || delivery.ProjectID != snapshot.Project.ID || delivery.BatchID != batchID || delivery.DeliveryID != NoticeDeliveryID(delivery.BatchID, delivery.Destination) {
			return errors.New("notice delivery snapshot contains a mismatched delivery")
		}
		ids, err := normalizeNoticeIDs(delivery.NoticeIDs)
		if err != nil {
			return err
		}
		encoded, _ := json.Marshal(ids)
		if _, err := tx.ExecContext(ctx, `INSERT INTO notice_delivery_receipts
			(delivery_id,batch_id,project_id,notice_ids_json,destination,generation,state,owner_token,claimed_at,lease_until,accepted_at,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, delivery.DeliveryID, delivery.BatchID, delivery.ProjectID, string(encoded), delivery.Destination, delivery.Generation, delivery.State, delivery.OwnerToken, delivery.ClaimedAt, delivery.LeaseUntil, delivery.AcceptedAt, delivery.CreatedAt, delivery.UpdatedAt); err != nil {
			return err
		}
		if delivery.State == "claimed" || delivery.State == "printed" {
			for _, id := range ids {
				if _, err := tx.ExecContext(ctx, `UPDATE notices SET claim_token=?,claimed_at=? WHERE id=? AND project_id=? AND delivered_at IS NULL AND acked_at IS NULL`, delivery.OwnerToken, delivery.ClaimedAt, id, delivery.ProjectID); err != nil {
					return err
				}
			}
		} else if delivery.State == "uncertain" {
			for _, id := range ids {
				if _, err := tx.ExecContext(ctx, `UPDATE notices SET claim_token=?,claimed_at=? WHERE id=? AND project_id=? AND delivered_at IS NULL AND acked_at IS NULL`, "uncertain:"+delivery.DeliveryID, delivery.UpdatedAt, id, delivery.ProjectID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func nullableInt64Value(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}
