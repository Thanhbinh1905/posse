package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/execution"
	"github.com/thanhbinh1905/posse/internal/store"
	"golang.org/x/sys/unix"
)

func resourceConflictReason(err error) (string, bool) {
	var conflict *store.ResourceLeaseConflict
	if !errors.As(err, &conflict) {
		return "", false
	}
	return fmt.Sprintf("%s lease held by %s at %s", conflict.Lease.ResourceType, conflict.Lease.Owner, conflict.Lease.Scope()), true
}

func resourceLeaseError(err error) error {
	var conflict *store.ResourceLeaseConflict
	if errors.As(err, &conflict) {
		return axi.Failure("resource_conflict", "resource scope is already leased by "+conflict.Lease.Owner, true,
			conflict.Lease.ResourceType+" scope: "+conflict.Lease.Scope(),
			"Wait for the recorded owner to finish or recover its custody before retrying")
	}
	return err
}

// Keep explicit Merge effects behind checkout sync so the two operations cannot
// contend for the same durable path and ref leases.
func withIntentGitResources(ctx context.Context, db *store.DB, project store.Project, task store.Task, intentID int64, repository string, refs []string, path string, effect func() error) error {
	syncLock, err := acquireRepositorySyncLock(ctx, repository)
	if err != nil {
		return err
	}
	defer func() {
		_ = unix.Flock(int(syncLock.Fd()), unix.LOCK_UN)
		_ = syncLock.Close()
	}()

	manager := execution.NewManager(db)
	if err := manager.EnsureSandboxLeases(ctx); err != nil {
		return resourceLeaseError(err)
	}
	refs = append([]string(nil), refs...)
	sort.Strings(refs)
	leases := make([]store.ResourceLease, 0, len(refs)+1)
	owner := fmt.Sprintf("Merge t%d", task.Seq)
	if task.Seq < 1 {
		owner = "Posse Merge"
	}
	request := func(source string) store.ResourceLeaseRequest {
		return store.ResourceLeaseRequest{OwnerProjectID: project.ID, OwnerTaskID: task.ID, Owner: owner,
			SourceKey: source, IntentID: intentID}
	}
	if path != "" {
		lease, err := manager.AcquirePath(ctx, path, request(fmt.Sprintf("intent:%d:path:%s", intentID, path)))
		if err != nil {
			return resourceLeaseError(err)
		}
		leases = append(leases, lease)
	}
	for _, ref := range refs {
		lease, err := manager.AcquireGitRef(ctx, repository, ref, request(fmt.Sprintf("intent:%d:git:%s:%s", intentID, repository, ref)))
		if err != nil {
			_ = releaseResourceLeases(ctx, manager, leases)
			return resourceLeaseError(err)
		}
		leases = append(leases, lease)
	}
	if err := effect(); err != nil {
		return err
	}
	return releaseResourceLeases(ctx, manager, leases)
}

func releaseResourceLeases(ctx context.Context, manager *execution.Manager, leases []store.ResourceLease) error {
	var releaseErr error
	for index := len(leases) - 1; index >= 0; index-- {
		if err := manager.Release(ctx, leases[index]); err != nil {
			releaseErr = errors.Join(releaseErr, err)
		}
	}
	return releaseErr
}

func localMergePathScope(ctx context.Context, project store.Project, task store.Task) (string, error) {
	worktrees, err := gitOutput(ctx, project.Root, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	root, err := execution.CanonicalPath(project.Root)
	if err != nil {
		return "", err
	}
	wantedRef := "refs/heads/" + taskTargetBranch(project, task)
	for _, block := range strings.Split(worktrees, "\n\n") {
		var path, branch string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "worktree "):
				path = strings.TrimPrefix(line, "worktree ")
			case strings.HasPrefix(line, "branch "):
				branch = strings.TrimPrefix(line, "branch ")
			}
		}
		if branch != wantedRef || path == "" {
			continue
		}
		canonical, err := execution.CanonicalPath(path)
		if err != nil {
			return "", err
		}
		if canonical == root {
			return project.Root, nil
		}
	}
	return "", nil
}
