package store

import (
	"context"
	"database/sql"
)

type MountResourceOwner struct {
	ID          int64
	ProjectID   int64
	ProjectName string
	Number      int
	Path        string
	State       string
	TaskID      int64
	TaskSeq     int
}

type ResourceMountLease struct {
	ID      int64
	MountID int64
}

func (db *DB) MountResourceOwners(ctx context.Context) ([]MountResourceOwner, error) {
	rows, err := db.queries.MountResourceOwners(ctx)
	if err != nil {
		return nil, err
	}
	owners := make([]MountResourceOwner, 0, len(rows))
	for _, row := range rows {
		owners = append(owners, MountResourceOwner{ID: row.ID, ProjectID: row.ProjectID, ProjectName: row.Name,
			Number: int(row.N), Path: row.Path, State: row.State, TaskID: row.TaskID, TaskSeq: int(row.Seq)})
	}
	return owners, nil
}

func (db *DB) ResourceMountLeases(ctx context.Context) ([]ResourceMountLease, error) {
	rows, err := db.queries.ResourceMountLeases(ctx)
	if err != nil {
		return nil, err
	}
	leases := make([]ResourceMountLease, 0, len(rows))
	for _, row := range rows {
		if row.MountID.Valid {
			leases = append(leases, ResourceMountLease{ID: row.ID, MountID: row.MountID.Int64})
		}
	}
	return leases, nil
}

// ReleaseMountResourceLease is for a Mount whose durable state is already idle
// or whose row no longer exists. Held and quarantined Mounts remain leased.
func (db *DB) ReleaseMountResourceLease(ctx context.Context, mountID int64) error {
	var projectID int64
	err := db.QueryRowContext(ctx, `SELECT project_id FROM mounts WHERE id=? AND state NOT IN ('idle','pruning')`, mountID).Scan(&projectID)
	if err == nil {
		return ErrStateRace
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	lease, leaseErr := db.resourceLeaseByMount(ctx, mountID)
	if leaseErr == nil {
		projectID = lease.OwnerProjectID
	} else if leaseErr != sql.ErrNoRows {
		return leaseErr
	}
	if err := db.queries.DeleteResourceLeaseByMount(ctx, sql.NullInt64{Int64: mountID, Valid: true}); err != nil {
		return err
	}
	if projectID != 0 {
		return db.PersistProject(ctx, projectID)
	}
	return nil
}

func (db *DB) resourceLeaseByMount(ctx context.Context, mountID int64) (ResourceLease, error) {
	var lease ResourceLease
	err := db.QueryRowContext(ctx, `SELECT id,resource_type,scope_root,scope_path,owner_project_id,COALESCE(owner_task_id,0),owner,source_key,custody_token,intent_id,COALESCE(mount_id,0),owner_pid,owner_boot_id,owner_start_time,custody,acquired_at FROM resource_leases WHERE mount_id=?`, mountID).Scan(
		&lease.ID, &lease.ResourceType, &lease.ScopeRoot, &lease.ScopePath, &lease.OwnerProjectID, &lease.OwnerTaskID,
		&lease.Owner, &lease.SourceKey, &lease.CustodyToken, &lease.IntentID, &lease.MountID, &lease.OwnerPID,
		&lease.OwnerBootID, &lease.OwnerStartTime, &lease.Custody, &lease.AcquiredAt,
	)
	return lease, err
}

func (db *DB) releaseMountLeaseTx(ctx context.Context, tx *writeTx, mountID int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM resource_leases WHERE mount_id=?`, mountID)
	return err
}
