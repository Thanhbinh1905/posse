package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/sessionref"
	"github.com/thanhbinh1905/posse/internal/store"
)

// errRecoveryHeld marks a Project whose Lead or Workers stopped with Herdr.
// Posse never restarts them on its own (ADR 0035); only `posse up` does.
var errRecoveryHeld = errors.New("restore is held until posse up")

// holdRestore records that a Herdr restart or group close stopped the agents
// of a Project, and tells the User once per episode how to restore them.
func (s *Service) holdRestore(ctx context.Context, db *store.DB, project store.Project, episode string) error {
	if project.IsDown() || episode == "" {
		return errRecoveryHeld
	}
	tasks, err := db.LiveTasks(ctx, project.ID)
	if err != nil {
		return err
	}
	workers := []string{}
	for _, task := range tasks {
		if recoverableTaskState(task.State) {
			workers = append(workers, taskDisplayName(task))
		}
	}
	data := marshalJSON(map[string]any{"episode": episode, "tasks": workers})
	var existing int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE project_id=? AND kind='restore_needed' AND json_extract(data_json,'$.episode')=?`, project.ID, episode).Scan(&existing); err != nil {
		return err
	}
	if existing == 0 {
		summary := "Herdr stopped the Lead of " + project.Name
		if len(workers) > 0 {
			summary += " and Workers " + strings.Join(workers, ", ")
		}
		summary += "; run `posse up --all` to restore them"
		if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "restore_needed", Summary: summary, DataJSON: data}); err != nil {
			return err
		}
		// With no Lead agent, delivery is a Herdr notification to the User.
		// The Notice stays durable if Herdr cannot show it yet.
		_ = s.deliverNotices(ctx, db, project)
	}
	return errRecoveryHeld
}

// adoptCurrentGeneration records Herdr's current generation for a held
// Project whose Lead pane and every recoverable Worker pane already run a
// confirmed agent in it. A recovery that failed after bringing every pane
// back, or a restore the User did by hand, then ends the hold instead of
// extending it.
// A missing Lead or Worker keeps the hold for `posse up` (ADR 0035).
func adoptCurrentGeneration(ctx context.Context, db *store.DB, project store.Project, snapshot herdr.Snapshot) (bool, error) {
	recorded, err := db.ProjectServerStartedAt(ctx, project.ID)
	if err != nil || recorded == "" || snapshot.ServerStartedAt == "" || recorded == snapshot.ServerStartedAt {
		return false, err
	}
	if project.IsDown() || !leadPaneLive(project, snapshot) {
		return false, nil
	}
	tasks, err := db.Tasks(ctx, project.ID, false)
	if err != nil {
		return false, err
	}
	for _, task := range tasks {
		if task.State == store.StateFailed || !recoverableTaskState(task.State) {
			continue
		}
		if pane, found := findAppPane(snapshot.Panes, task.PaneID, task.PaneLabel); !found || !paneAgentRunning(pane) {
			return false, nil
		}
		// A Worker whose relaunch failed may still run an agent that never
		// started working; its retry episode keeps the hold.
		state, err := db.TaskRecovery(ctx, task.ID)
		if err != nil {
			return false, err
		}
		if state.Status == "pending" || state.Status == "running" {
			return false, nil
		}
	}
	if err := db.SetProjectServerStartedAt(ctx, project.ID, snapshot.ServerStartedAt); err != nil {
		return false, err
	}
	_, err = db.AckUndeliveredNotices(ctx, project.ID, "restore_needed", currentTime())
	return err == nil, err
}

// leadPaneLive reports whether the Lead pane, found by its label first because
// Herdr can reuse pane ids after a restart, runs an agent.
func leadPaneLive(project store.Project, snapshot herdr.Snapshot) bool {
	pane, found := herdr.FindPane(snapshot.Panes, project.LeadPaneID, project.LeadLabel)
	return found && paneAgentRunning(pane)
}

// paneAgentRunning reports whether Herdr confirms a running agent in the pane.
func paneAgentRunning(pane herdr.Pane) bool {
	return pane.Agent != "" && pane.AgentStatus != "" && pane.AgentStatus != "unknown" && pane.AgentStatus != "exited" && pane.AgentStatus != "stopped"
}

// observeLeadAgent records the agent kind and session running in the Lead
// pane, so a restore brings back that Lead rather than lead.kind.
func observeLeadAgent(ctx context.Context, db *store.DB, project store.Project, snapshot herdr.Snapshot) error {
	if project.IsDown() || project.LeadPaneID == "" {
		return nil
	}
	for _, pane := range snapshot.Panes {
		if pane.PaneID != project.LeadPaneID || pane.WorkspaceID != project.HerdrWorkspaceID {
			continue
		}
		if pane.Agent == "" || pane.AgentStatus == "exited" || pane.AgentStatus == "stopped" {
			return nil
		}
		kind := sessionref.NormalizeKind(pane.Agent)
		session := ""
		if len(pane.AgentSession) > 0 && string(pane.AgentSession) != "null" {
			session = agentSessionID(kind, string(pane.AgentSession))
		}
		return db.ObserveProjectLeadAgent(ctx, project.ID, store.LeadAgent{Kind: kind, Session: session})
	}
	return nil
}

// restoreLeadAgent picks the kind and session for a restored Lead: the
// recorded Lead when its kind is still configured, else lead.kind.
func (s *Service) restoreLeadAgent(ctx context.Context, db *store.DB, project store.Project, cfg config.Config) (string, string, error) {
	recorded, err := db.ProjectLeadAgent(ctx, project.ID)
	if err != nil {
		return "", "", err
	}
	if _, ok := cfg.Kinds[recorded.Kind]; ok {
		return recorded.Kind, recorded.Session, nil
	}
	return s.currentLeadKind(ctx, project, cfg, cfg.Lead.Kind), "", nil
}

// withResume adds the kind's resume arguments for session. A Codex opening
// prompt stays the last positional argument.
func (launch leadLaunch) withResume(kind config.Kind, session string) leadLaunch {
	if session == "" || len(kind.ResumeArgs) == 0 {
		return launch
	}
	resume := make([]string, 0, len(kind.ResumeArgs))
	for _, arg := range kind.ResumeArgs {
		resume = append(resume, strings.ReplaceAll(arg, "{session}", session))
	}
	args := append([]string{}, launch.Args...)
	if launch.OpeningPromptArg && len(args) > 0 {
		last := args[len(args)-1]
		args = append(append(args[:len(args)-1], resume...), last)
	} else {
		args = append(args, resume...)
	}
	launch.Args = args
	return launch
}

// restoreAll is `posse up --all`: it brings back the Lead, Workers and watch
// of every Project that is not down, or only of those up.projects lists.
func (s *Service) restoreAll(ctx *axi.Context) error {
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	actor := s.configCaller(ctx.Context, db)
	if actor.role == "worker" {
		return axi.Failure("lead_only", "posse up --all cannot run from a Worker pane", false, "Run `posse up --all` from a User or Lead shell")
	}
	if s.Herdr == nil {
		return axi.Failure("herdr_unavailable", "Herdr adapter is not configured", true)
	}
	if err := s.Herdr.CheckProtocol(ctx.Context); err != nil {
		return herdrError(err)
	}
	cfg, err := config.Load(home, "")
	if err != nil {
		return configError(err)
	}
	projects, err := db.Projects(ctx.Context)
	if err != nil {
		return err
	}
	restored := []any{}
	failed := []any{}
	down := []any{}
	skipped := []any{}
	help := []any{}
	inLeadPane := 0
	pending := false
	for _, project := range projects {
		if cfg.Up.Projects != nil && !slices.Contains(cfg.Up.Projects, project.Name) {
			skipped = append(skipped, project.Name)
			continue
		}
		if project.IsDown() {
			down = append(down, project.Name)
			continue
		}
		if project.HerdrWorkspaceID == "" && project.LeadPaneID == "" {
			continue // Never started; `posse up` in its folder starts it.
		}
		workers, err := s.restoreProject(ctx.Context, db, home, project)
		if errors.Is(err, errRecoveryDeferred) {
			pending = true
			err = nil
		}
		// Herdr cannot start an agent in the pane running this command;
		// `posse up` there replaces the shell with the Lead instead.
		var failure *axi.Error
		if errors.As(err, &failure) && failure.Code == "agent_pane_busy" && actor.role == "lead" && actor.projectID == project.ID && actor.paneID == project.LeadPaneID {
			failed = append(failed, map[string]any{"project": project.Name, "error": "posse up --all runs in the Lead pane " + project.LeadPaneID + ", so Herdr cannot start the Lead there"})
			help = append(help, "Run `posse up` in the Lead pane of "+project.Name+" to start its Lead, or run `posse up --all` from another pane")
			inLeadPane++
			continue
		}
		if err != nil {
			failed = append(failed, map[string]any{"project": project.Name, "error": err.Error()})
			continue
		}
		restored = append(restored, map[string]any{"project": project.Name, "workers": workers})
	}
	state := "restored"
	if pending {
		state = "partial"
		help = append(help, "Some Workers wait for their relaunch backoff; run `posse up --all` again shortly")
	}
	if len(failed) > 0 {
		state = "partial"
	}
	if len(failed) > inLeadPane {
		help = append(help, "Fix the failed Projects and run `posse up --all` again")
	}
	if len(down) > 0 {
		help = append(help, "Run `posse up` in a down Project's folder to start it")
	}
	if len(skipped) > 0 {
		help = append(help, "Run `posse up` in a skipped Project's folder to restore it, or add it to up.projects")
	}
	result := axi.Object{{Key: "restored", Value: restored}, {Key: "failed_projects", Value: failed}}
	if len(down) > 0 {
		result = append(result, axi.Field{Key: "down_projects", Value: down})
	}
	if len(skipped) > 0 {
		result = append(result, axi.Field{Key: "skipped_projects", Value: skipped})
	}
	result = append(result, axi.Field{Key: "state", Value: state})
	if len(help) > 0 {
		result = append(result, axi.Field{Key: "help", Value: help})
	}
	return ctx.Print(result)
}

// restoreProject restores Workers stopped by a Herdr restart or group close,
// then starts the Lead when none is running and ensures the watch.
func (s *Service) restoreProject(ctx context.Context, db *store.DB, home string, project store.Project) (int, error) {
	restarted, err := s.recoverProject(ctx, db, home, project)
	if err != nil {
		return 0, err
	}
	workers := max(restarted-1, 0)
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		return workers, err
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return workers, err
	}
	if _, err := db.AckUndeliveredNotices(ctx, project.ID, "restore_needed", currentTime()); err != nil {
		return workers, err
	}
	if leadAgentStarted(project, snapshot) {
		return workers, s.ensureWatchRuntime(ctx, db, home, project)
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return workers, configError(err)
	}
	project, err = s.ensureRecoveryWorkspace(ctx, db, project, snapshot)
	if err != nil {
		return workers, err
	}
	if err := s.restartLead(ctx, db, home, project, cfg, snapshot); err != nil {
		return workers, fmt.Errorf("restore Lead: %w", err)
	}
	return workers, nil
}

// herdrStarted is the Herdr plugin's startup hook. It restarts nothing: each
// Project that is not down records the restart and raises one
// restore_needed Notice.
func (s *Service) herdrStarted(ctx *axi.Context, args []string) error {
	if len(args) != 0 {
		return axi.Usage("_herdr_started does not take arguments")
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
	held := []any{}
	failed := []any{}
	for _, project := range projects {
		if project.IsDown() {
			continue
		}
		cfg, err := config.Load(home, project.Name)
		if err != nil {
			failed = append(failed, map[string]any{"project": project.Name, "error": err.Error()})
			continue
		}
		if _, err := s.reconcileProject(ctx.Context, db, project, cfg, false); errors.Is(err, errRecoveryHeld) {
			held = append(held, project.Name)
		} else if err != nil {
			failed = append(failed, map[string]any{"project": project.Name, "error": err.Error()})
		}
	}
	result := axi.Object{{Key: "held_projects", Value: held}, {Key: "failed_projects", Value: failed}}
	if len(held) > 0 {
		result = append(result, axi.Field{Key: "help", Value: []any{"Run `posse up --all` to restore the held Projects"}})
	}
	return ctx.Print(result)
}
