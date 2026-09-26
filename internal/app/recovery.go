package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) context(ctx *axi.Context, args []string) error {
	if len(args) != 0 {
		return nil
	}
	paneID := os.Getenv("HERDR_PANE_ID")
	if paneID == "" {
		return nil
	}
	home, err := s.homePath()
	if err != nil {
		return nil
	}
	db, err := store.OpenReadOnly(home)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return nil
	}
	defer db.Close()
	if task, err := db.TaskByPane(ctx.Context, paneID); err == nil {
		project, err := db.ProjectByID(ctx.Context, task.ProjectID)
		if err != nil {
			return nil
		}
		_ = ctx.PrintWithoutHelp(axi.Object{
			{Key: "role", Value: "worker"},
			{Key: "project", Value: project.Name},
			{Key: "task", Value: taskIDString(task.Seq)},
			{Key: "state", Value: string(task.State)},
			{Key: "next", Value: "Run `posse brief` to read your launch Brief and continue"},
		})
	}
	if project, err := db.ProjectByLeadPane(ctx.Context, paneID); err == nil {
		if !s.recordedLeadIsLive(ctx.Context, paneID) {
			return nil
		}
		_ = ctx.PrintWithoutHelp(axi.Object{
			{Key: "role", Value: "lead"},
			{Key: "project", Value: project.Name},
			{Key: "next", Value: "Run `posse lead` to read the Lead protocol and continue"},
		})
	}
	return nil
}

func (s *Service) recordedLeadIsLive(ctx context.Context, paneID string) bool {
	if s.Herdr == nil {
		return false
	}
	snapshot, err := s.Herdr.Snapshot(ctx)
	if err != nil {
		return false
	}
	for _, pane := range snapshot.Panes {
		if pane.PaneID == paneID && pane.Agent != "" && pane.AgentStatus != "exited" && pane.AgentStatus != "stopped" {
			return true
		}
	}
	return false
}

func (s *Service) recover(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("recover", args, map[string]flagSpec{"all": {boolean: true}, "rebuild": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 || parsed.Bool("all") == parsed.Bool("rebuild") {
		return axi.Usage("recover requires exactly one of --all or --rebuild")
	}
	if parsed.Bool("rebuild") {
		home, err := s.homePath()
		if err != nil {
			return err
		}
		if !rebuildCallerMayBeUser(ctx.Context, home) {
			return axi.Failure("user_only", "recover --rebuild can only be run by the User", false)
		}
		databasePath := filepath.Join(home, "posse.db")
		if store.IsCorruptDatabaseFile(databasePath) {
			if _, moveErr := moveCorruptDatabaseAside(databasePath); moveErr != nil {
				return fmt.Errorf("preserve corrupt database before rebuild: %w", moveErr)
			}
		}
		db, err := store.Open(home)
		if err != nil {
			return schemaFailure(err)
		}
		defer db.Close()
		if s.configCallerRole(ctx.Context, db) != "user" {
			return axi.Failure("user_only", "recover --rebuild can only be run by the User", false)
		}
		count, err := db.RebuildFromSnapshots(ctx.Context, home)
		if err != nil {
			return axi.Failure("rebuild_failed", "could not rebuild the database from Task snapshots", false, err.Error())
		}
		return ctx.Print(axi.Object{{Key: "rebuilt_tasks", Value: count}, {Key: "source", Value: filepath.Join(home, "projects")}})
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	projects, err := db.Projects(ctx.Context)
	if err != nil {
		return err
	}
	reconciled := 0
	restarted := 0
	failed := []any{}
	for _, project := range projects {
		count, err := s.recoverProject(ctx.Context, db, home, project)
		if err != nil {
			failed = append(failed, map[string]any{"project": project.Name, "error": err.Error()})
			continue
		}
		restarted += count
		reconciled++
	}
	state := "reconciled"
	if len(failed) > 0 {
		state = "partial"
	}
	return ctx.Print(axi.Object{{Key: "projects", Value: reconciled}, {Key: "attempted", Value: len(projects)}, {Key: "restarted", Value: restarted}, {Key: "failed_projects", Value: failed}, {Key: "state", Value: state}})
}

func rebuildCallerMayBeUser(ctx context.Context, home string) bool {
	if os.Getenv("HERDR_PANE_ID") != "" {
		return false
	}
	if root, err := gitTop(ctx, ""); err == nil && store.IsTaskWorktreeSnapshot(home, root) {
		return false
	}
	return true
}

func moveCorruptDatabaseAside(path string) (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405.000000000")
	backup := path + ".corrupt-" + stamp
	type movedFile struct{ source, target string }
	var moved []movedFile
	for _, suffix := range []string{"-wal", "-shm", ""} {
		source := path + suffix
		target := backup + suffix
		if err := os.Rename(source, target); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			for index := len(moved) - 1; index >= 0; index-- {
				_ = os.Rename(moved[index].target, moved[index].source)
			}
			return "", err
		}
		moved = append(moved, movedFile{source: source, target: target})
	}
	return backup, nil
}

func (s *Service) recoverProject(ctx context.Context, db *store.DB, home string, project store.Project) (restarted int, returnedErr error) {
	previousGeneration, err := db.ProjectServerStartedAt(ctx, project.ID)
	if err != nil {
		return 0, err
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return 0, err
	}
	serverRestarted := previousGeneration != "" && snapshot.ServerStartedAt != "" && previousGeneration != snapshot.ServerStartedAt
	tasks, err := db.LiveTasks(ctx, project.ID)
	if err != nil {
		return 0, err
	}
	for _, task := range tasks {
		if taskNeedsRestartRecovery(task, snapshot) {
			serverRestarted = true
			break
		}
	}
	if !serverRestarted {
		if _, err := s.prepareProject(ctx, db, project); err != nil {
			return 0, err
		}
		return 0, db.RememberProjectServerStartedAt(ctx, project.ID, snapshot.ServerStartedAt)
	}
	recoveryOwnerPID := os.Getpid()
	recoveryNow := time.Now()
	claimed, previousRecovery, err := s.claimProjectRecovery(ctx, db, project.ID, snapshot.ServerStartedAt, recoveryOwnerPID, recoveryNow)
	if err != nil {
		return 0, err
	}
	if !claimed {
		return 0, nil
	}
	recoveryComplete := false
	defer func() {
		if err := db.FinishProjectRecovery(ctx, project.ID, snapshot.ServerStartedAt, recoveryOwnerPID, previousRecovery.Generation, recoveryComplete); err != nil {
			if returnedErr == nil {
				returnedErr = err
			} else {
				returnedErr = errors.Join(returnedErr, err)
			}
		}
	}()
	if _, err := os.Stat(project.Root); err != nil {
		return 0, axi.Failure("project_missing", fmt.Sprintf("Project %s path no longer exists: %s", project.Name, project.Root), false, "Run `posse project move "+project.Name+" <new-root>`")
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return 0, configError(err)
	}
	project, err = s.ensureRecoveryWorkspace(ctx, db, project, snapshot)
	if err != nil {
		return 0, err
	}
	recovered := []string{}
	for _, beforeRestart := range tasks {
		if !recoverableTaskState(beforeRestart.State) {
			continue
		}
		task, err := db.TaskByID(ctx, project.ID, beforeRestart.ID)
		if err != nil {
			return len(recovered), err
		}
		if task.State != beforeRestart.State {
			continue
		}
		if _, err := s.relaunchTask(ctx, db, home, project, cfg, task, ""); err != nil {
			var cliErr *axi.Error
			if errors.As(err, &cliErr) && cliErr.Code == "intent_active" {
				active, intentErr := db.IntentByTask(ctx, task.ID)
				if intentErr != nil {
					return len(recovered), fmt.Errorf("recover Task %s: %w (read active intent: %v)", taskIDString(task.Seq), err, intentErr)
				}
				if active.Command == "relaunch" && active.ProcessID != os.Getpid() && intentProcessAlive(active) {
					completed, waitErr := waitForRecoveryGeneration(ctx, db, project.ID, snapshot.ServerStartedAt, active.ProcessID)
					if waitErr != nil {
						return len(recovered), waitErr
					}
					if completed {
						return 0, nil
					}
					current, taskErr := db.TaskByID(ctx, project.ID, task.ID)
					if taskErr != nil {
						return len(recovered), taskErr
					}
					if current.State == store.StateWorking && (current.Launches > task.Launches || current.AgentName != task.AgentName) {
						recovered = append(recovered, taskDisplayName(task))
						continue
					}
				}
			}
			return len(recovered), fmt.Errorf("recover Task %s: %w", taskIDString(task.Seq), err)
		}
		recovered = append(recovered, taskDisplayName(task))
	}
	data := marshalJSON(map[string]any{"server_started_at": snapshot.ServerStartedAt, "tasks": recovered})
	summary := "Recovered after a Herdr restart"
	if len(recovered) > 0 {
		summary += ": " + strings.Join(recovered, ", ")
	}
	var existing int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE project_id=? AND kind='recovery' AND data_json=?`, project.ID, data).Scan(&existing); err != nil {
		return len(recovered), err
	}
	if existing == 0 {
		if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "recovery", Summary: summary, DataJSON: data}); err != nil {
			return len(recovered), err
		}
	}
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		return len(recovered), err
	}
	if err := s.restartLead(ctx, db, home, project, cfg, snapshot); err != nil {
		return len(recovered), err
	}
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		return len(recovered), err
	}
	if err := s.deliverNotices(ctx, db, project); err != nil {
		return len(recovered), err
	}
	// Keep the previous generation pending when any recovery step fails, so
	// the next command retries instead of silently reconciling an incomplete
	// layout as if recovery succeeded.
	if err := db.SetProjectServerStartedAt(ctx, project.ID, snapshot.ServerStartedAt); err != nil {
		return len(recovered), err
	}
	recoveryComplete = true
	return len(recovered) + 1, nil
}

func (s *Service) claimProjectRecovery(ctx context.Context, db *store.DB, projectID int64, generation string, ownerPID int, now time.Time) (bool, store.ProjectRecovery, error) {
	if generation == "" {
		return false, store.ProjectRecovery{}, nil
	}
	claimed, previous, err := db.ClaimProjectRecovery(ctx, projectID, generation, ownerPID, now.UnixMilli(), now.Add(-2*time.Minute).UnixMilli())
	if err != nil || claimed || previous.OwnerPID == 0 {
		return claimed, previous, err
	}
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := db.ProjectRecovery(ctx, projectID)
		if err != nil {
			return false, state, err
		}
		if state.OwnerPID == 0 {
			if state.Generation == generation {
				return false, state, nil
			}
			claimed, previous, err = db.ClaimProjectRecovery(ctx, projectID, generation, ownerPID, time.Now().UnixMilli(), time.Now().Add(-2*time.Minute).UnixMilli())
			if err != nil || claimed || previous.OwnerPID == 0 {
				return claimed, previous, err
			}
		}
		if state.OwnerPID != 0 && !processAlive(state.OwnerPID) {
			now = time.Now()
			claimed, previous, err = db.ClaimProjectRecovery(ctx, projectID, generation, ownerPID, now.UnixMilli(), now.Add(time.Second).UnixMilli())
			if err != nil || claimed {
				return claimed, previous, err
			}
		}
		select {
		case <-ctx.Done():
			return false, state, ctx.Err()
		case <-deadline.C:
			return false, state, axi.Failure("recovery_active", "another recovery is still running", true, "Retry `posse recover --all` after it completes")
		case <-ticker.C:
		}
	}
}

func taskNeedsRestartRecovery(task store.Task, snapshot herdr.Snapshot) bool {
	if !recoverableTaskState(task.State) || snapshot.ServerStartedAt == "" {
		return false
	}
	pane, found := findAppPane(snapshot.Panes, task.PaneID, task.PaneLabel)
	agentMissing := !found || pane.Agent == "" || pane.AgentStatus == "exited" || pane.AgentStatus == "stopped"
	return agentMissing && (task.AgentServerStartedAt == "" || task.AgentServerStartedAt != snapshot.ServerStartedAt)
}

func recoverableTaskState(state store.State) bool {
	switch state {
	case store.StateWorking, store.StateNeedsDecision, store.StateBlocked, store.StateStalled, store.StateLost, store.StateFailed:
		return true
	default:
		return false
	}
}

func waitForRecoveryGeneration(ctx context.Context, db *store.DB, projectID int64, generation string, ownerPID int) (bool, error) {
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		startedAt, err := db.ProjectServerStartedAt(ctx, projectID)
		if err != nil {
			return false, err
		}
		if startedAt == generation {
			return true, nil
		}
		if !processAlive(ownerPID) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, axi.Failure("intent_active", "another recovery is still running", true, "Retry `posse recover --all` after it completes")
		case <-ticker.C:
		}
	}
}

func (s *Service) ensureRecoveryWorkspace(ctx context.Context, db *store.DB, project store.Project, snapshot herdr.Snapshot) (store.Project, error) {
	// Reconcile follows a Lead pane that moved; a reused id is not the Lead's.
	if _, found := leadWorkspace(snapshot, project); found {
		return project, nil
	}
	var created json.RawMessage
	created, err := s.herdrCall(ctx, "workspace.create", map[string]any{"cwd": project.Root, "label": leadWorkspaceLabel(project), "focus": false, "no_focus": true})
	if err != nil {
		return store.Project{}, err
	}
	var result struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
	}
	if err := json.Unmarshal(created, &result); err != nil {
		return store.Project{}, err
	}
	if result.Workspace.WorkspaceID == "" {
		return store.Project{}, axi.Failure("herdr_invalid_response", "Herdr did not return a recovered Project workspace", true)
	}
	if err := db.SetProjectWorkspace(ctx, project.ID, result.Workspace.WorkspaceID); err != nil {
		return store.Project{}, err
	}
	return db.ProjectByID(ctx, project.ID)
}

func (s *Service) restartLead(ctx context.Context, db *store.DB, home string, project store.Project, cfg config.Config, snapshot herdr.Snapshot) error {
	now := time.Now()
	claimed, err := db.ClaimLeadStart(ctx, project.ID, now.UnixMilli(), now.Add(-2*time.Minute).UnixMilli())
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	defer db.ReleaseLeadStart(context.Background(), project.ID)
	var current *herdr.Pane
	for i := range snapshot.Panes {
		pane := &snapshot.Panes[i]
		if pane.PaneID == project.LeadPaneID || (project.LeadLabel != "" && pane.Label == project.LeadLabel) {
			current = pane
			break
		}
	}
	if current != nil && current.Agent != "" {
		if err := s.stopLeadAgent(ctx, current.PaneID, current.Agent); err != nil {
			return err
		}
	}
	if current == nil {
		var created json.RawMessage
		var err error
		created, err = s.herdrCall(ctx, "tab.create", map[string]any{"workspace_id": project.HerdrWorkspaceID, "cwd": project.Root, "label": project.LeadLabel, "focus": false, "no_focus": true})
		if err != nil {
			return err
		}
		var result struct {
			RootPane struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
		}
		if err := json.Unmarshal(created, &result); err != nil {
			return err
		}
		if result.RootPane.PaneID == "" {
			return axi.Failure("herdr_invalid_response", "Herdr did not return a pane for the restarted Lead", true)
		}
		current = &herdr.Pane{PaneID: result.RootPane.PaneID, WorkspaceID: project.HerdrWorkspaceID}
	}
	label := project.LeadLabel
	if label == "" {
		label = "posse:" + project.Name + ":lead"
	}
	if _, err := s.herdrCall(ctx, "pane.rename", map[string]any{"pane_id": current.PaneID, "label": label}); err != nil {
		return err
	}
	if _, err := s.herdrCall(ctx, "workspace.rename", map[string]any{"workspace_id": current.WorkspaceID, "label": leadWorkspaceLabel(project)}); err != nil {
		return err
	}
	if _, err := s.herdrCall(ctx, "pane.report_metadata", map[string]any{"pane_id": current.PaneID, "source": "posse", "title": "Lead: " + project.Name, "display_agent": "Lead"}); err != nil {
		return err
	}
	kind := s.currentLeadKind(ctx, project, cfg, cfg.Lead.Kind)
	if _, ok := cfg.Kinds[kind]; !ok {
		return axi.Failure("config_invalid", "unknown Lead agent kind "+kind, false)
	}
	leadStart, err := s.prepareLeadLaunch(home, project, cfg, kind)
	if err != nil {
		return err
	}
	launch, err := db.NextLeadLaunch(ctx, project.ID)
	if err != nil {
		return err
	}
	if err := s.exportLeadEnvironment(ctx, current.PaneID, leadStart.Env); err != nil {
		return err
	}
	if _, err := s.startAgent(ctx, map[string]any{"name": leadAgentName(project.Name, launch), "kind": kind, "pane_id": current.PaneID, "args": leadStart.Args}); err != nil {
		return err
	}
	if err := s.waitLeadStarted(ctx, current.PaneID, leadStart); err != nil {
		return err
	}
	if err := db.SetProjectLead(ctx, project.ID, current.WorkspaceID, current.PaneID, label); err != nil {
		return err
	}
	if leadStart.TypedPrompt != "" {
		if err := s.deliverLaunchPrompt(ctx, current.PaneID, leadStart.TypedPrompt); err != nil {
			return err
		}
	}
	return nil
}

// Herdr's agent.start has no env parameter. Export only into the Lead's
// shell before starting OpenCode, not into global config or unrelated panes.
func (s *Service) exportLeadEnvironment(ctx context.Context, paneID string, env map[string]string) error {
	for key, value := range env {
		quoted := "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
		if _, err := s.herdrCall(ctx, "pane.send_input", map[string]any{"pane_id": paneID, "text": "export " + key + "=" + quoted, "keys": []string{"enter"}}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) stopLeadAgent(ctx context.Context, paneID, agent string) error {
	if _, err := s.herdrCall(ctx, "agent.send_keys", map[string]any{"target": paneID, "keys": []string{"ctrl+c"}}); err != nil && !missingAgentError(err) {
		return err
	}
	if err := s.waitLeadShellFor(ctx, paneID, 500*time.Millisecond); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := s.herdrCall(ctx, "agent.send_keys", map[string]any{"target": paneID, "keys": []string{"esc"}}); err != nil && !missingAgentError(err) {
			return err
		}
		if err := s.waitLeadShellFor(ctx, paneID, 500*time.Millisecond); err == nil {
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
		command := leadExitCommand(agent)
		if command != "" {
			if _, err := s.herdrCall(ctx, "agent.prompt", map[string]any{"target": paneID, "text": command}); err != nil {
				return err
			}
		} else if _, err := s.herdrCall(ctx, "agent.send_keys", map[string]any{"target": paneID, "keys": []string{"ctrl+d"}}); err != nil && !missingAgentError(err) {
			return err
		}
	} else {
		return nil
	}
	return s.waitLeadShell(ctx, paneID)
}

func leadExitCommand(agent string) string {
	switch strings.ToLower(agent) {
	case "claude":
		return "/exit"
	case "codex", "pi", "opencode":
		return "/quit"
	default:
		return ""
	}
}

func (s *Service) waitLeadShell(ctx context.Context, paneID string) error {
	return s.waitLeadShellFor(ctx, paneID, 10*time.Second)
}

func (s *Service) waitLeadShellFor(ctx context.Context, paneID string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if s.leadPaneIsShell(ctx, paneID) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return axi.Failure("agent_exit_timeout", "Herdr did not return the recorded Lead pane to its shell", true, "Inspect the Lead pane and retry recovery")
		case <-ticker.C:
		}
	}
}

func (s *Service) leadPaneIsShell(ctx context.Context, paneID string) bool {
	snapshot, err := s.Herdr.Snapshot(ctx)
	if err != nil {
		return false
	}
	for _, pane := range snapshot.Panes {
		if pane.PaneID == paneID {
			return pane.Agent == "" || pane.AgentStatus == "exited" || pane.AgentStatus == "stopped"
		}
	}
	return false
}

func missingAgentError(err error) bool {
	var apiError *herdr.Error
	if errors.As(err, &apiError) {
		return apiError.Code == "agent_not_found" || apiError.Code == "not_found"
	}
	var cliError *axi.Error
	return errors.As(err, &cliError) && (cliError.Code == "agent_not_found" || cliError.Code == "not_found")
}
