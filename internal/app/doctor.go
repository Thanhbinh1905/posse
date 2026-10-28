package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/herdr"
	setupassets "github.com/thanhbinh1905/posse/internal/setup"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) doctor(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("doctor", args, map[string]flagSpec{"full": {boolean: true}, "verbose": {boolean: true}})
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
	if parsed.Bool("full") && len(result.Manifests) > 0 {
		output = append(output, axi.Field{Key: "agent_manifests", Value: jsonRaw(result.Manifests)})
	}
	update, _ := s.availableUpdate(ctx.Context, nil, nil)
	output = addUpdateHelp(output, update)
	for _, field := range output {
		if field.Key == "help" {
			if expanded, ok := field.Value.([]axi.Object); ok {
				help = expanded
			}
		}
	}
	if ctx.Human {
		return renderDoctorHuman(ctx, checks, help, parsed.Bool("verbose"))
	}
	return ctx.Print(output)
}

type doctorCheck struct {
	Name, Status, Detail, Action string
	GapCode, Key, Member         string
	UserOnly                     bool
}

type doctorResult struct {
	Checks    []doctorCheck
	Manifests json.RawMessage
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
	ownedBytes, usageErr := pathBytes(home)
	cfg, configErr := config.Load(home, "")
	if usageErr != nil {
		addCheck("Posse-owned disk usage", "warn", usageErr.Error(), "Check permissions under POSSE_HOME")
	} else {
		retention := "retention.task_artifacts unavailable"
		if configErr == nil {
			retention = "retention.task_artifacts=" + cfg.Retention.TaskArtifacts
		}
		addCheck("Posse-owned disk usage", "ok", fmt.Sprintf("%d bytes under %s (scratch, Pool, Project data and state); %s", ownedBytes, home, retention), "")
	}
	cacheStatus, cacheDetail, cacheAction := sharedGoCacheDiagnostic(ctx.Context)
	addCheck("Shared Go build cache", cacheStatus, cacheDetail, cacheAction)
	workerCachePolicy := defaultWorkerGoCachePolicy()
	if configErr == nil {
		if policy, err := workerGoCachePolicyFromConfig(cfg.WorkerGoCache); err == nil {
			workerCachePolicy = policy
		}
	}
	workerCacheStatus, workerCacheDetail, workerCacheAction := workerGoBuildCacheDiagnostic(home, workerCachePolicy)
	addCheck("Posse Worker Go build cache", workerCacheStatus, workerCacheDetail, workerCacheAction)
	if path, pathErr := herdrConfigPath(); pathErr != nil {
		addCheck("Herdr Agents sidebar layout", "warn", pathErr.Error(), "Inspect Herdr config path")
	} else if state, stateErr := sidebarLayoutState(path); stateErr != nil {
		addCheck("Herdr Agents sidebar layout", "warn", stateErr.Error(), "Repair the Herdr config and run `posse setup`")
	} else if state == "keep" {
		addCheck("Herdr Agents sidebar layout", "ok", "Posse layout present", "")
	} else if state == "symlink" {
		detail := "Herdr config is a symlink (managed elsewhere, e.g. Nix home-manager); add this snippet to its source:\n\n" + sidebarLayoutSnippet
		addCheck("Herdr Agents sidebar layout", "warn", detail, "Add the snippet to the symlink target's source")
	} else if state == "upgrade" {
		addCheck("Herdr Agents sidebar layout", "warn", "Posse layout lacks dim = false, so Worker rows render dimmed", "Run `posse setup` to upgrade the layout")
	} else if state == "manual" {
		addCheck("Herdr Agents sidebar layout", "warn", "custom Agents rows present", "Review the snippet from `posse setup --check` before editing Herdr config")
	} else {
		addCheck("Herdr Agents sidebar layout", "warn", "not installed", "Run `posse setup` to add the optional layout")
	}
	if configErr != nil {
		addCheck("config", "fail", configErr.Error(), configCheckAction(configErr))
	} else {
		detail := fmt.Sprintf("%d profiles; implicit dispatch=%s", len(cfg.Profiles), cfg.Defaults.DispatchInheritance)
		var disabledKinds []string
		for kind, settings := range cfg.Kinds {
			if settings.Disabled {
				disabledKinds = append(disabledKinds, kind)
			}
		}
		sort.Strings(disabledKinds)
		if len(disabledKinds) > 0 {
			detail += "; disabled dispatch kinds=" + strings.Join(disabledKinds, ",")
		}
		addCheck("config", "ok", detail, "")
		addCheck(recommendedSkillsCheck(home))
	}
	if dirs, err := setupDirectories(); err != nil {
		addCheck("global skill shadowing", "warn", err.Error(), "Check HOME")
	} else {
		addCheck(globalSkillShadowCheck(dirs))
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
			for _, project := range projects {
				if project.IsDown() {
					continue
				}
				check, checkErr := s.watchRuntimeHealth(ctx.Context, db, home, project)
				if checkErr != nil {
					addCheck("Project Watch "+project.Name, "warn", "runtime state unavailable: "+checkErr.Error(), "Inspect the Posse database, then rerun `posse doctor`")
				} else {
					checks = append(checks, check)
				}
			}
			hosts := map[string]bool{}
			for _, project := range projects {
				projectCfg, projectConfigErr := config.Load(home, project.Name)
				if detail, diskErr := projectDiskUsage(home, project); diskErr != nil {
					addCheck("Project disk usage "+project.Name, "warn", diskErr.Error(), "Inspect this Project's scratch, Pool and artifact directories")
				} else {
					addCheck("Project disk usage "+project.Name, "ok", detail, "")
				}
				tasks, tasksErr := db.Tasks(ctx.Context, project.ID, true)
				if tasksErr != nil {
					addCheck("Task temp and process leaks "+project.Name, "warn", tasksErr.Error(), "Inspect the Project Tasks and retry `posse doctor`")
				} else {
					status, detail := taskDiskLeakSummary(home, project, tasks)
					action := ""
					if status != "ok" {
						action = "Review `posse clean`; use `posse clean --yes` to reclaim eligible finished Tasks"
					}
					addCheck("Task temp and process leaks "+project.Name, status, detail, action)
				}
				var migration *config.MigrationError
				migrationRequired := configErr == nil && errors.As(projectConfigErr, &migration)
				if migrationRequired {
					addCheck("config "+project.Name, "fail", projectConfigErr.Error(), configCheckAction(projectConfigErr))
				}
				if reason := staleProjectRootReason(project.Root, home); reason != "" {
					addCheck("project "+project.Name, "warn", reason, "Run `posse project remove "+project.Name+"`")
					continue
				}
				if projectConfigErr != nil {
					if !migrationRequired {
						addCheck("forge "+project.Name, "warn", projectConfigErr.Error(), "Fix the Project config")
					}
					continue
				}
				if detail, countErr := executionResourceSummary(ctx.Context, db, project.ID, projectCfg.Defaults.MaxWorkers); countErr != nil {
					addCheck("execution resources "+project.Name, "warn", countErr.Error(), "Inspect the Project Tasks and Pool")
				} else {
					addCheck("execution resources "+project.Name, "ok", detail, "")
				}
				status, detail, action := s.reviewDispatchCheck(ctx.Context, project, projectCfg)
				addCheck("review dispatch "+project.Name, status, detail, action)
				forge, _, err := resolveForgeForRepositoryFresh(ctx.Context, project.Root, projectCfg, "")
				if err != nil {
					action := "Set defaults.forge for this Project"
					if strings.Contains(err.Error(), "forge CLI timed out") {
						action = "Check forge CLI connectivity and authentication for this host"
					}
					addCheck("forge "+project.Name, "warn", err.Error(), action)
					continue
				}
				if forge.Kind == "gitlab" {
					hosts[forge.Host] = true
				}
			}
			if len(hosts) > 0 {
				if _, err := exec.LookPath("glab"); err != nil {
					addCheck("glab", "warn", "not installed", "Install glab to use GitLab MR merging")
				} else {
					ordered := make([]string, 0, len(hosts))
					for host := range hosts {
						ordered = append(ordered, host)
					}
					sort.Strings(ordered)
					for _, host := range ordered {
						output, err := commandOutputArgsWithTimeout(ctx.Context, forgeProbeTimeout, "", "glab", "auth", "status", "--hostname", host)
						if err != nil {
							detail := strings.TrimSpace(output)
							action := "Run `glab auth login --hostname " + host + "`"
							if strings.Contains(err.Error(), "timed out") {
								detail = err.Error()
								action = "Check forge CLI connectivity and authentication for this host"
							}
							addCheck("glab auth "+host, "warn", detail, action)
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
	checks = append(checks, preferenceSizeWarnings(home, registeredProjects)...)
	checks = append(checks, knowledgeWarnings(home, registeredProjects)...)
	// A removed config value is already reported by the config checks above.
	var kindMigration *config.MigrationError
	if kindConfigErr != nil && !errors.As(kindConfigErr, &kindMigration) {
		addCheck("agent configuration", "warn", kindConfigErr.Error(), "Fix the agent settings in config.toml")
	}

	var manifests []byte
	if s.Herdr == nil {
		addCheck("Herdr", "warn", "adapter is not configured", "Configure Herdr and run `posse setup`")
		for _, project := range registeredProjects {
			if !project.IsDown() {
				addCheck("Project Watch "+project.Name, "warn", "watcher state unavailable: Herdr adapter is not configured", "Configure Herdr, then rerun `posse doctor`")
			}
		}
	} else {
		herdrStatus, statusErr := s.Herdr.Status(ctx.Context)
		if statusErr != nil {
			addCheck("Herdr", "warn", statusErr.Error(), "Start or repair the Herdr server")
		} else if herdrStatus.Running && herdrStatus.Compatible && herdrStatus.Protocol >= herdr.MinimumProtocol {
			addCheck("Herdr", "ok", fmt.Sprintf("protocol %d", herdrStatus.Protocol), "")
		} else {
			addCheck("Herdr", "warn", "server is stopped or incompatible", "Start a compatible Herdr server")
		}

		snapshot, snapErr := s.snapshot(ctx.Context)
		if snapErr != nil {
			for _, project := range registeredProjects {
				if !project.IsDown() {
					addCheck("Project Watch "+project.Name, "warn", "watcher state unavailable: "+snapErr.Error(), "Restore Herdr, then rerun `posse doctor`")
				}
			}
		} else {
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
						addCheck("Herdr group "+project.Name, "warn", "another primary workspace "+workspace.WorkspaceID+" shares the Lead's repository; its group close also closes the Lead and Workers", "Do not use workspace close --group on another primary; Posse recovers the group if it happens")
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
		globalSkills := []string{}
		for _, skill := range manifest.Skills {
			markerPath := filepath.Join(skill.AgentsDir, skill.Name, ".posse-version")
			marker, readErr := os.ReadFile(markerPath)
			if readErr != nil || strings.TrimSpace(string(marker)) != s.binaryVersion() {
				staleSkills = append(staleSkills, skill.Name)
			}
			if skillRecordHasUnmodifiedOwnedParts(skill) {
				globalSkills = append(globalSkills, skill.Name)
			}
		}
		if len(staleSkills) == 0 {
			addCheck("skills", "ok", "current", "")
		} else {
			addCheck("skills", "warn", "out of date: "+strings.Join(staleSkills, ", "), "Run `posse setup --global-skills` to refresh the optional global copies")
		}
		if len(globalSkills) == 0 {
			addCheck("global skills", "ok", "no unchanged Posse-owned global copies", "")
		} else {
			addCheck("global skills", "warn", "Posse-owned global copies: "+strings.Join(globalSkills, ", "), "Run `posse setup --remove-global-skills` to remove unchanged Posse-owned files")
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
			addCheck("agent hooks", "warn", strings.Join(staleHooks, ", "), "Run `posse setup` to refresh the SessionStart and Worker guard hooks")
		}
	}
	for _, tool := range []string{"gh"} {
		if path, lookErr := exec.LookPath(tool); lookErr == nil {
			addCheck(tool, "ok", path, "")
		} else {
			addCheck(tool, "warn", "not installed", "Install "+tool+" to use the corresponding Merge Mode")
		}
	}
	return doctorResult{Checks: checks, Manifests: manifests}, nil
}

// configCheckAction names the replacement a removed config value requires.
func configCheckAction(err error) string {
	var invalid *config.InvalidError
	var migration *config.MigrationError
	if !errors.As(err, &invalid) || !errors.As(err, &migration) {
		return "Fix the reported configuration key"
	}
	return strings.Join(configRepairHelp(invalid), "; ")
}

func executionResourceSummary(ctx context.Context, db *store.DB, projectID int64, maxWorkers int) (string, error) {
	activeWorkers, err := db.ActiveWorkerCount(ctx, projectID)
	if err != nil {
		return "", err
	}
	tasks, err := db.Tasks(ctx, projectID, true)
	if err != nil {
		return "", err
	}
	activeTaskIDs := make(map[int64]bool, len(tasks))
	for _, task := range tasks {
		switch task.State {
		case store.StateSpawning, store.StateWorking, store.StateNeedsDecision, store.StateBlocked, store.StateStalled:
			activeTaskIDs[task.ID] = true
		case store.StateDone, store.StateMerging:
			if task.Type == "change" {
				activeTaskIDs[task.ID] = true
			}
		}
	}
	sandboxes, err := db.Sandboxes(ctx, projectID)
	if err != nil {
		return "", err
	}
	parkedResources := 0
	for _, sandbox := range sandboxes {
		if sandbox.State == "held" && !activeTaskIDs[sandbox.TaskID] {
			parkedResources++
		}
	}
	children, err := db.ResearchAttempts(ctx, store.ResearchAttemptFilter{ProjectID: projectID, Active: true})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("active_workers=%d/%d; native_children=%d tracked (read-only research; native write execution unsupported); parked_resources=%d", activeWorkers, maxWorkers, len(children), parkedResources), nil
}

func stringListContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func recommendedSkillsCheck(home string) (name, status, detail, action string) {
	name = "workflow skills"
	row, offered := recommendedSkillsOfferRow(home, nil)
	if !offered {
		return name, "ok", "every recommended skill is in config", ""
	}
	if row["action"] == "offer" {
		return name, "warn", "no [skills] block in config, so Leads and Workers load only the platform skills", "Run `posse skills add` to pick recommended skills"
	}
	status, _ = row["status"].(string)
	return name, "ok", status, ""
}

// reviewDispatchCheck reports whether the Project's Dispatch Rules resolve a
// usable reviewer Profile, directly or through each `--profile` candidate.
func (s *Service) reviewDispatchCheck(ctx context.Context, project store.Project, cfg config.Config) (status, detail, action string) {
	const fix = "Fix the review Dispatch Rules or dispatch.default.use, then rerun `posse doctor`"
	resolution, err := s.resolveDispatch(ctx, project, cfg, "review", "")
	if err == nil {
		return "ok", "review Tasks use " + resolution.Profile + " (" + resolution.Rule + ")", ""
	}
	var profileErr *dispatch.ProfileError
	if !errors.As(err, &profileErr) || profileErr.Code != "profile_required" || len(profileErr.Candidates) == 0 {
		return "warn", "review Tasks cannot resolve a Profile: " + err.Error(), fix
	}
	var candidates, unusable []string
	for _, candidate := range profileErr.Candidates {
		candidates = append(candidates, candidate.Profile)
		if _, err := s.resolveDispatch(ctx, project, cfg, "review", candidate.Profile); err != nil {
			unusable = append(unusable, err.Error())
		}
	}
	if len(unusable) > 0 {
		return "warn", "review Dispatch Rule candidates cannot start: " + strings.Join(unusable, "; "), fix
	}
	return "ok", "review start chooses among " + strings.Join(candidates, ", "), ""
}

// globalSkillShadowCheck finds bundled skills installed in the global agent
// skill directories, which load beside every role snapshot and can shadow it.
func globalSkillShadowCheck(dirs setupDirs) (name, status, detail, action string) {
	name = "global skill shadowing"
	var found []string
	for _, root := range []string{dirs.agents, filepath.Join(dirs.claude, "skills")} {
		for _, name := range setupassets.SkillNames() {
			path := filepath.Join(root, name)
			if _, err := os.Stat(filepath.Join(path, "SKILL.md")); err == nil {
				found = append(found, path)
			}
		}
	}
	if len(found) == 0 {
		return name, "ok", "no global posse-* skills shadow the role snapshots", ""
	}
	return name, "warn", "global posse-* skills load beside every role snapshot and can shadow it: " + strings.Join(found, ", "), "Run `posse setup --remove-global-skills`, then delete the remaining copies; list skills a role needs under [skills]"
}
