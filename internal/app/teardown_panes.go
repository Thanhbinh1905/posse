package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type teardownPanes struct {
	Closed  []string
	Foreign []string
}

type unsaddleResult struct {
	Panes            teardownPanes
	BranchRemoved    bool
	StoppedProcesses []string
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
	if !shouldAutoUnsaddleLanded(cfg) || s.Herdr == nil {
		return nil
	}
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return err
	}
	notices, err := db.Notices(ctx, project.ID, true)
	if err != nil {
		return err
	}
	unackedFailures := make(map[int64]bool)
	for _, notice := range notices {
		if notice.Kind == "unsaddle_incomplete" && notice.TaskID != 0 {
			unackedFailures[notice.TaskID] = true
		}
	}
	for _, task := range tasks {
		if task.State != store.StateLanded {
			continue
		}
		if unackedFailures[task.ID] {
			continue
		}
		safe, err := safePRMergeTeardown(ctx, db, project, task)
		if err != nil {
			return err
		}
		if !safe {
			if err := restoreMergedTaskWithWork(ctx, db, project, task); err != nil {
				return err
			}
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
			unackedFailures[task.ID] = true
		}
	}
	return nil
}

func restoreMergedTaskWithWork(ctx context.Context, db *store.DB, project store.Project, task store.Task) error {
	return db.RestoreMergedTaskWithWork(ctx, project.ID, task)
}

func safePRMergeTeardown(ctx context.Context, db *store.DB, project store.Project, task store.Task) (bool, error) {
	if task.LandingMode != "pr" || task.PRURL == "" {
		return true, nil
	}
	observation, err := db.LatestPRObservation(ctx, task.ID)
	if store.IsNotFound(err) {
		return false, nil // No verified merge observation can justify resetting the Mount.
	}
	if err != nil {
		return false, err
	}
	if observation.State != "MERGED" {
		return false, nil
	}
	return safeMergedPRWorktree(ctx, db, project, task, observation)
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

// closeTaskPanes closes a Task's panes as found in a fresh snapshot. A tab
// whose panes are all the Task's closes with tab.close; in any other tab only
// the Task's own panes close, and the other panes of its tabs are reported as
// foreign. posse never closes a workspace: the Lead and sibling Riders share
// the Lead's, and a legacy Rider workspace disappears with its last tab.
func (s *Service) closeTaskPanes(ctx context.Context, project store.Project, task store.Task) (teardownPanes, error) {
	result := teardownPanes{}
	if task.PaneLabel == "" && task.HerdrWorkspaceID == "" && task.PaneID == "" {
		return result, nil
	}
	if s.Herdr == nil {
		return result, axi.Failure("herdr_unavailable", "Herdr is required to verify Task pane teardown", true)
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return result, err
	}
	tabs := taskTabs(snapshot, project, task)
	owned := make([]herdr.Pane, 0)
	ownedIDs := map[string]bool{}
	tabPanes := map[string][]herdr.Pane{}
	for _, pane := range snapshot.Panes {
		if ownsTaskPane(snapshot, project, task, tabs, pane) {
			owned = append(owned, pane)
			ownedIDs[pane.PaneID] = true
		}
		if tabs[pane.TabID] {
			tabPanes[pane.TabID] = append(tabPanes[pane.TabID], pane)
		}
	}
	leadPaneID := ""
	for _, pane := range owned {
		if pane.PaneID == snapshot.FocusedPaneID {
			leadPaneID = leadPaneBeside(snapshot, project, pane)
		}
	}
	closedTabs := map[string]bool{}
	tabIDs := make([]string, 0, len(tabPanes))
	for tabID := range tabPanes {
		tabIDs = append(tabIDs, tabID)
	}
	sort.Strings(tabIDs)
	for _, tabID := range tabIDs {
		whole := true
		for _, pane := range tabPanes[tabID] {
			if !ownedIDs[pane.PaneID] {
				whole = false
				result.Foreign = append(result.Foreign, pane.PaneID)
			}
		}
		if !whole {
			continue
		}
		if _, err := s.herdrCall(ctx, "tab.close", map[string]any{"tab_id": tabID}); err != nil && !missingPaneError(err) {
			return result, fmt.Errorf("close Task tab %s: %w", tabID, err)
		}
		closedTabs[tabID] = true
	}
	sort.Strings(result.Foreign)
	for _, pane := range owned {
		if !closedTabs[pane.TabID] {
			if _, err := s.herdrCall(ctx, "pane.close", map[string]any{"pane_id": pane.PaneID}); err != nil && !missingPaneError(err) {
				return result, fmt.Errorf("close Task pane %s: %w", pane.PaneID, err)
			}
		}
		result.Closed = append(result.Closed, pane.PaneID)
	}
	// The User was watching a Rider pane that just closed. Return focus to the
	// Lead instead of leaving Herdr to pick a sibling Rider's tab. Teardown has
	// already succeeded, so a focus failure is ignored.
	if leadPaneID != "" {
		_, _ = s.herdrCall(ctx, "pane.focus", map[string]any{"pane_id": leadPaneID})
	}

	verified, err := s.snapshot(ctx)
	if err != nil {
		return result, fmt.Errorf("verify Task pane closure: %w", err)
	}
	for _, pane := range verified.Panes {
		if ownedIDs[pane.PaneID] {
			return result, fmt.Errorf("task pane %s remains open", pane.PaneID)
		}
		if task.PaneLabel != "" && pane.Label == task.PaneLabel {
			return result, fmt.Errorf("task label %s remains on pane %s", task.PaneLabel, pane.PaneID)
		}
	}
	return result, nil
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

func (s *Service) unsaddleIncomplete(ctx context.Context, db *store.DB, project store.Project, task store.Task, cause error) error {
	if err := s.recordUnsaddleIncomplete(ctx, db, project, task, cause); err != nil {
		return errors.Join(cause, err)
	}
	current, err := db.TaskByID(ctx, project.ID, task.ID)
	if err != nil {
		return err
	}
	if task.State == store.StateLanded && current.State == store.StateWorking {
		return axi.Failure("unsaddle_incomplete", cause.Error(), true, "Follow-up work was preserved. Run `posse relaunch "+taskIDString(task.Seq)+"` if the Rider pane was closed, then publish the work")
	}
	return axi.Failure("unsaddle_incomplete", cause.Error(), true, "Resolve the remaining pane or Mount process, then retry `posse unsaddle "+taskIDString(task.Seq)+"`")
}

func (s *Service) recordUnsaddleIncomplete(ctx context.Context, db *store.DB, project store.Project, task store.Task, cause error) error {
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		return err
	}
	found := false
	for _, notice := range notices {
		if notice.TaskID == task.ID && notice.Kind == "unsaddle_incomplete" && notice.AckedAt == 0 {
			found = true
			break
		}
	}
	if !found {
		summary := fmt.Sprintf("%s teardown incomplete: %s", taskDisplayName(task), cause)
		if current, err := db.TaskByID(ctx, project.ID, task.ID); err == nil && task.State == store.StateLanded && current.State == store.StateWorking {
			summary += "; run `posse relaunch " + taskIDString(task.Seq) + "` if the Rider pane closed"
		}
		if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "unsaddle_incomplete", Summary: summary, DataJSON: `{}`}); err != nil {
			return err
		}
	}
	_ = s.regenerateProjects(ctx, db)
	return nil
}
