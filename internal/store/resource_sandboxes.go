package store

import (
	"context"
	"database/sql"
)

type SandboxResourceOwner struct {
	ID          int64
	ProjectID   int64
	ProjectName string
	Number      int
	Path        string
	State       string
	TaskID      int64
	TaskSeq     int
}

type ResourceSandboxLease struct {
	ID        int64
	SandboxID int64
}

func (db *DB) SandboxResourceOwners(ctx context.Context) ([]SandboxResourceOwner, error) {
	rows, err := db.queries.SandboxResourceOwners(ctx)
	if err != nil {
		return nil, err
	}
	owners := make([]SandboxResourceOwner, 0, len(rows))
	for _, row := range rows {
		owners = append(owners, SandboxResourceOwner{ID: row.ID, ProjectID: row.ProjectID, ProjectName: row.Name,
			Number: int(row.N), Path: row.Path, State: row.State, TaskID: row.TaskID, TaskSeq: int(row.Seq)})
	}
	return owners, nil
}

func (db *DB) ResourceSandboxLeases(ctx context.Context) ([]ResourceSandboxLease, error) {
	rows, err := db.queries.ResourceSandboxLeases(ctx)
	if err != nil {
		return nil, err
	}
	leases := make([]ResourceSandboxLease, 0, len(rows))
	for _, row := range rows {
		if row.SandboxID.Valid {
			leases = append(leases, ResourceSandboxLease{ID: row.ID, SandboxID: row.SandboxID.Int64})
		}
	}
	return leases, nil
}

// ReleaseSandboxResourceLease is for a Sandbox whose durable state is already idle
// or whose row no longer exists. Held and quarantined Sandboxes remain leased.
func (db *DB) ReleaseSandboxResourceLease(ctx context.Context, sandboxID int64) error {
	var projectID int64
	err := db.QueryRowContext(ctx, `SELECT project_id FROM mounts WHERE id=? AND state NOT IN ('idle','pruning')`, sandboxID).Scan(&projectID)
	if err == nil {
		return ErrStateRace
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	lease, leaseErr := db.resourceLeaseBySandbox(ctx, sandboxID)
	if leaseErr == nil {
		projectID = lease.OwnerProjectID
	} else if leaseErr != sql.ErrNoRows {
		return leaseErr
	}
	if err := db.queries.DeleteResourceLeaseBySandbox(ctx, sql.NullInt64{Int64: sandboxID, Valid: true}); err != nil {
		return err
	}
	if projectID != 0 {
		return db.PersistProject(ctx, projectID)
	}
	return nil
}

func (db *DB) resourceLeaseBySandbox(ctx context.Context, sandboxID int64) (ResourceLease, error) {
	var lease ResourceLease
	err := db.QueryRowContext(ctx, `SELECT id,resource_type,scope_root,scope_path,owner_project_id,COALESCE(owner_task_id,0),owner,source_key,custody_token,intent_id,COALESCE(mount_id,0),owner_pid,owner_boot_id,owner_start_time,custody,acquired_at FROM resource_leases WHERE mount_id=?`, sandboxID).Scan(
		&lease.ID, &lease.ResourceType, &lease.ScopeRoot, &lease.ScopePath, &lease.OwnerProjectID, &lease.OwnerTaskID,
		&lease.Owner, &lease.SourceKey, &lease.CustodyToken, &lease.IntentID, &lease.SandboxID, &lease.OwnerPID,
		&lease.OwnerBootID, &lease.OwnerStartTime, &lease.Custody, &lease.AcquiredAt,
	)
	return lease, err
}

func (db *DB) releaseSandboxLeaseTx(ctx context.Context, tx *writeTx, sandboxID int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM resource_leases WHERE mount_id=?`, sandboxID)
	return err
}
