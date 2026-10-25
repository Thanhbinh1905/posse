package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	dbgen "github.com/thanhbinh1905/posse/internal/store/sqlc"
)

const (
	ResourcePath   = "path"
	ResourceGitRef = "git_ref"

	ResourceCustodyHeld     = "held"
	ResourceCustodyRecovery = "recovery"
)

var ErrResourceConflict = errors.New("resource lease conflicts with an existing owner")

// ResourceLeaseConflict identifies the owner that currently holds an
// overlapping resource. The lease remains intact when acquisition is refused.
type ResourceLeaseConflict struct {
	Lease ResourceLease
}

func (e *ResourceLeaseConflict) Error() string {
	return fmt.Sprintf("%v: %s owns %s %s", ErrResourceConflict, e.Lease.Owner, e.Lease.ResourceType, e.Lease.Scope())
}

func (e *ResourceLeaseConflict) Unwrap() error { return ErrResourceConflict }

type ResourceLease struct {
	ID             int64
	ResourceType   string
	ScopeRoot      string
	ScopePath      string
	OwnerProjectID int64
	OwnerTaskID    int64
	Owner          string
	SourceKey      string
	CustodyToken   string
	IntentID       int64
	SandboxID      int64
	OwnerPID       int
	OwnerBootID    string
	OwnerStartTime string
	Custody        string
	AcquiredAt     int64
}

func (l ResourceLease) Scope() string {
	if l.ResourceType == ResourceGitRef {
		return l.ScopeRoot + "#" + l.ScopePath
	}
	return l.ScopePath
}

type ResourceLeaseRequest struct {
	ResourceType   string
	ScopeRoot      string
	ScopePath      string
	OwnerProjectID int64
	OwnerTaskID    int64
	Owner          string
	SourceKey      string
	IntentID       int64
	SandboxID      int64
}

func (db *DB) AcquireResourceLease(ctx context.Context, request ResourceLeaseRequest) (ResourceLease, error) {
	if err := validateResourceLeaseRequest(request); err != nil {
		return ResourceLease{}, err
	}
	token, err := newResourceLeaseToken()
	if err != nil {
		return ResourceLease{}, err
	}
	return db.claimResourceLease(ctx, request, token, false)
}

// EnsureSandboxResourceLease migrates and refreshes legacy Sandbox ownership into
// the shared lease table. Its stable source key makes reconciliation idempotent.
func (db *DB) EnsureSandboxResourceLease(ctx context.Context, request ResourceLeaseRequest) (ResourceLease, error) {
	if request.SandboxID == 0 {
		return ResourceLease{}, fmt.Errorf("sandbox resource lease requires a Sandbox id")
	}
	if err := validateResourceLeaseRequest(request); err != nil {
		return ResourceLease{}, err
	}
	// Every reconciliation ensures every held Sandbox. Confirm an existing
	// lease from a read so routine passes do not queue for the write lock.
	if existing, err := db.queries.ResourceLeaseBySource(ctx, request.SourceKey); err == nil {
		if lease := resourceLeaseFromSource(existing); sameSandboxResourceLease(lease, request) {
			return lease, nil
		}
	} else if !IsNotFound(err) {
		return ResourceLease{}, err
	}
	token := fmt.Sprintf("sandbox:%d", request.SandboxID)
	return db.claimResourceLease(ctx, request, token, true)
}

func (db *DB) claimResourceLease(ctx context.Context, request ResourceLeaseRequest, token string, ensure bool) (ResourceLease, error) {
	pid := os.Getpid()
	bootID, startTime, _ := ProcessIdentityForPID(pid)
	now := time.Now().UnixMilli()
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return ResourceLease{}, err
	}
	defer tx.Rollback()
	lease, inserted, err := claimResourceLeaseTx(ctx, tx, request, token, ensure, pid, bootID, startTime, now)
	if err != nil {
		return ResourceLease{}, err
	}
	if err := tx.Commit(); err != nil {
		return ResourceLease{}, err
	}
	if !inserted {
		return lease, nil
	}
	if err := db.PersistProject(ctx, request.OwnerProjectID); err != nil {
		return lease, err
	}
	return lease, nil
}

// AcquireResourceLeases claims every request in one transaction: either all
// leases are recorded or none is. Each affected Project snapshot is rewritten
// once, after the commit.
func (db *DB) AcquireResourceLeases(ctx context.Context, requests []ResourceLeaseRequest) ([]ResourceLease, error) {
	for _, request := range requests {
		if err := validateResourceLeaseRequest(request); err != nil {
			return nil, err
		}
	}
	if len(requests) == 0 {
		return nil, nil
	}
	tokens := make([]string, len(requests))
	for i := range requests {
		token, err := newResourceLeaseToken()
		if err != nil {
			return nil, err
		}
		tokens[i] = token
	}
	pid := os.Getpid()
	bootID, startTime, _ := ProcessIdentityForPID(pid)
	now := time.Now().UnixMilli()
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	leases := make([]ResourceLease, 0, len(requests))
	for i, request := range requests {
		lease, _, err := claimResourceLeaseTx(ctx, tx, request, tokens[i], false, pid, bootID, startTime, now)
		if err != nil {
			return nil, err
		}
		leases = append(leases, lease)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if err := db.persistProjects(ctx, resourceLeaseProjects(leases)); err != nil {
		return leases, err
	}
	return leases, nil
}

// claimResourceLeaseTx records one lease inside tx. inserted is false when ensure
// matched an existing identical lease.
func claimResourceLeaseTx(ctx context.Context, tx *writeTx, request ResourceLeaseRequest, token string, ensure bool, pid int, bootID, startTime string, now int64) (lease ResourceLease, inserted bool, err error) {
	queries := dbgen.New(tx)
	if request.IntentID != 0 {
		var intentExists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM intents WHERE id=?)`, request.IntentID).Scan(&intentExists); err != nil {
			return ResourceLease{}, false, err
		}
		if !intentExists {
			return ResourceLease{}, false, ErrStateRace
		}
	}
	if existing, lookupErr := queries.ResourceLeaseBySource(ctx, request.SourceKey); lookupErr == nil {
		lease := resourceLeaseFromSource(existing)
		if !ensure || !sameSandboxResourceLease(lease, request) {
			return ResourceLease{}, false, &ResourceLeaseConflict{Lease: lease}
		}
		return lease, false, nil
	} else if !IsNotFound(lookupErr) {
		return ResourceLease{}, false, lookupErr
	}

	conflicts, err := queries.ResourceLeasesByScope(ctx, dbgen.ResourceLeasesByScopeParams{ResourceType: request.ResourceType, ScopeRoot: request.ScopeRoot})
	if err != nil {
		return ResourceLease{}, false, err
	}
	for _, current := range conflicts {
		lease := resourceLeaseFromScope(current)
		if resourceScopesOverlap(request.ScopePath, lease.ScopePath) {
			return ResourceLease{}, false, &ResourceLeaseConflict{Lease: lease}
		}
	}
	result, err := queries.InsertResourceLease(ctx, dbgen.InsertResourceLeaseParams{
		ResourceType: request.ResourceType, ScopeRoot: request.ScopeRoot, ScopePath: request.ScopePath,
		OwnerProjectID: request.OwnerProjectID, NULLIF: request.OwnerTaskID, Owner: request.Owner,
		SourceKey: request.SourceKey, CustodyToken: token, IntentID: request.IntentID,
		NULLIF_2: request.SandboxID, OwnerPid: int64(pid), OwnerBootID: bootID,
		OwnerStartTime: startTime, Custody: ResourceCustodyHeld, AcquiredAt: now,
	})
	if err != nil {
		return ResourceLease{}, false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ResourceLease{}, false, err
	}
	return ResourceLease{
		ID: id, ResourceType: request.ResourceType, ScopeRoot: request.ScopeRoot, ScopePath: request.ScopePath,
		OwnerProjectID: request.OwnerProjectID, OwnerTaskID: request.OwnerTaskID, Owner: request.Owner,
		SourceKey: request.SourceKey, CustodyToken: token, IntentID: request.IntentID, SandboxID: request.SandboxID,
		OwnerPID: pid, OwnerBootID: bootID, OwnerStartTime: startTime, Custody: ResourceCustodyHeld, AcquiredAt: now,
	}, true, nil
}

func resourceLeaseProjects(leases []ResourceLease) []int64 {
	var projects []int64
	seen := map[int64]bool{}
	for _, lease := range leases {
		if !seen[lease.OwnerProjectID] {
			seen[lease.OwnerProjectID] = true
			projects = append(projects, lease.OwnerProjectID)
		}
	}
	return projects
}

func (db *DB) persistProjects(ctx context.Context, projects []int64) error {
	var persistErr error
	for _, projectID := range projects {
		if err := db.PersistProject(ctx, projectID); err != nil {
			persistErr = errors.Join(persistErr, err)
		}
	}
	return persistErr
}

func sameSandboxResourceLease(lease ResourceLease, request ResourceLeaseRequest) bool {
	return lease.ResourceType == request.ResourceType && lease.ScopeRoot == request.ScopeRoot && lease.ScopePath == request.ScopePath && lease.OwnerProjectID == request.OwnerProjectID && lease.SandboxID == request.SandboxID
}

func validateResourceLeaseRequest(request ResourceLeaseRequest) error {
	if request.ResourceType != ResourcePath && request.ResourceType != ResourceGitRef {
		return fmt.Errorf("invalid resource type %q", request.ResourceType)
	}
	if request.ResourceType == ResourcePath && request.ScopeRoot != "" || request.ResourceType == ResourceGitRef && request.ScopeRoot == "" {
		return fmt.Errorf("resource scope root is invalid for %s", request.ResourceType)
	}
	if request.ScopePath == "" || request.OwnerProjectID < 1 || strings.TrimSpace(request.Owner) == "" || strings.TrimSpace(request.SourceKey) == "" {
		return fmt.Errorf("resource scope, Project, owner and source key are required")
	}
	if request.ResourceType == ResourcePath && (!filepath.IsAbs(request.ScopePath) || filepath.Clean(request.ScopePath) != request.ScopePath) {
		return fmt.Errorf("path resource scope must be canonical and absolute: %q", request.ScopePath)
	}
	if request.ResourceType == ResourceGitRef && (filepath.Clean(request.ScopeRoot) != request.ScopeRoot || strings.HasPrefix(request.ScopePath, "/") || strings.Contains(request.ScopePath, "\\")) {
		return fmt.Errorf("git ref resource scope must use a canonical common directory and relative ref")
	}
	if request.OwnerTaskID < 0 || request.IntentID < 0 || request.SandboxID < 0 {
		return fmt.Errorf("resource owner identifiers cannot be negative")
	}
	return nil
}

func resourceScopesOverlap(left, right string) bool {
	left, right = strings.TrimSuffix(left, "/"), strings.TrimSuffix(right, "/")
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

func newResourceLeaseToken() (string, error) {
	var bytes [24]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate resource lease custody token: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}

// ReleaseResourceLeases deletes every lease in one transaction and rewrites each
// affected Project snapshot once. A lease whose custody token no longer matches
// fails the whole release with ErrStateRace and leaves all leases in place.
func (db *DB) ReleaseResourceLeases(ctx context.Context, leases []ResourceLease) error {
	if len(leases) == 0 {
		return nil
	}
	for _, lease := range leases {
		if lease.ID < 1 || lease.CustodyToken == "" {
			return fmt.Errorf("resource lease id and custody token are required")
		}
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbgen.New(tx)
	projects := make([]int64, 0, len(leases))
	for index := len(leases) - 1; index >= 0; index-- {
		lease := leases[index]
		result, err := queries.DeleteResourceLease(ctx, dbgen.DeleteResourceLeaseParams{ID: lease.ID, CustodyToken: lease.CustodyToken})
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
		projects = append(projects, lease.OwnerProjectID)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.persistProjects(ctx, dedupeInt64(projects))
}

func dedupeInt64(values []int64) []int64 {
	var out []int64
	seen := map[int64]bool{}
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func (db *DB) ReleaseResourceLease(ctx context.Context, id int64, token string) error {
	if id < 1 || token == "" {
		return fmt.Errorf("resource lease id and custody token are required")
	}
	lease, err := db.resourceLeaseByID(ctx, id)
	if err != nil {
		return err
	}
	result, err := db.queries.DeleteResourceLease(ctx, dbgen.DeleteResourceLeaseParams{ID: id, CustodyToken: token})
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
	return db.PersistProject(ctx, lease.OwnerProjectID)
}

// ReconcileResourceLease releases a durable claim only after its caller has
// reconciled the effect or custody represented by the lease.
func (db *DB) ReconcileResourceLease(ctx context.Context, id int64, token, reconciliation string) error {
	if strings.TrimSpace(reconciliation) == "" {
		return fmt.Errorf("resource lease reconciliation evidence is required")
	}
	if id < 1 || token == "" {
		return fmt.Errorf("resource lease id and custody token are required")
	}
	lease, err := db.resourceLeaseByID(ctx, id)
	if err != nil {
		return err
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := dbgen.New(tx).MarkResourceLeaseForRecovery(ctx, dbgen.MarkResourceLeaseForRecoveryParams{ID: id, CustodyToken: token})
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 && lease.Custody != ResourceCustodyRecovery {
		return ErrStateRace
	}
	result, err = dbgen.New(tx).DeleteResourceLease(ctx, dbgen.DeleteResourceLeaseParams{ID: id, CustodyToken: token})
	if err != nil {
		return err
	}
	changed, err = result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrStateRace
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistProject(ctx, lease.OwnerProjectID)
}

func (db *DB) ReconcileResourceLeasesForIntent(ctx context.Context, intentID int64, evidence string) error {
	if intentID < 1 || strings.TrimSpace(evidence) == "" {
		return fmt.Errorf("intent id and reconciliation evidence are required")
	}
	rows, err := db.queries.ResourceLeasesByIntent(ctx, intentID)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbgen.New(tx)
	projects := make(map[int64]bool)
	for _, row := range rows {
		if _, err := queries.MarkResourceLeaseForRecovery(ctx, dbgen.MarkResourceLeaseForRecoveryParams{ID: row.ID, CustodyToken: row.CustodyToken}); err != nil {
			return err
		}
		result, err := queries.DeleteResourceLease(ctx, dbgen.DeleteResourceLeaseParams{ID: row.ID, CustodyToken: row.CustodyToken})
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrStateRace
		}
		projects[row.OwnerProjectID] = true
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for projectID := range projects {
		if err := db.PersistProject(ctx, projectID); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) ResourceLeasesByIntent(ctx context.Context, intentID int64) ([]ResourceLease, error) {
	rows, err := db.queries.ResourceLeasesByIntent(ctx, intentID)
	if err != nil {
		return nil, err
	}
	leases := make([]ResourceLease, 0, len(rows))
	for _, row := range rows {
		leases = append(leases, resourceLeaseFromIntent(row))
	}
	return leases, nil
}

func (db *DB) ResourceLeaseByID(ctx context.Context, id int64) (ResourceLease, error) {
	return db.resourceLeaseByID(ctx, id)
}

func (db *DB) ResourceLeaseBySource(ctx context.Context, sourceKey string) (ResourceLease, error) {
	row, err := db.queries.ResourceLeaseBySource(ctx, sourceKey)
	if err != nil {
		return ResourceLease{}, err
	}
	return resourceLeaseFromSource(row), nil
}

func (db *DB) ResourceLeases(ctx context.Context, projectID int64) ([]ResourceLease, error) {
	rows, err := db.queries.ResourceLeasesByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	leases := make([]ResourceLease, 0, len(rows))
	for _, row := range rows {
		leases = append(leases, resourceLeaseFromProject(row))
	}
	return leases, nil
}

func (db *DB) AllResourceLeases(ctx context.Context) ([]ResourceLease, error) {
	rows, err := db.queries.AllResourceLeases(ctx)
	if err != nil {
		return nil, err
	}
	leases := make([]ResourceLease, 0, len(rows))
	for _, row := range rows {
		leases = append(leases, ResourceLease{ID: row.ID, ResourceType: row.ResourceType, ScopeRoot: row.ScopeRoot, ScopePath: row.ScopePath,
			OwnerProjectID: row.OwnerProjectID, OwnerTaskID: row.OwnerTaskID, Owner: row.Owner, SourceKey: row.SourceKey,
			CustodyToken: row.CustodyToken, IntentID: row.IntentID, SandboxID: row.SandboxID, OwnerPID: int(row.OwnerPid),
			OwnerBootID: row.OwnerBootID, OwnerStartTime: row.OwnerStartTime, Custody: row.Custody, AcquiredAt: row.AcquiredAt})
	}
	return leases, nil
}

func (db *DB) resourceLeaseByID(ctx context.Context, id int64) (ResourceLease, error) {
	var lease ResourceLease
	err := db.QueryRowContext(ctx, `SELECT id,resource_type,scope_root,scope_path,owner_project_id,COALESCE(owner_task_id,0),owner,source_key,custody_token,intent_id,COALESCE(mount_id,0),owner_pid,owner_boot_id,owner_start_time,custody,acquired_at FROM resource_leases WHERE id=?`, id).Scan(
		&lease.ID, &lease.ResourceType, &lease.ScopeRoot, &lease.ScopePath, &lease.OwnerProjectID, &lease.OwnerTaskID,
		&lease.Owner, &lease.SourceKey, &lease.CustodyToken, &lease.IntentID, &lease.SandboxID, &lease.OwnerPID,
		&lease.OwnerBootID, &lease.OwnerStartTime, &lease.Custody, &lease.AcquiredAt,
	)
	return lease, err
}

func resourceLeaseFromSource(row dbgen.ResourceLeaseBySourceRow) ResourceLease {
	return ResourceLease{ID: row.ID, ResourceType: row.ResourceType, ScopeRoot: row.ScopeRoot, ScopePath: row.ScopePath,
		OwnerProjectID: row.OwnerProjectID, OwnerTaskID: row.OwnerTaskID, Owner: row.Owner, SourceKey: row.SourceKey,
		CustodyToken: row.CustodyToken, IntentID: row.IntentID, SandboxID: row.SandboxID, OwnerPID: int(row.OwnerPid),
		OwnerBootID: row.OwnerBootID, OwnerStartTime: row.OwnerStartTime, Custody: row.Custody, AcquiredAt: row.AcquiredAt}
}

func resourceLeaseFromScope(row dbgen.ResourceLeasesByScopeRow) ResourceLease {
	return ResourceLease{ID: row.ID, ResourceType: row.ResourceType, ScopeRoot: row.ScopeRoot, ScopePath: row.ScopePath,
		OwnerProjectID: row.OwnerProjectID, OwnerTaskID: row.OwnerTaskID, Owner: row.Owner, SourceKey: row.SourceKey,
		CustodyToken: row.CustodyToken, IntentID: row.IntentID, SandboxID: row.SandboxID, OwnerPID: int(row.OwnerPid),
		OwnerBootID: row.OwnerBootID, OwnerStartTime: row.OwnerStartTime, Custody: row.Custody, AcquiredAt: row.AcquiredAt}
}

func resourceLeaseFromProject(row dbgen.ResourceLeasesByProjectRow) ResourceLease {
	return ResourceLease{ID: row.ID, ResourceType: row.ResourceType, ScopeRoot: row.ScopeRoot, ScopePath: row.ScopePath,
		OwnerProjectID: row.OwnerProjectID, OwnerTaskID: row.OwnerTaskID, Owner: row.Owner, SourceKey: row.SourceKey,
		CustodyToken: row.CustodyToken, IntentID: row.IntentID, SandboxID: row.SandboxID, OwnerPID: int(row.OwnerPid),
		OwnerBootID: row.OwnerBootID, OwnerStartTime: row.OwnerStartTime, Custody: row.Custody, AcquiredAt: row.AcquiredAt}
}

func resourceLeaseFromIntent(row dbgen.ResourceLeasesByIntentRow) ResourceLease {
	return ResourceLease{ID: row.ID, ResourceType: row.ResourceType, ScopeRoot: row.ScopeRoot, ScopePath: row.ScopePath,
		OwnerProjectID: row.OwnerProjectID, OwnerTaskID: row.OwnerTaskID, Owner: row.Owner, SourceKey: row.SourceKey,
		CustodyToken: row.CustodyToken, IntentID: row.IntentID, SandboxID: row.SandboxID, OwnerPID: int(row.OwnerPid),
		OwnerBootID: row.OwnerBootID, OwnerStartTime: row.OwnerStartTime, Custody: row.Custody, AcquiredAt: row.AcquiredAt}
}

// ResourceLeaseExists is used by recovery to avoid treating an interrupted
// executor as finished merely because its process identifier is no longer live.
func (db *DB) ResourceLeaseExists(ctx context.Context, sourceKey string) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM resource_leases WHERE source_key=?)`, sourceKey).Scan(&exists)
	return exists, err
}
