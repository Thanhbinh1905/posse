package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) remuda(ctx *axi.Context, args []string) error {
	if len(args) == 0 {
		return s.listMounts(ctx)
	}
	if args[0] != "prune" {
		return axi.Usage("remuda accepts only the prune subcommand", "Run `posse remuda` to list Mounts")
	}
	parsed, err := parseArgs("remuda prune", args[1:], map[string]flagSpec{"yes": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("remuda prune does not take positional arguments")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if err := s.requireLead(ctx.Context, db, project); err != nil {
		return err
	}
	cfg, err := s.prepareProject(ctx.Context, db, project)
	if err != nil {
		return err
	}
	mounts, err := db.Mounts(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	tasks, err := db.Tasks(ctx.Context, project.ID, true)
	if err != nil {
		return err
	}
	clean := mountWorktreeClean
	if project.IsWorkspace() {
		clean = workspaceMountClean
	}
	items := []pruneItem{}
	prunableMountPaths := map[string]bool{}
	managedMounts := make([]store.Mount, 0, len(mounts))
	for _, mount := range mounts {
		if safeManagedMountPath(home, project.Name, mount.Path) {
			managedMounts = append(managedMounts, mount)
		}
	}
	for _, mount := range pruneCandidates(ctx.Context, managedMounts, cfg.Remuda.KeepIdle, clean) {
		prunableMountPaths[filepath.Clean(mount.Path)] = true
		size, err := pathBytes(mount.Path)
		if err != nil {
			return err
		}
		reason := "idle Mount beyond keep_idle"
		if mount.State == "broken" {
			reason = "quarantined Mount is safe to remove"
		}
		items = append(items, pruneItem{kind: "mount", label: fmt.Sprintf("mount-%d", mount.Number), path: mount.Path, state: mount.State, reason: reason, bytes: size, mount: mount})
	}
	scratch, err := taskScratchPruneItems(ctx.Context, db, home, project)
	if err != nil {
		return err
	}
	items = append(items, scratch...)
	artifacts, err := expiredTaskArtifactItems(home, project, tasks, duration(cfg.Retention.TaskArtifacts), time.Now())
	if err != nil {
		return err
	}
	items = append(items, artifacts...)
	branches, err := landedOrDiscardedBranchItems(ctx.Context, db, home, project, tasks)
	if err != nil {
		return err
	}
	items = append(items, branches...)
	registrations, err := staleOwnedWorktreeItems(ctx.Context, db, project, home, mounts)
	if err != nil {
		return err
	}
	for _, registration := range registrations {
		if !prunableMountPaths[filepath.Clean(registration.path)] && !prunableMountPaths[filepath.Clean(registration.mountPath)] {
			items = append(items, registration)
		}
	}
	rows := make([]any, 0, len(items))
	for _, item := range items {
		rows = append(rows, map[string]any{"kind": item.kind, "item": item.label, "path": item.path, "state": item.state, "size_bytes": item.bytes, "reason": item.reason})
	}
	if parsed.Bool("yes") {
		for index, item := range items {
			if err := s.applyPruneItem(ctx.Context, db, home, project, item); err != nil {
				var commandError *axi.Error
				if errors.As(err, &commandError) && commandError.Code == "prune_skipped" {
					if row, ok := rows[index].(map[string]any); ok {
						row["state"] = "skipped"
						row["reason"] = commandError.Message
					}
					continue
				}
				return err
			}
		}
	}
	return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "prune", Value: rows}, {Key: "dry_run", Value: !parsed.Bool("yes")}, {Key: "help", Value: []any{"Run `posse remuda prune --yes` to remove these safe, Posse-owned items"}}})
}

func (s *Service) applyScratchPruneItem(ctx context.Context, db *store.DB, home string, project store.Project, item pruneItem) error {
	err := withMountStateLock(ctx, db, func() error {
		_, reason, taskID, eligible, err := taskScratchPruneOwnership(ctx, db, project, item.seq)
		if err != nil {
			return err
		}
		if !eligible || taskID != item.taskID {
			return axi.Failure("prune_incomplete", fmt.Sprintf("Task scratch ownership changed for %s (%s)", item.label, reason), true, "Retry `posse remuda prune` after the Task operation finishes")
		}
		expected, err := taskScratchPath(home, project, store.Task{Seq: item.seq})
		if err != nil {
			return err
		}
		if filepath.Clean(item.path) != filepath.Clean(expected) {
			return axi.Failure("prune_incomplete", fmt.Sprintf("refuse to prune scratch outside the validated Task path for %s", item.label), true, "Inspect POSSE_HOME, then retry `posse remuda prune`")
		}
		root, err := validatedTaskScratchRoot(home, project, store.Task{Seq: item.seq})
		if err != nil {
			return axi.Failure("prune_incomplete", err.Error(), true, "Repair the Task scratch path, then retry `posse remuda prune`")
		}
		if _, err := stopOwnedProcesses([]ownedProcessRoot{{path: root, includeOpenFiles: true, includeMappedFiles: true}}); err != nil {
			return axi.Failure("prune_incomplete", err.Error(), true, "Resolve Task scratch processes, then retry `posse remuda prune`")
		}
		if err := removeTaskScratch(home, project, store.Task{Seq: item.seq}); err != nil {
			return axi.Failure("prune_incomplete", err.Error(), true, "Repair Task scratch permissions, then retry `posse remuda prune`")
		}
		return nil
	})
	if err == nil {
		return nil
	}
	var commandError *axi.Error
	if errors.As(err, &commandError) && commandError.Code == "prune_incomplete" && commandError.Retryable {
		return err
	}
	return axi.Failure("prune_incomplete", err.Error(), true, "Resolve Task scratch ownership, then retry `posse remuda prune`")
}

func (s *Service) applyBranchPruneItem(ctx context.Context, db *store.DB, project store.Project, item pruneItem) error {
	// Serialize the final worktree check and ref deletion with Posse Mount claims.
	return withMountStateLock(ctx, db, func() error {
		task, err := db.Task(ctx, project.ID, taskIDString(item.seq))
		if err != nil {
			return err
		}
		if task.ID != item.taskID || task.Branch != item.label || task.State != store.StateLanded && task.State != store.StateTornDown {
			return fmt.Errorf("refuse to prune branch %s after its Task changed", item.label)
		}
		blocked, err := taskBranchPruneBlocked(ctx, db, task)
		if err != nil {
			return err
		}
		if blocked {
			return fmt.Errorf("refuse to prune branch %s while Task t%d has an active operation or held Mount", item.label, task.Seq)
		}
		ref := "refs/heads/" + item.label
		sha, err := gitOutput(ctx, item.path, "rev-parse", "--verify", ref)
		if err != nil {
			return err
		}
		if strings.TrimSpace(sha) != item.sha {
			return skippedPruneBranch(item, "branch tip changed since prune was planned")
		}
		checkedOut, err := branchCheckedOutInAnyWorktree(ctx, item.path, item.label)
		if err != nil {
			return err
		}
		if checkedOut {
			return skippedPruneBranch(item, "branch is checked out in a worktree")
		}
		_, err = gitOutput(ctx, item.path, "update-ref", "-d", ref, item.sha)
		if err != nil {
			currentSHA, readErr := gitOutput(ctx, item.path, "rev-parse", "--verify", ref)
			if readErr != nil && isMissingGitRef(readErr) || readErr == nil && strings.TrimSpace(currentSHA) != item.sha {
				return skippedPruneBranch(item, "branch ref changed before it could be deleted")
			}
			if readErr != nil {
				return errors.Join(err, readErr)
			}
			return err
		}
		checkedOut, err = branchCheckedOutInAnyWorktree(ctx, item.path, item.label)
		if err != nil {
			restoreErr := restorePrunedBranchRef(ctx, item.path, ref, item.sha)
			return errors.Join(fmt.Errorf("cannot verify worktree state after pruning branch %s", item.label), err, restoreErr)
		}
		if checkedOut {
			if err := restorePrunedBranchRef(ctx, item.path, ref, item.sha); err != nil {
				return fmt.Errorf("branch %s became checked out during prune and its ref could not be restored: %w", item.label, err)
			}
			return skippedPruneBranch(item, "branch became checked out in a worktree during prune; its ref was restored")
		}
		return nil
	})
}

func (s *Service) applyPruneItem(ctx context.Context, db *store.DB, home string, project store.Project, item pruneItem) error {
	switch item.kind {
	case "mount":
		claimed, err := db.ClaimMountPrune(ctx, project.ID, item.mount.ID)
		if err != nil || !claimed {
			return err
		}
		var removeErr error
		if project.IsWorkspace() {
			removeErr = s.removeWorkspaceMount(ctx, db, project, item.mount.Path)
		} else if _, statErr := os.Stat(item.mount.Path); os.IsNotExist(statErr) {
			removeErr = removeStaleWorktreeRegistration(ctx, project.Root, item.mount.Path)
		} else {
			if err := makeMountUntrackedWritable(ctx, item.mount.Path, "pristine"); err != nil {
				return errors.Join(err, db.RestoreMountAfterPrune(ctx, item.mount.ID, item.mount.State))
			}
			if item.mount.State == "broken" && !mountWorktreeClean(ctx, item.mount.Path) {
				if !mountTrackedWorktreeClean(ctx, item.mount.Path) {
					return errors.Join(fmt.Errorf("broken Mount has tracked changes and cannot be pruned safely"), db.RestoreMountAfterPrune(ctx, item.mount.ID, item.mount.State))
				}
				if _, err := gitOutput(ctx, item.mount.Path, "clean", "-fdx"); err != nil {
					return errors.Join(err, db.RestoreMountAfterPrune(ctx, item.mount.ID, item.mount.State))
				}
			}
			if err := unlockMount(ctx, project.Root, item.mount.Path); err != nil {
				return errors.Join(err, db.RestoreMountAfterPrune(ctx, item.mount.ID, item.mount.State))
			}
			removeErr = s.removeMount(ctx, project.Root, item.mount.Path)
		}
		if removeErr != nil {
			return errors.Join(removeErr, db.RestoreMountAfterPrune(ctx, item.mount.ID, item.mount.State))
		}
		return db.DeletePrunedMount(ctx, project.ID, item.mount.ID)
	case "scratch":
		return s.applyScratchPruneItem(ctx, db, home, project, item)
	case "artifacts":
		return removeTaskArtifacts(home, project, item.seq, item.files)
	case "branch":
		return s.applyBranchPruneItem(ctx, db, project, item)
	case "worktree-registration":
		if item.mount.ID != 0 {
			claimed, err := db.ClaimMountPrune(ctx, project.ID, item.mount.ID)
			if err != nil || !claimed {
				return err
			}
		}
		repoRoot := item.repoRoot
		if repoRoot == "" {
			repoRoot = project.Root
		}
		if err := removeStaleWorktreeRegistration(ctx, repoRoot, item.path); err != nil {
			if item.mount.ID != 0 {
				return errors.Join(err, db.RestoreMountAfterPrune(ctx, item.mount.ID, item.mount.State))
			}
			return err
		}
		if item.mount.ID != 0 {
			if project.IsWorkspace() && item.mountPath != "" {
				return db.RestoreMountAfterPrune(ctx, item.mount.ID, item.mount.State)
			}
			return db.DeletePrunedMount(ctx, project.ID, item.mount.ID)
		}
		return nil
	default:
		return fmt.Errorf("unknown prune item kind %q", item.kind)
	}
}

func pruneCandidates(ctx context.Context, mounts []store.Mount, keepIdle int, clean func(context.Context, string) bool) []store.Mount {
	idle := []store.Mount{}
	remove := []store.Mount{}
	for _, mount := range mounts {
		info, err := os.Lstat(mount.Path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		switch mount.State {
		case "idle":
			idle = append(idle, mount)
		case "broken":
			if clean(ctx, mount.Path) || mountTrackedWorktreeClean(ctx, mount.Path) && mountHasReadOnlyUntracked(ctx, mount.Path) {
				remove = append(remove, mount)
			}
		}
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].Number < idle[j].Number })
	if keepIdle < 0 {
		keepIdle = 0
	}
	if keepIdle < len(idle) {
		remove = append(remove, idle[keepIdle:]...)
	}
	sort.Slice(remove, func(i, j int) bool { return remove[i].Number > remove[j].Number })
	return remove
}

func (s *Service) listMounts(ctx *axi.Context) error {
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if err := s.requireLead(ctx.Context, db, project); err != nil {
		return err
	}
	_, err = s.prepareProject(ctx.Context, db, project)
	if err = deferObservationContention(ctx, err, "Project observations were deferred; showing the last recorded state."); err != nil {
		return err
	}
	mounts, err := db.Mounts(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	rows := make([]any, 0, len(mounts))
	for _, mount := range mounts {
		task := ""
		if mount.TaskID != 0 {
			if taskRow, e := db.TaskByID(ctx.Context, project.ID, mount.TaskID); e == nil {
				task = taskIDString(taskRow.Seq)
			}
		}
		rows = append(rows, map[string]any{"mount": fmt.Sprintf("mount-%d", mount.Number), "state": mount.State, "task": task, "path": mount.Path})
	}
	return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "mounts", Value: rows}, {Key: "help", Value: []any{"Run `posse remuda prune` to review idle Mounts beyond keep_idle"}}})
}

func acquireMount(ctx context.Context, db *store.DB, project store.Project, task store.Task, home string, cleanMode string, setup []string) (store.Mount, error) {
	base := filepath.Join(home, "remuda", project.Name)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return store.Mount{}, err
	}
	for attempts := 0; attempts < 64; attempts++ {
		var mount store.Mount
		err := withMountStateLock(ctx, db, func() error {
			// Protect the oldest idle checkout before AcquireMount can expose
			// it as held. New checkouts are created with Git's atomic --lock.
			mounts, err := db.Mounts(ctx, project.ID)
			if err != nil {
				return err
			}
			var prelocked store.Mount
			for _, candidate := range mounts {
				if candidate.State == "idle" && (prelocked.ID == 0 || candidate.Number < prelocked.Number) {
					prelocked = candidate
				}
			}
			if prelocked.ID != 0 {
				if _, statErr := os.Stat(filepath.Join(prelocked.Path, ".git")); statErr == nil {
					if err := lockAvailableMount(ctx, db, project, prelocked, task); err != nil {
						return err
					}
				} else if !os.IsNotExist(statErr) {
					return statErr
				}
			}
			var claimErr error
			mount, claimErr = db.AcquireMount(ctx, project.ID, task.ID, base)
			if claimErr != nil {
				if prelocked.ID != 0 {
					current, lookupErr := db.Mounts(ctx, project.ID)
					if lookupErr != nil {
						return errors.Join(claimErr, lookupErr)
					}
					for _, candidate := range current {
						if candidate.ID == prelocked.ID && candidate.State == "idle" {
							return errors.Join(claimErr, unlockTaskMount(ctx, project.Root, prelocked.Path, task.Seq))
						}
					}
				}
				return claimErr
			}
			if prelocked.ID != 0 && prelocked.ID != mount.ID {
				return fmt.Errorf("prelocked mount %d differs from claimed mount %d: %w", prelocked.ID, mount.ID, store.ErrStateRace)
			}
			return nil
		})
		if err != nil {
			return mount, err
		}
		if _, statErr := os.Stat(filepath.Join(mount.Path, ".git")); os.IsNotExist(statErr) {
			if err := os.MkdirAll(filepath.Dir(mount.Path), 0o700); err != nil {
				return mount, err
			}
			ref, remote := mountDefaultRef(ctx, projectRepoTarget(project))
			if remote {
				if _, err := gitFetch(ctx, project.Root, "origin"); err != nil {
					return mount, err
				}
				ref = "refs/remotes/origin/" + project.DefaultBranch
			}
			if _, err := gitOutput(ctx, project.Root, "worktree", "add", "--detach", "--lock", "--reason", fmt.Sprintf("posse: held by t%d", task.Seq), mount.Path, ref); err != nil {
				if breakErr := breakMount(ctx, db, project, task, mount, "Git could not create the Mount worktree"); breakErr != nil {
					return mount, errors.Join(err, breakErr)
				}
				continue
			}
		}
		if err := withMountStateLock(ctx, db, func() error {
			return lockClaimedMount(ctx, db, project, mount, task)
		}); err != nil {
			return mount, err
		}
		if err := resetMount(ctx, mount.Path, project, cleanMode); err != nil {
			if breakErr := breakMount(ctx, db, project, task, mount, "Mount could not be reset to a clean state"); breakErr != nil {
				return mount, errors.Join(err, breakErr)
			}
			continue
		}
		base, err := taskCheckoutBase(ctx, project.Root, task, "", task.BaseRef)
		if err != nil {
			return mount, err
		}
		if _, err := gitOutput(ctx, mount.Path, "checkout", "-b", task.Branch, base); err != nil {
			return mount, err
		}
		if err := db.UpdateTaskMountPath(ctx, task.ID, mount.ID, mount.Path); err != nil {
			return mount, err
		}
		for _, command := range setup {
			if output, err := commandOutput(ctx, mount.Path, command); err != nil {
				return mount, axi.Failure("mount_setup_failed", fmt.Sprintf("setup %q failed: %s", command, truncate(strings.TrimSpace(output), 1200)), false, "Fix the Remuda setup command, then retry the Task")
			}
		}
		return mount, nil
	}
	return store.Mount{}, fmt.Errorf("no clean Mount could be acquired")
}

var errMountDefaultRefUnavailable = errors.New("no safe local default ref is available to reset the Mount")

func mountDefaultRef(ctx context.Context, target repoTarget) (string, bool) {
	if _, err := gitOutput(ctx, target.Root, "remote", "get-url", "origin"); err == nil {
		return "refs/remotes/origin/" + target.DefaultBranch, true
	}
	return "refs/heads/" + target.DefaultBranch, false
}

func mountResetRef(ctx context.Context, target repoTarget, fetch bool) (string, bool, error) {
	remoteRef := "refs/remotes/origin/" + target.DefaultBranch
	localRef := "refs/heads/" + target.DefaultBranch
	_, remoteErr := gitOutput(ctx, target.Root, "remote", "get-url", "origin")
	remote := remoteErr == nil
	if remote && fetch {
		return remoteRef, true, nil
	}
	if remote {
		if _, err := gitOutput(ctx, target.Root, "rev-parse", "--verify", "--quiet", remoteRef+"^{commit}"); err == nil {
			return remoteRef, false, nil
		}
	}
	if _, err := gitOutput(ctx, target.Root, "rev-parse", "--verify", "--quiet", localRef+"^{commit}"); err == nil {
		return localRef, remote, nil
	}
	return "", remote, fmt.Errorf("%w: checked %s and %s", errMountDefaultRefUnavailable, remoteRef, localRef)
}

func projectRepoTarget(project store.Project) repoTarget {
	return repoTarget{Root: project.Root, DefaultBranch: project.DefaultBranch}
}

func resetMount(ctx context.Context, path string, project store.Project, clean string) error {
	return resetWorktree(ctx, path, projectRepoTarget(project), clean, true)
}

// resetWorktree detaches a Mount worktree at the repository's default branch
// and cleans it. Teardown and unrequested workspace Members use the last fetched
// ref; acquisition fetches before new work starts.
func resetWorktree(ctx context.Context, path string, target repoTarget, clean string, fetch bool) error {
	ref, remote, err := mountResetRef(ctx, target, fetch)
	if err != nil {
		return err
	}
	if remote && fetch {
		if _, err := gitFetch(ctx, target.Root, "origin"); err != nil {
			return err
		}
	}
	if _, err := gitOutput(ctx, path, "checkout", "--detach", "--force", ref); err != nil {
		return err
	}
	if _, err := gitOutput(ctx, path, "reset", "--hard", ref); err != nil {
		return err
	}
	if err := makeMountUntrackedWritable(ctx, path, clean); err != nil {
		return err
	}
	args := []string{"clean", "-fd"}
	if clean == "pristine" {
		args = []string{"clean", "-fdx"}
	}
	if _, err := gitOutput(ctx, path, args...); err != nil {
		return err
	}
	status, err := gitStatusOutput(ctx, path, "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("mount is not clean: %s", status)
	}
	return nil
}

func makeMountUntrackedWritable(ctx context.Context, path, clean string) error {
	root, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	trackedOutput, err := gitOutputRaw(ctx, root, "ls-files", "-z")
	if err != nil {
		return err
	}
	trackedDirs := map[string]bool{}
	for _, tracked := range strings.Split(trackedOutput, "\x00") {
		for parent := filepath.Dir(filepath.FromSlash(tracked)); parent != "."; parent = filepath.Dir(parent) {
			trackedDirs[filepath.ToSlash(parent)] = true
		}
	}
	untrackedOutput, err := gitOutputRaw(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	paths := strings.Split(untrackedOutput, "\x00")
	if clean == "pristine" {
		ignoredOutput, err := gitOutputRaw(ctx, root, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
		if err != nil {
			return err
		}
		paths = append(paths, strings.Split(ignoredOutput, "\x00")...)
	}
	writable := map[string]bool{}
	for _, candidate := range paths {
		if candidate == "" {
			continue
		}
		relative := filepath.Clean(filepath.FromSlash(candidate))
		if filepath.IsAbs(relative) || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("git reported an untracked path outside the Mount: %q", candidate)
		}
		target := relative
		for parent := filepath.Dir(relative); parent != "."; parent = filepath.Dir(parent) {
			if trackedDirs[filepath.ToSlash(parent)] {
				break
			}
			target = parent
		}
		writable[target] = true
	}
	for target := range writable {
		if err := makeMountPathWritable(root, target); err != nil {
			return err
		}
	}
	return nil
}

func makeMountPathWritable(root, relative string) error {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	current := root
	for index, part := range parts {
		current = filepath.Join(current, filepath.FromSlash(part))
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if index < len(parts)-1 {
			if !info.IsDir() {
				return nil
			}
			if info.Mode().Perm()&0o700 != 0o700 {
				if err := os.Chmod(current, info.Mode()|0o700); err != nil {
					return err
				}
			}
			continue
		}
		return makeMountTreeWritable(current)
	}
	return nil
}

func makeMountTreeWritable(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) || err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		if info.Mode().Perm()&0o200 == 0 {
			return os.Chmod(path, info.Mode()|0o200)
		}
		return nil
	}
	if info.Mode().Perm()&0o700 != 0o700 {
		if err := os.Chmod(path, info.Mode()|0o700); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := makeMountTreeWritable(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func mountWorktreeClean(ctx context.Context, path string) bool {
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		return false
	}
	status, err := gitStatusOutput(ctx, path, "--porcelain")
	return err == nil && status == ""
}

func mountTrackedWorktreeClean(ctx context.Context, path string) bool {
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		return false
	}
	status, err := gitStatusOutput(ctx, path, "--porcelain", "--untracked-files=no")
	return err == nil && status == ""
}

func mountHasReadOnlyUntracked(ctx context.Context, path string) bool {
	root, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	output, err := gitOutputRaw(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return false
	}
	ignored, err := gitOutputRaw(ctx, root, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return false
	}
	candidates := append(strings.Split(output, "\x00"), strings.Split(ignored, "\x00")...)
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		relative := filepath.Clean(filepath.FromSlash(candidate))
		if filepath.IsAbs(relative) || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		current := root
		for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
			current = filepath.Join(current, filepath.FromSlash(part))
			info, err := os.Lstat(current)
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				break
			}
			if info.Mode().Perm()&0o200 == 0 {
				return true
			}
		}
	}
	return false
}

func releaseMount(ctx context.Context, db *store.DB, project store.Project, task store.Task, clean string, approvedDiscard bool) ([]string, error) {
	mounts, err := db.Mounts(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	var mount store.Mount
	for _, candidate := range mounts {
		if candidate.ID == task.MountID {
			mount = candidate
			break
		}
	}
	if mount.ID == 0 || mount.TaskID != task.ID || (mount.State != "held" && mount.State != "releasing") {
		return nil, nil
	}
	if mount.State == "releasing" {
		if err := requireNoMountProcesses(mount.Path, approvedDiscard); err != nil {
			return nil, err
		}
		return nil, finishReleasingMount(ctx, db, project, mount, task)
	}
	var killed []string
	if err := requireNoMountProcesses(mount.Path, approvedDiscard); err != nil {
		return nil, err
	}
	var resetErr error
	if project.IsWorkspace() {
		resetErr = releaseWorkspaceMount(ctx, db, project, mount, clean)
	} else {
		// Teardown is local and must not wait on origin. The next Mount
		// acquisition fetches before starting new work.
		resetErr = resetWorktree(ctx, mount.Path, projectRepoTarget(project), clean, false)
	}
	if err := resetErr; err != nil {
		if errors.Is(err, errMountDefaultRefUnavailable) {
			// Keep the Task's Mount held and retryable until a safe default ref
			// can be fetched; never quarantine it as broken during local recovery.
			return killed, err
		}
		if breakErr := breakMount(ctx, db, project, task, mount, "Mount could not be reset during release"); breakErr != nil {
			return killed, errors.Join(err, breakErr)
		}
		if approvedDiscard {
			return killed, nil // Broken Mount stays quarantined; approved discard may finish.
		}
		return killed, err
	}
	return killed, withMountStateLock(ctx, db, func() error {
		if err := db.BeginMountRelease(ctx, mount.ID, task.ID); err != nil {
			return err
		}
		return finishReleasingMountLocked(ctx, db, project, mount, task)
	})
}

func requireNoMountProcesses(path string, approvedDiscard bool) error {
	root, err := normalizeOwnedProcessRoot(path)
	if err != nil {
		return err
	}
	pids, err := processIDsInRoots([]ownedProcessRoot{{path: root}})
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		return nil
	}
	labels := make([]string, len(pids))
	for index, pid := range pids {
		labels[index] = strconv.Itoa(pid)
	}
	if approvedDiscard {
		return axi.Failure("discard_processes_present", "refusing to release a discarded Task Mount while processes remain", true, strings.Join(labels, ","), "No process was stopped or signalled. Inspect the Mount processes, then retry discard")
	}
	return axi.Failure("mount_process_identity_unavailable", "cannot release the Mount because live processes were not authorized during ownership proof: "+strings.Join(labels, ","), true, strings.Join(labels, ","), "Inspect the Mount processes and retry Teardown after they exit")
}

func finishReleasingMount(ctx context.Context, db *store.DB, project store.Project, mount store.Mount, task store.Task) error {
	return withMountStateLock(ctx, db, func() error {
		return finishReleasingMountLocked(ctx, db, project, mount, task)
	})
}

func finishReleasingMountLocked(ctx context.Context, db *store.DB, project store.Project, mount store.Mount, task store.Task) error {
	current, err := db.MountByTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if current.ID != mount.ID || current.State != "releasing" {
		return store.ErrStateRace
	}
	if !project.IsWorkspace() {
		if err := unlockRegisteredMount(ctx, project.Root, mount.Path, task.Seq); err != nil {
			return err
		}
	}
	return db.FinishMountRelease(ctx, mount.ID, task.ID)
}

// Git worktree locks protect held Mounts from Herdr's single-force UI delete.
// A repeated acquire/reconcile may encounter its own existing lock.
func lockMount(ctx context.Context, repo, path string, seq int) error {
	reason := fmt.Sprintf("posse: held by t%d", seq)
	current, err := mountLockReason(ctx, repo, path)
	if err != nil {
		return err
	}
	if current == reason {
		return nil
	}
	if current != "" {
		return fmt.Errorf("mount %s is locked by %q, not %q", path, current, reason)
	}
	_, err = gitOutput(ctx, repo, "worktree", "lock", "--reason", reason, path)
	return err
}

func relockIfStillHeld(ctx context.Context, db *store.DB, project store.Project, mount store.Mount, task store.Task) error {
	mounts, err := db.Mounts(ctx, project.ID)
	if err != nil {
		return err
	}
	for _, current := range mounts {
		if current.ID == mount.ID && current.State == "held" && current.TaskID == task.ID {
			return lockMount(ctx, project.Root, mount.Path, task.Seq)
		}
	}
	return nil
}

func unlockTaskMount(ctx context.Context, repo, path string, seq int) error {
	reason, err := mountLockReason(ctx, repo, path)
	if err != nil {
		return err
	}
	if reason != "" && reason != fmt.Sprintf("posse: held by t%d", seq) {
		return fmt.Errorf("refuse to release mount %s locked by %q, not t%d", path, reason, seq)
	}
	return unlockMount(ctx, repo, path)
}

func unlockMount(ctx context.Context, repo, path string) error {
	reason, err := mountLockReason(ctx, repo, path)
	if err != nil {
		return err
	}
	if reason == "" {
		return nil
	}
	if _, ok := posseLockTaskSeq(reason); !ok {
		return fmt.Errorf("refuse to unlock Mount %s locked by %q", path, reason)
	}
	_, err = gitOutput(ctx, repo, "worktree", "unlock", path)
	return err
}

var errMountNotRegistered = errors.New("mount is missing from git worktree list")

func mountLockReason(ctx context.Context, repo, path string) (string, error) {
	listing, err := gitOutput(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	for _, block := range strings.Split(listing, "\n\n") {
		lines := strings.Split(block, "\n")
		if len(lines) == 0 || lines[0] != "worktree "+path {
			continue
		}
		for _, line := range lines {
			if line == "locked" {
				return "locked", nil
			}
			if strings.HasPrefix(line, "locked ") {
				return strings.TrimPrefix(line, "locked "), nil
			}
		}
		return "", nil
	}
	return "", fmt.Errorf("%w: %s", errMountNotRegistered, path)
}

func unlockRegisteredMount(ctx context.Context, repo, path string, seq int) error {
	_, err := mountLockReason(ctx, repo, path)
	if errors.Is(err, errMountNotRegistered) {
		return nil
	}
	if err != nil {
		return err
	}
	return unlockTaskMount(ctx, repo, path, seq)
}

func breakMount(ctx context.Context, db *store.DB, project store.Project, task store.Task, mount store.Mount, reason string) error {
	if mount.State != "held" || mount.TaskID != task.ID {
		return store.ErrStateRace
	}
	if err := withMountStateLock(ctx, db, func() error {
		if !project.IsWorkspace() {
			// A broken checkout is quarantined, not deleted. Git may still
			// register a missing checkout; release its lock before clearing
			// ownership. A failed worktree.add has no checkout to unlock.
			if err := unlockRegisteredMount(ctx, project.Root, mount.Path, task.Seq); err != nil {
				return err
			}
		}
		if err := db.BreakMount(ctx, mount.ID, task.ID); err != nil {
			if !project.IsWorkspace() {
				if _, reasonErr := mountLockReason(ctx, project.Root, mount.Path); reasonErr == nil {
					return errors.Join(err, relockIfStillHeld(ctx, db, project, mount, task))
				}
			}
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	_, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "mount_broken", Summary: fmt.Sprintf("mount-%d: %s", mount.Number, reason), DataJSON: `{}`})
	return err
}

func skippedPruneBranch(item pruneItem, reason string) error {
	return axi.Failure("prune_skipped", fmt.Sprintf("branch %s was skipped: %s", item.label, reason), false)
}

func restorePrunedBranchRef(ctx context.Context, root, ref, sha string) error {
	_, err := gitOutput(ctx, root, "update-ref", ref, sha, "")
	if err == nil {
		return nil
	}
	currentSHA, readErr := gitOutput(ctx, root, "rev-parse", "--verify", ref)
	if readErr == nil && strings.TrimSpace(currentSHA) == sha {
		return nil
	}
	return errors.Join(fmt.Errorf("restore branch ref %s at %s", ref, sha), err, readErr)
}

type ownedProcessRoot struct {
	path               string
	includeOpenFiles   bool
	includeMappedFiles bool
}

type ownedProcess struct {
	pid    int
	handle *mountProcessHandle
}

func verifyNoTaskOwnedProcesses(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task) error {
	roots, err := taskOwnedProcessRoots(ctx, db, home, project, task)
	if err != nil {
		return err
	}
	pids, err := processIDsInRoots(roots)
	if err != nil {
		return err
	}
	if len(pids) > 0 {
		labels := make([]string, len(pids))
		for index, pid := range pids {
			labels[index] = strconv.Itoa(pid)
		}
		return axi.Failure("discard_processes_present", "refusing approved discard while processes remain in Task-owned paths", true, strings.Join(labels, ","), "No process was stopped or signalled. Inspect the Mount and scratch processes, then retry discard")
	}
	return nil
}

func taskOwnedProcessRoots(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task) ([]ownedProcessRoot, error) {
	roots := []ownedProcessRoot{}
	mount, err := db.MountByTask(ctx, task.ID)
	if err == nil && mount.TaskID == task.ID && (mount.State == "held" || mount.State == "releasing") {
		if mount.ID != task.MountID || task.WorktreePath == "" || !pathInside(task.WorktreePath, mount.Path) {
			return nil, fmt.Errorf("refuse to inspect processes for Task %s without a verified held or releasing Mount", taskIDString(task.Seq))
		}
		root, err := normalizeOwnedProcessRoot(mount.Path)
		if err != nil {
			return nil, err
		}
		roots = append(roots, ownedProcessRoot{path: root})
	} else if err != nil && !store.IsNotFound(err) {
		return nil, err
	}
	scratch, err := validatedTaskScratchRoot(home, project, task)
	if err != nil {
		return nil, err
	}
	roots = append(roots, ownedProcessRoot{path: scratch, includeOpenFiles: true, includeMappedFiles: true})
	return roots, nil
}
func normalizeOwnedProcessRoot(path string) (string, error) {
	root, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("owned process root is not a directory: %s", root)
	}
	return root, nil
}

func stopOwnedProcesses(roots []ownedProcessRoot) ([]string, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	processes, err := ownedProcessesInRoots(roots)
	if err != nil {
		return nil, err
	}
	authorization := newMountProcessAuthorization()
	for _, process := range processes {
		authorization.handles[process.pid] = process.handle
		if groupID, err := mountProcessGroupID(process.pid); err == nil {
			authorization.retainProcessGroup(groupID, process.handle)
		}
	}
	return stopOwnedProcessSet(roots, processes, authorization, true)
}

func stopAuthorizedOwnedProcesses(roots []ownedProcessRoot, authorization *mountProcessAuthorization) ([]string, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	if authorization == nil {
		return stopOwnedProcesses(roots)
	}
	processes := make([]ownedProcess, 0, len(authorization.handles))
	for pid, handle := range authorization.handles {
		references, _ := processRootReferences(pid, roots)
		if !references {
			continue
		}
		alive, err := verifyMountProcessHandleIfAlive(pid, handle)
		if err != nil {
			return nil, err
		}
		if alive {
			processes = append(processes, ownedProcess{pid: pid, handle: handle})
		}
	}
	return stopOwnedProcessSet(roots, processes, authorization, false)
}

func stopOwnedProcessSet(roots []ownedProcessRoot, processes []ownedProcess, authorization *mountProcessAuthorization, closeHandles bool) ([]string, error) {
	retained := append([]ownedProcess(nil), processes...)
	if closeHandles {
		defer func() {
			for _, process := range retained {
				_ = process.handle.Close()
			}
		}()
	}
	transients := make(map[int]*mountProcessHandle)
	defer func() {
		for _, handle := range transients {
			_ = handle.Close()
		}
	}()
	if len(retained) == 0 {
		if err := verifyOwnedProcessSet(roots, retained, transients, authorization, true); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err := verifyOwnedProcessSet(roots, retained, transients, authorization, true); err != nil {
		return nil, err
	}
	labels := make([]string, 0, len(processes))
	for _, process := range processes {
		labels = append(labels, strconv.Itoa(process.pid))
		if err := signalOwnedProcess(process, roots, syscall.SIGTERM); err != nil {
			return labels, err
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := verifyOwnedProcessSet(roots, retained, transients, authorization, true); err != nil {
			return labels, err
		}
		remaining := make([]ownedProcess, 0, len(processes))
		for _, process := range processes {
			alive, err := ownedProcessStillInRoots(process, roots)
			if err != nil {
				return labels, err
			}
			if alive {
				remaining = append(remaining, process)
			}
		}
		processes = remaining
		if len(processes) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := verifyOwnedProcessSet(roots, retained, transients, authorization, true); err != nil {
		return labels, err
	}
	for _, process := range processes {
		if err := signalOwnedProcess(process, roots, syscall.SIGKILL); err != nil {
			return labels, err
		}
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := verifyOwnedProcessSet(roots, retained, transients, authorization, true); err != nil {
			return labels, err
		}
		var survivors []string
		for _, process := range processes {
			alive, err := ownedProcessStillInRoots(process, roots)
			if err != nil {
				return labels, err
			}
			if alive {
				survivors = append(survivors, strconv.Itoa(process.pid))
			}
		}
		if len(survivors) == 0 {
			return labels, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	var survivors []string
	for _, process := range processes {
		alive, err := ownedProcessStillInRoots(process, roots)
		if err != nil {
			return labels, err
		}
		if alive {
			survivors = append(survivors, strconv.Itoa(process.pid))
		}
	}
	return labels, fmt.Errorf("task-owned processes survived teardown: %s", strings.Join(survivors, ", "))
}

// processIDsInRoots detects candidates for fail-closed discard checks. It does
// not authorize signaling or Mount reset; callers use it only to refuse while
// any process still references a Task-owned path.
func processIDsInRoots(roots []ownedProcessRoot) ([]int, error) {
	if message := processIdentityCapabilityError(); message != "" {
		return nil, fmt.Errorf("%s; refusing to release Task-owned paths", message)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	pids := []int{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		if references, _ := processRootReferences(pid, roots); references {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids, nil
}

func ownedProcessesInRoots(roots []ownedProcessRoot) ([]ownedProcess, error) {
	if message := processIdentityCapabilityError(); message != "" {
		return nil, fmt.Errorf("%s; refusing to signal Task-owned processes", message)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	processes := []ownedProcess{}
	fail := func(err error) ([]ownedProcess, error) {
		for _, process := range processes {
			_ = process.handle.Close()
		}
		return nil, err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		references, fileReference := processRootReferences(pid, roots)
		if !references {
			continue
		}
		if !fileReference {
			return fail(fmt.Errorf("process %d is identified only by CWD; refusing to authorize a signal", pid))
		}
		handle, err := openMountProcessHandle(pid)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fail(fmt.Errorf("retain Task-owned process %d: %w", pid, err))
		}
		alive, err := handle.Alive()
		if err != nil {
			_ = handle.Close()
			return fail(fmt.Errorf("check Task-owned process %d: %w", pid, err))
		}
		if !alive {
			_ = handle.Close()
			continue
		}
		references, fileReference = processRootReferences(pid, roots)
		if !references {
			_ = handle.Close()
			continue
		}
		if !fileReference {
			_ = handle.Close()
			return fail(fmt.Errorf("process %d lost its file reference during ownership proof; refusing to authorize a signal", pid))
		}
		processes = append(processes, ownedProcess{pid: pid, handle: handle})
	}
	return processes, nil
}

func verifyOwnedProcessSet(roots []ownedProcessRoot, retained []ownedProcess, transients map[int]*mountProcessHandle, authorization *mountProcessAuthorization, waitForTransients bool) error {
	byPID := make(map[int]ownedProcess, len(retained))
	for _, process := range retained {
		byPID[process.pid] = process
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		references, _ := processRootReferences(pid, roots)
		if !references {
			continue
		}
		process, retained := byPID[pid]
		if !retained {
			handle := transients[pid]
			if handle == nil {
				for _, root := range roots {
					var found bool
					handle, found, err = trackShortLivedMountProcess(pid, root.path, authorization)
					if err != nil {
						return fmt.Errorf("verify short-lived process %d during teardown: %w", pid, err)
					}
					if found {
						transients[pid] = handle
						break
					}
				}
			}
			if handle != nil {
				alive, err := verifyMountProcessHandleIfAlive(pid, handle)
				if err != nil {
					return err
				}
				if alive && waitForTransients {
					for _, root := range roots {
						if processInMount(pid, root.path) {
							if err := waitForMountProcessExit(pid, root.path, handle); err != nil {
								return err
							}
							break
						}
					}
				}
				continue
			}
			if _, _, err := store.ProcessIdentityForPID(pid); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				return fmt.Errorf("verify unretained process %d during teardown: %w", pid, err)
			}
			return fmt.Errorf("unverified process %d appeared during teardown; preserving Task data", pid)
		}
		alive, err := process.handle.Alive()
		if err != nil {
			return fmt.Errorf("verify retained process %d during teardown: %w", pid, err)
		}
		if !alive {
			if _, _, err := store.ProcessIdentityForPID(pid); err == nil {
				return fmt.Errorf("PID %d was reused during teardown; preserving Task data", pid)
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("verify process %d after exit: %w", pid, err)
			}
		}
	}
	return nil
}

func ownedProcessStillInRoots(process ownedProcess, roots []ownedProcessRoot) (bool, error) {
	alive, err := process.handle.Alive()
	if err != nil {
		return false, fmt.Errorf("check Task-owned process %d: %w", process.pid, err)
	}
	if !alive {
		return false, nil
	}
	if _, _, err := store.ProcessIdentityForPID(process.pid); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("verify identity for Task-owned process %d: %w", process.pid, err)
	}
	return processReferencesRoots(process.pid, roots), nil
}

func signalOwnedProcess(process ownedProcess, roots []ownedProcessRoot, signal syscall.Signal) error {
	stillOwned, err := ownedProcessStillInRoots(process, roots)
	if err != nil || !stillOwned {
		return err
	}
	if err := process.handle.Signal(signal); err != nil {
		return fmt.Errorf("signal Task-owned process instance %d: %w", process.pid, err)
	}
	return nil
}

func processReferencesRoots(pid int, roots []ownedProcessRoot) bool {
	references, _ := processRootReferences(pid, roots)
	return references
}

func processRootReferences(pid int, roots []ownedProcessRoot) (references, fileReference bool) {
	procRoot := filepath.Join("/proc", strconv.Itoa(pid))
	if cwd, err := os.Readlink(filepath.Join(procRoot, "cwd")); err == nil {
		for _, root := range roots {
			if processPathInsideRoot(cwd, root.path) {
				references = true
				break
			}
		}
	}
	for _, root := range roots {
		if !root.includeOpenFiles {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(procRoot, "fd"))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			path, err := os.Readlink(filepath.Join(procRoot, "fd", entry.Name()))
			if err == nil && processPathInsideRoot(path, root.path) {
				references, fileReference = true, true
				break
			}
		}
	}
	for _, root := range roots {
		if !root.includeMappedFiles {
			continue
		}
		maps, err := os.ReadFile(filepath.Join(procRoot, "maps"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(maps), "\n") {
			mappedPath := procMapPath(line)
			if mappedPath != "" && processPathInsideRoot(mappedPath, root.path) {
				references, fileReference = true, true
				break
			}
		}
	}
	return references, fileReference
}

func procMapPath(line string) string {
	remaining := line
	for field := 0; field < 5; field++ {
		remaining = strings.TrimLeft(remaining, " ")
		separator := strings.IndexByte(remaining, ' ')
		if separator < 0 {
			return ""
		}
		remaining = remaining[separator+1:]
	}
	return strings.TrimLeft(remaining, " ")
}

func processPathInsideRoot(path, root string) bool {
	// /proc cwd, fd, and maps paths are kernel-resolved. The roots are
	// canonicalized before scanning, so avoid resolving every descriptor again.
	path = strings.TrimSuffix(path, " (deleted)")
	separator := string(filepath.Separator)
	if root == separator {
		return filepath.IsAbs(path)
	}
	return path == root || strings.HasPrefix(path, root+separator)
}

func mountProcessIDs(path string) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolved
	}
	pids := []int{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err == nil && processInMount(pid, root) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func processInMount(pid int, root string) bool {
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	if err != nil {
		return false
	}
	cwd = strings.TrimSuffix(cwd, " (deleted)")
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	rel, err := filepath.Rel(root, cwd)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
