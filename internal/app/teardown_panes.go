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

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
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

type unsaddleResult struct {
	Panes            teardownPanes
	BranchRemoved    bool
	StoppedProcesses []string
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

func autoUnsaddleMode(cfg config.Config) string {
	if cfg.Defaults.AutoUnsaddle == "" {
		return "finished"
	}
	return cfg.Defaults.AutoUnsaddle
}

func shouldAutoUnsaddleLanded(cfg config.Config) bool {
	return autoUnsaddleMode(cfg) == "finished" || autoUnsaddleMode(cfg) == "landed"
}

func (s *Service) autoTeardownLandedTasks(ctx context.Context, db *store.DB, project store.Project, cfg config.Config) error {
	if s.Herdr == nil {
		return nil
	}
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.State != store.StateLanded {
			continue
		}
		mergedPR := task.LandingMode == "pr" || task.LandingMode == "no-mistakes"
		if project.IsWorkspace() {
			repos, err := db.TaskRepos(ctx, task.ID)
			if err != nil {
				return err
			}
			for _, repo := range repos {
				mergedPR = mergedPR || (repo.PRURL != "" && repo.State == store.TaskRepoLanded)
			}
		}
		if !shouldAutoUnsaddleLanded(cfg) && !mergedPR {
			continue
		}
		if _, err := s.unsaddleTask(ctx, db, project, cfg, task, false, ""); err != nil {
			var commandError *axi.Error
			if errors.As(err, &commandError) && commandError.Code == "intent_active" {
				continue
			}
			if noticeErr := s.recordUnsaddleIncomplete(ctx, db, project, task, err); noticeErr != nil {
				return noticeErr
			}
		}
	}
	return nil
}

func safeMergedPRWorktree(ctx context.Context, db *store.DB, project store.Project, task store.Task, observation store.PRObservation) (bool, error) {
	if observation.PRURL != task.PRURL || observation.HeadSHA == "" || observation.MergeCommit == "" || task.LandedRef != observation.MergeCommit || task.Branch == "" || task.WorktreePath == "" {
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
		// A merge of origin/main after the PR was merged is safe only if
		// it contains both the PR head and the merge commit and carries no
		// additional tree changes. Never discard a subsequent follow-up.
		for _, ancestor := range []string{observation.HeadSHA, observation.MergeCommit} {
			if _, err := gitOutput(ctx, task.WorktreePath, "merge-base", "--is-ancestor", ancestor, branchSHA); err != nil {
				return false, nil
			}
		}
		// Compare against the newest default-branch ancestor of the Task
		// tip. Other changes that landed on main after this PR are safe;
		// changes made only on the Task branch are not.
		base, err := gitOutput(ctx, task.WorktreePath, "merge-base", branchSHA, "refs/heads/"+project.DefaultBranch)
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
	status, err := gitOutput(ctx, task.WorktreePath, "status", "--porcelain", "--untracked-files=all")
	if err != nil || status != "" {
		return false, nil
	}
	return true, nil
}

// verifyMountForegroundOwnershipWithAuthorization retains exact process
// handles for Task panes and shell-only panes in the held Mount. CWD discovery
// may detect a process but cannot authorize cleanup. Herdr 0.9.1 has no atomic
// pane-close precondition, so teardown never closes panes by target ID alone.
func (s *Service) verifyMountForegroundOwnershipWithAuthorization(ctx context.Context, db *store.DB, project store.Project, task store.Task, authorization *mountProcessAuthorization) error {
	mounts, err := db.Mounts(ctx, project.ID)
	if err != nil {
		return axi.Failure("mount_process_identity_unavailable", "could not verify Mount ownership before releasing it", true, err.Error())
	}
	var heldMount store.Mount
	for _, mount := range mounts {
		if mount.ID == task.MountID && mount.TaskID == task.ID && mount.State == "held" {
			heldMount = mount
			break
		}
	}
	if heldMount.ID == 0 {
		return nil
	}
	if heldMount.Path == "" || task.WorktreePath == "" || !pathInside(task.WorktreePath, heldMount.Path) {
		return axi.Failure("mount_process_identity_unavailable", "could not match the Task worktree to its held Mount before releasing it", true)
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return axi.Failure("mount_process_identity_unavailable", "could not verify foreground process ownership before releasing the Mount", true, err.Error())
	}
	tabs := taskTabs(snapshot, project, task)
	for _, pane := range snapshot.Panes {
		paneInsideMount := pathInside(pane.CWD, heldMount.Path)
		agentName := snapshotAgentName(snapshot, pane.PaneID)
		taskPane := ownsTaskPane(snapshot, project, task, tabs, pane)
		foreignShellPane := paneInsideMount && !taskPane && shellOnlyPaneInMount(snapshot, pane, heldMount.Path)
		if paneInsideMount && !taskPane && !foreignShellPane {
			return axi.Failure("mount_process_not_owned", fmt.Sprintf("pane %s in the held Mount is not owned by Task %s", pane.PaneID, taskIDString(task.Seq)), false, "Stop or inspect the unrelated pane, preserve its work, then retry Teardown")
		}
		if !paneInsideMount && agentName == "" && pane.Agent == "" {
			continue
		}
		first, err := s.mountPaneProcessInfo(ctx, pane.PaneID)
		if err != nil {
			return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("could not verify processes in Mount pane %s", pane.PaneID), true, err.Error())
		}
		foreground := first.ForegroundProcessGroup
		foregroundPID := foreground
		foregroundName, foregroundCWD, foregroundProcessFound := foregroundProcess(first, foregroundPID)
		expectedAgentName := pane.Agent
		if expectedAgentName == "" {
			expectedAgentName = agentName
		}
		if !paneInsideMount && foregroundCWD == "" && (agentName != "" || pane.Agent != "") {
			return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("Herdr did not identify the working directory of the foreground process in pane %s", pane.PaneID), true)
		}
		if !paneInsideMount && !pathInside(foregroundCWD, heldMount.Path) {
			continue
		}
		if foreground <= 1 {
			if pane.Agent != "" || agentName != "" || paneInsideMount {
				return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("Herdr reports no identifiable foreground process in Mount pane %s", pane.PaneID), true)
			}
			continue
		}
		if !foregroundProcessFound {
			return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("Herdr did not identify foreground process group %d in pane %s", foreground, pane.PaneID), true)
		}
		if foreignShellPane && foreground != first.ShellPID {
			return axi.Failure("mount_process_not_owned", fmt.Sprintf("pane %s in the held Mount has a foreground process that is not its shell", pane.PaneID), false, "Stop or inspect the unrelated pane, preserve its work, then retry Teardown")
		}
		if foreground != first.ShellPID && expectedAgentName != "" && (foregroundName == "" || !strings.EqualFold(foregroundName, expectedAgentName)) {
			return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("foreground process %d in Mount pane %s does not match Herdr's agent identity %q", foregroundPID, pane.PaneID, expectedAgentName), true)
		}

		firstBootID, firstStartTime, foregroundIdentityErr := store.ProcessIdentityForPID(foregroundPID)
		foregroundExited := processGone(foregroundIdentityErr)
		if foregroundIdentityErr != nil && !foregroundExited {
			return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("could not bind foreground PID %d to a process start identity", foregroundPID), true, foregroundIdentityErr.Error())
		}
		processPIDs := mountPaneProcessIDs(first, heldMount.Path)
		for _, pid := range processPIDs {
			bootID, startTime, identityErr := store.ProcessIdentityForPID(pid)
			if identityErr != nil {
				if processGone(identityErr) {
					if pid == foregroundPID {
						foregroundExited = true
					}
					continue
				}
				return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("could not bind Mount process %d to a process start identity", pid), true, identityErr.Error())
			}
			handle, bindErr := authorization.bind(pid)
			if bindErr != nil {
				if processGone(bindErr) {
					if pid == foregroundPID {
						foregroundExited = true
					}
					continue
				}
				return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("could not retain the exact Mount process %d for cleanup: %v", pid, bindErr), true, bindErr.Error())
			}
			if handle.Identity() != bootID+"/"+startTime || pid == foregroundPID && handle.Identity() != firstBootID+"/"+firstStartTime {
				return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("Mount PID %d changed process identity while retaining its cleanup handle", pid), true)
			}
		}
		currentSnapshot, err := s.snapshot(ctx)
		if err != nil {
			return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("could not recheck Herdr ownership for pane %s", pane.PaneID), true, err.Error())
		}
		currentPane, currentFound := snapshotPane(currentSnapshot, pane.PaneID)
		currentAgentName := snapshotAgentName(currentSnapshot, pane.PaneID)
		currentTaskPane := currentFound && ownsTaskPane(currentSnapshot, project, task, taskTabs(currentSnapshot, project, task), currentPane)
		currentForeignShell := currentFound && shellOnlyPaneInMount(currentSnapshot, currentPane, heldMount.Path)
		currentAgentMatches := agentNameMatchesTask(currentAgentName, project.Name, task.Seq)
		if currentFound && currentAgentName == "" && currentPane.Agent == "" && pathInside(currentPane.CWD, heldMount.Path) {
			currentAgentMatches = true // The Task pane's shell or command is authorized by its exact pane identity.
		}
		if !currentFound || foreignShellPane && !currentForeignShell || !foreignShellPane && (!currentTaskPane || !currentAgentMatches) {
			return axi.Failure("mount_process_not_owned", fmt.Sprintf("foreground process %d in Mount pane %s no longer belongs to Task %s", foreground, pane.PaneID, taskIDString(task.Seq)), false, "Stop or inspect the unrelated agent, preserve its work, then retry Teardown")
		}
		second, err := s.mountPaneProcessInfo(ctx, pane.PaneID)
		if err != nil {
			return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("could not recheck foreground process identity in pane %s", pane.PaneID), true, err.Error())
		}
		if second.ShellPID != first.ShellPID {
			return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("shell process identity changed while verifying pane %s", pane.PaneID), true)
		}
		if second.ForegroundProcessGroup != foreground && second.ForegroundProcessGroup > 1 {
			if authorization.handle(second.ForegroundProcessGroup) != nil {
				if _, err := authorization.verifyIfAlive(second.ForegroundProcessGroup); err != nil {
					return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("could not verify changed foreground process in pane %s", pane.PaneID), true, err.Error())
				}
			} else {
				alive, err := unretainedMountProcessAlive(second.ForegroundProcessGroup)
				if err != nil {
					return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("could not verify changed foreground process in pane %s", pane.PaneID), true, err.Error())
				}
				if alive {
					return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("unretained foreground process %d appeared while verifying pane %s", second.ForegroundProcessGroup, pane.PaneID), true)
				}
			}
		}
		// Herdr's foreground list can gain or lose short-lived children between
		// reads. Only the first read authorizes cleanup; verify those retained
		// instances here and let the later Mount scan refuse any new live PID.
		for _, pid := range processPIDs {
			if _, err := authorization.verifyIfAlive(pid); err != nil {
				return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("could not recheck Mount process %d identity", pid), true, err.Error())
			}
		}
		foregroundHandle := authorization.handle(foregroundPID)
		if foregroundHandle == nil {
			if !foregroundExited {
				return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("foreground PID %d has no retained cleanup handle", foregroundPID), true)
			}
			if _, _, err := store.ProcessIdentityForPID(foregroundPID); err == nil || !processGone(err) {
				return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("foreground PID %d could not be proved exited during verification", foregroundPID), true)
			}
		} else {
			if foregroundHandle.Identity() != firstBootID+"/"+firstStartTime {
				return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("foreground PID %d changed process identity while verifying pane %s", foregroundPID, pane.PaneID), true)
			}
			if _, err := authorization.verifyIfAlive(foregroundPID); err != nil {
				return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("could not recheck process start identity for foreground PID %d", foregroundPID), true, err.Error())
			}
		}
		finalSnapshot, err := s.snapshot(ctx)
		if err != nil {
			return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("could not confirm Herdr ownership for pane %s", pane.PaneID), true, err.Error())
		}
		finalPane, finalFound := snapshotPane(finalSnapshot, pane.PaneID)
		finalName := snapshotAgentName(finalSnapshot, pane.PaneID)
		finalTaskPane := finalFound && ownsTaskPane(finalSnapshot, project, task, taskTabs(finalSnapshot, project, task), finalPane)
		finalForeignShell := finalFound && shellOnlyPaneInMount(finalSnapshot, finalPane, heldMount.Path)
		if !finalFound || finalName != currentAgentName || finalPane.Agent != currentPane.Agent || foreignShellPane && !finalForeignShell || !foreignShellPane && !finalTaskPane {
			return axi.Failure("mount_process_not_owned", fmt.Sprintf("foreground process %d in Mount pane %s changed Herdr ownership during verification", foreground, pane.PaneID), false, "Stop or inspect the unrelated agent, preserve its work, then retry Teardown")
		}
		for _, pid := range processPIDs {
			handle := authorization.handle(pid)
			if handle == nil {
				continue
			}
			groupID, groupErr := mountProcessGroupID(pid)
			if groupErr != nil {
				if processGone(groupErr) {
					continue
				}
				return axi.Failure("mount_process_identity_unavailable", fmt.Sprintf("could not verify process group for Mount process %d", pid), true, groupErr.Error())
			}
			authorization.retainProcessGroup(groupID, handle)
		}
		if processInMount(first.ShellPID, heldMount.Path) && authorization.handle(first.ShellPID) != nil {
			expectedPane, shellPID, taskOwnedPane := pane, first.ShellPID, taskPane
			authorization.setSignalGuard(shellPID, func(signal syscall.Signal) error {
				if signal != syscall.SIGKILL {
					return nil
				}
				return s.verifyMountShellSignalOwnership(ctx, project, task, heldMount.Path, expectedPane, shellPID, taskOwnedPane, foreignShellPane, authorization)
			})
		}
	}
	return nil
}

func (s *Service) verifyMountShellSignalOwnership(ctx context.Context, project store.Project, task store.Task, mountPath string, expectedPane herdr.Pane, shellPID int, taskOwnedPane, foreignShellPane bool, authorization *mountProcessAuthorization) error {
	paneID := expectedPane.PaneID
	alive, err := authorization.verifyIfAlive(shellPID)
	if err != nil {
		return err
	}
	if !alive {
		return nil
	}
	info, err := s.mountPaneProcessInfo(ctx, paneID)
	if err != nil {
		return fmt.Errorf("could not recheck pane %s before signaling its retained shell: %w", paneID, err)
	}
	if info.ShellPID != shellPID {
		return fmt.Errorf("pane %s no longer contains retained shell process %d", paneID, shellPID)
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return fmt.Errorf("could not recheck pane %s ownership before signaling its retained shell: %w", paneID, err)
	}
	pane, found := snapshotPane(snapshot, paneID)
	if !found || pane.WorkspaceID != expectedPane.WorkspaceID || pane.TabID != expectedPane.TabID || pane.Label != expectedPane.Label || !pathInside(pane.CWD, mountPath) {
		return fmt.Errorf("pane %s no longer matches its proved identity or Mount; preserving its processes", paneID)
	}
	if foreignShellPane {
		if !shellOnlyPaneInMount(snapshot, pane, mountPath) || info.ForegroundProcessGroup != shellPID {
			return fmt.Errorf("shell-only pane %s gained a foreground process or changed ownership; preserving it", paneID)
		}
		return nil
	}
	if !taskOwnedPane {
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
	if !agentNameMatchesTask(agentName, project.Name, task.Seq) {
		return fmt.Errorf("pane %s now has unrelated agent %q; preserving it", paneID, agentName)
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
	if !found || !strings.EqualFold(foregroundName, expectedName) || !pathInside(foregroundCWD, mountPath) {
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
		return fmt.Errorf("pane %s foreground process %d exited during ownership recheck", paneID, foregroundPID)
	}
	return nil
}

func shellOnlyPaneInMount(snapshot herdr.Snapshot, pane herdr.Pane, mountPath string) bool {
	return pane.Agent == "" && snapshotAgentName(snapshot, pane.PaneID) == "" && pathInside(pane.CWD, mountPath)
}

func mountPaneProcessIDs(info paneProcessInfo, mountPath string) []int {
	pids := make([]int, 0, len(info.ForegroundProcesses)+1)
	if processInMount(info.ShellPID, mountPath) {
		pids = append(pids, info.ShellPID)
	}
	for _, process := range info.ForegroundProcesses {
		if processInMount(process.PID, mountPath) {
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

func (s *Service) mountPaneProcessInfo(ctx context.Context, paneID string) (paneProcessInfo, error) {
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
	if !project.IsWorkspace() {
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
// label, or it is an unlabeled pane inside the Mount in one of the Task's
// tabs. An unlabeled pane elsewhere, even inside the Mount, is not the Task's.
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
	for _, agent := range snapshot.Agents {
		if agent.PaneID != pane.PaneID {
			continue
		}
		if agentNameMatchesTask(agent.Name, project.Name, task.Seq) {
			return true
		}
	}
	return false
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

func (s *Service) unsaddleIncomplete(ctx context.Context, db *store.DB, project store.Project, task store.Task, step string, cause error) error {
	if err := s.recordUnsaddleIncomplete(ctx, db, project, task, cause); err != nil {
		return errors.Join(cause, err)
	}
	help := []string{}
	var structured *axi.Error
	if errors.As(cause, &structured) {
		help = append(help, structured.Help...)
	}
	help = append(help, "Repair the failed "+step+" step, then retry `posse unsaddle "+taskIDString(task.Seq)+"`")
	return axi.Failure("unsaddle_incomplete", cause.Error(), true, help...)
}

func (s *Service) recordUnsaddleIncomplete(ctx context.Context, db *store.DB, project store.Project, task store.Task, cause error) error {
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		return err
	}
	found := false
	for _, notice := range notices {
		if notice.TaskID == task.ID && notice.Kind == "unsaddle_incomplete" && notice.AckedAt == 0 && strings.Contains(notice.Summary, cause.Error()) {
			found = true
			break
		}
	}
	if !found {
		summary := fmt.Sprintf("%s teardown incomplete: %s", taskDisplayName(task), cause)
		if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "unsaddle_incomplete", Summary: summary, DataJSON: `{}`}); err != nil {
			return err
		}
	}
	_ = s.regenerateProjects(ctx, db)
	return nil
}
