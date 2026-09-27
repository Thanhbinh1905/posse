package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
)

func (s *Service) doctor(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("doctor", args, map[string]flagSpec{"full": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("doctor does not take positional arguments")
	}
	home, err := s.homePath()
	if err != nil {
		return err
	}
	checks := []axi.Object{}
	help := []axi.Object{}
	addCheck := func(name, status, detail, action string) {
		checks = append(checks, axi.Object{{Key: "check", Value: name}, {Key: "status", Value: status}, {Key: "detail", Value: detail}})
		if status != "ok" {
			help = append(help, axi.Object{{Key: "check", Value: name}, {Key: "action", Value: action}})
		}
	}

	if path, pathErr := herdrConfigPath(); pathErr != nil {
		addCheck("Herdr Agents sidebar layout", "warn", pathErr.Error(), "Inspect Herdr config path")
	} else if state, stateErr := sidebarLayoutState(path); stateErr != nil {
		addCheck("Herdr Agents sidebar layout", "warn", stateErr.Error(), "Repair the Herdr config and run `posse setup`")
	} else if state == "keep" {
		addCheck("Herdr Agents sidebar layout", "ok", "Posse layout present", "")
	} else if state == "symlink" {
		detail := "Herdr config is a symlink (managed elsewhere, e.g. Nix home-manager); add this snippet to its source:\n\n" + sidebarLayoutSnippet
		addCheck("Herdr Agents sidebar layout", "warn", detail, "Add the snippet to the symlink target's source")
	} else if state == "manual" {
		addCheck("Herdr Agents sidebar layout", "warn", "custom Agents rows present", "Review the snippet from `posse setup --check` before editing Herdr config")
	} else {
		addCheck("Herdr Agents sidebar layout", "warn", "not installed", "Run `posse setup` to add the optional layout")
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
	update, _ := s.availableUpdate(ctx.Context, nil, nil)
	if parsed.Bool("full") && len(manifests) > 0 {
		return ctx.Print(addUpdateHelp(axi.Object{{Key: "checks", Value: checks}, {Key: "help", Value: help}, {Key: "agent_manifests", Value: jsonRaw(manifests)}}, update))
	}
	return ctx.Print(addUpdateHelp(axi.Object{{Key: "checks", Value: checks}, {Key: "help", Value: help}}, update))
}

func stringListContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
