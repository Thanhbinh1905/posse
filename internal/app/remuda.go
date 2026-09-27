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
	cfg, err := s.prepareProject(ctx.Context, db, project)
	if err != nil {
		return err
	}
	mounts, err := db.Mounts(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	clean := mountWorktreeClean
	if project.IsWorkspace() {
		clean = workspaceMountClean
	}
	remove := pruneCandidates(ctx.Context, mounts, cfg.Remuda.KeepIdle, clean)
	rows := make([]any, 0, len(remove))
	for _, mount := range remove {
		rows = append(rows, map[string]any{"mount": fmt.Sprintf("mount-%d", mount.Number), "path": mount.Path, "state": mount.State})
		if parsed.Bool("yes") {
			claimed, err := db.ClaimMountPrune(ctx.Context, project.ID, mount.ID)
			if err != nil {
				return err
			}
			if !claimed {
				continue
			}
			var removeErr error
			if project.IsWorkspace() {
				removeErr = s.removeWorkspaceMount(ctx.Context, db, project, mount.Path)
			} else {
				if err := makeMountUntrackedWritable(ctx.Context, mount.Path, "pristine"); err != nil {
					return errors.Join(err, db.RestoreMountAfterPrune(ctx.Context, mount.ID, mount.State))
				}
				if mount.State == "broken" && !mountWorktreeClean(ctx.Context, mount.Path) {
					if !mountTrackedWorktreeClean(ctx.Context, mount.Path) {
						return errors.Join(fmt.Errorf("broken mount has tracked changes and cannot be pruned safely"), db.RestoreMountAfterPrune(ctx.Context, mount.ID, mount.State))
					}
					if _, err := gitOutput(ctx.Context, mount.Path, "clean", "-fdx"); err != nil {
						return errors.Join(err, db.RestoreMountAfterPrune(ctx.Context, mount.ID, mount.State))
					}
				}
				if err := unlockMount(ctx.Context, project.Root, mount.Path); err != nil {
					return errors.Join(err, db.RestoreMountAfterPrune(ctx.Context, mount.ID, mount.State))
				}
				removeErr = s.removeMount(ctx.Context, project.Root, mount.Path)
			}
			if err := removeErr; err != nil {
				return errors.Join(err, db.RestoreMountAfterPrune(ctx.Context, mount.ID, mount.State))
			}
			if err := db.DeletePrunedMount(ctx.Context, project.ID, mount.ID); err != nil {
				return err
			}
		}
	}
	return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "prune", Value: rows}, {Key: "dry_run", Value: !parsed.Bool("yes")}, {Key: "help", Value: []any{"Run `posse remuda prune --yes` to remove these Mounts"}}})
}

func pruneCandidates(ctx context.Context, mounts []store.Mount, keepIdle int, clean func(context.Context, string) bool) []store.Mount {
	idle := []store.Mount{}
	remove := []store.Mount{}
	for _, mount := range mounts {
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
	if _, err := s.prepareProject(ctx.Context, db, project); err != nil {
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
		mount, err := db.AcquireMount(ctx, project.ID, task.ID, base)
		if err != nil {
			return store.Mount{}, err
		}
		if _, statErr := os.Stat(filepath.Join(mount.Path, ".git")); os.IsNotExist(statErr) {
			if err := os.MkdirAll(filepath.Dir(mount.Path), 0o700); err != nil {
				return mount, err
			}
			ref, remote := mountDefaultRef(ctx, projectRepoTarget(project))
			if remote {
				if _, err := gitOutput(ctx, project.Root, "fetch", "origin"); err != nil {
					return mount, err
				}
				ref = "refs/remotes/origin/" + project.DefaultBranch
			}
			if _, err := gitOutput(ctx, project.Root, "worktree", "add", "--detach", mount.Path, ref); err != nil {
				if breakErr := breakMount(ctx, db, project, task, mount, "Git could not create the Mount worktree"); breakErr != nil {
					return mount, errors.Join(err, breakErr)
				}
				continue
			}
		}
		if err := lockMount(ctx, project.Root, mount.Path, task.Seq); err != nil {
			return mount, err
		}
		if err := resetMount(ctx, mount.Path, project, cleanMode); err != nil {
			if breakErr := breakMount(ctx, db, project, task, mount, "Mount could not be reset to a clean state"); breakErr != nil {
				return mount, errors.Join(err, breakErr)
			}
			continue
		}
		if _, err := gitOutput(ctx, mount.Path, "checkout", "-b", task.Branch, task.BaseRef); err != nil {
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

func mountDefaultRef(ctx context.Context, target repoTarget) (string, bool) {
	if _, err := gitOutput(ctx, target.Root, "remote", "get-url", "origin"); err == nil {
		return "refs/remotes/origin/" + target.DefaultBranch, true
	}
	return "refs/heads/" + target.DefaultBranch, false
}

func projectRepoTarget(project store.Project) repoTarget {
	return repoTarget{Root: project.Root, DefaultBranch: project.DefaultBranch}
}

func resetMount(ctx context.Context, path string, project store.Project, clean string) error {
	return resetWorktree(ctx, path, projectRepoTarget(project), clean, true)
}

// resetWorktree detaches a Mount worktree at the repository's fresh default
// branch and cleans it. fetch is false for workspace members a Task does not
// request, which are reset to what the last fetch saw.
func resetWorktree(ctx context.Context, path string, target repoTarget, clean string, fetch bool) error {
	ref, remote := mountDefaultRef(ctx, target)
	if remote && fetch {
		if _, err := gitOutput(ctx, target.Root, "fetch", "origin"); err != nil {
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
	status, err := gitOutput(ctx, path, "status", "--porcelain")
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
	status, err := gitOutput(ctx, path, "status", "--porcelain")
	return err == nil && status == ""
}

func mountTrackedWorktreeClean(ctx context.Context, path string) bool {
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		return false
	}
	status, err := gitOutput(ctx, path, "status", "--porcelain", "--untracked-files=no")
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

func releaseMount(ctx context.Context, db *store.DB, project store.Project, task store.Task, clean string) ([]string, error) {
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
	if mount.ID == 0 || mount.State != "held" || mount.TaskID != task.ID {
		return nil, nil
	}
	killed, err := stopMountProcesses(mount.Path)
	if err != nil {
		return killed, err
	}
	var resetErr error
	if project.IsWorkspace() {
		resetErr = releaseWorkspaceMount(ctx, db, project, mount, clean)
	} else {
		resetErr = resetMount(ctx, mount.Path, project, clean)
	}
	if err := resetErr; err != nil {
		if breakErr := breakMount(ctx, db, project, task, mount, "Mount could not be reset during release"); breakErr != nil {
			return killed, errors.Join(err, breakErr)
		}
		return killed, err
	}
	if !project.IsWorkspace() {
		if err := unlockMount(ctx, project.Root, mount.Path); err != nil {
			return killed, err
		}
	}
	if err := db.ReleaseMount(ctx, mount.ID, task.ID); err != nil {
		if !project.IsWorkspace() {
			return killed, errors.Join(err, lockMount(ctx, project.Root, mount.Path, task.Seq))
		}
		return killed, err
	}
	return killed, nil
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
		return fmt.Errorf("Mount %s is locked by %q, not %q", path, current, reason)
	}
	_, err = gitOutput(ctx, repo, "worktree", "lock", "--reason", reason, path)
	return err
}

func unlockMount(ctx context.Context, repo, path string) error {
	reason, err := mountLockReason(ctx, repo, path)
	if err != nil {
		return err
	}
	if reason == "" {
		return nil
	}
	if !strings.HasPrefix(reason, "posse: held by t") {
		return fmt.Errorf("refuse to unlock Mount %s locked by %q", path, reason)
	}
	_, err = gitOutput(ctx, repo, "worktree", "unlock", path)
	return err
}

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
	return "", fmt.Errorf("Mount %s is missing from git worktree list", path)
}

func breakMount(ctx context.Context, db *store.DB, project store.Project, task store.Task, mount store.Mount, reason string) error {
	if mount.State != "held" || mount.TaskID != task.ID {
		return store.ErrStateRace
	}
	if err := db.BreakMount(ctx, mount.ID, task.ID); err != nil {
		return err
	}
	_, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "mount_broken", Summary: fmt.Sprintf("mount-%d: %s", mount.Number, reason), DataJSON: `{}`})
	return err
}

func stopMountProcesses(path string) ([]string, error) {
	root, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	pids, err := mountProcessIDs(root)
	if err != nil {
		return nil, err
	}
	allPIDs := append([]int(nil), pids...)
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(pids) > 0 && time.Now().Before(deadline) {
		remaining := pids[:0]
		for _, pid := range pids {
			if processInMount(pid, root) {
				remaining = append(remaining, pid)
			}
		}
		pids = remaining
		if len(pids) > 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	deadline = time.Now().Add(time.Second)
	var survivors []int
	for time.Now().Before(deadline) {
		survivors, err = mountProcessIDs(root)
		if err != nil {
			return nil, err
		}
		if len(survivors) == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	labels := make([]string, 0, len(allPIDs))
	for _, pid := range allPIDs {
		labels = append(labels, strconv.Itoa(pid))
	}
	if len(survivors) > 0 {
		remaining := make([]string, len(survivors))
		for index, pid := range survivors {
			remaining[index] = strconv.Itoa(pid)
		}
		return labels, fmt.Errorf("mount processes survived teardown: %s", strings.Join(remaining, ", "))
	}
	return labels, nil
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
