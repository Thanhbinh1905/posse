package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type attachArgs struct {
	PaneID    string
	SessionID string
}

func parseAttachArgs(args []string) (attachArgs, error) {
	var result attachArgs
	for index := 0; index < len(args); index++ {
		name, value, hasValue := strings.Cut(strings.TrimPrefix(args[index], "--"), "=")
		if !strings.HasPrefix(args[index], "--") {
			return attachArgs{}, axi.Usage("attach accepts only --pane and --session")
		}
		if !hasValue {
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
				return attachArgs{}, axi.Usage("attach --" + name + " requires a value")
			}
			index++
			value = args[index]
		}
		switch name {
		case "pane":
			if result.PaneID != "" {
				return attachArgs{}, axi.Usage("attach accepts --pane only once")
			}
			result.PaneID = value
		case "session":
			if result.SessionID != "" {
				return attachArgs{}, axi.Usage("attach accepts --session only once")
			}
			result.SessionID = value
		default:
			return attachArgs{}, axi.Usage("attach accepts only --pane and --session")
		}
	}
	if result.PaneID == "" || result.SessionID == "" {
		return attachArgs{}, axi.Usage("attach requires --pane <pane-id> and --session <session-id>")
	}
	return result, nil
}

func (s *Service) attach(ctx *axi.Context, args []string) (returnErr error) {
	parsed, err := parseAttachArgs(args)
	if err != nil {
		return err
	}
	if s.herdrContext == nil || !s.herdrContext() {
		return axi.Failure("not_in_herdr", "posse attach must run from a Herdr pane", false, "Open a User shell pane in Herdr, then retry `posse attach`")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if actor := s.configCaller(ctx.Context, db); actor.role != "user" {
		return axi.Failure("lead_only", "posse attach must run from a User shell", false, "Run `posse attach` from a User shell in the Project folder")
	}
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return configError(err)
	}
	snapshot, err := s.snapshot(ctx.Context)
	if err != nil {
		return err
	}
	pane, kind, err := attachedSession(snapshot, parsed.PaneID, parsed.SessionID)
	if err != nil {
		return err
	}
	if kind != "claude" && kind != "codex" && kind != "pi" {
		return unsupportedAttachedSession(kind)
	}
	if err := refuseAttachForeignLeadWorkspace(snapshot, project, pane.WorkspaceID); err != nil {
		return err
	}
	if _, ok := cfg.Kinds[kind]; !ok {
		return unsupportedAttachedSession(kind)
	}
	leadPaneID, running := projectLeadPane(snapshot, project)
	recovering := running && leadPaneID == pane.PaneID && project.LeadPaneID == pane.PaneID && project.HerdrWorkspaceID == pane.WorkspaceID
	if running && !recovering {
		return axi.Failure("lead_running", fmt.Sprintf("Project %s already has a Lead running in pane %s", project.Name, leadPaneID), false, "Use the existing Lead or stop it with `posse down` before attaching another session")
	}
	if recovering {
		lead, err := db.ProjectLeadAgent(ctx.Context, project.ID)
		if err != nil {
			return err
		}
		if (lead.Kind != "" && lead.Kind != kind) || (lead.Session != "" && lead.Session != parsed.SessionID) {
			return axi.Failure("lead_running", fmt.Sprintf("Project %s already records a different Lead session in pane %s", project.Name, pane.PaneID), false, "Use the existing Lead or stop it with `posse down` before attaching another session")
		}
	}
	now := time.Now()
	claimed, err := db.ClaimLeadStart(ctx.Context, project.ID, now.UnixMilli(), now.Add(-2*time.Minute).UnixMilli(), "attach", os.Getpid())
	if err != nil {
		return err
	}
	if !claimed {
		return axi.Failure("lead_starting", "another Lead start or attach is in progress", true, "Wait for it to finish, then run `posse attach` again")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if releaseErr := db.ReleaseLeadStart(cleanup, project.ID, "attach", os.Getpid()); releaseErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("release Lead attach claim: %w", releaseErr))
		}
	}()
	gaps, err := s.readiness(ctx.Context, db, project, cfg)
	if err != nil {
		return err
	}
	instructions, err := composeLeadInstructions(home, project, cfg, kind, gaps)
	if err != nil {
		return err
	}
	if len(instructions.Text) > leadInstructionMaxBytes {
		return axi.Failure("lead_instructions_too_large", fmt.Sprintf("Lead instructions exceed the %d-token budget: %d bytes (maximum %d bytes at %d bytes per token)", leadInstructionTokenBudget, len(instructions.Text), leadInstructionMaxBytes, leadInstructionBytesPerToken), false)
	}
	prompt := "You are now the Lead for Project " + project.Name + ". Keep this conversation and follow these Posse Lead instructions. Do not start work until the User speaks again.\n\n" + instructions.Text
	verify := func(current herdr.Snapshot) error {
		if current.ServerStartedAt != snapshot.ServerStartedAt {
			return axi.Failure("attach_generation_changed", "Herdr restarted while the session was being verified", false, "Inspect the target session, then retry `posse attach`")
		}
		if paneID, running := projectLeadPane(current, project); running && !(recovering && paneID == pane.PaneID) {
			return axi.Failure("lead_running", fmt.Sprintf("Project %s already has a Lead running in pane %s", project.Name, paneID), false, "Use the existing Lead or stop it with `posse down` before attaching another session")
		}
		currentPane, currentKind, verifyErr := attachedSession(current, parsed.PaneID, parsed.SessionID)
		if verifyErr != nil || currentKind != kind || currentPane.WorkspaceID != pane.WorkspaceID {
			return axi.Failure("attach_session_changed", "the selected pane or session changed during verification", false, "Inspect the target session, then retry `posse attach`")
		}
		return refuseAttachForeignLeadWorkspace(current, project, currentPane.WorkspaceID)
	}
	if recovering {
		current, err := s.snapshot(ctx.Context)
		if err != nil {
			return err
		}
		if err := verify(current); err != nil {
			return err
		}
	} else if err := s.safePromptWhen(ctx.Context, pane.PaneID, prompt, verify); err != nil {
		return err
	}
	label := "posse:" + project.Name + ":lead"
	if err := db.SetProjectLead(ctx.Context, project.ID, pane.WorkspaceID, pane.PaneID, label); err != nil {
		return err
	}
	if err := db.SetProjectLeadAgent(ctx.Context, project.ID, store.LeadAgent{Kind: kind, Session: parsed.SessionID}); err != nil {
		return err
	}
	if _, err := s.herdrCall(ctx.Context, "pane.rename", map[string]any{"pane_id": pane.PaneID, "label": label}); err != nil {
		return herdrError(err)
	}
	if _, err := s.herdrCall(ctx.Context, "workspace.rename", map[string]any{"workspace_id": pane.WorkspaceID, "label": leadWorkspaceLabel(project)}); err != nil {
		return herdrError(err)
	}
	if _, err := s.herdrCall(ctx.Context, "pane.report_metadata", attachedLeadMetadata(project, pane.PaneID, kind)); err != nil {
		return herdrError(err)
	}
	project, err = db.ProjectByID(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	s.relabelProjectTabs(ctx.Context, db, project)
	if err := s.ensureWatchRuntime(ctx.Context, db, home, project); err != nil {
		return err
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return err
	}
	return ctx.Print(axi.Object{
		{Key: "state", Value: "attached"},
		{Key: "project", Value: project.Name},
		{Key: "pane_id", Value: pane.PaneID},
		{Key: "session_id", Value: parsed.SessionID},
		{Key: "help", Value: []any{"The existing conversation was preserved. Continue as the Lead for this Project."}},
	})
}

func refuseAttachForeignLeadWorkspace(snapshot herdr.Snapshot, project store.Project, workspaceID string) error {
	return refuseForeignLeadWorkspaceWithCommand(snapshot, project, os.Getenv("HERDR_PANE_ID"), workspaceID, "posse attach --pane <pane-id> --session <session-id>")
}

func attachedSession(snapshot herdr.Snapshot, paneID, sessionID string) (herdr.Pane, string, error) {
	var pane *herdr.Pane
	for index := range snapshot.Panes {
		if snapshot.Panes[index].PaneID == paneID {
			pane = &snapshot.Panes[index]
			break
		}
	}
	if pane == nil {
		return herdr.Pane{}, "", axi.Failure("attach_pane_not_found", "the selected pane is not present in the Herdr session", false, "Inspect Herdr, then run `posse attach` with an active pane and session")
	}
	if pane.Agent == "" || pane.AgentStatus == "" || pane.AgentStatus == "unknown" || pane.AgentStatus == "exited" || pane.AgentStatus == "stopped" {
		return herdr.Pane{}, "", axi.Failure("attach_session_not_live", "the selected pane has no live agent session", false, "Inspect the pane or run `posse up` to start a fresh Lead")
	}
	kind := pane.Agent
	if len(pane.AgentSession) > 0 && string(pane.AgentSession) != "null" {
		var session struct {
			Agent string `json:"agent"`
		}
		if json.Unmarshal(pane.AgentSession, &session) == nil {
			if kind == "" {
				kind = session.Agent
			}
			if session.Agent != "" && kind != session.Agent {
				return herdr.Pane{}, "", unsupportedAttachedSession(kind)
			}
		}
	}
	agentSession := ""
	for _, agent := range snapshot.Agents {
		if agent.PaneID != paneID {
			continue
		}
		if kind == "" {
			kind = agent.Kind
			if kind == "" {
				kind = agent.Agent
			}
		}
		if (agent.Kind != "" && kind != agent.Kind) || (agent.Agent != "" && kind != agent.Agent) {
			return herdr.Pane{}, "", unsupportedAttachedSession(kind)
		}
		agentSession = agent.SessionID
	}
	paneSession := agentSessionID(kind, string(pane.AgentSession))
	agentSession = agentSessionID(kind, agentSession)
	if kind == "" || pane.WorkspaceID == "" || paneSession == "" || agentSession == "" {
		return herdr.Pane{}, "", unsupportedAttachedSession(kind)
	}
	if paneSession != agentSession || agentSession != sessionID {
		return herdr.Pane{}, "", axi.Failure("attach_session_mismatch", "the requested session does not match the exact session running in the selected pane", false, "Verify the pane and session IDs in Herdr, then retry `posse attach`")
	}
	return *pane, kind, nil
}

func projectLeadPane(snapshot herdr.Snapshot, project store.Project) (string, bool) {
	label := "posse:" + project.Name + ":lead"
	namePrefix := "posse-" + project.Name + "-lead-"
	for _, pane := range snapshot.Panes {
		recordedLead := project.LeadPaneID != "" && pane.PaneID == project.LeadPaneID && pane.WorkspaceID == project.HerdrWorkspaceID
		ownsLead := recordedLead || pane.Label == label || (project.LeadLabel != "" && pane.Label == project.LeadLabel)
		hasAgent := pane.Agent != ""
		for _, agent := range snapshot.Agents {
			if agent.PaneID != pane.PaneID {
				continue
			}
			hasAgent = hasAgent || agent.Agent != "" || agent.Kind != "" || agent.SessionID != ""
			ownsLead = ownsLead || strings.HasPrefix(agent.Name, namePrefix)
		}
		if ownsLead && hasAgent {
			return pane.PaneID, true
		}
	}
	return "", false
}

func attachedLeadMetadata(project store.Project, paneID, kind string) map[string]any {
	metadata := leadDisplayMetadata(project, paneID, kind, kind)
	metadata["tokens"] = map[string]string{"posse_row": leadWorkspaceLabel(project), "posse_attached": "true"}
	return metadata
}

func unsupportedAttachedSession(kind string) error {
	name := kind
	if name == "" {
		name = "unknown"
	}
	return axi.Failure("attach_unsupported_session", "Herdr session integration is not supported for "+name, false, "Run `posse up` to start a Lead with a supported integration")
}
