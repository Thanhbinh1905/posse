package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/sessionref"
	"github.com/thanhbinh1905/posse/internal/store"
)

type teardownPanes struct {
	Closed  []string
	Foreign []string
}

type taskPaneTeardownPlan struct {
	panes             teardownPanes
	paneIDs           map[string]bool
	tabs              map[string]bool
	workspaces        map[string]bool
	returnFocusToLead bool
}

type teardownResult struct {
	Panes               teardownPanes
	BranchRemoved       bool
	RemoteBranchRemoved bool
	StoppedProcesses    []string
	// EscapedProcesses are Task-marked processes that left the Sandbox and
	// Task scratch without a retained ancestor; Teardown cannot prove they are
	// Task-owned, so it reports them instead of stopping them.
	EscapedProcesses []taskProcessDetail
	CacheMaintenance string
	AlreadyTornDown  bool
}

type paneProcessInfo struct {
	PaneID                 string `json:"pane_id"`
	ShellPID               int    `json:"shell_pid"`
	ForegroundProcessGroup int    `json:"foreground_process_group_id"`
	ForegroundProcesses    []struct {
		PID  int    `json:"pid"`
		Name string `json:"name"`
		CWD  string `json:"cwd"`
	} `json:"foreground_processes"`
}

func autoTeardownMode(cfg config.Config) string {
	if cfg.Defaults.AutoTeardown == "" {
		return "finished"
	}
	return cfg.Defaults.AutoTeardown
}

func shouldAutoTeardownMerged(cfg config.Config) bool {
	return autoTeardownMode(cfg) == "finished" || autoTeardownMode(cfg) == "merged"
}

func (s *Service) autoTeardownMergedTasks(ctx context.Context, db *store.DB, project store.Project, cfg config.Config) error {
	if s.Herdr == nil {
		return nil
	}
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.State != store.StateMerged {
			continue
		}
		mergedPR := task.MergeMode == "pr" || task.MergeMode == "no-mistakes"
		if project.IsWorkspace() {
			repos, err := db.TaskRepos(ctx, task.ID)
			if err != nil {
				return err
			}
			for _, repo := range repos {
				mergedPR = mergedPR || (repo.PRURL != "" && repo.State == store.TaskRepoMerged)
			}
		}
		if !shouldAutoTeardownMerged(cfg) && !mergedPR {
			continue
		}
		if _, err := s.teardownTask(ctx, db, project, cfg, task, false, ""); err != nil {
			var commandError *axi.Error
			if errors.As(err, &commandError) && commandError.Code == "intent_active" {
				continue
			}
			if noticeErr := s.recordTeardownIncomplete(ctx, db, project, task, err); noticeErr != nil {
				return noticeErr
			}
		}
	}
	return nil
}

func safeMergedPRWorktree(ctx context.Context, db *store.DB, project store.Project, task store.Task, observation store.PRObservation) (bool, error) {
	if observation.PRURL != task.PRURL || observation.HeadSHA == "" || observation.MergeCommit == "" || task.MergedRef != observation.MergeCommit || task.Branch == "" || task.WorktreePath == "" {
		return false, nil
	}
	branchSHA, err := gitOutput(ctx, project.Root, "rev-parse", "refs/heads/"+task.Branch)
	if err != nil {
		return false, nil
	}
	worktreeBranch, err := gitOutput(ctx, task.WorktreePath, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || worktreeBranch != task.Branch {
		return false, nil
	}
	worktreeSHA, err := gitOutput(ctx, task.WorktreePath, "rev-parse", "HEAD")
	if err != nil || worktreeSHA != branchSHA {
		return false, nil
	}
	if branchSHA != observation.HeadSHA {
		verified, err := db.WasVerifiedPRHead(ctx, task.ID, observation.PRURL, observation.HeadSHA)
		if err != nil || !verified {
			return false, err
		}
		if observation.MergeCommit == observation.HeadSHA {
			return false, nil // No distinct merged commit proves the moved tip is safe.
		}
		// A merge of the PR target after the PR was merged is safe only if
		// it contains both the PR head and the merge commit and carries no
		// additional tree changes. Never discard a subsequent follow-up.
		for _, ancestor := range []string{observation.HeadSHA, observation.MergeCommit} {
			if _, err := gitOutput(ctx, task.WorktreePath, "merge-base", "--is-ancestor", ancestor, branchSHA); err != nil {
				return false, nil
			}
		}
		// Compare against the newest target-branch ancestor of the Task
		// tip. Other changes that merged on the target after this PR are safe;
		// changes made only on the Task branch are not.
		targetRef, err := taskTargetRef(ctx, project, task)
		if err != nil {
			return false, nil
		}
		base, err := gitOutput(ctx, task.WorktreePath, "merge-base", branchSHA, targetRef)
		if err != nil {
			return false, nil
		}
		if _, err := gitOutput(ctx, task.WorktreePath, "merge-base", "--is-ancestor", observation.MergeCommit, base); err != nil {
			return false, nil
		}
		if _, err := gitOutput(ctx, task.WorktreePath, "diff", "--quiet", base, branchSHA); err != nil {
			return false, nil
		}
	}
	status, err := gitStatusOutput(ctx, task.WorktreePath, "--porcelain", "--untracked-files=all")
	if err != nil || status != "" {
		return false, nil
	}
	return true, nil
}

// requireNoLiveTaskAgent refuses approved discard while a Task-owned pane has
// a foreground process. Interrupted discard recovery requires Worker exit;
// a fresh approved teardown retains exact-instance shutdown authority.
func (s *Service) requireNoLiveTaskAgent(ctx context.Context, project store.Project, task store.Task) error {
	const identityHelp = "Inspect the Worker with `posse peek`, then retry `posse teardown <task> --discard --user-approved \"<User's words>\"` to cancel its verified instance"
	if s.Herdr == nil {
		return axi.Failure("discard_agent_identity_unavailable", "cannot verify whether the Task has a live agent", true, identityHelp)
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return axi.Failure("discard_agent_identity_unavailable", "cannot verify whether the Task has a live agent", true, err.Error(), identityHelp)
	}
	tabs := taskTabs(snapshot, project, task)
	for _, pane := range snapshot.Panes {
		if pane.Label == task.PaneLabel && pane.Agent != "" && !ownsTaskPane(snapshot, project, task, tabs, pane) {
			return axi.Failure("discard_agent_identity_unavailable", "cannot verify that the Task-labeled pane belongs to this Worker", true, pane.PaneID, identityHelp)
		}
		if !ownsTaskPane(snapshot, project, task, tabs, pane) {
			continue
		}
		raw, err := s.herdrCall(ctx, "pane.process_info", map[string]any{"pane_id": pane.PaneID})
		if err != nil {
			return axi.Failure("discard_agent_identity_unavailable", "cannot verify the Task pane's foreground process", true, err.Error(), identityHelp)
		}
		var info struct {
			ProcessInfo struct {
				PaneID                 string `json:"pane_id"`
				ShellPID               int    `json:"shell_pid"`
				ForegroundProcessGroup int    `json:"foreground_process_group_id"`
			} `json:"process_info"`
		}
		if err := json.Unmarshal(raw, &info); err != nil || info.ProcessInfo.PaneID != pane.PaneID || info.ProcessInfo.ShellPID <= 1 {
			return axi.Failure("discard_agent_identity_unavailable", "Herdr did not return a valid process identity for the Task pane", true, pane.PaneID, identityHelp)
		}
		foreground := info.ProcessInfo.ForegroundProcessGroup
		if foreground > 1 && foreground != info.ProcessInfo.ShellPID {
			if pane.Agent != "" {
				return axi.Failure("discard_live_agent", "refusing to discard a Task while its Worker agent is still running", false, "Interrupted discard recovery will not retarget a live Worker. Inspect it, then retry approved teardown to retain its exact process instance")
			}
			return axi.Failure("discard_agent_identity_unavailable", "cannot verify that the Task pane's non-shell foreground process has exited", true, pane.PaneID, identityHelp)
		}
	}
	return nil
}

// verifySandboxForegroundOwnershipWithAuthorization retains exact process
// handles for Task panes and shell-only panes in the held Sandbox. CWD discovery
// may detect a process but cannot authorize cleanup. Herdr 0.9.1 has no atomic
// pane-close precondition, so teardown never closes panes by target ID alone.
func (s *Service) verifyNoForeignTaskMarkedForeground(ctx context.Context, task store.Task, pane herdr.Pane) error {
	info, err := s.sandboxPaneProcessInfo(ctx, pane.PaneID)
	if err != nil {
		// A sibling pane that closed during the scan has no foreground left.
		if missingPaneError(err) {
			return nil
		}
		if snapshot, snapshotErr := s.snapshot(ctx); snapshotErr == nil {
			if _, found := snapshotPane(snapshot, pane.PaneID); !found {
				return nil
			}
		}
		return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("could not verify processes in Task-marked pane %s", pane.PaneID), true, err.Error())
	}
	for _, process := range info.ForegroundProcesses {
		if process.PID > 1 && processHasTaskMarker(process.PID, task.ContainmentMarker) {
			return axi.Failure("sandbox_process_not_owned", fmt.Sprintf("Task-marked foreground PID %d is running in unowned pane %s", process.PID, pane.PaneID), false, "Stop or inspect the foreground, preserve its work, then retry Teardown")
		}
	}
	return nil
}

func (s *Service) verifySandboxForegroundOwnershipWithAuthorization(ctx context.Context, db *store.DB, project store.Project, task store.Task, authorization *sandboxProcessAuthorization) error {
	sandboxes, err := db.Sandboxes(ctx, project.ID)
	if err != nil {
		return axi.Failure("sandbox_process_identity_unavailable", "could not verify Sandbox ownership before releasing it", true, err.Error())
	}
	var heldSandbox store.Sandbox
	for _, sandbox := range sandboxes {
		if sandbox.ID == task.SandboxID && sandbox.TaskID == task.ID && sandbox.State == "held" {
			heldSandbox = sandbox
			break
		}
	}
	if heldSandbox.ID == 0 {
		return nil
	}
	if heldSandbox.Path == "" || task.WorktreePath == "" || !pathInside(task.WorktreePath, heldSandbox.Path) {
		return axi.Failure("sandbox_process_identity_unavailable", "could not match the Task worktree to its held Sandbox before releasing it", true)
	}
	home, err := s.homePath()
	if err != nil {
		return axi.Failure("sandbox_process_identity_unavailable", "could not locate Posse home while retaining Task process identities", true, err.Error())
	}
	roots, err := taskOwnedProcessRoots(ctx, db, home, project, task)
	if err != nil {
		return axi.Failure("sandbox_process_identity_unavailable", "could not verify Task-owned process paths", true, err.Error())
	}
	for index := range roots {
		roots[index].includeOpenFiles = true
		roots[index].includeMappedFiles = true
	}
	if err := retainTaskContainmentProcesses(authorization, task, roots); err != nil {
		return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("could not retain Task-contained process identities before releasing the Sandbox: %v", err), true)
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return axi.Failure("sandbox_process_identity_unavailable", "could not verify foreground process ownership before releasing the Sandbox", true, err.Error())
	}
	tabs := taskTabs(snapshot, project, task)
	for _, pane := range snapshot.Panes {
		paneInsideSandbox := pathInside(pane.CWD, heldSandbox.Path)
		agentName := snapshotAgentName(snapshot, pane.PaneID)
		taskPane := ownsTaskPane(snapshot, project, task, tabs, pane)
		if !taskPane && task.ContainmentMarker != "" && pane.WorkspaceID == task.HerdrWorkspaceID {
			if err := s.verifyNoForeignTaskMarkedForeground(ctx, task, pane); err != nil {
				return err
			}
		}
		foreignShellPane := paneInsideSandbox && !taskPane && shellOnlyPaneInSandbox(snapshot, pane, heldSandbox.Path)
		if paneInsideSandbox && !taskPane && !foreignShellPane {
			return axi.Failure("sandbox_process_not_owned", fmt.Sprintf("pane %s in the held Sandbox is not owned by Task %s", pane.PaneID, taskIDString(task.Seq)), false, "Stop or inspect the unrelated pane, preserve its work, then retry Teardown")
		}
		if !paneInsideSandbox && !taskPane {
			continue
		}
		first, err := s.sandboxPaneProcessInfo(ctx, pane.PaneID)
		if err != nil {
			return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("could not verify processes in Sandbox pane %s", pane.PaneID), true, err.Error())
		}
		foreground := first.ForegroundProcessGroup
		foregroundPID := foreground
		foregroundName, foregroundCWD, foregroundProcessFound := foregroundProcess(first, foregroundPID)
		expectedAgentName := pane.Agent
		if expectedAgentName == "" {
			expectedAgentName = agentName
		}
		if !paneInsideSandbox && foregroundCWD == "" && (agentName != "" || pane.Agent != "") {
			return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("Herdr did not identify the working directory of the foreground process in pane %s", pane.PaneID), true)
		}
		if !paneInsideSandbox && !taskPane && !pathInside(foregroundCWD, heldSandbox.Path) {
			continue
		}
		if foreground <= 1 {
			if pane.Agent != "" || agentName != "" || paneInsideSandbox {
				return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("Herdr reports no identifiable foreground process in Sandbox pane %s", pane.PaneID), true)
			}
			continue
		}
		if !foregroundProcessFound {
			return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("Herdr did not identify foreground process group %d in pane %s", foreground, pane.PaneID), true)
		}
		if foreignShellPane && foreground != first.ShellPID {
			return axi.Failure("sandbox_process_not_owned", fmt.Sprintf("pane %s in the held Sandbox has a foreground process that is not its shell", pane.PaneID), false, "Stop or inspect the unrelated pane, preserve its work, then retry Teardown")
		}
		if foreground == first.ShellPID {
			if !taskPane {
				return axi.Failure("sandbox_process_not_owned", fmt.Sprintf("unowned pane %s still references the Sandbox through shell PID %d", pane.PaneID, foregroundPID), false, "Close the unowned pane or move it outside the Sandbox, then retry Teardown")
			}
			if err := verifyTaskPaneShellImage(task, foregroundPID); err != nil {
				return axi.Failure("sandbox_process_not_owned", fmt.Sprintf("Task pane %s shell PID %d no longer matches its recorded shell image: %v", pane.PaneID, foregroundPID, err), false, "Stop or inspect the foreground, preserve its work, or close the Task pane, then retry Teardown")
			}
		}
		if taskPane && foreground != first.ShellPID && expectedAgentName == "" {
			return axi.Failure("sandbox_process_not_owned", fmt.Sprintf("pane %s has a foreground process without a verified Task Worker identity", pane.PaneID), false, "Stop or inspect the foreground command, preserve its work, then retry Teardown")
		}
		if foreground != first.ShellPID && expectedAgentName != "" && (foregroundName == "" || !strings.EqualFold(foregroundName, expectedAgentName)) {
			return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("foreground process %d in Sandbox pane %s does not match Herdr's agent identity %q", foregroundPID, pane.PaneID, expectedAgentName), true)
		}

		firstBootID, firstStartTime, foregroundIdentityErr := store.ProcessIdentityForPID(foregroundPID)
		foregroundExited := processGone(foregroundIdentityErr)
		if foregroundIdentityErr != nil && !foregroundExited {
			return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("could not bind foreground PID %d to a process start identity", foregroundPID), true, foregroundIdentityErr.Error())
		}
		processPIDs := sandboxPaneProcessIDs(first, heldSandbox.Path)
		if taskPane {
			processPIDs = append(processPIDs, first.ShellPID, foregroundPID)
			sort.Ints(processPIDs)
			processPIDs = uniqueTeardownProcessIDs(processPIDs)
		}
		for _, pid := range processPIDs {
			bootID, startTime, identityErr := store.ProcessIdentityForPID(pid)
			if identityErr != nil {
				if processGone(identityErr) {
					if pid == foregroundPID {
						foregroundExited = true
					}
					continue
				}
				return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("could not bind Sandbox process %d to a process start identity", pid), true, identityErr.Error())
			}
			handle, bindErr := authorization.bind(pid)
			if bindErr != nil {
				if processGone(bindErr) {
					if pid == foregroundPID {
						foregroundExited = true
					}
					continue
				}
				return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("could not retain the exact Sandbox process %d for cleanup: %v", pid, bindErr), true, bindErr.Error())
			}
			if handle.Identity() != bootID+"/"+startTime || pid == foregroundPID && handle.Identity() != firstBootID+"/"+firstStartTime {
				return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("Sandbox PID %d changed process identity while retaining its cleanup handle", pid), true)
			}
		}
		if err := authorization.captureDescendants(); err != nil {
			return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("could not retain Task process descendants in pane %s", pane.PaneID), true, err.Error())
		}
		currentSnapshot, err := s.snapshot(ctx)
		if err != nil {
			return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("could not recheck Herdr ownership for pane %s", pane.PaneID), true, err.Error())
		}
		currentPane, currentFound := snapshotPane(currentSnapshot, pane.PaneID)
		currentAgentName := snapshotAgentName(currentSnapshot, pane.PaneID)
		currentTaskPane := currentFound && ownsTaskPane(currentSnapshot, project, task, taskTabs(currentSnapshot, project, task), currentPane)
		currentForeignShell := currentFound && shellOnlyPaneInSandbox(currentSnapshot, currentPane, heldSandbox.Path)
		currentAgentMatches := currentFound && taskAgentMatchesPane(currentSnapshot, project, task, currentPane)
		if currentFound && currentAgentName == "" && currentPane.Agent == "" && pathInside(currentPane.CWD, heldSandbox.Path) {
			currentAgentMatches = true // The Task pane's shell or command is authorized by its exact pane identity.
		}
		if !currentFound || foreignShellPane && !currentForeignShell || !foreignShellPane && (!currentTaskPane || !currentAgentMatches) {
			return axi.Failure("sandbox_process_not_owned", fmt.Sprintf("foreground process %d in Sandbox pane %s no longer belongs to Task %s", foreground, pane.PaneID, taskIDString(task.Seq)), false, "Stop or inspect the unrelated agent, preserve its work, then retry Teardown")
		}
		second, err := s.sandboxPaneProcessInfo(ctx, pane.PaneID)
		if err != nil {
			return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("could not recheck foreground process identity in pane %s", pane.PaneID), true, err.Error())
		}
		if second.ShellPID != first.ShellPID {
			return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("shell process identity changed while verifying pane %s", pane.PaneID), true)
		}
		if second.ForegroundProcessGroup != foreground && second.ForegroundProcessGroup > 1 {
			if authorization.handle(second.ForegroundProcessGroup) != nil {
				if _, err := authorization.verifyIfAlive(second.ForegroundProcessGroup); err != nil {
					return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("could not verify changed foreground process in pane %s", pane.PaneID), true, err.Error())
				}
			} else {
				alive, err := unretainedSandboxProcessAlive(second.ForegroundProcessGroup)
				if err != nil {
					return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("could not verify changed foreground process in pane %s", pane.PaneID), true, err.Error())
				}
				if alive {
					return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("unretained foreground process %d appeared while verifying pane %s", second.ForegroundProcessGroup, pane.PaneID), true)
				}
			}
		}
		// Herdr's foreground list can gain or lose short-lived children between
		// reads. Only the first read authorizes cleanup; verify those retained
		// instances here and let the later Sandbox scan refuse any new live PID.
		for _, pid := range processPIDs {
			if _, err := authorization.verifyIfAlive(pid); err != nil {
				return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("could not recheck Sandbox process %d identity", pid), true, err.Error())
			}
		}
		foregroundHandle := authorization.handle(foregroundPID)
		if foregroundHandle == nil {
			if !foregroundExited {
				return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("foreground PID %d has no retained cleanup handle", foregroundPID), true)
			}
			if _, _, err := store.ProcessIdentityForPID(foregroundPID); err == nil || !processGone(err) {
				return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("foreground PID %d could not be proved exited during verification", foregroundPID), true)
			}
		} else {
			if foregroundHandle.Identity() != firstBootID+"/"+firstStartTime {
				return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("foreground PID %d changed process identity while verifying pane %s", foregroundPID, pane.PaneID), true)
			}
			if _, err := authorization.verifyIfAlive(foregroundPID); err != nil {
				return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("could not recheck process start identity for foreground PID %d", foregroundPID), true, err.Error())
			}
		}
		finalSnapshot, err := s.snapshot(ctx)
		if err != nil {
			return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("could not confirm Herdr ownership for pane %s", pane.PaneID), true, err.Error())
		}
		finalPane, finalFound := snapshotPane(finalSnapshot, pane.PaneID)
		finalName := snapshotAgentName(finalSnapshot, pane.PaneID)
		finalTaskPane := finalFound && ownsTaskPane(finalSnapshot, project, task, taskTabs(finalSnapshot, project, task), finalPane)
		finalForeignShell := finalFound && shellOnlyPaneInSandbox(finalSnapshot, finalPane, heldSandbox.Path)
		if !finalFound || finalName != currentAgentName || finalPane.Agent != currentPane.Agent || foreignShellPane && !finalForeignShell || !foreignShellPane && !finalTaskPane {
			return axi.Failure("sandbox_process_not_owned", fmt.Sprintf("foreground process %d in Sandbox pane %s changed Herdr ownership during verification", foreground, pane.PaneID), false, "Stop or inspect the unrelated agent, preserve its work, then retry Teardown")
		}
		// Backfill a legacy Task's shell image, or record a respawned upgraded
		// shell, from the retained handle whose image was verified above.
		if shellHandle := authorization.handle(first.ShellPID); taskPane && shellHandle != nil && shellHandle.ExecutableIdentity() != "" && shellHandle.ExecutableIdentity() != task.PaneShellIdentity {
			if task.PaneShellIdentity == "" || verifyTaskPaneShellImageIdentity(task.Seq, first.ShellPID, task.PaneShellIdentity) == nil {
				if err := db.UpdateTaskPaneShellIdentity(ctx, task.ID, shellHandle.ExecutableIdentity()); err != nil {
					return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("could not record the verified shell image for Task pane %s", pane.PaneID), true, err.Error())
				}
			}
		} else if taskPane && task.PaneShellIdentity == "" && foreground != first.ShellPID {
			return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("Task pane %s shell PID %d has no verified executable image", pane.PaneID, first.ShellPID), true)
		}
		for _, pid := range processPIDs {
			handle := authorization.handle(pid)
			if handle == nil {
				continue
			}
			groupID, groupErr := sandboxProcessGroupID(pid)
			if groupErr != nil {
				if processGone(groupErr) {
					continue
				}
				return axi.Failure("sandbox_process_identity_unavailable", fmt.Sprintf("could not verify process group for Sandbox process %d", pid), true, groupErr.Error())
			}
			authorization.retainProcessGroup(groupID, handle)
		}
		if (taskPane || processInSandbox(first.ShellPID, heldSandbox.Path)) && authorization.handle(first.ShellPID) != nil {
			expectedPane, shellPID, taskOwnedPane := pane, first.ShellPID, taskPane
			authorization.setSignalGuard(shellPID, func(signal syscall.Signal) error {
				return s.verifySandboxShellSignalOwnership(ctx, project, task, heldSandbox.Path, expectedPane, shellPID, taskOwnedPane, foreignShellPane, authorization)
			})
		}
		if taskPane && foreground != first.ShellPID && expectedAgentName != "" && authorization.handle(foregroundPID) != nil {
			expectedPane, expectedPID, agentName := pane, foregroundPID, expectedAgentName
			authorization.setSignalGuard(expectedPID, func(syscall.Signal) error {
				return s.verifySandboxForegroundSignalOwnership(ctx, project, task, heldSandbox.Path, expectedPane, expectedPID, agentName, authorization)
			})
		}
	}
	return nil
}

func (s *Service) verifySandboxShellSignalOwnership(ctx context.Context, project store.Project, task store.Task, sandboxPath string, expectedPane herdr.Pane, shellPID int, taskOwnedPane, foreignShellPane bool, authorization *sandboxProcessAuthorization) error {
	paneID := expectedPane.PaneID
	alive, err := authorization.verifyIfAlive(shellPID)
	if err != nil {
		return err
	}
	if !alive {
		return nil
	}
	info, err := s.sandboxPaneProcessInfo(ctx, paneID)
	if err != nil {
		return fmt.Errorf("could not recheck pane %s before signaling its retained shell: %w", paneID, err)
	}
	if info.ShellPID != shellPID {
		return fmt.Errorf("pane %s no longer contains retained shell process %d", paneID, shellPID)
	}
	if err := verifyTaskPaneShellImageAuthorized(task, shellPID, authorization, taskOwnedPane); err != nil {
		return fmt.Errorf("pane %s shell PID %d no longer matches its verified image: %w", paneID, shellPID, err)
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return fmt.Errorf("could not recheck pane %s ownership before signaling its retained shell: %w", paneID, err)
	}
	pane, found := snapshotPane(snapshot, paneID)
	if !found {
		if !taskOwnedPane || foreignShellPane {
			return fmt.Errorf("pane %s no longer matches its proved identity or Sandbox; preserving its processes", paneID)
		}
		return verifyClosedTaskPaneShellOwnership(task, sandboxPath, paneID, shellPID, authorization)
	}
	if pane.WorkspaceID != expectedPane.WorkspaceID || pane.TabID != expectedPane.TabID || pane.Label != expectedPane.Label || !taskOwnedPane && !pathInside(pane.CWD, sandboxPath) {
		return fmt.Errorf("pane %s no longer matches its proved identity or Sandbox; preserving its processes", paneID)
	}
	if foreignShellPane {
		if !shellOnlyPaneInSandbox(snapshot, pane, sandboxPath) || info.ForegroundProcessGroup != shellPID {
			return fmt.Errorf("shell-only pane %s gained a foreground process or changed ownership; preserving it", paneID)
		}
		return nil
	}
	if !taskOwnedPane || !ownsTaskPane(snapshot, project, task, taskTabs(snapshot, project, task), pane) {
		return fmt.Errorf("pane %s was not proved to belong to Task %s; preserving its processes", paneID, taskIDString(task.Seq))
	}
	foreground := info.ForegroundProcessGroup
	if foreground <= 1 {
		return fmt.Errorf("pane %s has no verified foreground process; preserving it", paneID)
	}
	agentName := snapshotAgentName(snapshot, paneID)
	if pane.Agent == "" && agentName == "" {
		if foreground != shellPID {
			return fmt.Errorf("shell-only pane %s gained a foreground process; preserving it", paneID)
		}
		return nil
	}
	if !taskAgentMatchesPane(snapshot, project, task, pane) {
		return fmt.Errorf("pane %s no longer has the Task's verified agent identity; preserving it", paneID)
	}
	if foreground == shellPID {
		return nil // The exact retained Task pane shell is the only signalable process Herdr exposed.
	}
	expectedName := pane.Agent
	if expectedName == "" {
		expectedName = agentName
	}
	foregroundName, foregroundCWD, found := foregroundProcess(info, foreground)
	foregroundPID := foreground
	if !found || !strings.EqualFold(foregroundName, expectedName) || !taskOwnedPane && !pathInside(foregroundCWD, sandboxPath) {
		return fmt.Errorf("pane %s no longer exposes its Task-owned foreground agent", paneID)
	}
	if authorization.handle(foregroundPID) == nil {
		return fmt.Errorf("pane %s foreground process %d was not retained during ownership proof", paneID, foregroundPID)
	}
	foregroundAlive, err := authorization.verifyIfAlive(foregroundPID)
	if err != nil {
		return err
	}
	if !foregroundAlive {
		// A competing Teardown may stop the agent between this pane snapshot and
		// the pidfd check. Re-read the pane before authorizing its retained shell.
		return s.verifyTaskPaneShellAfterForegroundExit(ctx, project, task, sandboxPath, expectedPane, shellPID, foregroundPID, authorization)
	}
	return nil
}

func (s *Service) verifyTaskPaneShellAfterForegroundExit(ctx context.Context, project store.Project, task store.Task, sandboxPath string, expectedPane herdr.Pane, shellPID, foregroundPID int, authorization *sandboxProcessAuthorization) error {
	info, err := s.sandboxPaneProcessInfo(ctx, expectedPane.PaneID)
	if err != nil {
		return fmt.Errorf("could not recheck pane %s after foreground process %d exited: %w", expectedPane.PaneID, foregroundPID, err)
	}
	if info.ShellPID != shellPID || info.ForegroundProcessGroup != shellPID {
		return fmt.Errorf("pane %s foreground changed after Task process %d exited; preserving its processes", expectedPane.PaneID, foregroundPID)
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return fmt.Errorf("could not confirm pane %s ownership after foreground process %d exited: %w", expectedPane.PaneID, foregroundPID, err)
	}
	pane, found := snapshotPane(snapshot, expectedPane.PaneID)
	if !found {
		return verifyClosedTaskPaneShellOwnership(task, sandboxPath, expectedPane.PaneID, shellPID, authorization)
	}
	if pane.WorkspaceID != expectedPane.WorkspaceID || pane.TabID != expectedPane.TabID || pane.Label != expectedPane.Label || !ownsTaskPane(snapshot, project, task, taskTabs(snapshot, project, task), pane) {
		return fmt.Errorf("pane %s no longer matches Task %s after foreground process %d exited; preserving its processes", expectedPane.PaneID, taskIDString(task.Seq), foregroundPID)
	}
	if pane.Agent != "" || snapshotAgentName(snapshot, expectedPane.PaneID) != "" {
		return fmt.Errorf("pane %s still exposes a foreground agent after Task process %d exited; preserving its processes", expectedPane.PaneID, foregroundPID)
	}
	shellAlive, err := authorization.verifyIfAlive(shellPID)
	if err != nil {
		return err
	}
	if !shellAlive {
		return nil
	}
	return verifyTaskPaneShellImageAuthorized(task, shellPID, authorization, true)
}

func verifyClosedTaskPaneShellOwnership(task store.Task, sandboxPath, paneID string, shellPID int, authorization *sandboxProcessAuthorization) error {
	alive, err := authorization.verifyIfAlive(shellPID)
	if err != nil {
		return err
	}
	if !alive {
		return nil
	}
	if !processInSandbox(shellPID, sandboxPath) {
		return fmt.Errorf("closed Task pane %s shell PID %d no longer references its Sandbox; preserving it", paneID, shellPID)
	}
	if err := verifyTaskPaneShellImageAuthorized(task, shellPID, authorization, true); err != nil {
		return fmt.Errorf("closed Task pane %s shell PID %d no longer matches its verified image: %w", paneID, shellPID, err)
	}
	return nil
}

func (s *Service) verifySandboxForegroundSignalOwnership(ctx context.Context, project store.Project, task store.Task, sandboxPath string, expectedPane herdr.Pane, foregroundPID int, expectedAgentName string, authorization *sandboxProcessAuthorization) error {
	alive, err := authorization.verifyIfAlive(foregroundPID)
	if err != nil {
		return err
	}
	if !alive {
		return nil
	}
	info, err := s.sandboxPaneProcessInfo(ctx, expectedPane.PaneID)
	if err != nil {
		snapshot, snapshotErr := s.snapshot(ctx)
		if snapshotErr != nil {
			return fmt.Errorf("could not recheck foreground process in pane %s before signaling: %w; could not confirm pane closure: %v", expectedPane.PaneID, err, snapshotErr)
		}
		if _, found := snapshotPane(snapshot, expectedPane.PaneID); !found {
			return nil // The retained process identity remains authoritative after Herdr closes the verified pane.
		}
		return fmt.Errorf("could not recheck foreground process in pane %s before signaling: %w", expectedPane.PaneID, err)
	}
	if info.ShellPID <= 1 || info.ForegroundProcessGroup != foregroundPID {
		return fmt.Errorf("pane %s no longer has retained process %d in its foreground", expectedPane.PaneID, foregroundPID)
	}
	name, _, found := foregroundProcess(info, foregroundPID)
	if !found || !strings.EqualFold(name, expectedAgentName) {
		return fmt.Errorf("pane %s no longer exposes Task Worker %q as its foreground", expectedPane.PaneID, expectedAgentName)
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return fmt.Errorf("could not recheck Task Worker ownership in pane %s: %w", expectedPane.PaneID, err)
	}
	pane, found := snapshotPane(snapshot, expectedPane.PaneID)
	if !found {
		return nil // The retained process identity remains authoritative after Herdr closes the verified pane.
	}
	if pane.WorkspaceID != expectedPane.WorkspaceID || pane.TabID != expectedPane.TabID || pane.Label != expectedPane.Label || pane.Agent != expectedPane.Agent || !ownsTaskPane(snapshot, project, task, taskTabs(snapshot, project, task), pane) {
		return fmt.Errorf("pane %s no longer has the verified Task Worker identity", expectedPane.PaneID)
	}
	return nil
}

func (s *Service) verifyInPlaceForegroundOwnership(ctx context.Context, project store.Project, task store.Task) (paneProcessInfo, error) {
	return s.verifyInPlaceForegroundOwnershipWithShellIdentity(ctx, project, task, task.PaneShellIdentity)
}

func (s *Service) verifyInPlaceForegroundOwnershipWithShellIdentity(ctx context.Context, project store.Project, task store.Task, shellIdentity string) (paneProcessInfo, error) {
	if s.Herdr == nil {
		return paneProcessInfo{}, axi.Failure("task_process_identity_unavailable", "Herdr is required to verify an in-place Task pane before signaling", true)
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return paneProcessInfo{}, axi.Failure("task_process_identity_unavailable", "could not verify the in-place Task pane before signaling", true, err.Error())
	}
	pane, found := findTaskPane(snapshot.Panes, task)
	if !found {
		return paneProcessInfo{}, nil // No pane remains to contain a foreign foreground; process containment remains authoritative.
	}
	if !ownsTaskPane(snapshot, project, task, taskTabs(snapshot, project, task), pane) {
		return paneProcessInfo{}, axi.Failure("task_process_identity_unavailable", "could not verify the in-place Task pane before signaling", true)
	}
	info, err := s.sandboxPaneProcessInfo(ctx, pane.PaneID)
	if err != nil {
		return paneProcessInfo{}, axi.Failure("task_process_identity_unavailable", "could not verify the in-place Task foreground before signaling", true, err.Error())
	}
	foreground := info.ForegroundProcessGroup
	if foreground <= 1 {
		return paneProcessInfo{}, axi.Failure("task_process_identity_unavailable", fmt.Sprintf("Herdr reports no identifiable foreground process in in-place Task pane %s", pane.PaneID), true)
	}
	if foreground == info.ShellPID {
		if err := verifyTaskPaneShellImageIdentity(task.Seq, foreground, shellIdentity); err != nil {
			return paneProcessInfo{}, axi.Failure("task_process_not_owned", fmt.Sprintf("in-place Task pane %s shell PID %d no longer matches its recorded shell image: %v", pane.PaneID, foreground, err), false, "Stop or inspect the foreground, preserve its work, or close the Task pane, then retry Teardown")
		}
		return info, nil
	}
	if pane.Agent == "" || !taskAgentMatchesPane(snapshot, project, task, pane) {
		return paneProcessInfo{}, axi.Failure("task_process_not_owned", fmt.Sprintf("in-place Task pane %s has a foreign foreground process; preserving it", pane.PaneID), false, "Stop or inspect the unrelated process, preserve its work, then retry Teardown")
	}
	name, cwd, processFound := foregroundProcess(info, foreground)
	if !processFound || !strings.EqualFold(name, pane.Agent) || !pathInside(cwd, task.WorktreePath) {
		return paneProcessInfo{}, axi.Failure("task_process_not_owned", fmt.Sprintf("in-place Task pane %s has a foreign foreground process that is not the Task's verified Worker; preserving it", pane.PaneID), false, "Stop or inspect the unrelated process, preserve its work, then retry Teardown")
	}
	return info, nil
}

func (s *Service) retainInPlaceTaskProcesses(ctx context.Context, db *store.DB, project store.Project, task store.Task) (paneProcessInfo, *sandboxProcessAuthorization, error) {
	info, err := s.verifyInPlaceForegroundOwnership(ctx, project, task)
	if err != nil {
		return paneProcessInfo{}, nil, err
	}
	if processIdentityCapabilityError() != "" || info.ShellPID <= 1 {
		return info, nil, nil
	}
	authorization := newSandboxProcessAuthorization()
	for _, pid := range []int{info.ShellPID, info.ForegroundProcessGroup} {
		if pid <= 1 {
			continue
		}
		bootID, startTime, identityErr := store.ProcessIdentityForPID(pid)
		if processGone(identityErr) {
			continue
		}
		if identityErr != nil {
			authorization.Close()
			return paneProcessInfo{}, nil, fmt.Errorf("retain in-place Task process %d: %w", pid, identityErr)
		}
		handle, bindErr := authorization.bind(pid)
		if bindErr != nil {
			authorization.Close()
			return paneProcessInfo{}, nil, fmt.Errorf("retain in-place Task process %d: %w", pid, bindErr)
		}
		if handle.Identity() != bootID+"/"+startTime {
			authorization.Close()
			return paneProcessInfo{}, nil, fmt.Errorf("in-place Task process %d changed identity during ownership proof", pid)
		}
	}
	if err := authorization.captureDescendants(); err != nil {
		authorization.Close()
		return paneProcessInfo{}, nil, fmt.Errorf("retain in-place Task process descendants: %w", err)
	}
	if task.PaneShellIdentity == "" && info.ForegroundProcessGroup != info.ShellPID {
		shellHandle := authorization.handle(info.ShellPID)
		if shellHandle == nil || shellHandle.ExecutableIdentity() == "" {
			authorization.Close()
			return paneProcessInfo{}, nil, fmt.Errorf("in-place Task shell PID %d has no verified executable image", info.ShellPID)
		}
		if err := db.UpdateTaskPaneShellIdentity(ctx, task.ID, shellHandle.ExecutableIdentity()); err != nil {
			authorization.Close()
			return paneProcessInfo{}, nil, fmt.Errorf("record verified in-place Task shell image: %w", err)
		}
	}
	return info, authorization, nil
}

func (s *Service) verifyInPlaceProcessSignalOwnership(ctx context.Context, project store.Project, task store.Task, pid, recordedShellPID, recordedForegroundPID int, authorization *sandboxProcessAuthorization) error {
	if authorization == nil || authorization.handle(pid) == nil {
		if pid == recordedShellPID && pid > 1 && !processHasTaskMarker(pid, task.ContainmentMarker) {
			return errPaneShellNotRecorded
		}
		return axi.Failure("task_process_identity_unavailable", fmt.Sprintf("in-place Task process %d was not retained during ownership proof", pid), true, "Preserve the process and retry Teardown after it exits")
	}
	alive, err := authorization.verifyIfAlive(pid)
	if err != nil {
		return err
	}
	if !alive {
		return nil
	}
	if pid != recordedShellPID && pid != recordedForegroundPID {
		return nil // Exact retained descendants are Task-owned; newly appearing processes are not retained.
	}
	shellIdentity := task.PaneShellIdentity
	if pid == recordedShellPID && shellIdentity == "" && authorization.handle(pid) != nil {
		shellIdentity = authorization.handle(pid).ExecutableIdentity()
	}
	info, err := s.verifyInPlaceForegroundOwnershipWithShellIdentity(ctx, project, task, shellIdentity)
	if err != nil {
		return err
	}
	if pid == recordedShellPID {
		if err := verifyTaskPaneShellImageIdentity(task.Seq, pid, shellIdentity); err != nil {
			return axi.Failure("task_process_not_owned", fmt.Sprintf("in-place Task pane shell PID %d no longer matches its verified image: %v", pid, err), false, "Stop or inspect the foreground, preserve its work, or close the Task pane, then retry Teardown")
		}
		if info.ShellPID == recordedShellPID {
			return nil
		}
	}
	if pid == recordedForegroundPID && info.ForegroundProcessGroup == recordedForegroundPID {
		return nil
	}
	return axi.Failure("task_process_not_owned", fmt.Sprintf("in-place Task pane no longer exposes process %d as its proved shell or Worker foreground", pid), false, "Stop or inspect the foreground command, preserve its work, then retry Teardown")
}

func shellOnlyPaneInSandbox(snapshot herdr.Snapshot, pane herdr.Pane, sandboxPath string) bool {
	return pane.Agent == "" && snapshotAgentName(snapshot, pane.PaneID) == "" && pathInside(pane.CWD, sandboxPath)
}

func sandboxPaneProcessIDs(info paneProcessInfo, sandboxPath string) []int {
	pids := make([]int, 0, len(info.ForegroundProcesses)+1)
	if processInSandbox(info.ShellPID, sandboxPath) {
		pids = append(pids, info.ShellPID)
	}
	for _, process := range info.ForegroundProcesses {
		if processInSandbox(process.PID, sandboxPath) {
			pids = append(pids, process.PID)
		}
	}
	sort.Ints(pids)
	unique := pids[:0]
	for _, pid := range pids {
		if len(unique) == 0 || unique[len(unique)-1] != pid {
			unique = append(unique, pid)
		}
	}
	return unique
}

func (s *Service) sandboxPaneProcessInfo(ctx context.Context, paneID string) (paneProcessInfo, error) {
	raw, err := s.herdrCall(ctx, "pane.process_info", map[string]any{"pane_id": paneID})
	if err != nil {
		return paneProcessInfo{}, err
	}
	var response struct {
		ProcessInfo paneProcessInfo `json:"process_info"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return paneProcessInfo{}, err
	}
	if response.ProcessInfo.PaneID != paneID || response.ProcessInfo.ShellPID <= 1 {
		return paneProcessInfo{}, fmt.Errorf("herdr did not return a valid process identity")
	}
	return response.ProcessInfo, nil
}

func foregroundProcess(info paneProcessInfo, foreground int) (name, cwd string, found bool) {
	for _, process := range info.ForegroundProcesses {
		if process.PID == foreground {
			return process.Name, process.CWD, true
		}
	}
	return "", "", false
}

func verifyTaskPaneShellImage(task store.Task, pid int) error {
	return verifyTaskPaneShellImageIdentity(task.Seq, pid, task.PaneShellIdentity)
}

func verifyTaskPaneShellImageAuthorized(task store.Task, pid int, authorization *sandboxProcessAuthorization, taskOwned bool) error {
	identity := ""
	if taskOwned {
		identity = task.PaneShellIdentity
	}
	if identity == "" && authorization != nil {
		if handle := authorization.handle(pid); handle != nil {
			identity = handle.ExecutableIdentity()
		}
	}
	return verifyTaskPaneShellImageIdentity(task.Seq, pid, identity)
}

// verifyTaskPaneShellImageIdentity proves the Task pane's shell PID still runs
// a shell rather than a command that replaced it. A legacy Task has no recorded
// image, and a shell package upgrade gives a respawned shell a new inode, so a
// missing or different image is accepted when it is still a login shell.
func verifyTaskPaneShellImageIdentity(taskSeq, pid int, expected string) error {
	identity, err := sandboxProcessExecutableIdentity(pid)
	if err != nil {
		return fmt.Errorf("inspect executable image for PID %d: %w", pid, err)
	}
	if expected != "" && identity == expected {
		return nil
	}
	shell, err := sandboxProcessRunsLoginShell(pid)
	if err != nil {
		return fmt.Errorf("inspect executable path for PID %d: %w", pid, err)
	}
	if shell {
		return nil
	}
	if expected == "" {
		return fmt.Errorf("task t%d has no verified pane-shell image identity and PID %d does not run a login shell", taskSeq, pid)
	}
	return fmt.Errorf("PID %d executable image differs from the verified Task pane shell", pid)
}

func uniqueTeardownProcessIDs(pids []int) []int {
	if len(pids) < 2 {
		return pids
	}
	result := pids[:1]
	for _, pid := range pids[1:] {
		if pid != result[len(result)-1] {
			result = append(result, pid)
		}
	}
	return result
}

func snapshotPane(snapshot herdr.Snapshot, paneID string) (herdr.Pane, bool) {
	for _, pane := range snapshot.Panes {
		if pane.PaneID == paneID {
			return pane, true
		}
	}
	return herdr.Pane{}, false
}

func snapshotAgentName(snapshot herdr.Snapshot, paneID string) string {
	for _, agent := range snapshot.Agents {
		if agent.PaneID == paneID {
			return agent.Name
		}
	}
	return ""
}

// stopFailedStartProcesses stops the processes of a Task whose start failed
// before any Worker received its Brief. It applies Teardown's ownership checks,
// so a foreign process in the pane or Sandbox is preserved and reported.
func (s *Service) stopFailedStartProcesses(ctx context.Context, db *store.DB, project store.Project, task store.Task) error {
	home, err := s.homePath()
	if err != nil {
		return err
	}
	held, err := taskSandboxHeldBy(ctx, db, task)
	if err != nil {
		return err
	}
	var authorization *sandboxProcessAuthorization
	signalGuard := taskProcessSignalGuard(nil)
	if held {
		authorization = newSandboxProcessAuthorization()
		defer authorization.Close()
		if err := s.verifySandboxForegroundOwnershipWithAuthorization(ctx, db, project, task, authorization); err != nil {
			return err
		}
		signalGuard = authorization.guardSignal
	}
	preserved, err := stopTaskProcessContainmentWithGuard(ctx, task, signalGuard)
	if err != nil {
		return err
	}
	excluded := taskProcessDetailsPIDSet(preserved)
	if held {
		if _, err := stopSandboxProcessesAuthorized(task.WorktreePath, authorization, excluded); err != nil {
			return err
		}
	}
	_, err = stopTaskScratchProcessesAuthorized(home, project, task, authorization, excluded)
	return err
}

// waitTaskPanesGone verifies Herdr removed the Task's panes, allowing it a
// moment to notice that the stopped shell exited.
func (s *Service) waitTaskPanesGone(ctx context.Context, project store.Project, task store.Task) (teardownPanes, error) {
	deadline := time.Now().Add(failedStartPaneRemovalWait)
	for {
		result, err := s.verifyTaskPanesGone(ctx, project, task)
		if err == nil || !time.Now().Before(deadline) || ctx.Err() != nil {
			return result, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

const failedStartPaneRemovalWait = 5 * time.Second

func (s *Service) verifyTaskPanesGone(ctx context.Context, project store.Project, task store.Task) (teardownPanes, error) {
	plan, err := s.planTaskPaneTeardown(ctx, project, task)
	if err != nil {
		return teardownPanes{}, err
	}
	return s.verifyTaskPanesClosed(ctx, project, task, plan)
}

// planTaskPaneTeardown records the panes Herdr currently associates with a
// Task. Teardown uses the plan only to verify that Herdr removes those panes
// when their retained processes exit; it never closes a pane by id alone.
func (s *Service) planTaskPaneTeardown(ctx context.Context, project store.Project, task store.Task) (taskPaneTeardownPlan, error) {
	plan := taskPaneTeardownPlan{paneIDs: map[string]bool{}, tabs: map[string]bool{}, workspaces: map[string]bool{}}
	if task.PaneLabel == "" && task.HerdrWorkspaceID == "" && task.PaneID == "" {
		return plan, nil
	}
	if s.Herdr == nil {
		return plan, axi.Failure("herdr_unavailable", "Herdr is required to verify Task pane teardown", true)
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return plan, err
	}
	plan.tabs = taskTabs(snapshot, project, task)
	childID := ""
	if !project.IsWorkspace() && !project.IsDirectory() {
		for _, workspace := range snapshot.Workspaces {
			if workspace.Worktree.CheckoutPath != "" && workspace.Worktree.CheckoutPath == task.WorktreePath {
				for _, pane := range snapshot.Panes {
					if pane.WorkspaceID == workspace.WorkspaceID && pane.Label == task.PaneLabel && ownsTaskPane(snapshot, project, task, plan.tabs, pane) {
						childID = workspace.WorkspaceID
						plan.workspaces[childID] = true
					}
				}
			}
		}
		if task.ShortName != "" {
			for _, workspace := range snapshot.Workspaces {
				if workspace.Label != task.ShortName || workspace.Worktree.CheckoutPath != task.WorktreePath || !workspace.Worktree.IsLinkedWorktree {
					continue
				}
				var panes []herdr.Pane
				for _, pane := range snapshot.Panes {
					if pane.WorkspaceID == workspace.WorkspaceID {
						panes = append(panes, pane)
					}
				}
				if len(panes) == 1 && panes[0].Label == "" && panes[0].Agent == "" && pathInside(panes[0].CWD, task.WorktreePath) {
					plan.paneIDs[panes[0].PaneID] = true
					plan.workspaces[workspace.WorkspaceID] = true
					plan.returnFocusToLead = snapshot.FocusedWorkspaceID == workspace.WorkspaceID
				}
			}
		}
	}
	ownedIDs := map[string]bool{}
	for _, pane := range snapshot.Panes {
		if ownsTaskPane(snapshot, project, task, plan.tabs, pane) {
			ownedIDs[pane.PaneID] = true
			plan.paneIDs[pane.PaneID] = true
			if pane.PaneID == snapshot.FocusedPaneID && leadPaneBeside(snapshot, project, pane) != "" {
				plan.returnFocusToLead = true
			}
		}
	}
	for paneID := range ownedIDs {
		plan.panes.Closed = append(plan.panes.Closed, paneID)
	}
	if childID != "" {
		if snapshot.FocusedWorkspaceID == childID {
			if _, found := findAppPane(snapshot.Panes, project.LeadPaneID, project.LeadLabel); found {
				plan.returnFocusToLead = true
			}
		}
		for _, pane := range snapshot.Panes {
			if pane.WorkspaceID == childID && !ownedIDs[pane.PaneID] {
				plan.panes.Foreign = append(plan.panes.Foreign, pane.PaneID)
			}
		}
	}
	for _, pane := range snapshot.Panes {
		if !plan.tabs[pane.TabID] || ownedIDs[pane.PaneID] {
			continue
		}
		if childID == "" || pane.WorkspaceID != childID {
			plan.panes.Foreign = append(plan.panes.Foreign, pane.PaneID)
		}
	}
	sort.Strings(plan.panes.Closed)
	plan.panes.Foreign = uniqueSorted(plan.panes.Foreign)
	return plan, nil
}

// verifyTaskPanesClosed accepts only panes Herdr removed after the retained
// process instances exited. Herdr 0.9.1 has no atomic close precondition, so a
// surviving Task pane is preserved and teardown fails closed.
func (s *Service) verifyTaskPanesClosed(ctx context.Context, project store.Project, task store.Task, plan taskPaneTeardownPlan) (teardownPanes, error) {
	result := teardownPanes{Foreign: append([]string(nil), plan.panes.Foreign...)}
	if len(plan.paneIDs) == 0 && task.PaneLabel == "" {
		return result, nil
	}
	if s.Herdr == nil {
		return result, axi.Failure("herdr_unavailable", "Herdr is required to verify Task pane teardown", true)
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return result, err
	}
	currentTabs := taskTabs(snapshot, project, task)
	for _, pane := range snapshot.Panes {
		if task.PaneLabel != "" && pane.Label == task.PaneLabel || ownsTaskPane(snapshot, project, task, plan.tabs, pane) {
			return result, axi.Failure("herdr_conditional_close_unavailable", fmt.Sprintf("Task pane %s remains open and Herdr cannot close it atomically against the verified process instance", pane.PaneID), false, "Inspect the pane, close it manually if it is still the Task's, then retry Teardown")
		}
		if plan.paneIDs[pane.PaneID] {
			return result, axi.Failure("herdr_conditional_close_unavailable", fmt.Sprintf("pane %s now occupies a verified Task pane identity; refusing to close it without Herdr's atomic process-instance check", pane.PaneID), false, "Inspect the pane and preserve any unrelated process or work, then retry Teardown")
		}
		if plan.tabs[pane.TabID] || plan.workspaces[pane.WorkspaceID] || currentTabs[pane.TabID] {
			result.Foreign = append(result.Foreign, pane.PaneID)
		}
	}
	for paneID := range plan.paneIDs {
		if _, found := snapshotPane(snapshot, paneID); !found {
			result.Closed = append(result.Closed, paneID)
		}
	}
	result.Closed = uniqueSorted(result.Closed)
	result.Foreign = uniqueSorted(result.Foreign)
	if plan.returnFocusToLead {
		if lead, found := findAppPane(snapshot.Panes, project.LeadPaneID, project.LeadLabel); found {
			_, _ = s.herdrCall(ctx, "pane.focus", map[string]any{"pane_id": lead.PaneID})
		}
	}
	return result, nil
}

// uniqueSorted returns sorted values without duplicates.
func uniqueSorted(values []string) []string {
	if len(values) == 0 {
		return values
	}
	sort.Strings(values)
	unique := values[:1]
	for _, value := range values[1:] {
		if value != unique[len(unique)-1] {
			unique = append(unique, value)
		}
	}
	return unique
}

// leadPaneBeside returns the Lead's pane when it shares pane's workspace.
func leadPaneBeside(snapshot herdr.Snapshot, project store.Project, pane herdr.Pane) string {
	lead, found := findAppPane(snapshot.Panes, project.LeadPaneID, project.LeadLabel)
	if !found || lead.WorkspaceID != pane.WorkspaceID {
		return ""
	}
	return lead.PaneID
}

// taskTabs returns the tabs that hold a pane carrying the Task's label and
// passing the agent check. Recorded tab, pane and workspace ids are never
// trusted alone: Herdr reuses workspace ids, and with them tab and pane ids,
// after a restart.
func taskTabs(snapshot herdr.Snapshot, project store.Project, task store.Task) map[string]bool {
	tabs := map[string]bool{}
	if task.PaneLabel == "" {
		return tabs
	}
	for _, pane := range snapshot.Panes {
		if pane.TabID != "" && pane.Label == task.PaneLabel && ownsTaskPane(snapshot, project, task, nil, pane) {
			tabs[pane.TabID] = true
		}
	}
	return tabs
}

// ownsTaskPane reports whether pane is the Task's: it carries the Task's
// label, or it is an unlabeled pane inside the Sandbox in one of the Task's
// tabs. An unlabeled pane elsewhere, even inside the Sandbox, is not the Task's.
// A pane running an agent must run one of the Task's launches.
func ownsTaskPane(snapshot herdr.Snapshot, project store.Project, task store.Task, tabs map[string]bool, pane herdr.Pane) bool {
	labelMatch := task.PaneLabel != "" && pane.Label == task.PaneLabel
	pathMatch := !labelMatch && pane.Label == "" && pane.TabID != "" && tabs[pane.TabID] && pathInside(pane.CWD, task.WorktreePath)
	if !labelMatch && !pathMatch {
		return false
	}
	if pane.Agent == "" {
		return true
	}
	return taskAgentMatchesPane(snapshot, project, task, pane)
}

// taskAgentMatchesPane accepts a canonical launch name or the exact recorded
// session for a named Task pane whose current Herdr agent has no name.
func taskAgentMatchesPane(snapshot herdr.Snapshot, project store.Project, task store.Task, pane herdr.Pane) bool {
	var nameless *herdr.Agent
	namelessCount := 0
	hasNamedAgent := false
	for i := range snapshot.Agents {
		agent := snapshot.Agents[i]
		if agent.PaneID != pane.PaneID {
			continue
		}
		if agent.Name != "" {
			if agentNameMatchesTask(agent.Name, project.Name, task.Seq) {
				return true
			}
			hasNamedAgent = true
			continue
		}
		namelessCount++
		if nameless == nil {
			nameless = &snapshot.Agents[i]
		}
	}
	if nameless == nil || namelessCount != 1 || hasNamedAgent {
		return false
	}
	return namelessAgentMatchesTaskSession(*nameless, task, pane)
}

func namelessAgentMatchesTaskSession(agent herdr.Agent, task store.Task, pane herdr.Pane) bool {
	expectedKind := sessionref.NormalizeKind(task.AgentSessionKind)
	if expectedKind == "" || task.PaneID == "" || pane.PaneID != task.PaneID || !pathInside(pane.CWD, task.WorktreePath) {
		return false
	}
	if sessionref.NormalizeKind(pane.Agent) != expectedKind {
		return false
	}
	agentKind := agent.Agent
	if agentKind == "" {
		agentKind = agent.Kind
	}
	if agentKind == "" {
		agentKind = pane.Agent
	}
	if sessionref.NormalizeKind(agentKind) != expectedKind {
		return false
	}
	expectedSession := agentSessionID(task.AgentSessionKind, task.AgentSession)
	if expectedSession == "" {
		return false
	}
	agentSession := agentSessionID(agentKind, agent.SessionID)
	paneSession := agentSessionID(agentKind, string(pane.AgentSession))
	if agentSession != "" && paneSession != "" && agentSession != paneSession {
		return false
	}
	observedSession := agentSession
	if observedSession == "" {
		observedSession = paneSession
	}
	return observedSession != "" && observedSession == expectedSession
}

// agentNameMatchesTask reports whether name is one of the Task's launches as
// agentName builds it, including its sanitizing and length limit.
func agentNameMatchesTask(name, project string, sequence int) bool {
	cut := strings.LastIndexByte(name, '-')
	if cut < 0 {
		return false
	}
	launch, err := strconv.Atoi(name[cut+1:])
	if err != nil || launch < 1 || strconv.Itoa(launch) != name[cut+1:] {
		return false
	}
	return name == agentName(project, sequence, launch)
}

func pathInside(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(resolvedRoot), filepath.Clean(resolvedPath))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func missingPaneError(err error) bool {
	var axiErr *axi.Error
	if errors.As(err, &axiErr) {
		return axiErr.Code == "pane_not_found" || axiErr.Code == "tab_not_found" || axiErr.Code == "workspace_not_found" || axiErr.Code == "not_found"
	}
	var herdrErr *herdr.Error
	if !errors.As(err, &herdrErr) {
		return false
	}
	return herdrErr.Code == "pane_not_found" || herdrErr.Code == "tab_not_found" || herdrErr.Code == "workspace_not_found" || herdrErr.Code == "not_found"
}

func (s *Service) teardownIncomplete(ctx context.Context, db *store.DB, project store.Project, task store.Task, step string, cause error) error {
	if err := s.recordTeardownIncomplete(ctx, db, project, task, cause); err != nil {
		return errors.Join(cause, err)
	}
	retryable := !errors.Is(cause, store.ErrInvalidApprovedDiscardTransition)
	help := []string{}
	var structured *axi.Error
	if errors.As(cause, &structured) {
		help = append(help, structured.Help...)
	}
	help = append(help, "Repair the failed "+step+" step, then retry `posse teardown "+taskIDString(task.Seq)+"`")
	return axi.Failure("teardown_incomplete", cause.Error(), retryable, help...)
}

func (s *Service) teardownDiscardIncomplete(ctx context.Context, db *store.DB, project store.Project, task store.Task, step string, cause error) error {
	if err := s.recordTeardownIncomplete(ctx, db, project, task, cause); err != nil {
		return errors.Join(cause, err)
	}
	return axi.Failure("teardown_incomplete", cause.Error(), true, "Repair the failed "+step+" step, then retry `posse teardown "+taskIDString(task.Seq)+" --discard --user-approved \"<User's original words>\"`")
}

func (s *Service) recordTeardownIncomplete(ctx context.Context, db *store.DB, project store.Project, task store.Task, cause error) error {
	if err := s.recordTeardownBlocked(ctx, db, project, task, cause); err != nil {
		return err
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		return err
	}
	found := false
	for _, notice := range notices {
		if notice.TaskID == task.ID && notice.Kind == "teardown_incomplete" && notice.AckedAt == 0 && strings.Contains(notice.Summary, cause.Error()) {
			found = true
			break
		}
	}
	if !found {
		summary := fmt.Sprintf("%s teardown incomplete: %s", taskDisplayName(task), cause)
		if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "teardown_incomplete", Summary: summary, DataJSON: `{}`}); err != nil {
			return err
		}
	}
	_ = s.regenerateProjects(ctx, db)
	return nil
}
