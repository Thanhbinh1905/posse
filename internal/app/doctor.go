package app

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) doctor(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("doctor", args, map[string]flagSpec{"full": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("doctor does not take positional arguments")
	}
	result, err := s.collectDoctorChecks(ctx)
	if err != nil {
		return err
	}
	checks, help := doctorRows(result.Checks)
	output := axi.Object{{Key: "checks", Value: checks}, {Key: "help", Value: help}}
	output = withReadiness(output, result.Readiness)
	if parsed.Bool("full") && len(result.Manifests) > 0 {
		output = append(output, axi.Field{Key: "agent_manifests", Value: jsonRaw(result.Manifests)})
	}
	update, _ := s.availableUpdate(ctx.Context, nil, nil)
	return ctx.Print(addUpdateHelp(output, update))
}

type doctorCheck struct {
	Name, Status, Detail, Action string
	GapCode, Key, Member         string
	UserOnly                     bool
}

type doctorResult struct {
	Checks    []doctorCheck
	Manifests json.RawMessage
	Readiness []readinessGap
}

func doctorRows(checks []doctorCheck) ([]axi.Object, []axi.Object) {
	rows, help := []axi.Object{}, []axi.Object{}
	for _, check := range checks {
		rows = append(rows, axi.Object{{Key: "check", Value: check.Name}, {Key: "status", Value: check.Status}, {Key: "detail", Value: check.Detail}})
		if check.Status != "ok" {
			help = append(help, axi.Object{{Key: "check", Value: check.Name}, {Key: "action", Value: check.Action}})
		}
	}
	return rows, help
}

// Collect diagnostics without printing or applying setup. The scoped check
// collectors below are also used by status and the Lead's startup context.
func (s *Service) collectDoctorChecks(ctx *axi.Context) (doctorResult, error) {
	home, err := s.homePath()
	if err != nil {
		return doctorResult{}, err
	}
	checks := []doctorCheck{}
	var registeredProjects []store.Project
	addCheck := func(name, status, detail, action string) {
		checks = append(checks, doctorCheck{Name: name, Status: status, Detail: detail, Action: action})
	}

	cfg, configErr := config.Load(home, "")
	if configErr != nil {
		addCheck("config", "fail", configErr.Error(), "Fix the reported configuration key")
	} else {
		addCheck("config", "ok", fmt.Sprintf("%d profiles", len(cfg.Profiles)), "")
	}
	db, _, dbErr := s.openDB()
	if dbErr != nil {
		addCheck("database", "fail", dbErr.Error(), "Inspect the Posse database and its permissions")
	} else {
		projects, projectsErr := db.Projects(ctx.Context)
		if projectsErr != nil {
			addCheck("database", "fail", projectsErr.Error(), "Inspect the Posse database")
		} else {
			registeredProjects = projects
			addCheck("database", "ok", "database opened", "")
			hosts := map[string]bool{}
			for _, project := range projects {
				if reason := staleProjectRootReason(project.Root, home); reason != "" {
					addCheck("project "+project.Name, "warn", reason, "Run `posse project remove "+project.Name+"`")
					continue
				}
				projectCfg, err := config.Load(home, project.Name)
				if err != nil {
					addCheck("forge "+project.Name, "warn", err.Error(), "Fix the Project config")
					continue
				}
				forge, err := forgeForRepository(ctx.Context, project.Root, projectCfg, "")
				if err != nil {
					addCheck("forge "+project.Name, "warn", err.Error(), "Set defaults.forge for this Project")
					continue
				}
				if forge.Kind == "gitlab" {
					hosts[forge.Host] = true
				}
			}
			if len(hosts) > 0 {
				if _, err := exec.LookPath("glab"); err != nil {
					addCheck("glab", "warn", "not installed", "Install glab to use GitLab MR landing")
				} else {
					ordered := make([]string, 0, len(hosts))
					for host := range hosts {
						ordered = append(ordered, host)
					}
					sort.Strings(ordered)
					for _, host := range ordered {
						output, err := commandOutputArgs(ctx.Context, "", "glab", "auth", "status", "--hostname", host)
						if err != nil {
							addCheck("glab auth "+host, "warn", strings.TrimSpace(output), "Run `glab auth login --hostname "+host+"`")
						} else {
							addCheck("glab auth "+host, "ok", "authenticated", "")
						}
					}
				}
			}
		}
		if err := db.Close(); err != nil {
			addCheck("database close", "fail", err.Error(), "Check the database permissions")
		}
	}
	referencedKinds, kindConfigErr := configuredAgentKinds(home)
	if claude, lookupErr := exec.LookPath("claude"); lookupErr == nil {
		version, versionErr := commandOutputArgs(ctx.Context, "", claude, "--version")
		if versionErr == nil {
			version = strings.TrimSpace(version)
			action := "Verify with `" + claudeLowkeyVerificationCommand + "`"
			switch claudeLowkeyVersionStatus(version) {
			case "ok":
				addCheck("Claude Code lowkey", "ok", version+" function hooks verified", "")
			case "info":
				addCheck("Claude Code lowkey", "info", version+" not yet verified; the mod probes its hooks and falls back to stock rendering", action)
			default:
				addCheck("Claude Code lowkey", "warn", version+" function hooks unverified", action)
			}
		} else {
			addCheck("Claude Code lowkey", "warn", versionErr.Error(), "Check the Claude Code installation")
		}
	}
	if kindConfigErr != nil {
		addCheck("agent configuration", "warn", kindConfigErr.Error(), "Fix the agent settings in config.toml")
	}

	var manifests []byte
	if s.Herdr == nil {
		addCheck("Herdr", "warn", "adapter is not configured", "Configure Herdr and run `posse setup`")
	} else {
		herdrStatus, statusErr := s.Herdr.Status(ctx.Context)
		if statusErr != nil {
			addCheck("Herdr", "warn", statusErr.Error(), "Start or repair the Herdr server")
		} else if herdrStatus.Running && herdrStatus.Compatible && herdrStatus.Protocol >= herdr.MinimumProtocol {
			addCheck("Herdr", "ok", fmt.Sprintf("protocol %d", herdrStatus.Protocol), "")
		} else {
			addCheck("Herdr", "warn", "server is stopped or incompatible", "Start a compatible Herdr server")
		}

		if snapshot, snapErr := s.snapshot(ctx.Context); snapErr == nil {
			for _, project := range registeredProjects {
				if project.IsWorkspace() {
					continue
				}
				leadID, found := leadWorkspace(snapshot, project)
				if !found {
					continue
				}
				key := ""
				for _, workspace := range snapshot.Workspaces {
					if workspace.WorkspaceID == leadID {
						key = workspace.Worktree.RepoKey
					}
				}
				if key == "" {
					continue
				}
				for _, workspace := range snapshot.Workspaces {
					if workspace.WorkspaceID != leadID && workspace.Worktree.RepoKey == key && !workspace.Worktree.IsLinkedWorktree {
						addCheck("Herdr group "+project.Name, "warn", "another primary workspace "+workspace.WorkspaceID+" shares the Lead's repository; its group close also closes the Lead and Riders", "Do not use workspace close --group on another primary; Posse recovers the group if it happens")
					}
				}
			}
		}
		pluginData, pluginErr := s.herdrCall(ctx.Context, "plugin.list", map[string]any{})
		if pluginErr != nil {
			addCheck("Posse Herdr plugin", "warn", pluginErr.Error(), "Run `posse setup` to inspect plugin setup")
		} else if pluginPath, found := pluginLocation(pluginData, possePluginID); found {
			addCheck("Posse Herdr plugin", "ok", pluginPath, "")
		} else {
			addCheck("Posse Herdr plugin", "warn", "not linked", "Run `posse setup` to link the plugin")
		}

		manifestData, manifestErr := s.herdrCall(ctx.Context, "server.agent_manifests", map[string]any{})
		if manifestErr != nil {
			addCheck("agent manifests", "warn", manifestErr.Error(), "Repair Herdr before inspecting agent integrations")
		} else {
			manifests = append([]byte(nil), manifestData...)
			detected := agentKinds(manifestData)
			detectedSet := make(map[string]bool, len(detected))
			for _, kind := range detected {
				detectedSet[kind] = true
			}
			integrations, integrationErr := integrationStatus(s.Herdr, ctx.Context)
			if integrationErr != nil {
				addCheck("agent integrations", "warn", integrationErr.Error(), "Run `posse setup` to inspect agent integrations")
			} else {
				for _, kind := range referencedKinds {
					if _, cliErr := exec.LookPath(agentCLIName(kind)); cliErr != nil {
						addCheck("agent CLI "+kind, "warn", agentCLIName(kind)+" is not on PATH", "Install the configured agent CLI or update config.toml")
						continue
					}
					if !detectedSet[kind] {
						addCheck("agent integration "+kind, "warn", "Herdr has no available agent manifest", "Install or repair the Herdr integration for "+kind)
						continue
					}
					integration := integrations[kind]
					if integrationNeedsRepair(integration) {
						addCheck("agent integration "+kind, "warn", integration, "Run `posse setup` to repair this integration")
					} else if !integrationPresent(integration) {
						addCheck("agent integration "+kind, "warn", defaultValue(integration, "not installed"), "Run `posse setup` to install this integration")
					} else {
						addCheck("agent integration "+kind, "ok", integration, "")
					}
				}
				available := []string{}
				for _, kind := range detected {
					if !stringListContains(referencedKinds, kind) {
						available = append(available, kind)
					}
				}
				if len(available) > 0 {
					addCheck("other agent kinds", "ok", "available: "+strings.Join(available, ", "), "")
				}
			}
		}

		logs, logErr := s.Herdr.Run(ctx.Context, "plugin", "log", "list", "--plugin", possePluginID)
		if logErr != nil {
			addCheck("plugin hook logs", "warn", logErr.Error(), "Inspect Herdr plugin logs")
		} else if jsonHasFailure(logs) {
			addCheck("plugin hook logs", "warn", "Herdr reports hook failures", "Inspect logs with `herdr plugin log list --plugin posse.herdr`")
		} else {
			addCheck("plugin hook logs", "ok", "no hook failures reported", "")
		}
	}

	manifest, manifestFound, manifestErr := readSetupManifest(filepath.Join(home, setupManifestName))
	if manifestErr != nil {
		addCheck("setup manifest", "fail", manifestErr.Error(), "Inspect or restore the setup manifest")
	} else if !manifestFound {
		addCheck("setup manifest", "warn", "not installed", "Run `posse setup`")
	} else {
		addCheck("setup manifest", "ok", "installed", "")
		staleSkills := []string{}
		for _, skill := range manifest.Skills {
			markerPath := filepath.Join(skill.AgentsDir, skill.Name, ".posse-version")
			marker, readErr := os.ReadFile(markerPath)
			if readErr != nil || strings.TrimSpace(string(marker)) != s.binaryVersion() {
				staleSkills = append(staleSkills, skill.Name)
			}
		}
		if len(staleSkills) == 0 {
			addCheck("skills", "ok", "current", "")
		} else {
			addCheck("skills", "warn", "out of date: "+strings.Join(staleSkills, ", "), "Run `posse setup` to refresh installed skills")
		}
		staleHooks := []string{}
		for _, hook := range manifest.Hooks {
			if hook.Version != s.binaryVersion() || !hookCommandExists(hook.Path, hook.spec(), hook.Command) {
				staleHooks = append(staleHooks, hook.spec().Event+":"+hook.Path)
			}
		}
		if len(staleHooks) == 0 {
			addCheck("agent hooks", "ok", "current", "")
		} else {
			addCheck("agent hooks", "warn", strings.Join(staleHooks, ", "), "Run `posse setup` to refresh the SessionStart and Rider guard hooks")
		}
	}
	for _, tool := range []string{"gh", "no-mistakes"} {
		if path, lookErr := exec.LookPath(tool); lookErr == nil {
			addCheck(tool, "ok", path, "")
		} else {
			addCheck(tool, "warn", "not installed", "Install "+tool+" to use the corresponding Landing Mode")
		}
	}
	scoped, gaps, err := s.doctorReadiness(ctx.Context, home)
	if err != nil {
		addCheck("readiness", "warn", err.Error(), "Fix the reported Project configuration")
	}
	checks = append(checks, scoped...)
	return doctorResult{Checks: checks, Manifests: manifests, Readiness: gaps}, nil
}

func stringListContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
