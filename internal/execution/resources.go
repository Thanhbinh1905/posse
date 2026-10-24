package execution

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/thanhbinh1905/posse/internal/store"
)

// Manager coordinates durable filesystem and Git-ref claims in the shared
// Posse store. Claims have no expiry: callers release them only after the
// associated effect or custody has been reconciled.
type Manager struct {
	DB *store.DB
}

func NewManager(db *store.DB) *Manager { return &Manager{DB: db} }

func (m *Manager) AcquirePath(ctx context.Context, path string, request store.ResourceLeaseRequest) (store.ResourceLease, error) {
	if err := m.EnsureSandboxLeases(ctx); err != nil {
		return store.ResourceLease{}, err
	}
	canonical, err := CanonicalPath(path)
	if err != nil {
		return store.ResourceLease{}, err
	}
	request.ResourceType = store.ResourcePath
	request.ScopeRoot = ""
	request.ScopePath = canonical
	return m.DB.AcquireResourceLease(ctx, request)
}

func (m *Manager) EnsureSandboxPath(ctx context.Context, path string, request store.ResourceLeaseRequest) (store.ResourceLease, error) {
	canonical, err := CanonicalPath(path)
	if err != nil {
		return store.ResourceLease{}, err
	}
	request.ResourceType = store.ResourcePath
	request.ScopeRoot = ""
	request.ScopePath = canonical
	return m.DB.EnsureSandboxResourceLease(ctx, request)
}

func (m *Manager) AcquireGitRef(ctx context.Context, repository, ref string, request store.ResourceLeaseRequest) (store.ResourceLease, error) {
	commonDir, err := RepositoryCommonDir(ctx, repository)
	if err != nil {
		return store.ResourceLease{}, err
	}
	if err := validateGitRef(ctx, repository, ref); err != nil {
		return store.ResourceLease{}, err
	}
	request.ResourceType = store.ResourceGitRef
	request.ScopeRoot = commonDir
	request.ScopePath = strings.TrimPrefix(ref, "refs/")
	return m.DB.AcquireResourceLease(ctx, request)
}

// LeaseRequest is one resource of an atomic AcquireAll. Path is canonicalized;
// a non-empty Ref selects a Git ref of Repository instead of a path.
type LeaseRequest struct {
	Path       string
	Repository string
	Ref        string
	Request    store.ResourceLeaseRequest
}

// AcquireAll claims every resource in one transaction so a command either holds
// all of its leases or none, and persists the Project snapshot once.
func (m *Manager) AcquireAll(ctx context.Context, requests []LeaseRequest) ([]store.ResourceLease, error) {
	if err := m.EnsureSandboxLeases(ctx); err != nil {
		return nil, err
	}
	resolved := make([]store.ResourceLeaseRequest, 0, len(requests))
	for _, item := range requests {
		request := item.Request
		if item.Ref == "" {
			canonical, err := CanonicalPath(item.Path)
			if err != nil {
				return nil, err
			}
			request.ResourceType = store.ResourcePath
			request.ScopeRoot = ""
			request.ScopePath = canonical
		} else {
			commonDir, err := RepositoryCommonDir(ctx, item.Repository)
			if err != nil {
				return nil, err
			}
			if err := validateGitRef(ctx, item.Repository, item.Ref); err != nil {
				return nil, err
			}
			request.ResourceType = store.ResourceGitRef
			request.ScopeRoot = commonDir
			request.ScopePath = strings.TrimPrefix(item.Ref, "refs/")
		}
		resolved = append(resolved, request)
	}
	leases, err := m.DB.AcquireResourceLeases(ctx, resolved)
	if err != nil && len(leases) > 0 {
		// The claims committed but the snapshot write failed. Callers never see
		// the leases on error, so release them rather than strand custody.
		_ = m.DB.ReleaseResourceLeases(context.WithoutCancel(ctx), leases)
		return nil, err
	}
	return leases, err
}

// ReleaseAll releases leases in one transaction.
func (m *Manager) ReleaseAll(ctx context.Context, leases []store.ResourceLease) error {
	return m.DB.ReleaseResourceLeases(ctx, leases)
}

func (m *Manager) EnsureSandboxLeases(ctx context.Context) error {
	sandboxes, err := m.DB.SandboxResourceOwners(ctx)
	if err != nil {
		return err
	}
	active := make(map[int64]bool, len(sandboxes))
	for _, sandbox := range sandboxes {
		active[sandbox.ID] = true
		owner := fmt.Sprintf("%s sandbox-%d", sandbox.ProjectName, sandbox.Number)
		if sandbox.TaskSeq > 0 {
			owner += fmt.Sprintf(" (t%d)", sandbox.TaskSeq)
		}
		if sandbox.State == "broken" {
			owner += " (quarantined)"
		}
		_, err := m.EnsureSandboxPath(ctx, sandbox.Path, store.ResourceLeaseRequest{
			OwnerProjectID: sandbox.ProjectID,
			OwnerTaskID:    sandbox.TaskID,
			Owner:          owner,
			SourceKey:      fmt.Sprintf("sandbox:%d", sandbox.ID),
			SandboxID:      sandbox.ID,
		})
		if err != nil {
			return err
		}
	}
	leases, err := m.DB.ResourceSandboxLeases(ctx)
	if err != nil {
		return err
	}
	for _, lease := range leases {
		if !active[lease.SandboxID] {
			if err := m.DB.ReleaseSandboxResourceLease(ctx, lease.SandboxID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) Release(ctx context.Context, lease store.ResourceLease) error {
	return m.DB.ReleaseResourceLease(ctx, lease.ID, lease.CustodyToken)
}

func (m *Manager) Reconcile(ctx context.Context, lease store.ResourceLease, evidence string) error {
	return m.DB.ReconcileResourceLease(ctx, lease.ID, lease.CustodyToken, evidence)
}

// CanonicalPath resolves existing symlink components and preserves any
// not-yet-created suffix. This lets two aliases contend for the same scope
// before either path is created.
func CanonicalPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("resource path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	current := absolute
	var missing []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr == nil {
			current = resolved
			break
		}
		if !os.IsNotExist(resolveErr) {
			return "", fmt.Errorf("resolve resource path %s: %w", current, resolveErr)
		}
		if _, statErr := os.Lstat(current); statErr == nil {
			return "", fmt.Errorf("resolve resource path %s: %w", current, resolveErr)
		} else if !os.IsNotExist(statErr) {
			return "", fmt.Errorf("inspect resource path %s: %w", current, statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("resource path has no resolvable ancestor: %s", absolute)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
	for index := len(missing) - 1; index >= 0; index-- {
		current = filepath.Join(current, missing[index])
	}
	return filepath.Clean(current), nil
}

func RepositoryCommonDir(ctx context.Context, repository string) (string, error) {
	command := exec.CommandContext(ctx, "git", "-C", repository, "rev-parse", "--git-common-dir")
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolve Git common directory for %s: %w: %s", repository, err, strings.TrimSpace(string(output)))
	}
	commonDir := strings.TrimSpace(string(output))
	if commonDir == "" {
		return "", fmt.Errorf("git returned an empty common directory for %s", repository)
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(repository, commonDir)
	}
	return CanonicalPath(commonDir)
}

func validateGitRef(ctx context.Context, repository, ref string) error {
	if !strings.HasPrefix(ref, "refs/") || strings.ContainsAny(ref, "\x00\n\r") {
		return fmt.Errorf("git resource ref must be a full refs/... name: %q", ref)
	}
	command := exec.CommandContext(ctx, "git", "-C", repository, "check-ref-format", ref)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("invalid Git resource ref %q: %w: %s", ref, err, strings.TrimSpace(string(output)))
	}
	return nil
}
