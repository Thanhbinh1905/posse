package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) repairPoolSandbox(ctx *axi.Context, args []string) error {
	if len(args) != 1 {
		return axi.Usage("pool repair requires one Sandbox number, such as sandbox-2")
	}
	number, err := strconv.Atoi(args[0])
	if err != nil {
		var ok bool
		number, ok = sandboxDirectoryNumber(args[0])
		if !ok {
			return axi.Usage("pool repair requires a Sandbox number, such as sandbox-2")
		}
	}
	if number < 1 {
		return axi.Usage("pool repair requires a positive Sandbox number")
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

	var repaired store.Sandbox
	var preservedTaskSeq int
	err = withSandboxStateLock(ctx.Context, db, func() error {
		sandboxes, err := db.Sandboxes(ctx.Context, project.ID)
		if err != nil {
			return err
		}
		for _, sandbox := range sandboxes {
			if sandbox.Number == number {
				repaired = sandbox
				break
			}
		}
		if repaired.ID == 0 {
			return axi.Failure("sandbox_not_found", fmt.Sprintf("Sandbox %d does not exist in project %s", number, project.Name), false)
		}
		if repaired.State != "broken" || repaired.TaskID != 0 {
			return axi.Failure("sandbox_not_broken", fmt.Sprintf("sandbox-%d is %s and cannot be repaired", number, repaired.State), false)
		}
		if project.IsWorkspace() {
			return axi.Failure("sandbox_repair_unsupported", "workspace Sandboxes require Member-level recovery and cannot be repaired by this command", false)
		}
		if !safeManagedSandboxPath(home, project.Name, repaired.Path) {
			return axi.Failure("sandbox_repair_unsafe_path", "refusing to repair a Sandbox outside its validated Pool directory", false)
		}
		task, hasTask, err := repairTaskForSandbox(ctx.Context, db, project, repaired.Path)
		if err != nil {
			return err
		}
		if hasTask {
			preservedTaskSeq = task.Seq
		}
		if err := requireSandboxRepairHasNoProcesses(repaired.Path); err != nil {
			return err
		}
		if err := verifySandboxRepository(ctx.Context, project.Root, repaired.Path); err != nil {
			return axi.Failure("sandbox_repair_git_metadata", err.Error(), true, "Repair the worktree's Git metadata, then retry `posse pool repair`")
		}
		lockOwnerSeq, err := inspectSandboxWorktreeRegistration(ctx.Context, project, repaired, task, hasTask)
		if err != nil {
			return axi.Failure("sandbox_repair_git_metadata", err.Error(), true, "Resolve the Git worktree registration or lock, then retry `posse pool repair`")
		}

		status, err := gitStatusOutput(ctx.Context, repaired.Path, "--porcelain", "--untracked-files=all")
		if err != nil {
			return axi.Failure("sandbox_repair_git_metadata", err.Error(), true, "Repair the checkout's Git metadata, then retry `posse pool repair`")
		}
		if hasTask {
			if err := preserveReportAttachments(ctx.Context, db, home, project, task); err != nil {
				return axi.Failure("sandbox_repair_preservation_failed", err.Error(), true, "Resolve the attachment conflict without deleting the Sandbox, then retry `posse pool repair`")
			}
		} else if status != "" {
			return axi.Failure("sandbox_repair_unowned_work", "the broken Sandbox contains changes but no completed Task can own their preservation", false, "Keep the Sandbox quarantined and recover its files before retrying repair")
		}

		if err := resetWorktree(ctx.Context, repaired.Path, projectRepoTarget(project), cfg.Pool.Clean, false); err != nil {
			return axi.Failure("sandbox_repair_reset_failed", err.Error(), true, "The Sandbox remains quarantined; fix the reported reset error and retry `posse pool repair`")
		}
		status, err = gitStatusOutput(ctx.Context, repaired.Path, "--porcelain", "--untracked-files=all")
		if err != nil || status != "" {
			return axi.Failure("sandbox_repair_not_clean", fmt.Sprintf("Sandbox did not become clean after reset: %s", strings.TrimSpace(status)), true, "Keep the Sandbox quarantined and resolve the remaining Git state")
		}
		if err := requireSandboxRepairHasNoProcesses(repaired.Path); err != nil {
			return err
		}
		if lockOwnerSeq != 0 {
			if err := unlockTaskSandbox(ctx.Context, project.Root, repaired.Path, lockOwnerSeq); err != nil {
				return axi.Failure("sandbox_repair_git_lock", err.Error(), true, "Resolve the Posse worktree lock, then retry `posse pool repair`")
			}
		}
		lockReason, err := sandboxLockReason(ctx.Context, project.Root, repaired.Path)
		if err != nil || lockReason != "" {
			return axi.Failure("sandbox_repair_git_lock", fmt.Sprintf("Sandbox worktree remains locked or unregistered: %s %s", lockReason, errorText(err)), true, "Resolve the Git worktree lock or registration, then retry `posse pool repair`")
		}
		if err := db.RepairBrokenSandbox(ctx.Context, repaired.ID); err != nil {
			return err
		}
		repaired.State = "idle"
		repaired.TaskID = 0
		return nil
	})
	if err != nil {
		return err
	}
	result := axi.Object{
		{Key: "project", Value: project.Name},
		{Key: "sandbox", Value: fmt.Sprintf("sandbox-%d", repaired.Number)},
		{Key: "state", Value: repaired.State},
		{Key: "preserved", Value: preservedTaskSeq != 0},
		{Key: "help", Value: []any{"Unlanded files were saved to the completed Task's attachments before reset", "Run `posse pool prune` separately to remove safe, idle Sandboxes"}},
	}
	if preservedTaskSeq != 0 {
		result = append(result, axi.Field{Key: "task_id", Value: taskIDString(preservedTaskSeq)})
	}
	return ctx.Print(result)
}

func repairTaskForSandbox(ctx context.Context, db *store.DB, project store.Project, path string) (store.Task, bool, error) {
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return store.Task{}, false, err
	}
	var latest store.Task
	for _, task := range tasks {
		if filepath.Clean(task.WorktreePath) != filepath.Clean(path) {
			continue
		}
		if task.State != store.StateTornDown || task.SandboxID != 0 {
			return store.Task{}, false, axi.Failure("sandbox_repair_task_active", fmt.Sprintf("Task t%d still owns or may need this Sandbox; finish or retry its Teardown first", task.Seq), false)
		}
		if intent, err := db.IntentByTask(ctx, task.ID); err == nil {
			return store.Task{}, false, axi.Failure("sandbox_repair_task_active", fmt.Sprintf("Task t%d still has an active %s intent at step %s", task.Seq, intent.Command, intent.Step), true)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return store.Task{}, false, err
		}
		if task.Seq > latest.Seq {
			latest = task
		}
	}
	if latest.ID == 0 {
		return store.Task{}, false, nil
	}
	return latest, true, nil
}

func requireSandboxRepairHasNoProcesses(path string) error {
	root, err := normalizeOwnedProcessRoot(path)
	if err != nil {
		return axi.Failure("sandbox_process_scan_failed", err.Error(), true, "Resolve Sandbox path access and retry repair")
	}
	pids, err := processIDsInRoots([]ownedProcessRoot{{path: root, includeOpenFiles: true, includeMappedFiles: true}})
	if err != nil {
		return axi.Failure("sandbox_process_scan_failed", err.Error(), true, "Resolve process inspection and retry repair")
	}
	if len(pids) == 0 {
		return nil
	}
	labels := make([]string, len(pids))
	for index, pid := range pids {
		labels[index] = strconv.Itoa(pid)
	}
	return axi.Failure("sandbox_processes_present", "refusing to repair a Sandbox while processes still reference it", true, strings.Join(labels, ","), "No process was stopped or signalled. Inspect the listed processes, then retry repair")
}

func verifySandboxRepository(ctx context.Context, repo, sandbox string) error {
	repoCommon, err := gitCommonDirectory(ctx, repo)
	if err != nil {
		return fmt.Errorf("inspect project Git metadata: %w", err)
	}
	sandboxCommon, err := gitCommonDirectory(ctx, sandbox)
	if err != nil {
		return fmt.Errorf("inspect Sandbox Git metadata: %w", err)
	}
	if repoCommon != sandboxCommon {
		return fmt.Errorf("sandbox Git metadata belongs to %s, not project repository %s", sandboxCommon, repoCommon)
	}
	return nil
}

func gitCommonDirectory(ctx context.Context, root string) (string, error) {
	value, err := gitOutput(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(root, value)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func inspectSandboxWorktreeRegistration(ctx context.Context, project store.Project, sandbox store.Sandbox, task store.Task, hasTask bool) (int, error) {
	reason, err := sandboxLockReason(ctx, project.Root, sandbox.Path)
	if errors.Is(err, errSandboxNotRegistered) {
		if _, repairErr := gitOutput(ctx, project.Root, "worktree", "repair", sandbox.Path); repairErr != nil {
			return 0, fmt.Errorf("git does not register the Sandbox and worktree repair failed: %w", repairErr)
		}
		reason, err = sandboxLockReason(ctx, project.Root, sandbox.Path)
	}
	if err != nil {
		return 0, err
	}
	if reason == "" {
		return 0, nil
	}
	if !hasTask {
		return 0, fmt.Errorf("sandbox is locked by %q and no completed Task owns that lock", reason)
	}
	seq, ok := posseLockTaskSeq(reason)
	if !ok || seq != task.Seq {
		return 0, fmt.Errorf("sandbox is locked by %q, not completed Task t%d", reason, task.Seq)
	}
	if filepath.Clean(task.WorktreePath) != filepath.Clean(sandbox.Path) {
		return 0, fmt.Errorf("task t%d does not own the locked Sandbox path", task.Seq)
	}
	return seq, nil
}
