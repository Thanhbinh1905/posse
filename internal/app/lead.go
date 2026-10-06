package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type upArgs struct {
	Name        string
	Kind        string
	Replace     bool
	Yes         bool
	Positionals []string
}

func parseUpArgs(args []string) (upArgs, error) {
	result := upArgs{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			result.Positionals = append(result.Positionals, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "--") {
			result.Positionals = append(result.Positionals, arg)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		switch name {
		case "name":
			if !hasValue {
				if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
					return upArgs{}, axi.Usage("--name requires a value")
				}
				index++
				value = args[index]
			}
			result.Name = value
		case "replace":
			if hasValue {
				return upArgs{}, axi.Usage("--replace does not take a value")
			}
			result.Replace = true
		case "yes":
			if hasValue {
				return upArgs{}, axi.Usage("--yes does not take a value")
			}
			result.Yes = true
		default:
			if name == "" {
				return upArgs{}, axi.Usage("up requires an agent kind flag such as --claude")
			}
			if hasValue {
				return upArgs{}, axi.Usage("agent kind flags do not take values")
			}
			if result.Kind != "" {
				return upArgs{}, axi.Usage("up accepts at most one kind flag")
			}
			result.Kind = name
		}
	}
	return result, nil
}

func (s *Service) lead(ctx *axi.Context, args []string) error {
	if len(args) != 0 {
		return axi.Usage("lead does not take arguments")
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
	cfg, err := s.prepareProjectObservation(ctx.Context, db, project)
	if err != nil {
		return err
	}
	kind := s.currentLeadKind(ctx.Context, project, cfg, cfg.Lead.Kind)
	gaps, err := s.readiness(ctx.Context, db, project, cfg)
	if err != nil {
		return err
	}
	composed, err := composeLeadInstructions(home, project, cfg, kind, gaps)
	if err != nil {
		return err
	}
	result := axi.Object{
		{Key: "instructions", Value: composed.Text},
		{Key: "lowkey", Value: composed.Lowkey},
		{Key: "reporting_rule", Value: composed.ReportingRule},
		{Key: "identity", Value: composed.Identity},
		{Key: "language_rule", Value: composed.LanguageRule},
		{Key: "help", Value: []any{"Continue the Lead conversation with the User. For code changes, follow the runtime obligations in these instructions and use `posse ride`."}},
	}
	return ctx.Print(withReadiness(result, gaps))
}

func (s *Service) currentLeadKind(ctx context.Context, project store.Project, cfg config.Config, fallback string) string {
	if s.Herdr != nil {
		if snapshot, err := s.Herdr.Snapshot(ctx); err == nil {
			for _, pane := range snapshot.Panes {
				if pane.PaneID != project.LeadPaneID && !(project.LeadLabel != "" && pane.Label == project.LeadLabel) {
					continue
				}
				for _, agent := range snapshot.Agents {
					if agent.PaneID == pane.PaneID && agent.Kind != "" {
						return agent.Kind
					}
				}
				if pane.Agent != "" {
					return pane.Agent
				}
			}
		}
	}
	if _, exists := cfg.Kinds[fallback]; exists {
		return fallback
	}
	available := s.availableLeadKinds(ctx, cfg)
	if len(available) == 1 {
		return available[0]
	}
	return ""
}

// The role belongs in the title and posse_row, not in Herdr's harness field.
// Like Riders, Leads show the resolved kind before start and detection afterward.
func leadDisplayMetadata(project store.Project, paneID, detectedAgent, expectedAgent string) map[string]any {
	metadata := map[string]any{
		"pane_id": paneID, "source": "posse", "title": "Lead: " + project.Name,
		"tokens": map[string]string{"posse_row": leadWorkspaceLabel(project)},
	}
	displayAgent := detectedAgent
	if displayAgent == "" {
		displayAgent = expectedAgent
	}
	if displayAgent != "" {
		metadata["display_agent"] = displayAgent
	} else {
		metadata["clear_display_agent"] = true
	}
	return metadata
}

func (s *Service) availableLeadKinds(ctx context.Context, cfg config.Config) []string {
	available := []string{}
	if s.Herdr != nil {
		if manifests, err := s.herdrCall(ctx, "server.agent_manifests", map[string]any{}); err == nil {
			available = availableAgentKinds(manifests)
		}
	}
	if len(available) == 0 {
		for kind := range cfg.Kinds {
			if _, err := exec.LookPath(agentCLIName(kind)); err == nil {
				available = append(available, kind)
			}
		}
		sort.Strings(available)
	}
	return available
}

func (s *Service) upCore(ctx *axi.Context, args []string) error {
	parsed, err := parseUpArgs(args)
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("up does not take positional arguments")
	}
	if s.herdrContext == nil || !s.herdrContext() {
		return axi.Failure("not_in_herdr", "posse up must run from a Herdr pane", false, "Open a shell pane in Herdr and run `posse up`")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	dir, err := currentDir()
	if err != nil {
		return err
	}
	if _, taskErr := taskForPath(ctx.Context, db, dir); taskErr == nil {
		return axi.Failure("lead_only", "posse up cannot run inside a Rider Mount", false, "Run `posse up` in the Project folder")
	}
	project, registered, err := registeredProjectFor(ctx.Context, db, dir)
	if err != nil {
		return err
	}
	var detected detectedProject
	name := parsed.Name
	if registered {
		if name != "" && project.Name != projectName(name) {
			return axi.Failure("project_name_conflict", fmt.Sprintf("this folder is already registered as Project %q", project.Name), false, "Use its existing name or `posse project move` for a moved Project")
		}
		name = project.Name
	} else {
		detected, err = detectProject(ctx.Context, dir)
		if err != nil {
			return err
		}
		if err := applyProjectName(&detected, name); err != nil {
			return err
		}
		name = detected.Name
	}
	root := project.Root
	if !registered {
		root = detected.Root
	}
	cfg, err := config.Load(home, name)
	if err != nil {
		return configError(err)
	}
	kind := parsed.Kind
	kindSource := "flag"
	if kind == "" {
		kindSource = "lead.kind"
		kind = cfg.Lead.Kind
	}
	if kind == "" {
		available := s.availableLeadKinds(ctx.Context, cfg)
		if len(available) != 1 {
			return axi.Failure("lead_kind_required", "choose a Lead agent kind", false,
				"Pass `posse up --<kind>` or set `lead.kind`. Available kinds: "+strings.Join(available, ", "))
		}
		kind = available[0]
		kindSource = "only available kind"
	}
	if _, kindFound := cfg.Kinds[kind]; !kindFound {
		return axi.Failure("agent_kind_unknown", "unknown Lead agent kind "+kind, false, "Choose a configured kind from `posse config show --effective`")
	}
	workspace := (registered && project.IsWorkspace()) || (!registered && detected.Kind == store.ProjectKindWorkspace)
	if cfg.Defaults.LandingMode == "no-mistakes" {
		if workspace {
			return axi.Failure("config_invalid", "no-mistakes landing is not available for workspace Projects", false, "Set `landing_mode` to local or pr for Project "+name)
		}
		if err := ensureNoMistakesInitialized(ctx.Context, root); err != nil {
			readinessProject := project
			if !registered {
				readinessProject = store.Project{Name: name, Root: root, Kind: detected.Kind, DefaultBranch: detected.DefaultBranch}
			}
			gaps, readinessErr := s.readiness(ctx.Context, db, readinessProject, cfg)
			if readinessErr == nil {
				return withReadinessHelp(err, gaps)
			}
			return err
		}
	}
	if s.Herdr == nil {
		return axi.Failure("herdr_unavailable", "Herdr adapter is not configured", true)
	}
	if err := s.Herdr.CheckProtocol(ctx.Context); err != nil {
		return herdrError(err)
	}
	if !registered {
		if _, lookupErr := db.ProjectByName(ctx.Context, name); lookupErr == nil {
			return axi.Failure("name_taken", fmt.Sprintf("Project name %q is already in use", name), false, "Retry with `posse up --name <different-name>`")
		} else if !store.IsNotFound(lookupErr) {
			return lookupErr
		}
		if err := s.confirmRegistration(ctx, detected, cfg, parsed.Yes, "posse up"); err != nil {
			return err
		}
		project, err = registerDetected(ctx.Context, db, detected)
		if err != nil {
			return err
		}
	}
	claimed, err := db.ClaimLeadStart(ctx.Context, project.ID, time.Now().UnixMilli(), time.Now().Add(-2*time.Minute).UnixMilli())
	if err != nil {
		return err
	}
	if !claimed {
		return axi.Failure("lead_starting", "a Lead is already starting or being checked for Project "+project.Name, true, "Wait a moment, then run `posse up` again")
	}
	defer db.ReleaseLeadStart(context.Background(), project.ID)
	var snapshot herdr.Snapshot
	if registered {
		// With no Lead workspace left (`posse down`, a closed group or a
		// held restart), the caller's workspace becomes it before recovery,
		// so recovered Riders open beside the Lead this command starts.
		if workspaceID := os.Getenv("HERDR_WORKSPACE_ID"); workspaceID != "" {
			current, err := s.Herdr.Snapshot(ctx.Context)
			if err != nil {
				return herdrError(err)
			}
			if _, found := leadWorkspace(current, project); !found {
				if err := db.SetProjectWorkspace(ctx.Context, project.ID, workspaceID); err != nil {
					return err
				}
				if _, err := s.herdrCall(ctx.Context, "workspace.rename", map[string]any{"workspace_id": workspaceID, "label": leadWorkspaceLabel(project)}); err != nil {
					return err
				}
				if project, err = db.ProjectByID(ctx.Context, project.ID); err != nil {
					return err
				}
			}
		}
		runtimeResult, err := s.reconcileProject(ctx.Context, db, project, cfg, true)
		if err != nil {
			return herdrError(err)
		}
		snapshot = runtimeResult.Snapshot
		if err := s.reconcileTaskPanes(ctx.Context, db, project, snapshot); err != nil {
			return err
		}
		if err := s.reconcileIntents(ctx.Context, db, project, cfg, snapshot); err != nil {
			return err
		}
		project, err = db.ProjectByID(ctx.Context, project.ID)
		if err != nil {
			return err
		}
		if err := s.deliverQueuedMessages(ctx.Context, db, project, snapshot); err != nil {
			return herdrError(err)
		}
		if err := s.regenerateProjects(ctx.Context, db); err != nil {
			return err
		}
		if err := s.deliverNotices(ctx.Context, db, project); err != nil {
			return herdrError(err)
		}
	} else {
		snapshot, err = s.Herdr.Snapshot(ctx.Context)
		if err != nil {
			return herdrError(err)
		}
	}
	if err := s.pollProjectPullRequests(ctx.Context, db, project, cfg, false); err != nil {
		return err
	}
	if _, err := s.syncProjectRoot(ctx.Context, db, project, cfg, false); err != nil {
		return err
	}
	if err := s.autoTeardownLandedTasks(ctx.Context, db, project, cfg); err != nil {
		return err
	}
	callerPaneID := os.Getenv("HERDR_PANE_ID")
	callerWorkspaceID := os.Getenv("HERDR_WORKSPACE_ID")
	if callerPaneID == "" || callerWorkspaceID == "" {
		return axi.Failure("not_in_herdr", "Herdr did not provide the caller pane and workspace ids", false, "Run `posse up` from a Herdr-managed shell pane")
	}
	var callerPane *herdr.Pane
	for _, pane := range snapshot.Panes {
		if pane.PaneID == callerPaneID && pane.WorkspaceID == callerWorkspaceID {
			current := pane
			callerPane = &current
			break
		}
	}
	if callerPane == nil {
		return axi.Failure("not_in_herdr", "the caller pane is not present in the Herdr session snapshot", false, "Retry `posse up` from an active Herdr shell pane")
	}
	label := "posse:" + name + ":lead"
	leadAgentPanes := make(map[string]herdr.Agent)
	leadAgentPrefix := "posse-" + name + "-lead-"
	for _, agent := range snapshot.Agents {
		if strings.HasPrefix(agent.Name, leadAgentPrefix) {
			leadAgentPanes[agent.PaneID] = agent
		}
	}
	candidates := make([]herdr.Pane, 0)
	for _, pane := range snapshot.Panes {
		if _, named := leadAgentPanes[pane.PaneID]; pane.Label == label || named {
			candidates = append(candidates, pane)
		}
	}
	var liveLead *herdr.Pane
	for index := range candidates {
		agent, named := leadAgentPanes[candidates[index].PaneID]
		if candidates[index].Agent != "" || named {
			if liveLead == nil || candidates[index].PaneID == project.LeadPaneID {
				pane := candidates[index]
				if pane.Agent == "" {
					pane.Agent = agent.Kind
					if pane.Agent == "" {
						pane.Agent = agent.Agent
					}
				}
				liveLead = &pane
			}
		}
	}
	if liveLead != nil && !parsed.Replace {
		return axi.Failure("lead_running", fmt.Sprintf("Project %s already has a %s Lead running in pane %s", project.Name, liveLead.Agent, liveLead.PaneID), false, "Pass `--replace` to stop and replace it")
	}
	replacedLeadPaneID := ""
	if parsed.Replace && liveLead != nil {
		replacedLeadPaneID = liveLead.PaneID
		if err := s.stopLeadAgent(ctx.Context, liveLead.PaneID, liveLead.Agent); err != nil {
			return err
		}
		if _, err := s.herdrCall(ctx.Context, "pane.rename", map[string]any{"pane_id": liveLead.PaneID, "label": ""}); err != nil {
			return err
		}
		if _, err := s.herdrCall(ctx.Context, "pane.report_metadata", map[string]any{"pane_id": liveLead.PaneID, "source": "posse", "clear_title": true, "clear_display_agent": true}); err != nil {
			return err
		}
	}
	for _, pane := range snapshot.Panes {
		if pane.Label == label && pane.PaneID != callerPane.PaneID && pane.PaneID != replacedLeadPaneID {
			if _, err := s.herdrCall(ctx.Context, "pane.rename", map[string]any{"pane_id": pane.PaneID, "label": ""}); err != nil {
				return err
			}
		}
	}
	leadPane := callerPane
	if _, err := s.herdrCall(ctx.Context, "pane.rename", map[string]any{"pane_id": leadPane.PaneID, "label": label}); err != nil {
		return err
	}
	if _, err := s.herdrCall(ctx.Context, "workspace.rename", map[string]any{"workspace_id": callerWorkspaceID, "label": leadWorkspaceLabel(project)}); err != nil {
		return err
	}
	if _, err := s.herdrCall(ctx.Context, "pane.report_metadata", leadDisplayMetadata(project, leadPane.PaneID, "", kind)); err != nil {
		return err
	}
	gaps, err := s.readiness(ctx.Context, db, project, cfg)
	if err != nil {
		return err
	}
	launch, err := s.prepareLeadLaunch(home, project, cfg, kind, gaps)
	if err != nil {
		return err
	}
	leadLaunch, err := db.NextLeadLaunch(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	agentName := leadAgentName(name, leadLaunch)
	if err := db.SetProjectLead(ctx.Context, project.ID, callerWorkspaceID, leadPane.PaneID, label); err != nil {
		return err
	}
	project.HerdrWorkspaceID = callerWorkspaceID
	s.relabelProjectTabs(ctx.Context, db, project)
	if err := s.ensureLookoutTab(ctx.Context, db, project, snapshot, false); err != nil {
		return err
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return err
	}
	binary, err := directAgentCommand(kind)
	if err != nil {
		return axi.Failure("agent_kind_unknown", "could not start Lead agent "+kind, false, err.Error())
	}
	s.pendingLead = &leadExecPlan{
		ProjectID: project.ID, PaneID: leadPane.PaneID, WorkspaceID: callerWorkspaceID,
		AgentName: agentName, Kind: kind, Binary: binary, Args: launch.Args, Env: launch.Env,
		NeedsPrompt: launch.TypedPrompt != "",
	}
	result := axi.Object{
		{Key: "project", Value: project.Name},
		{Key: "lead", Value: "starting"},
		{Key: "kind", Value: kind},
		{Key: "kind_source", Value: kindSource},
		{Key: "pane", Value: leadPane.PaneID},
		{Key: "help", Value: []any{"The Lead is starting in this pane."}},
	}
	return ctx.Print(withReadiness(result, gaps))
}

func agentName(project string, sequence, launch int) string {
	return namedAgent("posse", project, "-t"+fmt.Sprint(sequence)+"-"+fmt.Sprint(launch))
}

func leadAgentName(project string, launch int) string {
	return namedAgent("posse", project, "-lead-"+fmt.Sprint(launch))
}

func namedAgent(prefix, project, suffix string) string {
	sanitize := func(value string) string {
		value = strings.ToLower(value)
		value = safeName.ReplaceAllString(value, "-")
		value = strings.Trim(value, "-_ ")
		return value
	}
	if prefix == "" {
		prefix = "posse"
	}
	base := sanitize(prefix) + "-" + sanitize(project)
	if len(base)+len(suffix) > 32 {
		base = strings.TrimRight(base[:max(1, 32-len(suffix))], "-_")
	}
	name := base + suffix
	if len(name) > 32 {
		name = name[:32]
	}
	if name[0] < 'a' || name[0] > 'z' {
		name = "w" + name[:31]
	}
	return name
}
