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
	"github.com/thanhbinh1905/posse/internal/execution"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) pool(ctx *axi.Context, args []string) error {
	if len(args) == 0 {
		return s.listSandboxes(ctx)
	}
	if args[0] == "reconcile-resource" {
		return s.reconcileResourceLease(ctx, args[1:])
	}
	if args[0] == "repair" {
		return s.repairPoolSandbox(ctx, args[1:])
	}
	if args[0] != "prune" {
		return axi.Usage("pool accepts the prune, repair, or reconcile-resource subcommand", "Run `posse pool` to list Sandboxes")
	}
	parsed, err := parseArgs("pool prune", args[1:], map[string]flagSpec{"yes": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("pool prune does not take positional arguments")
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
	sandboxes, err := db.Sandboxes(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	tasks, err := db.Tasks(ctx.Context, project.ID, true)
	if err != nil {
		return err
	}
	clean := sandboxWorktreeClean
	if project.IsWorkspace() {
		clean = workspaceSandboxClean
	}
	items := []pruneItem{}
	prunableSandboxPaths := map[string]bool{}
	managedSandboxes := make([]store.Sandbox, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		if safeManagedSandboxPath(home, project.Name, sandbox.Path) {
			managedSandboxes = append(managedSandboxes, sandbox)
		}
	}
	for _, sandbox := range pruneCandidates(ctx.Context, managedSandboxes, cfg.Pool.KeepIdle, clean) {
		prunableSandboxPaths[filepath.Clean(sandbox.Path)] = true
		size, err := pathBytes(sandbox.Path)
		if err != nil {
			return err
		}
		reason := "idle Sandbox beyond keep_idle"
		if sandbox.State == "broken" {
			reason = "quarantined Sandbox is safe to remove"
		}
		items = append(items, pruneItem{kind: "sandbox", label: fmt.Sprintf("sandbox-%d", sandbox.Number), path: sandbox.Path, state: sandbox.State, reason: reason, bytes: size, sandbox: sandbox})
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
	branches, err := mergedOrDiscardedBranchItems(ctx.Context, db, home, project, tasks)
	if err != nil {
		return err
	}
	items = append(items, branches...)
	remoteBranches, err := mergedRemoteBranchItems(ctx.Context, db, project, cfg, tasks)
	if err != nil {
		return err
	}
	items = append(items, remoteBranches...)
	registrations, err := staleOwnedWorktreeItems(ctx.Context, db, project, home, sandboxes)
	if err != nil {
		return err
	}
	for _, registration := range registrations {
		if !prunableSandboxPaths[filepath.Clean(registration.path)] && !prunableSandboxPaths[filepath.Clean(registration.sandboxPath)] {
			items = append(items, registration)
		}
	}
	rows := make([]map[string]any, 0, len(items))
	for _, item := range items {
		rows = append(rows, map[string]any{"kind": item.kind, "item": item.label, "path": item.path, "state": item.state, "size_bytes": item.bytes, "reason": item.reason, "applied": false})
	}
	var pruneErr error
	if parsed.Bool("yes") {
		for index, item := range items {
			if err := s.applyPruneItem(ctx.Context, db, home, project, item); err != nil {
				var commandError *axi.Error
				if errors.As(err, &commandError) && commandError.Code == "prune_skipped" {
					rows[index]["state"] = "skipped"
					rows[index]["reason"] = commandError.Message
					continue
				}
				pruneErr = err
				break
			}
			rows[index]["applied"] = true
		}
	}
	var orphanPanes axi.Object
	if pruneErr == nil {
		orphanPanes, pruneErr = s.pruneOrphanPanes(ctx, db, project, parsed.Bool("yes"))
	}
	result := axi.Object{{Key: "project", Value: project.Name}, {Key: "prune", Value: rows}, {Key: "dry_run", Value: !parsed.Bool("yes")}, {Key: "help", Value: []any{"Run `posse pool prune --yes` to remove these safe, Posse-owned items"}}}
	for _, field := range orphanPanes {
		if field.Key == "orphan_panes" || field.Key == "closed_panes" {
			result = append(result, field)
		}
	}
	if pruneErr != nil {
		var structured *axi.Error
		if !errors.As(normalizeCommandError(pruneErr), &structured) {
			structured = axi.Failure("prune_incomplete", pruneErr.Error(), true, "Inspect the applied rows and retry `posse pool prune` after resolving the failure")
		}
		result = append(result, axi.Field{Key: "error", Value: structured})
		if err := ctx.Print(result); err != nil {
			return errors.Join(pruneErr, err)
		}
		return axi.Reported(structured)
	}
	return ctx.Print(result)
}

func (s *Service) applyScratchPruneItem(ctx context.Context, db *store.DB, home string, project store.Project, item pruneItem) error {
	err := withSandboxStateLock(ctx, db, func() error {
		_, reason, taskID, eligible, err := taskScratchPruneOwnership(ctx, db, project, item.seq)
		if err != nil {
			return err
		}
		if !eligible || taskID != item.taskID {
			return axi.Failure("prune_incomplete", fmt.Sprintf("Task scratch ownership changed for %s (%s)", item.label, reason), true, "Retry `posse pool prune` after the Task operation finishes")
		}
		expected, err := taskScratchPath(home, project, store.Task{Seq: item.seq})
		if err != nil {
			return err
		}
		if filepath.Clean(item.path) != filepath.Clean(expected) {
			return axi.Failure("prune_incomplete", fmt.Sprintf("refuse to prune scratch outside the validated Task path for %s", item.label), true, "Inspect POSSE_HOME, then retry `posse pool prune`")
		}
		root, err := validatedTaskScratchRoot(home, project, store.Task{Seq: item.seq})
		if err != nil {
			return axi.Failure("prune_incomplete", err.Error(), true, "Repair the Task scratch path, then retry `posse pool prune`")
		}
		if _, err := stopOwnedProcesses([]ownedProcessRoot{{path: root, includeOpenFiles: true, includeMappedFiles: true}}); err != nil {
			return axi.Failure("prune_incomplete", err.Error(), true, "Resolve Task scratch processes, then retry `posse pool prune`")
		}
		if err := removeTaskScratch(home, project, store.Task{Seq: item.seq}); err != nil {
			return axi.Failure("prune_incomplete", err.Error(), true, "Repair Task scratch permissions, then retry `posse pool prune`")
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
	return axi.Failure("prune_incomplete", err.Error(), true, "Resolve Task scratch ownership, then retry `posse pool prune`")
}

func (s *Service) applyBranchPruneItem(ctx context.Context, db *store.DB, project store.Project, item pruneItem) error {
	// Serialize the final worktree check and ref deletion with Posse Sandbox claims.
	return withSandboxStateLock(ctx, db, func() error {
		task, err := db.Task(ctx, project.ID, taskIDString(item.seq))
		if err != nil {
			return err
		}
		if task.ID != item.taskID || task.Branch != item.label || task.State != store.StateMerged && task.State != store.StateTornDown {
			return fmt.Errorf("refuse to prune branch %s after its Task changed", item.label)
		}
		blocked, err := taskBranchPruneBlocked(ctx, db, task)
		if err != nil {
			return err
		}
		if blocked {
			return fmt.Errorf("refuse to prune branch %s while Task t%d has an active operation or held Sandbox", item.label, task.Seq)
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
	case "sandbox":
		claimed, err := db.ClaimSandboxPrune(ctx, project.ID, item.sandbox.ID)
		if err != nil {
			return err
		}
		if !claimed {
			return axi.Failure("prune_skipped", "Sandbox ownership changed since prune was planned", false)
		}
		var removeErr error
		if project.IsWorkspace() {
			removeErr = s.removeWorkspaceSandbox(ctx, db, project, item.sandbox.Path)
		} else if _, statErr := os.Stat(item.sandbox.Path); os.IsNotExist(statErr) {
			removeErr = removeStaleWorktreeRegistration(ctx, project.Root, item.sandbox.Path)
		} else {
			if err := makeSandboxUntrackedWritable(ctx, item.sandbox.Path, "pristine"); err != nil {
				return errors.Join(err, db.RestoreSandboxAfterPrune(ctx, item.sandbox.ID, item.sandbox.State))
			}
			if item.sandbox.State == "broken" && !sandboxWorktreeClean(ctx, item.sandbox.Path) {
				if !sandboxTrackedWorktreeClean(ctx, item.sandbox.Path) {
					return errors.Join(fmt.Errorf("broken Sandbox has tracked changes and cannot be pruned safely"), db.RestoreSandboxAfterPrune(ctx, item.sandbox.ID, item.sandbox.State))
				}
				if _, err := gitOutput(ctx, item.sandbox.Path, "clean", "-fdx"); err != nil {
					return errors.Join(err, db.RestoreSandboxAfterPrune(ctx, item.sandbox.ID, item.sandbox.State))
				}
			}
			if err := unlockSandbox(ctx, project.Root, item.sandbox.Path); err != nil {
				return errors.Join(err, db.RestoreSandboxAfterPrune(ctx, item.sandbox.ID, item.sandbox.State))
			}
			removeErr = s.removeSandbox(ctx, project.Root, item.sandbox.Path)
		}
		if removeErr != nil {
			return errors.Join(removeErr, db.RestoreSandboxAfterPrune(ctx, item.sandbox.ID, item.sandbox.State))
		}
		return db.DeletePrunedSandbox(ctx, project.ID, item.sandbox.ID)
	case "scratch":
		return s.applyScratchPruneItem(ctx, db, home, project, item)
	case "artifacts":
		return removeTaskArtifacts(home, project, item.seq, item.files)
	case "branch":
		return s.applyBranchPruneItem(ctx, db, project, item)
	case "remote-branch":
		return applyRemoteBranchPruneItem(ctx, db, home, project, item)
	case "worktree-registration":
		if item.sandbox.ID != 0 {
			claimed, err := db.ClaimSandboxPrune(ctx, project.ID, item.sandbox.ID)
			if err != nil || !claimed {
				return err
			}
		}
		repoRoot := item.repoRoot
		if repoRoot == "" {
			repoRoot = project.Root
		}
		if err := removeStaleWorktreeRegistration(ctx, repoRoot, item.path); err != nil {
			if item.sandbox.ID != 0 {
				return errors.Join(err, db.RestoreSandboxAfterPrune(ctx, item.sandbox.ID, item.sandbox.State))
			}
			return err
		}
		if item.sandbox.ID != 0 {
			if project.IsWorkspace() && item.sandboxPath != "" {
				return db.RestoreSandboxAfterPrune(ctx, item.sandbox.ID, item.sandbox.State)
			}
			return db.DeletePrunedSandbox(ctx, project.ID, item.sandbox.ID)
		}
		return nil
	default:
		return fmt.Errorf("unknown prune item kind %q", item.kind)
	}
}

func pruneCandidates(ctx context.Context, sandboxes []store.Sandbox, keepIdle int, clean func(context.Context, string) bool) []store.Sandbox {
	idle := []store.Sandbox{}
	remove := []store.Sandbox{}
	for _, sandbox := range sandboxes {
		info, err := os.Lstat(sandbox.Path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		switch sandbox.State {
		case "idle":
			idle = append(idle, sandbox)
		case "broken":
			if clean(ctx, sandbox.Path) || sandboxTrackedWorktreeClean(ctx, sandbox.Path) && sandboxHasReadOnlyUntracked(ctx, sandbox.Path) {
				remove = append(remove, sandbox)
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

func (s *Service) reconcileResourceLease(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("pool reconcile-resource", args, map[string]flagSpec{"user-approved": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 || strings.TrimSpace(parsed.Flags["user-approved"]) == "" {
		return axi.Usage("pool reconcile-resource requires a lease ID and --user-approved <explanation>", "Review `posse pool` first; reconcile only after the recorded owner has stopped and its filesystem or Git effect has been inspected")
	}
	id, err := strconv.ParseInt(parsed.Positionals[0], 10, 64)
	if err != nil || id < 1 {
		return axi.Usage("resource lease ID must be a positive integer")
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
	lease, err := db.ResourceLeaseByID(ctx.Context, id)
	if store.IsNotFound(err) {
		return axi.Failure("resource_lease_missing", fmt.Sprintf("resource lease %d no longer exists", id), false)
	}
	if err != nil {
		return err
	}
	if lease.SandboxID != 0 {
		return axi.Failure("sandbox_lease_managed", "Sandbox resource leases are reconciled by Sandbox recovery and cannot be released manually", false)
	}
	if lease.IntentID != 0 {
		var intentExists bool
		if err := db.QueryRowContext(ctx.Context, `SELECT EXISTS(SELECT 1 FROM intents WHERE id=?)`, lease.IntentID).Scan(&intentExists); err != nil {
			return err
		}
		if intentExists {
			return axi.Failure("resource_intent_active", "the resource lease belongs to an active intent; recover that operation before reconciling it", true)
		}
	}
	if lease.OwnerPID > 0 && lease.OwnerBootID != "" && lease.OwnerStartTime != "" {
		bootID, startTime, identityErr := store.ProcessIdentityForPID(lease.OwnerPID)
		if identityErr == nil && bootID == lease.OwnerBootID && startTime == lease.OwnerStartTime {
			return axi.Failure("resource_owner_live", "the recorded resource owner process is still running", true,
				fmt.Sprintf("lease %d is %s scope %s owned by %s (pid %d)", lease.ID, lease.ResourceType, lease.Scope(), lease.Owner, lease.OwnerPID))
		}
	}
	quote := strings.TrimSpace(parsed.Flags["user-approved"])
	if err := db.ReconcileResourceLease(ctx.Context, lease.ID, lease.CustodyToken, "User-approved custody reconciliation: "+quote); err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "reconciled_resource_lease", Value: lease.ID}, {Key: "scope", Value: lease.Scope()}, {Key: "owner", Value: lease.Owner}})
}

func (s *Service) listSandboxes(ctx *axi.Context) error {
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
	_, err = s.prepareProject(store.WithDeferrableWrites(ctx.Context), db, project)
	if err = deferObservationContention(ctx, err, "Project observations were deferred; showing the last recorded state."); err != nil {
		return err
	}
	if err := execution.NewManager(db).EnsureSandboxLeases(ctx.Context); err != nil {
		return resourceLeaseError(err)
	}
	sandboxes, err := db.Sandboxes(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	brokenReasons := map[int]string{}
	for _, sandbox := range sandboxes {
		if sandbox.State == "broken" {
			notices, err := db.Notices(ctx.Context, project.ID, false)
			if err != nil {
				return err
			}
			brokenReasons = latestSandboxBrokenSummaries(notices)
			break
		}
	}
	rows := make([]any, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		task := ""
		if sandbox.TaskID != 0 {
			if taskRow, e := db.TaskByID(ctx.Context, project.ID, sandbox.TaskID); e == nil {
				task = taskIDString(taskRow.Seq)
			}
		}
		row := map[string]any{"sandbox": fmt.Sprintf("sandbox-%d", sandbox.Number), "state": sandbox.State, "task": task, "path": sandbox.Path}
		if sandbox.State == "broken" {
			why := brokenReasons[sandbox.Number]
			if why == "" {
				why = fmt.Sprintf("sandbox-%d: no recorded reason", sandbox.Number) + sandboxBrokenRemedy(project, sandbox)
			}
			row["why"] = why
		}
		rows = append(rows, row)
	}
	leases, err := db.AllResourceLeases(ctx.Context)
	if err != nil {
		return err
	}
	leaseRows := make([]any, 0, len(leases))
	for _, lease := range leases {
		leaseRows = append(leaseRows, map[string]any{"id": lease.ID, "type": lease.ResourceType, "scope": lease.Scope(), "owner": lease.Owner, "custody": lease.Custody})
	}
	return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "sandboxes", Value: rows}, {Key: "resources", Value: leaseRows}, {Key: "help", Value: []any{"Run `posse pool prune` to review idle Sandboxes beyond keep_idle", "Inspect a resource lease before `posse pool reconcile-resource <id> --user-approved <explanation>`"}}})
}

func acquireSandbox(ctx context.Context, db *store.DB, project store.Project, task store.Task, home string, cleanMode string, setup []string) (store.Sandbox, error) {
	base := poolProjectPath(home, project.Name)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return store.Sandbox{}, err
	}
	for attempts := 0; attempts < 64; attempts++ {
		var sandbox store.Sandbox
		err := withSandboxStateLock(ctx, db, func() error {
			// Protect the oldest idle checkout before AcquireSandbox can expose
			// it as held. New checkouts are created with Git's atomic --lock.
			sandboxes, err := db.Sandboxes(ctx, project.ID)
			if err != nil {
				return err
			}
			var prelocked store.Sandbox
			for _, candidate := range sandboxes {
				if candidate.State == "idle" && (prelocked.ID == 0 || candidate.Number < prelocked.Number) {
					prelocked = candidate
				}
			}
			if prelocked.ID != 0 {
				if _, statErr := os.Stat(filepath.Join(prelocked.Path, ".git")); statErr == nil {
					if err := lockAvailableSandbox(ctx, db, project, prelocked, task); err != nil {
						return err
					}
				} else if !os.IsNotExist(statErr) {
					return statErr
				}
			}
			var claimErr error
			sandbox, claimErr = db.AcquireSandbox(ctx, project.ID, task.ID, base)
			if claimErr != nil {
				if prelocked.ID != 0 {
					current, lookupErr := db.Sandboxes(ctx, project.ID)
					if lookupErr != nil {
						return errors.Join(claimErr, lookupErr)
					}
					for _, candidate := range current {
						if candidate.ID == prelocked.ID && candidate.State == "idle" {
							return errors.Join(claimErr, unlockTaskSandbox(ctx, project.Root, prelocked.Path, task.Seq))
						}
					}
				}
				return claimErr
			}
			if prelocked.ID != 0 && prelocked.ID != sandbox.ID {
				return fmt.Errorf("prelocked sandbox %d differs from claimed sandbox %d: %w", prelocked.ID, sandbox.ID, store.ErrStateRace)
			}
			if err := execution.NewManager(db).EnsureSandboxLeases(ctx); err != nil {
				return errors.Join(resourceLeaseError(err), abandonUnpreparedSandbox(ctx, db, project, task, sandbox))
			}
			return nil
		})
		if err != nil {
			return sandbox, err
		}
		unborn := defaultBranchUnborn(ctx, projectRepoTarget(project))
		if _, statErr := os.Stat(filepath.Join(sandbox.Path, ".git")); os.IsNotExist(statErr) {
			if err := os.MkdirAll(filepath.Dir(sandbox.Path), 0o700); err != nil {
				return sandbox, err
			}
			lock := []string{"--lock", "--reason", fmt.Sprintf("posse: held by t%d", task.Seq)}
			args := append(append([]string{"worktree", "add", "--orphan", "-b", task.Branch}, lock...), sandbox.Path)
			if !unborn {
				ref, remote := sandboxDefaultRef(ctx, projectRepoTarget(project))
				if remote {
					if _, err := gitFetch(ctx, project.Root, "origin"); err != nil {
						return sandbox, err
					}
					ref = "refs/remotes/origin/" + project.DefaultBranch
				}
				args = append(append([]string{"worktree", "add", "--detach"}, lock...), sandbox.Path, ref)
			}
			if _, err := gitOutput(ctx, project.Root, args...); err != nil {
				if breakErr := breakSandbox(ctx, db, project, task, sandbox, "Git could not create the Sandbox worktree", err); breakErr != nil {
					return sandbox, errors.Join(err, breakErr)
				}
				continue
			}
		}
		if err := withSandboxStateLock(ctx, db, func() error {
			return lockClaimedSandbox(ctx, db, project, sandbox, task)
		}); err != nil {
			return sandbox, err
		}
		if unborn {
			// With no commit to branch from, the Task branch starts as an
			// orphan; its first commit becomes the default branch's first.
			if err := resetUnbornWorktree(ctx, sandbox.Path, task.Branch, cleanMode); err != nil {
				if breakErr := breakSandbox(ctx, db, project, task, sandbox, "Sandbox could not be reset to a clean state", err); breakErr != nil {
					return sandbox, errors.Join(err, breakErr)
				}
				continue
			}
		} else {
			if err := resetSandbox(ctx, sandbox.Path, project, cleanMode); err != nil {
				if breakErr := breakSandbox(ctx, db, project, task, sandbox, "Sandbox could not be reset to a clean state", err); breakErr != nil {
					return sandbox, errors.Join(err, breakErr)
				}
				continue
			}
			base, err := taskCheckoutBase(ctx, project.Root, task, "", task.BaseRef)
			if err != nil {
				return sandbox, err
			}
			if _, err := gitOutput(ctx, sandbox.Path, "checkout", "-b", task.Branch, base); err != nil {
				return sandbox, err
			}
		}
		if err := db.UpdateTaskSandboxPath(ctx, task.ID, sandbox.ID, sandbox.Path); err != nil {
			return sandbox, err
		}
		for _, command := range setup {
			if output, err := commandOutput(ctx, sandbox.Path, command); err != nil {
				return sandbox, axi.Failure("sandbox_setup_failed", fmt.Sprintf("setup %q failed: %s", command, truncate(strings.TrimSpace(output), 1200)), false, "Fix the Pool setup command, then retry the Task")
			}
		}
		return sandbox, nil
	}
	return store.Sandbox{}, fmt.Errorf("no clean Sandbox could be acquired")
}

func abandonUnpreparedSandbox(ctx context.Context, db *store.DB, project store.Project, task store.Task, sandbox store.Sandbox) error {
	if !project.IsWorkspace() {
		if _, err := os.Stat(filepath.Join(sandbox.Path, ".git")); err == nil {
			if err := unlockRegisteredSandbox(ctx, project.Root, sandbox.Path, task.Seq); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if err := db.BeginSandboxRelease(ctx, sandbox.ID, task.ID); err != nil {
		return err
	}
	return finishReleasingSandboxLocked(ctx, db, project, sandbox, task)
}

var errSandboxDefaultRefUnavailable = errors.New("no safe local default ref is available to reset the Sandbox")

func sandboxDefaultRef(ctx context.Context, target repoTarget) (string, bool) {
	if _, err := gitOutput(ctx, target.Root, "remote", "get-url", "origin"); err == nil {
		return "refs/remotes/origin/" + target.DefaultBranch, true
	}
	return "refs/heads/" + target.DefaultBranch, false
}

// unbornIdleBranch is the unborn branch an idle Sandbox waits on while the
// default branch has no commit. Git cannot detach HEAD without a commit, and
// waiting on the default branch would check it out in the Sandbox once it
// gains its first commit.
const unbornIdleBranch = "posse/unborn"

// defaultBranchUnborn reports whether the repository's default branch has no
// commit yet: its checkout is on that branch, and neither the branch nor
// origin's copy exists.
func defaultBranchUnborn(ctx context.Context, target repoTarget) bool {
	head, err := gitOutput(ctx, target.Root, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || head != "refs/heads/"+target.DefaultBranch {
		return false
	}
	for _, ref := range []string{"refs/heads/" + target.DefaultBranch, "refs/remotes/origin/" + target.DefaultBranch} {
		if _, err := gitOutput(ctx, target.Root, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
			return false
		}
	}
	return true
}

// resetUnbornWorktree empties a worktree of a repository whose default branch
// has no commit and leaves it on the unborn branch named branch.
func resetUnbornWorktree(ctx context.Context, path, branch, clean string) error {
	if _, err := gitOutput(ctx, path, "switch", "--discard-changes", "--orphan", branch); err != nil {
		return err
	}
	if _, err := gitOutput(ctx, path, "read-tree", "--empty"); err != nil {
		return err
	}
	return cleanWorktree(ctx, path, clean)
}

func sandboxResetRef(ctx context.Context, target repoTarget, fetch bool) (string, bool, error) {
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
	return "", remote, fmt.Errorf("%w: checked %s and %s", errSandboxDefaultRefUnavailable, remoteRef, localRef)
}

func projectRepoTarget(project store.Project) repoTarget {
	return repoTarget{Root: project.Root, DefaultBranch: project.DefaultBranch}
}

func resetSandbox(ctx context.Context, path string, project store.Project, clean string) error {
	return resetWorktree(ctx, path, projectRepoTarget(project), clean, true)
}

// resetWorktree detaches a Sandbox worktree at the repository's default branch
// and cleans it. Teardown and unrequested workspace Members use the last fetched
// ref; acquisition fetches before new work starts.
func resetWorktree(ctx context.Context, path string, target repoTarget, clean string, fetch bool) error {
	if defaultBranchUnborn(ctx, target) {
		return resetUnbornWorktree(ctx, path, unbornIdleBranch, clean)
	}
	ref, remote, err := sandboxResetRef(ctx, target, fetch)
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
	return cleanWorktree(ctx, path, clean)
}

// cleanWorktree removes untracked files, and ignored ones for a pristine clean,
// and verifies that nothing remains.
func cleanWorktree(ctx context.Context, path, clean string) error {
	if err := makeSandboxUntrackedWritable(ctx, path, clean); err != nil {
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
		return fmt.Errorf("sandbox is not clean: %s", status)
	}
	return nil
}

func makeSandboxUntrackedWritable(ctx context.Context, path, clean string) error {
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
			return fmt.Errorf("git reported an untracked path outside the Sandbox: %q", candidate)
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
		if err := makeSandboxPathWritable(root, target); err != nil {
			return err
		}
	}
	return nil
}

func makeSandboxPathWritable(root, relative string) error {
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
		return makeSandboxTreeWritable(current)
	}
	return nil
}

func makeSandboxTreeWritable(path string) error {
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
		if err := makeSandboxTreeWritable(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func sandboxWorktreeClean(ctx context.Context, path string) bool {
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		return false
	}
	status, err := gitStatusOutput(ctx, path, "--porcelain")
	return err == nil && status == ""
}

func sandboxTrackedWorktreeClean(ctx context.Context, path string) bool {
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		return false
	}
	status, err := gitStatusOutput(ctx, path, "--porcelain", "--untracked-files=no")
	return err == nil && status == ""
}

func sandboxHasReadOnlyUntracked(ctx context.Context, path string) bool {
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

func releaseSandbox(ctx context.Context, db *store.DB, project store.Project, task store.Task, clean string, approvedDiscard bool) ([]string, error) {
	sandboxes, err := db.Sandboxes(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	var sandbox store.Sandbox
	for _, candidate := range sandboxes {
		if candidate.ID == task.SandboxID {
			sandbox = candidate
			break
		}
	}
	if sandbox.ID == 0 || sandbox.TaskID != task.ID || (sandbox.State != "held" && sandbox.State != "releasing") {
		return nil, nil
	}
	if sandbox.State == "releasing" {
		if err := requireNoSandboxProcesses(sandbox.Path, approvedDiscard); err != nil {
			return nil, err
		}
		return nil, finishReleasingSandbox(ctx, db, project, sandbox, task)
	}
	var killed []string
	if err := requireNoSandboxProcesses(sandbox.Path, approvedDiscard); err != nil {
		return nil, err
	}
	var resetErr error
	if project.IsWorkspace() {
		resetErr = releaseWorkspaceSandbox(ctx, db, project, sandbox, clean, removeGitWorktree)
	} else {
		// Teardown is local and must not wait on origin. The next Sandbox
		// acquisition fetches before starting new work.
		resetErr = resetWorktree(ctx, sandbox.Path, projectRepoTarget(project), clean, false)
	}
	if err := resetErr; err != nil {
		if errors.Is(err, errSandboxDefaultRefUnavailable) {
			// Keep the Task's Sandbox held and retryable until a safe default ref
			// can be fetched; never quarantine it as broken during local recovery.
			return killed, err
		}
		if breakErr := breakSandbox(ctx, db, project, task, sandbox, "Sandbox could not be reset during release", err); breakErr != nil {
			return killed, errors.Join(err, breakErr)
		}
		if approvedDiscard {
			return killed, nil // Broken Sandbox stays quarantined; approved discard may finish.
		}
		return killed, err
	}
	return killed, withSandboxStateLock(ctx, db, func() error {
		if err := db.BeginSandboxRelease(ctx, sandbox.ID, task.ID); err != nil {
			return err
		}
		return finishReleasingSandboxLocked(ctx, db, project, sandbox, task)
	})
}

func requireNoSandboxProcesses(path string, approvedDiscard bool) error {
	root, err := normalizeOwnedProcessRoot(path)
	if err != nil {
		return err
	}
	pids, err := processIDsInRoots([]ownedProcessRoot{{path: root, includeOpenFiles: true, includeMappedFiles: true}})
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		return nil
	}
	processes := taskOwnedProcessDetails(pids)
	description := describeTaskProcesses(processes)
	if approvedDiscard {
		return axi.Failure("discard_processes_present", "refusing to release a discarded Task Sandbox while processes still reference it: "+description, true, "No process was stopped or signalled. Inspect the listed processes, then retry discard")
	}
	return axi.Failure("sandbox_processes_present", "cannot release the Sandbox while live processes still reference it: "+description, true, "Stop or wait for the listed processes to exit, then retry Teardown. The Sandbox remains held until no process has it as its cwd, open file, or mapped file")
}

func finishReleasingSandbox(ctx context.Context, db *store.DB, project store.Project, sandbox store.Sandbox, task store.Task) error {
	return withSandboxStateLock(ctx, db, func() error {
		return finishReleasingSandboxLocked(ctx, db, project, sandbox, task)
	})
}

func finishReleasingSandboxLocked(ctx context.Context, db *store.DB, project store.Project, sandbox store.Sandbox, task store.Task) error {
	current, err := db.SandboxByTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if current.ID != sandbox.ID || current.State != "releasing" {
		return store.ErrStateRace
	}
	if !project.IsWorkspace() {
		if err := unlockRegisteredSandbox(ctx, project.Root, sandbox.Path, task.Seq); err != nil {
			return err
		}
	}
	return db.FinishSandboxRelease(ctx, sandbox.ID, task.ID)
}

// Git worktree locks protect held Sandboxes from Herdr's single-force UI delete.
// A repeated acquire/reconcile may encounter its own existing lock.
func lockSandbox(ctx context.Context, repo, path string, seq int) error {
	reason := fmt.Sprintf("posse: held by t%d", seq)
	current, err := sandboxLockReason(ctx, repo, path)
	if err != nil {
		return err
	}
	if current == reason {
		return nil
	}
	if current != "" {
		return fmt.Errorf("sandbox %s is locked by %q, not %q", path, current, reason)
	}
	_, err = gitOutput(ctx, repo, "worktree", "lock", "--reason", reason, path)
	return err
}

func relockIfStillHeld(ctx context.Context, db *store.DB, project store.Project, sandbox store.Sandbox, task store.Task) error {
	sandboxes, err := db.Sandboxes(ctx, project.ID)
	if err != nil {
		return err
	}
	for _, current := range sandboxes {
		if current.ID == sandbox.ID && current.State == "held" && current.TaskID == task.ID {
			return lockSandbox(ctx, project.Root, sandbox.Path, task.Seq)
		}
	}
	return nil
}

func unlockTaskSandbox(ctx context.Context, repo, path string, seq int) error {
	reason, err := sandboxLockReason(ctx, repo, path)
	if err != nil {
		return err
	}
	if reason != "" && reason != fmt.Sprintf("posse: held by t%d", seq) {
		return fmt.Errorf("refuse to release sandbox %s locked by %q, not t%d", path, reason, seq)
	}
	return unlockSandbox(ctx, repo, path)
}

func unlockSandbox(ctx context.Context, repo, path string) error {
	reason, err := sandboxLockReason(ctx, repo, path)
	if err != nil {
		return err
	}
	if reason == "" {
		return nil
	}
	if _, ok := posseLockTaskSeq(reason); !ok {
		return fmt.Errorf("refuse to unlock Sandbox %s locked by %q", path, reason)
	}
	_, err = gitOutput(ctx, repo, "worktree", "unlock", path)
	return err
}

var errSandboxNotRegistered = errors.New("sandbox is missing from git worktree list")

func sandboxLockReason(ctx context.Context, repo, path string) (string, error) {
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
	return "", fmt.Errorf("%w: %s", errSandboxNotRegistered, path)
}

func unlockRegisteredSandbox(ctx context.Context, repo, path string, seq int) error {
	_, err := sandboxLockReason(ctx, repo, path)
	if errors.Is(err, errSandboxNotRegistered) {
		return nil
	}
	if err != nil {
		return err
	}
	return unlockTaskSandbox(ctx, repo, path, seq)
}

func breakSandbox(ctx context.Context, db *store.DB, project store.Project, task store.Task, sandbox store.Sandbox, reason string, cause error) error {
	if sandbox.State != "held" || sandbox.TaskID != task.ID {
		return store.ErrStateRace
	}
	if err := withSandboxStateLock(ctx, db, func() error {
		if !project.IsWorkspace() {
			// A broken checkout is quarantined, not deleted. Git may still
			// register a missing checkout; release its lock before clearing
			// ownership. A failed worktree.add has no checkout to unlock.
			if err := unlockRegisteredSandbox(ctx, project.Root, sandbox.Path, task.Seq); err != nil {
				return err
			}
		}
		if err := db.BreakSandbox(ctx, sandbox.ID, task.ID); err != nil {
			if !project.IsWorkspace() {
				if _, reasonErr := sandboxLockReason(ctx, project.Root, sandbox.Path); reasonErr == nil {
					return errors.Join(err, relockIfStillHeld(ctx, db, project, sandbox, task))
				}
			}
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	_, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "sandbox_broken", Summary: sandboxBrokenSummary(project, sandbox, reason, cause), DataJSON: `{}`})
	return err
}

// sandboxBrokenSummary states why the Sandbox was quarantined and the commands
// that recover it. Workspace Sandboxes cannot be repaired, only pruned.
func sandboxBrokenSummary(project store.Project, sandbox store.Sandbox, reason string, cause error) string {
	summary := fmt.Sprintf("sandbox-%d: %s", sandbox.Number, reason)
	if cause != nil {
		summary += ": " + truncate(strings.TrimSpace(cause.Error()), 400)
	}
	return summary + sandboxBrokenRemedy(project, sandbox)
}

func sandboxBrokenRemedy(project store.Project, sandbox store.Sandbox) string {
	if project.IsWorkspace() {
		return ". Remove it with `posse pool prune --yes` once its work is saved"
	}
	return fmt.Sprintf(". Repair it with `posse pool repair sandbox-%d`, or remove it with `posse pool prune --yes`", sandbox.Number)
}

// latestSandboxBrokenSummaries maps each Sandbox number to the summary of its
// most recent sandbox_broken Notice, acknowledged or not.
func latestSandboxBrokenSummaries(notices []store.Notice) map[int]string {
	latest := map[int]string{}
	newest := map[int]int64{}
	for _, notice := range notices {
		var number int
		if notice.Kind != "sandbox_broken" {
			continue
		}
		if _, err := fmt.Sscanf(notice.Summary, "sandbox-%d:", &number); err != nil {
			continue
		}
		if notice.ID > newest[number] {
			newest[number], latest[number] = notice.ID, notice.Summary
		}
	}
	return latest
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
	excluded           map[int]bool
	excludedExact      map[int]bool
}

type ownedProcess struct {
	pid    int
	handle *sandboxProcessHandle
}

func verifyNoTaskOwnedProcesses(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task) error {
	if processes := taskContainmentProcessDetails(task); len(processes) > 0 {
		return axi.Failure("discard_processes_present", "refusing approved discard while processes remain in Task-owned paths", true, fmt.Sprint(processes), "No process was stopped or signalled. Inspect the Sandbox and scratch processes, then retry discard")
	}
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
		return axi.Failure("discard_processes_present", "refusing approved discard while processes remain in Task-owned paths", true, strings.Join(labels, ","), "No process was stopped or signalled. Inspect the Sandbox and scratch processes, then retry discard")
	}
	return nil
}

func taskOwnedProcessRoots(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task) ([]ownedProcessRoot, error) {
	roots := []ownedProcessRoot{}
	sandbox, err := db.SandboxByTask(ctx, task.ID)
	if err == nil && sandbox.TaskID == task.ID && (sandbox.State == "held" || sandbox.State == "releasing") {
		if sandbox.ID != task.SandboxID || task.WorktreePath == "" || !pathInside(task.WorktreePath, sandbox.Path) {
			return nil, fmt.Errorf("refuse to inspect processes for Task %s without a verified held or releasing Sandbox", taskIDString(task.Seq))
		}
		root, err := normalizeOwnedProcessRoot(sandbox.Path)
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
	authorization := newSandboxProcessAuthorization()
	for _, process := range processes {
		authorization.handles[process.pid] = process.handle
		if groupID, err := sandboxProcessGroupID(process.pid); err == nil {
			authorization.retainProcessGroup(groupID, process.handle)
		}
	}
	return stopOwnedProcessSet(roots, processes, authorization, true)
}

func stopAuthorizedOwnedProcesses(roots []ownedProcessRoot, authorization *sandboxProcessAuthorization) ([]string, error) {
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
		alive, err := verifySandboxProcessHandleIfAlive(pid, handle)
		if err != nil {
			return nil, err
		}
		if alive {
			processes = append(processes, ownedProcess{pid: pid, handle: handle})
		}
	}
	return stopOwnedProcessSet(roots, processes, authorization, false)
}

func stopOwnedProcessSet(roots []ownedProcessRoot, processes []ownedProcess, authorization *sandboxProcessAuthorization, closeHandles bool) ([]string, error) {
	retained := append([]ownedProcess(nil), processes...)
	if closeHandles {
		defer func() {
			for _, process := range retained {
				_ = process.handle.Close()
			}
		}()
	}
	transients := make(map[int]*sandboxProcessHandle)
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
// not authorize signaling or Sandbox reset; callers use it only to refuse while
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
		handle, err := openSandboxProcessHandle(pid)
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

func verifyOwnedProcessSet(roots []ownedProcessRoot, retained []ownedProcess, transients map[int]*sandboxProcessHandle, authorization *sandboxProcessAuthorization, waitForTransients bool) error {
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
					handle, found, err = trackShortLivedSandboxProcess(pid, root.path, authorization)
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
				alive, err := verifySandboxProcessHandleIfAlive(pid, handle)
				if err != nil {
					return err
				}
				if alive && waitForTransients {
					for _, root := range roots {
						if processInSandbox(pid, root.path) {
							if err := waitForSandboxProcessExit(pid, root.path, handle); err != nil {
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
			return fmt.Errorf("unverified process %d %s; preserving Task data", pid, authorization.unverifiedProcessState(pid))
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
			excluded, err := ownedProcessRootExcludesPID(pid, root)
			if err != nil {
				return true, false
			}
			if excluded {
				continue
			}
			if processPathInsideRoot(cwd, root.path) {
				references = true
				break
			}
		}
	}
	for _, root := range roots {
		excluded, err := ownedProcessRootExcludesPID(pid, root)
		if err != nil {
			return true, false
		}
		if excluded || !root.includeOpenFiles {
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
		excluded, err := ownedProcessRootExcludesPID(pid, root)
		if err != nil {
			return true, false
		}
		if excluded || !root.includeMappedFiles {
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

func ownedProcessRootExcludesPID(pid int, root ownedProcessRoot) (bool, error) {
	if root.excludedExact[pid] {
		return true, nil
	}
	if len(root.excluded) == 0 {
		return false, nil
	}
	return taskProcessHasExcludedAncestor(pid, root.excluded)
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

func sandboxProcessIDs(path string) ([]int, error) {
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
		if err == nil && processInSandbox(pid, root) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func processInSandbox(pid int, root string) bool {
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
