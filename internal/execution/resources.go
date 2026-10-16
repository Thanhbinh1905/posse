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
	if err := m.EnsureMountLeases(ctx); err != nil {
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

func (m *Manager) EnsureMountPath(ctx context.Context, path string, request store.ResourceLeaseRequest) (store.ResourceLease, error) {
	canonical, err := CanonicalPath(path)
	if err != nil {
		return store.ResourceLease{}, err
	}
	request.ResourceType = store.ResourcePath
	request.ScopeRoot = ""
	request.ScopePath = canonical
	return m.DB.EnsureMountResourceLease(ctx, request)
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

func (m *Manager) EnsureMountLeases(ctx context.Context) error {
	mounts, err := m.DB.MountResourceOwners(ctx)
	if err != nil {
		return err
	}
	active := make(map[int64]bool, len(mounts))
	for _, mount := range mounts {
		active[mount.ID] = true
		owner := fmt.Sprintf("%s mount-%d", mount.ProjectName, mount.Number)
		if mount.TaskSeq > 0 {
			owner += fmt.Sprintf(" (t%d)", mount.TaskSeq)
		}
		if mount.State == "broken" {
			owner += " (quarantined)"
		}
		_, err := m.EnsureMountPath(ctx, mount.Path, store.ResourceLeaseRequest{
			OwnerProjectID: mount.ProjectID,
			OwnerTaskID:    mount.TaskID,
			Owner:          owner,
			SourceKey:      fmt.Sprintf("mount:%d", mount.ID),
			MountID:        mount.ID,
		})
		if err != nil {
			return err
		}
	}
	leases, err := m.DB.ResourceMountLeases(ctx)
	if err != nil {
		return err
	}
	for _, lease := range leases {
		if !active[lease.MountID] {
			if err := m.DB.ReleaseMountResourceLease(ctx, lease.MountID); err != nil {
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
