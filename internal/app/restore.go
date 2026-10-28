package app

import (
	"context"
	"errors"
	"fmt"
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
// of every Project that is not down.
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
	projects, err := db.Projects(ctx.Context)
	if err != nil {
		return err
	}
	restored := []any{}
	failed := []any{}
	down := []any{}
	help := []any{}
	inLeadPane := 0
	pending := false
	for _, project := range projects {
		if project.IsDown() {
			down = append(down, project.Name)
			continue
		}
		if project.HerdrWorkspaceID == "" && project.LeadPaneID == "" {
			continue // Never started; `posse up` in its folder starts it.
		}
		blocked, err := s.leadPaneRunsRestore(ctx.Context, actor, project)
		if err != nil {
			return err
		}
		if blocked {
			failed = append(failed, map[string]any{"project": project.Name, "error": "posse up --all runs in the Lead pane " + project.LeadPaneID + ", so Herdr cannot start the Lead there"})
			help = append(help, "Run `posse up` in the Lead pane of "+project.Name+" to restore its Lead and Workers, or run `posse up --all` from another pane")
			inLeadPane++
			continue
		}
		workers, err := s.restoreProject(ctx.Context, db, home, project)
		if errors.Is(err, errRecoveryDeferred) {
			pending = true
			err = nil
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
	result := axi.Object{{Key: "restored", Value: restored}, {Key: "failed_projects", Value: failed}}
	if len(down) > 0 {
		result = append(result, axi.Field{Key: "down_projects", Value: down})
	}
	result = append(result, axi.Field{Key: "state", Value: state})
	if len(help) > 0 {
		result = append(result, axi.Field{Key: "help", Value: help})
	}
	return ctx.Print(result)
}

// leadPaneRunsRestore reports whether `posse up --all` runs in the shell of
// project's stopped Lead pane. Herdr cannot start an agent in the pane running
// this command; `posse up` there replaces the shell with the Lead instead.
func (s *Service) leadPaneRunsRestore(ctx context.Context, actor caller, project store.Project) (bool, error) {
	if actor.role != "lead" || actor.projectID != project.ID || actor.paneID != project.LeadPaneID {
		return false, nil
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return false, err
	}
	return !leadAgentStarted(project, snapshot), nil
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
