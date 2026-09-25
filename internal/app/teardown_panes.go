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

func safePRMergeTeardown(ctx context.Context, db *store.DB, project store.Project, task store.Task) (bool, error) {
	if task.LandingMode != "pr" || task.PRURL == "" {
		return true, nil
	}
	observation, err := db.LatestPRObservation(ctx, task.ID)
	if store.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if observation.State != "MERGED" {
		return true, nil
	}
	if observation.PRURL != task.PRURL || observation.HeadSHA == "" || observation.MergeCommit == "" || task.LandedRef != observation.MergeCommit || task.Branch == "" || task.WorktreePath == "" {
		return false, nil
	}
	branchSHA, err := gitOutput(ctx, project.Root, "rev-parse", "refs/heads/"+task.Branch)
	if err != nil || branchSHA != observation.HeadSHA {
		return false, nil
	}
	worktreeBranch, err := gitOutput(ctx, task.WorktreePath, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || worktreeBranch != task.Branch {
		return false, nil
	}
	worktreeSHA, err := gitOutput(ctx, task.WorktreePath, "rev-parse", "HEAD")
	if err != nil || worktreeSHA != observation.HeadSHA {
		return false, nil
	}
	status, err := gitOutput(ctx, task.WorktreePath, "status", "--porcelain", "--untracked-files=all", "--ignored")
	if err != nil || status != "" {
		return false, nil
	}
	return true, nil
}

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
	owned := make([]herdr.Pane, 0)
	ownedIDs := map[string]bool{}
	workspacePanes := make([]herdr.Pane, 0)
	for _, pane := range snapshot.Panes {
		if pane.WorkspaceID == task.HerdrWorkspaceID && task.HerdrWorkspaceID != "" {
			workspacePanes = append(workspacePanes, pane)
		}
		if ownsTaskPane(snapshot, project, task, pane) {
			owned = append(owned, pane)
			ownedIDs[pane.PaneID] = true
		}
	}
	for _, pane := range workspacePanes {
		if !ownedIDs[pane.PaneID] {
			result.Foreign = append(result.Foreign, pane.PaneID)
		}
	}
	sort.Strings(result.Foreign)

	workspaceOnlyTask := task.HerdrWorkspaceID != "" && len(workspacePanes) > 0 && len(workspacePanes) == len(owned)
	for _, pane := range owned {
		if pane.WorkspaceID != task.HerdrWorkspaceID {
			workspaceOnlyTask = false
			break
		}
	}
	if workspaceOnlyTask {
		if _, err := s.herdrCall(ctx, "workspace.close", map[string]any{"workspace_id": task.HerdrWorkspaceID}); err != nil && !missingPaneError(err) {
			return result, fmt.Errorf("close Task workspace %s: %w", task.HerdrWorkspaceID, err)
		}
		for _, pane := range owned {
			result.Closed = append(result.Closed, pane.PaneID)
		}
	} else {
		for _, pane := range owned {
			if _, err := s.herdrCall(ctx, "pane.close", map[string]any{"pane_id": pane.PaneID}); err != nil && !missingPaneError(err) {
				return result, fmt.Errorf("close Task pane %s: %w", pane.PaneID, err)
			}
			result.Closed = append(result.Closed, pane.PaneID)
		}
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

func ownsTaskPane(snapshot herdr.Snapshot, project store.Project, task store.Task, pane herdr.Pane) bool {
	labelMatch := task.PaneLabel != "" && pane.Label == task.PaneLabel
	pathMatch := false
	if !labelMatch && task.HerdrWorkspaceID != "" && pane.WorkspaceID == task.HerdrWorkspaceID && task.WorktreePath != "" {
		pathMatch = pathInside(pane.CWD, task.WorktreePath)
	}
	if !labelMatch && !pathMatch {
		return false
	}
	if pane.Label != "" && pane.Label != task.PaneLabel {
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

func agentNameMatchesTask(name, project string, sequence int) bool {
	base := "posse-" + strings.ToLower(project) + "-t" + strconv.Itoa(sequence) + "-"
	if !strings.HasPrefix(name, base) {
		return false
	}
	launch := strings.TrimPrefix(name, base)
	if launch == "" {
		return false
	}
	for _, char := range launch {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
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
		return axiErr.Code == "pane_not_found" || axiErr.Code == "workspace_not_found" || axiErr.Code == "not_found"
	}
	var herdrErr *herdr.Error
	if !errors.As(err, &herdrErr) {
		return false
	}
	return herdrErr.Code == "pane_not_found" || herdrErr.Code == "workspace_not_found" || herdrErr.Code == "not_found"
}

func (s *Service) unsaddleIncomplete(ctx context.Context, db *store.DB, project store.Project, task store.Task, cause error) error {
	if err := s.recordUnsaddleIncomplete(ctx, db, project, task, cause); err != nil {
		return errors.Join(cause, err)
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
		if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "unsaddle_incomplete", Summary: fmt.Sprintf("%s teardown incomplete: %s", taskDisplayName(task), cause), DataJSON: `{}`}); err != nil {
			return err
		}
	}
	_ = s.regenerateProjects(ctx, db)
	return nil
}
