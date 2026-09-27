package app

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/thanhbinh1905/posse/internal/atomicfile"
	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	setupassets "github.com/thanhbinh1905/posse/internal/setup"
	"github.com/thanhbinh1905/posse/internal/store"
)

const setupManifestName = "setup.json"
const possePluginID = "posse.herdr"

type setupManifest struct {
	Version            string             `json:"version"`
	Binary             string             `json:"binary"`
	PluginPath         string             `json:"plugin_path"`
	PluginHash         string             `json:"plugin_hash"`
	PluginFileMade     bool               `json:"plugin_file_made"`
	PluginLinked       bool               `json:"plugin_linked"`
	PreviousPluginPath string             `json:"previous_plugin_path,omitempty"`
	ConfigCreated      bool               `json:"config_created"`
	ConfigHash         string             `json:"config_hash,omitempty"`
	Integrations       []string           `json:"integrations,omitempty"`
	Skills             []setupSkillRecord `json:"skills,omitempty"`
	Hooks              []setupHookRecord  `json:"hooks,omitempty"`
	PiGuardPath        string             `json:"pi_guard_path,omitempty"`
	PiGuardHash        string             `json:"pi_guard_hash,omitempty"`
	PiGuardDirs        []string           `json:"pi_guard_dirs,omitempty"`
}

type setupSkillRecord struct {
	Name          string   `json:"name"`
	AgentsDir     string   `json:"agents_dir"`
	CreatedDirs   []string `json:"created_dirs,omitempty"`
	DirectoryMade bool     `json:"directory_made"`
	FileMade      bool     `json:"file_made"`
	FileHash      string   `json:"file_hash"`
	MarkerMade    bool     `json:"marker_made"`
	MarkerVersion string   `json:"marker_version"`
	LinkPath      string   `json:"link_path"`
	LinkTarget    string   `json:"link_target"`
	LinkMade      bool     `json:"link_made"`
}

type setupHookRecord struct {
	Event             string   `json:"event,omitempty"` // empty means SessionStart
	Path              string   `json:"path"`
	ConfiguredPath    string   `json:"configured_path,omitempty"`
	Command           string   `json:"command"`
	Version           string   `json:"version"`
	CreatedDirs       []string `json:"created_dirs,omitempty"`
	HookMade          bool     `json:"hook_made,omitempty"`
	FileMade          bool     `json:"file_made"`
	CreatedRoot       bool     `json:"created_root"`
	CreatedSessionKey bool     `json:"created_session_key"`
	CreatedGroup      bool     `json:"created_group"`
}

type setupInspection struct {
	Kinds            []string
	ReferencedKinds  []string
	AvailableKinds   []string
	Integrations     map[string]string
	PluginPath       string
	PluginPresent    bool
	PluginListJSON   []byte
	SidebarSupported bool
}

type setupRun struct {
	plan          []map[string]any
	prerequisites []axi.Object
	inspection    setupInspection
	changed       []any
	manifestPath  string
	applied       bool
}

func (s *Service) setup(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("setup", args, map[string]flagSpec{"check": {boolean: true}, "uninstall": {boolean: true}, "human": {boolean: true}, "exit-code": {boolean: true}, "sidebar-layout": {boolean: true}, "no-sidebar-layout": {boolean: true}, "binary": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 || parsed.Bool("check") && parsed.Bool("uninstall") {
		return axi.Usage("setup accepts either --check or --uninstall")
	}
	human := parsed.Bool("human")
	if human && (parsed.Bool("uninstall") || ctx.JSON) {
		return axi.Usage("--human cannot be combined with --uninstall or --json")
	}
	if parsed.Bool("exit-code") && !parsed.Bool("check") {
		return axi.Usage("--exit-code applies only to setup --check")
	}
	if parsed.Bool("sidebar-layout") && (parsed.Bool("check") || parsed.Bool("uninstall") || parsed.Bool("no-sidebar-layout")) || parsed.Bool("no-sidebar-layout") && (parsed.Bool("check") || parsed.Bool("uninstall")) {
		return axi.Usage("sidebar layout flags apply only when installing setup")
	}
	home, err := s.homePath()
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(home, setupManifestName)
	manifest, found, err := readSetupManifest(manifestPath)
	if err != nil {
		return axi.Failure("setup_manifest_invalid", err.Error(), false, "Inspect or restore "+manifestPath)
	}
	if parsed.Bool("uninstall") {
		return s.uninstallSetup(ctx, home, manifestPath, manifest, found)
	}
	run, err := s.runSetup(ctx.Context, home, manifestPath, manifest, found, parsed.Flags["binary"], !parsed.Bool("check"), human)
	if err == nil && run.applied && sidebarOffer(run.plan) && !parsed.Bool("no-sidebar-layout") {
		confirmed := parsed.Bool("sidebar-layout")
		if !confirmed && !parsed.Bool("human") && setupInputIsTerminal() {
			fmt.Fprint(ctx.Out, "Add Posse Agents sidebar layout to Herdr config? [y/N] ")
			answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			confirmed = strings.EqualFold(strings.TrimSpace(answer), "y")
		}
		if confirmed {
			path, pathErr := herdrConfigPath()
			if pathErr == nil {
				var changed bool
				changed, pathErr = installSidebarLayout(path)
				if changed && pathErr == nil {
					run.changed = append(run.changed, "sidebar_layout:"+path)
					for _, row := range run.plan {
						if row["step"] == "sidebar_layout" {
							row["action"] = "installed"
						}
					}
				}
			}
			err = pathErr
		}
	}
	if human {
		printHumanSetup(ctx.Out, run, err)
		if err != nil {
			return axi.Reported(err)
		}
	} else if err != nil {
		return err
	}
	plan := renderSetupPlan(run.plan)
	if !run.applied {
		if !human {
			if err := ctx.Print(axi.Object{{Key: "plan", Value: plan}, {Key: "prerequisites", Value: run.prerequisites}, {Key: "available_agent_kinds", Value: run.inspection.AvailableKinds}, {Key: "changed", Value: []any{}}, {Key: "help", Value: []any{"Codex will ask once to trust the new hook", "Run `posse setup` to apply this plan"}}}); err != nil {
				return err
			}
		}
		if parsed.Bool("exit-code") && setupPending(run.plan) {
			return &axi.Error{Code: "setup_pending", Message: "setup has changes to apply", ExitCode: setupPendingExitCode, Reported: true}
		}
		return nil
	}
	if human {
		return nil
	}
	return ctx.Print(axi.Object{{Key: "plan", Value: plan}, {Key: "prerequisites", Value: run.prerequisites}, {Key: "available_agent_kinds", Value: run.inspection.AvailableKinds}, {Key: "changed", Value: run.changed}, {Key: "manifest", Value: run.manifestPath}, {Key: "help", Value: []any{"Codex will ask once to trust the new hook", "Run `posse doctor` to verify the installed hooks and integrations"}}})
}

// runSetup inspects the machine and, when apply is set, applies the plan. A
// human preview also validates the plan so conflicts surface before the prompt.
func (s *Service) runSetup(ctx context.Context, home, manifestPath string, manifest setupManifest, found bool, binaryFlag string, apply, human bool) (setupRun, error) {
	run := setupRun{manifestPath: manifestPath}
	binary, err := resolveSetupBinary(binaryFlag)
	if err != nil {
		return run, err
	}
	if s.Herdr == nil {
		return run, axi.Failure("herdr_unavailable", "Herdr adapter is not configured", true)
	}
	run.inspection, err = s.inspectSetup(ctx, home)
	if err != nil {
		return run, err
	}
	version := s.binaryVersion()
	dirs, err := setupDirectories()
	if err != nil {
		return run, err
	}
	run.plan = s.setupPlan(home, binary, version, dirs, manifest, found, run.inspection)
	path, pathErr := herdrConfigPath()
	if pathErr != nil {
		return run, pathErr
	}
	state, stateErr := sidebarLayoutState(path)
	row := map[string]any{"step": "sidebar_layout", "target": path, "action": state}
	if stateErr != nil {
		row["action"] = "manual"
		row["error"] = stateErr.Error()
	}
	if state != "keep" {
		row["snippet"] = sidebarLayoutSnippet
	}
	if state == "offer" && !run.inspection.SidebarSupported {
		row["action"] = "manual"
		row["error"] = "Herdr 0.9.1 or newer is required for styled sidebar rules"
	}
	run.plan = append(run.plan, row)
	run.prerequisites = setupPrerequisites(run.inspection)
	if setupPrerequisiteMissing(run.prerequisites, "git") {
		return run, axi.Failure("setup_prerequisite_missing", "git is required for posse", false)
	}
	if setupPrerequisiteMissing(run.prerequisites, "herdr") {
		return run, axi.Failure("setup_prerequisite_missing", "herdr is required for posse", false)
	}
	if !apply && !human {
		return run, nil
	}
	if err := validateSetupPlan(run.plan); err != nil {
		return run, err
	}
	if !apply {
		return run, nil
	}
	if err := validateSetupMutationScope(); err != nil {
		return run, err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return run, err
	}
	run.changed, err = s.applySetup(ctx, home, manifestPath, manifest, binary, version, dirs, run.inspection)
	if err != nil {
		return run, err
	}
	// Migrate here, from the User's own run, so a Worker's first Signal after
	// an upgrade never meets a pending migration it may not apply.
	if _, statErr := os.Stat(filepath.Join(home, "posse.db")); statErr == nil {
		db, openErr := store.Open(home)
		if openErr != nil {
			return run, schemaFailure(openErr)
		}
		_ = db.Close()
	}
	run.applied = true
	return run, nil
}

func (s *Service) inspectSetup(ctx context.Context, home string) (setupInspection, error) {
	versionData, versionErr := runHerdrCLI(s.Herdr, ctx, "--version")
	if versionErr != nil {
		return setupInspection{}, axi.Failure("herdr_version_failed", "could not determine Herdr version", true, versionErr.Error())
	}
	pluginData, err := runHerdrCLI(s.Herdr, ctx, "plugin", "list", "--json")
	if err != nil {
		return setupInspection{}, axi.Failure("plugin_list_failed", "could not list Herdr plugins", true, err.Error())
	}
	status, err := integrationStatus(s.Herdr, ctx)
	if err != nil {
		return setupInspection{}, err
	}
	referenced, err := configuredAgentKinds(home)
	if err != nil {
		return setupInspection{}, configError(err)
	}
	detected := integrationStatusKinds(status)
	availableSet := make(map[string]bool, len(detected))
	referencedSet := make(map[string]bool, len(referenced))
	for _, kind := range detected {
		availableSet[kind] = true
	}
	for _, kind := range referenced {
		referencedSet[kind] = true
	}
	installKinds := []string{}
	availableKinds := []string{}
	for _, kind := range referenced {
		_, cliErr := exec.LookPath(agentCLIName(kind))
		if availableSet[kind] && cliErr == nil {
			installKinds = append(installKinds, kind)
		}
	}
	for _, kind := range detected {
		if !referencedSet[kind] {
			availableKinds = append(availableKinds, kind)
		}
	}
	pluginPath, pluginFound := pluginLocation(pluginData, possePluginID)
	return setupInspection{
		Kinds: installKinds, ReferencedKinds: referenced, AvailableKinds: availableKinds, Integrations: status,
		PluginPath: pluginPath, PluginPresent: pluginFound, PluginListJSON: pluginData,
		SidebarSupported: herdrSupportsSidebarRules(string(versionData)),
	}, nil
}

func (s *Service) setupPlan(home, binary, version string, dirs setupDirs, manifest setupManifest, hasManifest bool, state setupInspection) []map[string]any {
	claudeDir, codexDir, agentsDir := dirs.claude, dirs.codex, dirs.agents
	rows := []map[string]any{}
	for _, kind := range state.Kinds {
		action := "install"
		if integrationNeedsRepair(state.Integrations[kind]) {
			action = "repair"
		} else if integrationPresent(state.Integrations[kind]) {
			action = "keep"
		}
		rows = append(rows, map[string]any{"step": "integration", "target": kind, "action": action})
	}
	pluginPath := filepath.Join(home, "plugin", "herdr-plugin.toml")
	pluginContent, err := setupassets.PluginManifest(binary, version)
	pluginHash := ""
	if err == nil {
		pluginHash = fileHash(pluginContent)
	}
	pluginAction := "link"
	if data, readErr := os.ReadFile(pluginPath); readErr == nil {
		if fileHash(data) != pluginHash {
			pluginAction = "update"
		} else if samePathAfterEval(state.PluginPath, pluginPath) && state.PluginPresent {
			pluginAction = "keep"
		} else {
			pluginAction = "link"
		}
	}
	if data, readErr := os.ReadFile(pluginPath); readErr == nil && fileHash(data) != pluginHash && (!hasManifest || !manifest.PluginFileMade || fileHash(data) != manifest.PluginHash) {
		pluginAction = "conflict"
	}
	rows = append(rows, map[string]any{"step": "plugin", "target": pluginPath, "action": pluginAction})
	configPath := filepath.Join(home, "config.toml")
	configAction := "keep"
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		configAction = "create"
	}
	rows = append(rows, map[string]any{"step": "default_config", "target": configPath, "action": configAction})
	for _, name := range []string{"posse", "posse-setup"} {
		directory := filepath.Join(agentsDir, name)
		link := filepath.Join(claudeDir, "skills", name)
		action := "install"
		contents, _ := setupassets.Skill(name)
		installed, fileErr := os.ReadFile(filepath.Join(directory, "SKILL.md"))
		marker, markerErr := os.ReadFile(filepath.Join(directory, ".posse-version"))
		linkTarget, linkErr := os.Readlink(link)
		if fileErr == nil && markerErr == nil && strings.TrimSpace(string(marker)) == version && fileHash(installed) == fileHash(contents) && linkErr == nil && linkTarget == directory {
			action = "keep"
		} else {
			previous := previousSkill(manifest.Skills, name)
			fileIsManaged := fileErr != nil || previous.FileMade && fileHash(installed) == previous.FileHash
			markerIsManaged := markerErr != nil || previous.MarkerMade && strings.TrimSpace(string(marker)) == previous.MarkerVersion
			linkInfo, lstatErr := os.Lstat(link)
			linkExists := lstatErr == nil
			linkIsSymlink := linkExists && linkInfo.Mode()&os.ModeSymlink != 0
			linkIsManaged := errors.Is(lstatErr, os.ErrNotExist)
			if linkIsSymlink {
				resolvedLink, linkResolveErr := filepath.EvalSymlinks(link)
				resolvedDirectory, dirResolveErr := filepath.EvalSymlinks(directory)
				linkIsManaged = linkResolveErr == nil && dirResolveErr == nil && resolvedLink == resolvedDirectory
				if !linkIsManaged && previous.LinkMade && linkErr == nil && linkTarget == previous.LinkTarget {
					linkIsManaged = true
				}
			}
			if lstatErr != nil && !errors.Is(lstatErr, os.ErrNotExist) || fileErr == nil && !fileIsManaged || markerErr == nil && !markerIsManaged || linkExists && !linkIsManaged {
				action = "conflict"
			}
		}
		rows = append(rows, map[string]any{"step": "skill", "target": directory, "link": link, "action": action})
	}
	for _, hook := range setupHookTargets(claudeDir, codexDir) {
		path, spec := hook.path, hook.spec
		command := hookCommand(binary, spec)
		resolved, pathErr := hookWritePath(path)
		row := map[string]any{"step": hook.step(), "target": path, "action": "merge_" + hook.stepName(), "agent": hook.agent}
		if resolvedPath, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil && resolvedPath != path {
			row["resolved_target"] = resolvedPath
		}
		if pathErr != nil {
			var blocked *setupHookSymlinkError
			if errors.As(pathErr, &blocked) {
				row["action"] = "setup_hook_symlink"
				row["resolved_target"] = blocked.Path
				row["snippet"] = hookSnippet(spec, command)
			} else {
				row["action"] = "invalid"
				row["error"] = pathErr.Error()
			}
		} else if err := validateHookJSON(resolved); err != nil {
			row["action"] = "invalid"
			row["error"] = err.Error()
		} else if hookCommandExists(resolved, spec, command) {
			previous := previousHook(manifest.Hooks, path, spec)
			if previous.Version != "" && previous.Version != version {
				row["action"] = "stamp_version"
			} else {
				row["action"] = "keep"
			}
		}
		rows = append(rows, row)
	}
	if path := dirs.piGuardPath(); path != "" {
		action := "install"
		contents, _ := setupassets.PiWorkerGuard(binary)
		if current, err := os.ReadFile(path); err == nil {
			switch {
			case fileHash(current) == fileHash(contents):
				action = "keep"
			case manifest.PiGuardHash != "" && fileHash(current) == manifest.PiGuardHash:
				action = "update"
			default:
				action = "conflict"
			}
		}
		rows = append(rows, map[string]any{"step": "pi_guard", "target": path, "action": action})
	}
	return rows
}

// setupPendingExitCode is what `setup --check --exit-code` returns when
// applying the plan would change the machine.
const setupPendingExitCode = 3

// setupPending reports whether applying the plan changes the machine. A
// read-only hook link stays a manual step, so it is never pending.
func setupPending(plan []map[string]any) bool {
	for _, row := range plan {
		if action, _ := row["action"].(string); action != "keep" && action != "setup_hook_symlink" && action != "offer" && action != "manual" {
			return true
		}
	}
	return false
}

func setupInputIsTerminal() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS)
	return err == nil
}

func herdrSupportsSidebarRules(version string) bool {
	var major, minor, patch int
	if _, err := fmt.Sscanf(strings.TrimPrefix(strings.TrimSpace(version), "herdr "), "%d.%d.%d", &major, &minor, &patch); err != nil {
		return false
	}
	return major > 0 || minor > 9 || minor == 9 && patch >= 1
}

func sidebarOffer(plan []map[string]any) bool {
	for _, row := range plan {
		if row["step"] == "sidebar_layout" && row["action"] == "offer" {
			return true
		}
	}
	return false
}

func validateSetupPlan(plan []map[string]any) error {
	for _, row := range plan {
		action, _ := row["action"].(string)
		if action == "conflict" {
			return axi.Failure("setup_path_conflict", "setup would overwrite an unmanaged path", false, fmt.Sprint(row["target"]), "Move that path aside, then rerun `posse setup`")
		}
		if action == "invalid" {
			if event, ok := hookStepEvents[fmt.Sprint(row["step"])]; ok {
				return axi.Failure("setup_hook_invalid", "cannot safely merge "+event+" hook into "+fmt.Sprint(row["target"]), false, fmt.Sprint(row["error"]))
			}
			return axi.Failure("setup_path_invalid", "setup found an invalid target", false, fmt.Sprint(row["target"]), fmt.Sprint(row["error"]))
		}
	}
	return nil
}

func validateHookJSON(path string) error {
	root, exists, err := readJSONFile(path)
	if err != nil || !exists {
		return err
	}
	hooks, found := root["hooks"]
	if !found {
		return nil
	}
	hookMap, ok := hooks.(map[string]any)
	if !ok {
		return fmt.Errorf("hooks must be an object")
	}
	for _, spec := range setupHooks {
		if value, found := hookMap[spec.Event]; found {
			if _, ok := value.([]any); !ok {
				return fmt.Errorf("hooks.%s must be an array", spec.Event)
			}
		}
	}
	return nil
}

func renderSetupPlan(plan []map[string]any) []axi.Object {
	rows := make([]axi.Object, 0, len(plan))
	for _, row := range plan {
		step, _ := row["step"].(string)
		target, _ := row["target"].(string)
		action, _ := row["action"].(string)
		note := ""
		if value, ok := row["note"].(string); ok {
			note = value
		}
		if value, ok := row["error"].(string); ok && value != "" {
			note = value
		}
		if value, ok := row["resolved_target"].(string); ok && value != "" {
			configured := target
			target = value
			note = "edited through link " + configured
		}
		if value, ok := row["snippet"].(string); ok && value != "" {
			if step == "sidebar_layout" {
				note = "Posse Agents layout (existing rows are never replaced): " + value
			} else {
				note = "add the hook through the config manager; snippet: " + value
			}
		}
		if value, ok := row["link"].(string); ok && value != "" {
			note = "Claude skill link: " + value
		}
		if action == "keep" {
			if state, ok := row["status"].(string); ok {
				note = state
			}
		}
		rows = append(rows, axi.Object{{Key: "step", Value: step}, {Key: "target", Value: target}, {Key: "action", Value: action}, {Key: "note", Value: note}})
	}
	return rows
}

func setupPrerequisites(state setupInspection) []axi.Object {
	tools := []string{"git", "herdr", "gh", "no-mistakes"}
	for _, kind := range state.ReferencedKinds {
		tools = append(tools, agentCLIName(kind))
	}
	sort.Strings(tools)
	rows := make([]axi.Object, 0, len(tools))
	previous := ""
	for _, tool := range tools {
		if tool == previous {
			continue
		}
		previous = tool
		path, err := exec.LookPath(tool)
		status := "available"
		if err != nil {
			status = "missing"
			if tool == "gh" || tool == "no-mistakes" {
				status = "optional_missing"
			}
			path = ""
		}
		rows = append(rows, axi.Object{{Key: "tool", Value: tool}, {Key: "path", Value: path}, {Key: "status", Value: status}})
	}
	return rows
}

func setupPrerequisiteMissing(rows []axi.Object, tool string) bool {
	for _, row := range rows {
		if len(row) == 3 && row[0].Key == "tool" && row[0].Value == tool && row[2].Key == "status" && row[2].Value == "missing" {
			return true
		}
	}
	return false
}

func configuredAgentKinds(home string) ([]string, error) {
	set := make(map[string]bool)
	globalConfigPath := filepath.Join(home, "config.toml")
	if _, err := os.Stat(globalConfigPath); errors.Is(err, os.ErrNotExist) {
		for _, kind := range defaultSetupAgentKinds() {
			set[kind] = true
		}
	} else if err != nil {
		return nil, err
	}
	collect := func(cfg config.Config) {
		if integrationKinds[cfg.Lead.Kind] {
			set[cfg.Lead.Kind] = true
		}
		for kind := range cfg.Lead.Profiles {
			if integrationKinds[kind] {
				set[kind] = true
			}
		}
		for _, profile := range cfg.Profiles {
			if integrationKinds[profile.Kind] {
				set[profile.Kind] = true
			}
		}
	}
	global, err := config.Load(home, "")
	if err != nil {
		return nil, err
	}
	collect(global)
	projects := filepath.Join(home, "projects")
	entries, err := os.ReadDir(projects)
	if errors.Is(err, os.ErrNotExist) {
		return sortedKeys(set), nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(projects, entry.Name(), "config.toml")
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		cfg, err := config.Load(home, entry.Name())
		if err != nil {
			return nil, err
		}
		collect(cfg)
	}
	return sortedKeys(set), nil
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func defaultSetupAgentKinds() []string {
	var available []string
	for _, kind := range []string{"claude", "codex"} {
		if _, err := exec.LookPath(kind); err == nil {
			available = append(available, kind)
		}
	}
	return available
}

func (s *Service) applySetup(ctx context.Context, home, manifestPath string, manifest setupManifest, binary, version string, dirs setupDirs, state setupInspection) ([]any, error) {
	claudeDir, codexDir, agentsDir := dirs.claude, dirs.codex, dirs.agents
	configPath := filepath.Join(home, "config.toml")
	var defaultConfig []byte
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		generated, configErr := defaultSetupConfig()
		if configErr != nil {
			return nil, configErr
		}
		defaultConfig = generated
	} else if err != nil {
		return nil, err
	}
	if manifest.Version == "" {
		manifest.Integrations = []string{}
		manifest.Skills = []setupSkillRecord{}
		manifest.Hooks = []setupHookRecord{}
	}
	manifest.Version, manifest.Binary = version, binary
	if err := writeSetupManifest(manifestPath, manifest); err != nil {
		return nil, err
	}
	var changed []any
	for _, kind := range state.Kinds {
		status := state.Integrations[kind]
		if integrationNeedsRepair(status) {
			if _, err := runHerdrCLI(s.Herdr, ctx, "integration", "install", kind); err != nil {
				return changed, axi.Failure("integration_repair_failed", "could not repair Herdr integration "+kind, true, err.Error())
			}
			changed = append(changed, "integration_repair:"+kind)
			continue
		}
		if integrationPresent(status) {
			continue
		}
		if _, err := runHerdrCLI(s.Herdr, ctx, "integration", "install", kind); err != nil {
			return changed, axi.Failure("integration_install_failed", "could not install Herdr integration "+kind, true, err.Error())
		}
		manifest.Integrations = appendUnique(manifest.Integrations, kind)
		if err := writeSetupManifest(manifestPath, manifest); err != nil {
			return changed, err
		}
		changed = append(changed, "integration:"+kind)
	}
	pluginPath := filepath.Join(home, "plugin", "herdr-plugin.toml")
	pluginContent, err := setupassets.PluginManifest(binary, version)
	if err != nil {
		return changed, err
	}
	priorPluginHash := ""
	if manifest.PluginHash != "" {
		priorPluginHash = manifest.PluginHash
	}
	pluginMade, err := installManagedFile(pluginPath, pluginContent, priorPluginHash, 0o600)
	if err != nil {
		return changed, axi.Failure("setup_path_conflict", "cannot install the embedded Herdr plugin", false, err.Error())
	}
	manifest.PluginPath, manifest.PluginHash = pluginPath, fileHash(pluginContent)
	manifest.PluginFileMade = manifest.PluginFileMade || pluginMade
	if err := writeSetupManifest(manifestPath, manifest); err != nil {
		return changed, err
	}
	if pluginMade {
		changed = append(changed, "plugin_file")
	}
	pluginPathMatches := samePathAfterEval(state.PluginPath, pluginPath)
	if !pluginPathMatches || !state.PluginPresent || pluginMade {
		if manifest.PluginLinked && state.PluginPath != "" && !pluginPathMatches {
			return changed, axi.Failure("setup_plugin_conflict", "the posse plugin link changed after setup", false, state.PluginPath)
		}
		if !manifest.PluginLinked && state.PluginPresent {
			manifest.PreviousPluginPath = state.PluginPath
		}
		if _, err := runHerdrCLI(s.Herdr, ctx, "plugin", "link", pluginPath); err != nil {
			return changed, axi.Failure("plugin_link_failed", "could not link the posse plugin into Herdr", true, err.Error())
		}
		manifest.PluginLinked = true
		if err := writeSetupManifest(manifestPath, manifest); err != nil {
			return changed, err
		}
		changed = append(changed, "plugin_link")
	}
	if err := writeSetupManifest(manifestPath, manifest); err != nil {
		return changed, err
	}
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		if err := atomicfile.Write(configPath, defaultConfig, 0o600); err != nil {
			return changed, err
		}
		manifest.ConfigCreated = true
		manifest.ConfigHash = fileHash(defaultConfig)
		changed = append(changed, "default_config")
	}
	if err := writeSetupManifest(manifestPath, manifest); err != nil {
		return changed, err
	}
	for _, name := range []string{"posse", "posse-setup"} {
		record, skillChanged, err := installSkill(name, agentsDir, claudeDir, version, previousSkill(manifest.Skills, name))
		if err != nil {
			return changed, axi.Failure("setup_path_conflict", "cannot install skill "+name, false, err.Error())
		}
		manifest.Skills = replaceSkillRecord(manifest.Skills, record)
		if skillChanged {
			changed = append(changed, "skill:"+name)
		}
		if err := writeSetupManifest(manifestPath, manifest); err != nil {
			return changed, err
		}
	}
	for _, hook := range setupHookTargets(claudeDir, codexDir) {
		path, spec := hook.path, hook.spec
		command := hookCommand(binary, spec)
		previous := previousHook(manifest.Hooks, path, spec)
		record, didChange, err := installHook(path, spec, command, previous)
		if err != nil {
			var blocked *setupHookSymlinkError
			if errors.As(err, &blocked) {
				changed = append(changed, map[string]any{"code": "setup_hook_symlink", "target": path, "resolved_target": blocked.Path, "snippet": blocked.Snippet})
				continue
			}
			return changed, axi.Failure("setup_hook_invalid", "cannot merge "+spec.Event+" hook into "+path, false, err.Error())
		}
		record.Version = version
		if didChange || previous.HookMade || previous.CreatedGroup {
			manifest.Hooks = replaceHookRecord(manifest.Hooks, record)
		} else {
			manifest.Hooks = removeHookRecord(manifest.Hooks, path, spec)
		}
		if didChange {
			changed = append(changed, spec.Event+":"+path)
		}
		if err := writeSetupManifest(manifestPath, manifest); err != nil {
			return changed, err
		}
	}
	if path := dirs.piGuardPath(); path != "" {
		contents, err := setupassets.PiWorkerGuard(binary)
		if err != nil {
			return changed, err
		}
		createdDirs, err := makeSetupDirs(filepath.Dir(path))
		if err != nil {
			return changed, err
		}
		manifest.PiGuardDirs = appendUniqueStrings(manifest.PiGuardDirs, createdDirs...)
		made, err := installManagedFile(path, contents, manifest.PiGuardHash, 0o600)
		if err != nil {
			return changed, axi.Failure("setup_path_conflict", "cannot install the pi Rider guard", false, err.Error())
		}
		manifest.PiGuardPath, manifest.PiGuardHash = path, fileHash(contents)
		if made {
			changed = append(changed, "pi_guard")
		}
		if err := writeSetupManifest(manifestPath, manifest); err != nil {
			return changed, err
		}
	}
	if err := writeSetupManifest(manifestPath, manifest); err != nil {
		return changed, err
	}
	return changed, nil
}

func (s *Service) uninstallSetup(ctx *axi.Context, home, manifestPath string, manifest setupManifest, found bool) error {
	if !found {
		return ctx.Print(axi.Object{{Key: "removed", Value: []any{}}, {Key: "state", Value: "not installed"}})
	}
	if err := validateSetupMutationScope(); err != nil {
		return err
	}
	var removed []any
	if manifest.PluginLinked && s.Herdr == nil {
		return axi.Failure("herdr_unavailable", "Herdr adapter is not configured", true)
	}
	if len(manifest.Integrations) > 0 && s.Herdr == nil {
		return axi.Failure("herdr_unavailable", "Herdr adapter is not configured", true)
	}
	if manifest.PluginLinked {
		if manifest.PreviousPluginPath != "" {
			if _, err := runHerdrCLI(s.Herdr, ctx.Context, "plugin", "link", manifest.PreviousPluginPath); err != nil {
				return axi.Failure("plugin_link_failed", "could not restore the previous Herdr plugin link", true, err.Error())
			}
		} else if _, err := runHerdrCLI(s.Herdr, ctx.Context, "plugin", "uninstall", possePluginID); err != nil {
			return axi.Failure("plugin_unlink_failed", "could not unlink the posse plugin from Herdr", true, err.Error())
		}
		removed = append(removed, "plugin_link")
		manifest.PluginLinked = false
		manifest.PreviousPluginPath = ""
		if err := writeSetupManifest(manifestPath, manifest); err != nil {
			return err
		}
	}
	for len(manifest.Integrations) > 0 {
		kind := manifest.Integrations[0]
		if _, err := runHerdrCLI(s.Herdr, ctx.Context, "integration", "uninstall", kind); err != nil {
			return axi.Failure("integration_uninstall_failed", "could not uninstall Herdr integration "+kind, true, err.Error())
		}
		removed = append(removed, "integration:"+kind)
		manifest.Integrations = append([]string(nil), manifest.Integrations[1:]...)
		if err := writeSetupManifest(manifestPath, manifest); err != nil {
			return err
		}
	}
	for len(manifest.Hooks) > 0 {
		record := manifest.Hooks[0]
		changed, err := removeHook(record)
		if err != nil {
			return err
		}
		if changed {
			removed = append(removed, record.Path)
		}
		manifest.Hooks = append([]setupHookRecord(nil), manifest.Hooks[1:]...)
		if err := writeSetupManifest(manifestPath, manifest); err != nil {
			return err
		}
		removeCreatedDirs(record.CreatedDirs)
	}
	for len(manifest.Skills) > 0 {
		record := manifest.Skills[0]
		if err := uninstallSkill(record); err != nil {
			return err
		}
		removed = append(removed, "skill:"+record.Name)
		manifest.Skills = append([]setupSkillRecord(nil), manifest.Skills[1:]...)
		if err := writeSetupManifest(manifestPath, manifest); err != nil {
			return err
		}
		removeCreatedDirs(record.CreatedDirs)
	}
	if manifest.PiGuardPath != "" && manifest.PiGuardHash != "" {
		data, err := os.ReadFile(manifest.PiGuardPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && fileHash(data) == manifest.PiGuardHash {
			if err := os.Remove(manifest.PiGuardPath); err != nil {
				return err
			}
			removed = append(removed, "pi_guard")
		}
		removeCreatedDirs(manifest.PiGuardDirs)
		manifest.PiGuardPath, manifest.PiGuardHash, manifest.PiGuardDirs = "", "", nil
		if err := writeSetupManifest(manifestPath, manifest); err != nil {
			return err
		}
	}
	pluginPath := filepath.Join(home, "plugin", "herdr-plugin.toml")
	if manifest.PluginFileMade && manifest.PluginHash != "" {
		data, err := os.ReadFile(pluginPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && fileHash(data) == manifest.PluginHash {
			if err := os.Remove(pluginPath); err != nil {
				return err
			}
			_ = os.Remove(filepath.Dir(pluginPath))
			removed = append(removed, "plugin_file")
		}
		manifest.PluginFileMade = false
		manifest.PluginHash = ""
		if err := writeSetupManifest(manifestPath, manifest); err != nil {
			return err
		}
	}
	configPath := filepath.Join(home, "config.toml")
	if manifest.ConfigCreated && manifest.ConfigHash != "" {
		data, err := os.ReadFile(configPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && fileHash(data) == manifest.ConfigHash {
			if err := os.Remove(configPath); err != nil {
				return err
			}
			removed = append(removed, "default_config")
		}
		manifest.ConfigCreated = false
		manifest.ConfigHash = ""
		if err := writeSetupManifest(manifestPath, manifest); err != nil {
			return err
		}
	}
	if err := os.Remove(manifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return ctx.Print(axi.Object{{Key: "removed", Value: removed}, {Key: "state", Value: "uninstalled"}, {Key: "help", Value: []any{"Project and Task data in POSSE_HOME were preserved"}}})
}

func defaultSetupConfig() ([]byte, error) {
	kinds := defaultSetupAgentKinds()
	if len(kinds) == 0 {
		return nil, axi.Failure("setup_prerequisite_missing", "claude or codex must be on PATH to create the default Posse Profile", false, "Install Claude Code or Codex CLI, then rerun `posse setup`")
	}
	leadKind := "codex"
	if kinds[0] == "claude" {
		leadKind = "claude"
	}
	var contents strings.Builder
	contents.WriteString(`# Generated by posse setup. Use posse config set to change values.
[identity.lead]
name = "Sheriff"
language = "en"

[identity.worker]
display_prefix = "rider"

[lead]
`)
	fmt.Fprintf(&contents, "kind = %q\n", leadKind)
	contents.WriteString(`

[defaults]
max_workers = 4
stall_after = "20m"
idle_after = "3m"
landing_mode = "pr"
merge_method = "squash"
review = "on_risk"

[remuda]
clean = "warm"
keep_idle = 4
`)
	for _, kind := range kinds {
		fmt.Fprintf(&contents, "\n[profiles.%s]\nkind = %q\n", kind, kind)
	}
	fmt.Fprintf(&contents, "\n[dispatch.default]\nuse = %q\n", leadKind)
	return []byte(contents.String()), nil
}

func (s *Service) binaryVersion() string {
	if strings.TrimSpace(s.Version) == "" {
		return "dev"
	}
	return s.Version
}

func resolveSetupBinary(explicit string) (string, error) {
	path := explicit
	installed := false
	if path == "" {
		executable, err := os.Executable()
		if err != nil {
			return "", err
		}
		path, err = filepath.Abs(executable)
		if err != nil {
			return "", err
		}
		home, homeErr := os.UserHomeDir()
		if homeErr == nil {
			installedPath := filepath.Join(home, ".local", "bin", "posse")
			if installedInfo, installedErr := os.Stat(installedPath); installedErr == nil {
				if executableInfo, executableErr := os.Stat(path); executableErr == nil && os.SameFile(installedInfo, executableInfo) {
					path, installed = installedPath, true
				}
			}
		}
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("setup binary %s is unavailable: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("setup binary %s must be an executable regular file", path)
	}
	if isUnsafeSetupBinaryPath(path) {
		return "", unsafeSetupBinary(path)
	}
	if !installed {
		resolved, resolveErr := filepath.EvalSymlinks(path)
		if resolveErr != nil {
			return "", resolveErr
		}
		if isUnsafeSetupBinaryPath(resolved) {
			return "", unsafeSetupBinary(path)
		}
	}
	return path, nil
}

func unsafeSetupBinary(path string) error {
	return axi.Failure("setup_binary_unsafe", "setup binary "+path+" resolves to a temporary or store path", false, "Install to `~/.local/bin/posse` or pass `--binary <path>` with a stable executable")
}

func isUnsafeSetupBinaryPath(path string) bool {
	clean := filepath.Clean(path)
	if insideNixStore(clean) || clean == "/tmp" || strings.HasPrefix(clean, "/tmp"+string(os.PathSeparator)) {
		return true
	}
	for _, part := range strings.Split(clean, string(os.PathSeparator)) {
		if strings.HasPrefix(part, "go-build") {
			return true
		}
	}
	return false
}

// setupDirs are the agent config directories setup writes into.
type setupDirs struct {
	claude, codex, agents string
	pi                    string // pi's agent directory; the guard is installed only when it exists
}

func setupDirectories() (setupDirs, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return setupDirs{}, err
	}
	claudeDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeDir == "" {
		claudeDir = filepath.Join(home, ".claude")
	}
	codexDir := os.Getenv("CODEX_HOME")
	if codexDir == "" {
		codexDir = filepath.Join(home, ".codex")
	}
	piDir := os.Getenv("PI_CODING_AGENT_DIR")
	if piDir == "" {
		piDir = filepath.Join(home, ".pi", "agent")
	}
	return setupDirs{claude: filepath.Clean(claudeDir), codex: filepath.Clean(codexDir), agents: filepath.Join(home, ".agents", "skills"), pi: filepath.Clean(piDir)}, nil
}

// piGuardPath is where the pi Worker guard goes, or "" when pi is not set up.
func (d setupDirs) piGuardPath() string {
	if d.pi == "" {
		return ""
	}
	if info, err := os.Stat(d.pi); err != nil || !info.IsDir() {
		return ""
	}
	return filepath.Join(d.pi, "extensions", "posse-worker-guard.ts")
}

func readSetupManifest(path string) (setupManifest, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return setupManifest{}, false, nil
	}
	if err != nil {
		return setupManifest{}, false, err
	}
	var manifest setupManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return setupManifest{}, true, err
	}
	return manifest, true, nil
}

func writeSetupManifest(path string, manifest setupManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(path, append(data, '\n'), 0o600)
}

func validateSetupMutationScope() error {
	if os.Getenv("POSSE_TEST_HERDR") != "1" {
		return nil
	}
	if _, err := herdr.ValidateIsolatedEnvironment(os.Environ()); err != nil {
		return axi.Failure("unsafe_test_environment", err.Error(), false)
	}
	return nil
}

func runHerdrCLI(adapter herdr.Adapter, ctx context.Context, args ...string) ([]byte, error) {
	if adapter == nil {
		return nil, &herdr.Error{Code: "herdr_unavailable", Message: "Herdr adapter is not configured"}
	}
	output, err := adapter.Run(ctx, args...)
	if err != nil {
		return output, fmt.Errorf("herdr %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func integrationStatus(adapter herdr.Adapter, ctx context.Context) (map[string]string, error) {
	output, err := runHerdrCLI(adapter, ctx, "integration", "status")
	if err != nil {
		return nil, axi.Failure("integration_status_failed", "could not read Herdr integration status", true, err.Error())
	}
	status := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		kind, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		rest = strings.TrimSpace(rest)
		status[strings.TrimSpace(kind)] = rest
	}
	return status, nil
}

// integrationStatusKinds lists the agent kinds Herdr can install an integration for.
func integrationStatusKinds(status map[string]string) []string {
	kinds := make([]string, 0, len(status))
	for kind := range status {
		if integrationKinds[kind] {
			kinds = append(kinds, kind)
		}
	}
	sort.Strings(kinds)
	return kinds
}

func integrationPresent(status string) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	return status == "installed" || strings.HasPrefix(status, "installed (") || strings.HasPrefix(status, "current (") || strings.HasPrefix(status, "outdated (") || integrationNeedsRepair(status)
}

func integrationNeedsRepair(status string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(status)), "needs repair")
}

var integrationKinds = map[string]bool{
	"pi": true, "omp": true, "claude": true, "codex": true, "copilot": true,
	"devin": true, "droid": true, "kimi": true, "opencode": true, "kilo": true,
	"hermes": true, "qodercli": true, "qwen": true, "cursor": true,
	"mastracode": true, "antigravity-cli": true, "grok": true,
}

func agentKinds(data []byte) []string {
	var decoded any
	if json.Unmarshal(data, &decoded) != nil {
		return nil
	}
	found := map[string]bool{}
	var walk func(any)
	walk = func(value any) {
		switch item := value.(type) {
		case map[string]any:
			for key, nested := range item {
				if integrationKinds[key] {
					found[key] = true
				}
				if text, ok := nested.(string); ok && integrationKinds[text] && (key == "kind" || key == "name" || key == "agent" || key == "agent_kind" || key == "binary" || key == "command") {
					found[text] = true
				}
				walk(nested)
			}
		case []any:
			for _, nested := range item {
				walk(nested)
			}
		}
	}
	walk(decoded)
	result := make([]string, 0, len(found))
	for kind := range found {
		result = append(result, kind)
	}
	sort.Strings(result)
	return result
}

func availableAgentKinds(data []byte) []string {
	var available []string
	for _, kind := range agentKinds(data) {
		if _, err := exec.LookPath(agentCLIName(kind)); err == nil {
			available = append(available, kind)
		}
	}
	return available
}

func agentCLIName(kind string) string {
	if kind == "cursor" {
		return "cursor-agent"
	}
	return kind
}

func integrationConfigRoot(kind string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch kind {
	case "claude":
		if path := os.Getenv("CLAUDE_CONFIG_DIR"); path != "" {
			return path, nil
		}
		return filepath.Join(home, ".claude"), nil
	case "codex":
		if path := os.Getenv("CODEX_HOME"); path != "" {
			return path, nil
		}
		return filepath.Join(home, ".codex"), nil
	case "pi":
		return filepath.Join(home, ".pi"), nil
	case "omp":
		return filepath.Join(home, ".omp"), nil
	case "copilot":
		return filepath.Join(home, ".copilot"), nil
	case "devin":
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		}
		return filepath.Join(configHome, "devin"), nil
	case "droid":
		return filepath.Join(home, ".factory"), nil
	case "kimi":
		return filepath.Join(home, ".kimi-code"), nil
	case "opencode":
		return filepath.Join(home, ".config", "opencode"), nil
	case "kilo":
		return filepath.Join(home, ".config", "kilo"), nil
	case "hermes":
		return filepath.Join(home, ".hermes"), nil
	case "qodercli":
		return filepath.Join(home, ".qoder"), nil
	case "qwen":
		return filepath.Join(home, ".qwen"), nil
	case "cursor":
		return filepath.Join(home, ".cursor"), nil
	case "mastracode":
		return filepath.Join(home, ".mastracode"), nil
	case "antigravity-cli":
		return filepath.Join(home, ".gemini", "config"), nil
	case "grok":
		return filepath.Join(home, ".grok"), nil
	default:
		return "", fmt.Errorf("no integration config root for %s", kind)
	}
}

func pluginLocation(data []byte, pluginID string) (string, bool) {
	var decoded any
	if json.Unmarshal(data, &decoded) != nil {
		return "", strings.Contains(string(data), pluginID)
	}
	path, found := "", false
	var walk func(any)
	walk = func(value any) {
		switch item := value.(type) {
		case map[string]any:
			matches := false
			for _, key := range []string{"id", "plugin_id", "pluginId"} {
				if item[key] == pluginID {
					matches = true
					found = true
				}
			}
			if matches {
				for _, key := range []string{"path", "manifest", "manifest_path", "manifestPath"} {
					if text, ok := item[key].(string); ok {
						path = text
						break
					}
				}
			}
			for _, nested := range item {
				walk(nested)
			}
		case []any:
			for _, nested := range item {
				walk(nested)
			}
		}
	}
	walk(decoded)
	if !found && strings.Contains(string(data), pluginID) {
		found = true
	}
	return path, found
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func fileHash(contents []byte) string {
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])
}

func samePathAfterEval(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	canonical := func(path string) string {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return filepath.Clean(path)
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err == nil {
			return filepath.Clean(resolved)
		}
		return filepath.Clean(absolute)
	}
	return canonical(left) == canonical(right)
}

func installManagedFile(path string, contents []byte, ownedHash string, mode os.FileMode) (bool, error) {
	current, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := atomicfile.Write(path, contents, mode); err != nil {
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	currentHash := fileHash(current)
	if currentHash != fileHash(contents) && ownedHash == "" {
		return false, fmt.Errorf("%s already exists and is not managed by setup", path)
	}
	if ownedHash != "" && currentHash != ownedHash && currentHash != fileHash(contents) {
		return false, fmt.Errorf("%s changed since setup installed it", path)
	}
	if currentHash != fileHash(contents) {
		if err := atomicfile.Write(path, contents, mode); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func installSkill(name, agentsDir, claudeDir, version string, previous setupSkillRecord) (setupSkillRecord, bool, error) {
	contents, err := setupassets.Skill(name)
	if err != nil {
		return setupSkillRecord{}, false, err
	}
	directory := filepath.Join(agentsDir, name)
	info, statErr := os.Lstat(directory)
	directoryMade := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !directoryMade {
		return setupSkillRecord{}, false, statErr
	}
	if !directoryMade && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
		return setupSkillRecord{}, false, fmt.Errorf("%s is not a setup-owned directory", directory)
	}
	createdDirs := append([]string(nil), previous.CreatedDirs...)
	if directoryMade {
		dirs, err := makeSetupDirs(directory)
		if err != nil {
			return setupSkillRecord{}, false, err
		}
		createdDirs = appendUniqueStrings(createdDirs, dirs...)
	}
	skillPath := filepath.Join(directory, "SKILL.md")
	ownedHash := ""
	if previous.FileMade {
		ownedHash = previous.FileHash
	}
	fileMade, err := installManagedFile(skillPath, contents, ownedHash, 0o600)
	if err != nil {
		return setupSkillRecord{}, false, err
	}
	markerPath := filepath.Join(directory, ".posse-version")
	markerData := []byte(version + "\n")
	markerOwned := previous.MarkerMade
	markerMade, err := installManagedFile(markerPath, markerData, func() string {
		if markerOwned {
			return fileHash([]byte(previous.MarkerVersion + "\n"))
		}
		return ""
	}(), 0o600)
	if err != nil {
		return setupSkillRecord{}, false, err
	}
	linkPath := filepath.Join(claudeDir, "skills", name)
	linkMade := previous.LinkMade
	linkInfo, linkErr := os.Lstat(linkPath)
	if errors.Is(linkErr, os.ErrNotExist) {
		dirs, err := makeSetupDirs(filepath.Dir(linkPath))
		if err != nil {
			return setupSkillRecord{}, false, err
		}
		createdDirs = appendUniqueStrings(createdDirs, dirs...)
		if err := os.Symlink(directory, linkPath); err != nil {
			return setupSkillRecord{}, false, err
		}
		linkMade = true
	} else if linkErr != nil {
		return setupSkillRecord{}, false, linkErr
	} else if linkInfo.Mode()&os.ModeSymlink == 0 {
		return setupSkillRecord{}, false, fmt.Errorf("%s exists and is not a symlink", linkPath)
	} else {
		target, err := os.Readlink(linkPath)
		if err != nil {
			return setupSkillRecord{}, false, err
		}
		if target != directory {
			return setupSkillRecord{}, false, fmt.Errorf("%s points to %s", linkPath, target)
		}
	}
	record := setupSkillRecord{Name: name, AgentsDir: agentsDir, CreatedDirs: createdDirs, DirectoryMade: previous.DirectoryMade || directoryMade,
		FileMade: previous.FileMade || fileMade, FileHash: fileHash(contents),
		MarkerMade: previous.MarkerMade || markerMade, MarkerVersion: version,
		LinkPath: linkPath, LinkTarget: directory, LinkMade: linkMade}
	return record, directoryMade || fileMade || markerMade || !previous.LinkMade && linkMade, nil
}

func uninstallSkill(record setupSkillRecord) error {
	if record.Name != "posse" && record.Name != "posse-setup" {
		return nil
	}
	directory := filepath.Join(record.AgentsDir, record.Name)
	if record.AgentsDir == "" {
		return fmt.Errorf("setup manifest does not record the skill directory for %s", record.Name)
	}
	if record.LinkMade && record.LinkPath != "" {
		if target, err := os.Readlink(record.LinkPath); err == nil && target == record.LinkTarget {
			if err := os.Remove(record.LinkPath); err != nil {
				return err
			}
		}
	}
	if record.FileMade {
		path := filepath.Join(directory, "SKILL.md")
		if data, err := os.ReadFile(path); err == nil && fileHash(data) == record.FileHash {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	if record.MarkerMade {
		path := filepath.Join(directory, ".posse-version")
		if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) == record.MarkerVersion {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	if record.DirectoryMade {
		_ = os.Remove(directory)
	}
	return nil
}

func previousSkill(records []setupSkillRecord, name string) setupSkillRecord {
	for _, record := range records {
		if record.Name == name {
			return record
		}
	}
	return setupSkillRecord{Name: name}
}

func replaceSkillRecord(records []setupSkillRecord, record setupSkillRecord) []setupSkillRecord {
	for index, existing := range records {
		if existing.Name == record.Name {
			records[index] = record
			return records
		}
	}
	return append(records, record)
}

// hookSpec is one agent hook posse installs: the event, the tool or source
// matcher (empty matches everything) and the posse subcommand it runs.
type hookSpec struct {
	Event      string
	Matcher    string
	Subcommand string
}

var (
	sessionStartHook = hookSpec{Event: "SessionStart", Matcher: "startup|resume|compact", Subcommand: "_context"}
	guardHook        = hookSpec{Event: "PreToolUse", Subcommand: "_guard"}
	setupHooks       = []hookSpec{sessionStartHook, guardHook}
)

func hookSpecForEvent(event string) hookSpec {
	for _, spec := range setupHooks {
		if spec.Event == event {
			return spec
		}
	}
	return sessionStartHook
}

func (r setupHookRecord) spec() hookSpec { return hookSpecForEvent(r.Event) }

func hookCommand(binary string, spec hookSpec) string {
	return shellQuote(binary) + " " + spec.Subcommand
}

func setupHookGroup(spec hookSpec, command string) map[string]any {
	group := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 5}}}
	if spec.Matcher != "" {
		group["matcher"] = spec.Matcher
	}
	return group
}

func setupHookGroupJSON(spec hookSpec, command string) []byte {
	encodedCommand, _ := json.Marshal(command)
	matcher := ""
	if spec.Matcher != "" {
		encodedMatcher, _ := json.Marshal(spec.Matcher)
		matcher = `"matcher":` + string(encodedMatcher) + `,`
	}
	return []byte(`{` + matcher + `"hooks":[{"type":"command","command":` + string(encodedCommand) + `,"timeout":5}]}`)
}

type setupHookSymlinkError struct {
	Path    string
	Snippet string
}

func (e *setupHookSymlinkError) Error() string {
	return fmt.Sprintf("hook config resolves to protected or unwritable file %s; add this snippet through the config manager", e.Path)
}

func hookSnippet(spec hookSpec, command string) string {
	data, _ := json.MarshalIndent(map[string]any{"hooks": map[string]any{spec.Event: []any{setupHookGroup(spec, command)}}}, "", "  ")
	return string(data)
}

func hookWritePath(path string) (string, error) {
	entryInfo, entryErr := os.Lstat(path)
	if entryErr != nil && !errors.Is(entryErr, os.ErrNotExist) {
		return "", entryErr
	}
	entryIsSymlink := entryErr == nil && entryInfo.Mode()&os.ModeSymlink != 0
	var resolved string
	if entryIsSymlink || entryErr == nil {
		var err error
		resolved, err = filepath.EvalSymlinks(path)
		if err != nil {
			if entryIsSymlink {
				return "", &setupHookSymlinkError{Path: path}
			}
			return "", err
		}
	} else {
		parent, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			parent, err = filepath.Abs(filepath.Dir(path))
			if err != nil {
				return "", err
			}
		}
		resolved = filepath.Join(parent, filepath.Base(path))
	}
	resolved, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	if insideNixStore(resolved) {
		return "", &setupHookSymlinkError{Path: resolved}
	}
	info, err := os.Stat(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return resolved, nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("hook config %s is not a regular file", resolved)
	}
	if !fileIsWritable(resolved, info) {
		return "", &setupHookSymlinkError{Path: resolved}
	}
	return resolved, nil
}

func insideNixStore(path string) bool {
	clean := filepath.Clean(path)
	return clean == "/nix/store" || strings.HasPrefix(clean, "/nix/store"+string(os.PathSeparator))
}

func fileIsWritable(path string, info os.FileInfo) bool {
	if info.Mode().Perm()&0o222 == 0 {
		return false
	}
	if unix.Access(path, unix.W_OK) != nil {
		return false
	}
	return unix.Access(filepath.Dir(path), unix.W_OK) == nil
}

func installHook(path string, spec hookSpec, command string, previous setupHookRecord) (setupHookRecord, bool, error) {
	writePath, err := hookWritePath(path)
	if err != nil {
		var blocked *setupHookSymlinkError
		if errors.As(err, &blocked) {
			blocked.Snippet = hookSnippet(spec, command)
		}
		return setupHookRecord{}, false, err
	}
	data, readErr := os.ReadFile(writePath)
	existing := readErr == nil
	if errors.Is(readErr, os.ErrNotExist) {
		data = []byte("{}")
	} else if readErr != nil {
		return setupHookRecord{}, false, readErr
	}
	updatedData, createdRoot, createdSessionKey, createdGroup, updated, err := installHookBytes(data, spec, command, previous.Command)
	if err != nil {
		return setupHookRecord{}, false, err
	}
	if updated {
		mode := fileMode(writePath, 0o600)
		createdDirs := append([]string(nil), previous.CreatedDirs...)
		if !existing {
			dirs, err := makeSetupDirs(filepath.Dir(writePath))
			if err != nil {
				return setupHookRecord{}, false, err
			}
			createdDirs = appendUniqueStrings(createdDirs, dirs...)
		}
		if err := atomicfile.Write(writePath, updatedData, mode); err != nil {
			return setupHookRecord{}, false, err
		}
		previous.CreatedDirs = createdDirs
	}
	event := spec.Event
	if spec == sessionStartHook {
		event = ""
	}
	return setupHookRecord{Event: event, Path: writePath, ConfiguredPath: path, Command: command, CreatedDirs: previous.CreatedDirs, FileMade: previous.FileMade || !existing,
		HookMade:    previous.HookMade || createdGroup,
		CreatedRoot: previous.CreatedRoot || createdRoot, CreatedSessionKey: previous.CreatedSessionKey || createdSessionKey,
		CreatedGroup: previous.CreatedGroup || createdGroup}, updated, nil
}

func removeHook(record setupHookRecord) (bool, error) {
	writePath, err := hookWritePath(record.Path)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(writePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	updated, changed, err := removeHookBytes(data, record)
	if err != nil || !changed {
		return false, err
	}
	root, err := parseJSONSpans(updated)
	if err != nil {
		return false, err
	}
	if record.FileMade && len(root.members) == 0 {
		return true, os.Remove(writePath)
	}
	return true, atomicfile.Write(writePath, updated, fileMode(writePath, 0o600))
}

func installHookBytes(data []byte, spec hookSpec, command, previousCommand string) ([]byte, bool, bool, bool, bool, error) {
	root, err := parseJSONSpans(data)
	if err != nil {
		return nil, false, false, false, false, err
	}
	if root.kind != '{' {
		return nil, false, false, false, false, fmt.Errorf("hook config root must be an object")
	}
	group := setupHookGroupJSON(spec, command)
	eventKey, _ := json.Marshal(spec.Event)
	hooks, hooksIndex := root.member("hooks")
	if hooksIndex < 0 {
		value := append([]byte(`{`+string(eventKey)+`:[`), group...)
		value = append(value, []byte(`]}`)...)
		return insertJSONMember(data, root, "hooks", value), true, true, true, true, nil
	}
	if hooks.kind != '{' {
		return nil, false, false, false, false, fmt.Errorf("hooks must be an object")
	}
	start, startIndex := hooks.member(spec.Event)
	if startIndex < 0 {
		value := append([]byte{'['}, group...)
		value = append(value, ']')
		return insertJSONMember(data, hooks, spec.Event, value), false, true, true, true, nil
	}
	if start.kind != '[' {
		return nil, false, false, false, false, fmt.Errorf("hooks.%s must be an array", spec.Event)
	}
	oldCommand := previousCommand
	if oldCommand == "" && hasHookCommand(data, start, command) {
		return data, false, false, false, false, nil
	}
	if oldCommand == "" {
		groups := decodeHookGroups(data[start.start:start.end])
		oldCommand = findPosseHookCommand(groups, spec)
	}
	if oldCommand != "" {
		for _, item := range start.elements {
			updated, found, err := replaceCommandInGroup(data, item, oldCommand, command)
			if err != nil {
				return nil, false, false, false, false, err
			}
			if found {
				return updated, false, false, false, oldCommand != command, nil
			}
		}
	}
	return appendJSONArrayItem(data, start, group), false, false, true, true, nil
}

func decodeHookGroups(data []byte) []any {
	var groups []any
	_ = json.Unmarshal(data, &groups)
	return groups
}

func hookCommandExists(path string, spec hookSpec, command string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	root, err := parseJSONSpans(data)
	if err != nil || root.kind != '{' {
		return false
	}
	hooks, _ := root.member("hooks")
	start, _ := hooks.member(spec.Event)
	return hasHookCommand(data, start, command)
}

func hasHookCommand(data []byte, start *jsonSpan, command string) bool {
	if start == nil || start.kind != '[' {
		return false
	}
	for _, group := range start.elements {
		if groupHasCommand(data, group, command) {
			return true
		}
	}
	return false
}

func replaceCommandInGroup(data []byte, group *jsonSpan, oldCommand, nextCommand string) ([]byte, bool, error) {
	if group.kind != '{' {
		return data, false, nil
	}
	groupHooks, _ := group.member("hooks")
	if groupHooks == nil || groupHooks.kind != '[' {
		return data, false, nil
	}
	for index := len(groupHooks.elements) - 1; index >= 0; index-- {
		handler := groupHooks.elements[index]
		commandNode, _ := handler.member("command")
		if commandNode == nil {
			continue
		}
		var value string
		if err := json.Unmarshal(data[commandNode.start:commandNode.end], &value); err != nil || value != oldCommand {
			continue
		}
		replacement, _ := json.Marshal(nextCommand)
		updated := make([]byte, 0, len(data)+len(replacement)-(commandNode.end-commandNode.start))
		updated = append(updated, data[:commandNode.start]...)
		updated = append(updated, replacement...)
		updated = append(updated, data[commandNode.end:]...)
		return updated, true, nil
	}
	return data, false, nil
}

func removeHookBytes(data []byte, record setupHookRecord) ([]byte, bool, error) {
	if !record.HookMade && !record.CreatedGroup {
		return data, false, nil
	}
	event := record.spec().Event
	root, err := parseJSONSpans(data)
	if err != nil {
		return nil, false, err
	}
	hooks, hooksIndex := root.member("hooks")
	if hooksIndex < 0 || hooks.kind != '{' {
		return data, false, nil
	}
	start, startIndex := hooks.member(event)
	if startIndex < 0 || start.kind != '[' {
		return data, false, nil
	}
	groupIndex := -1
	for index, group := range start.elements {
		if groupHasCommand(data, group, record.Command) {
			groupIndex = index
			break
		}
	}
	if groupIndex < 0 {
		return data, false, nil
	}
	group := start.elements[groupIndex]
	groupHooks, _ := group.member("hooks")
	if groupHooks == nil || groupHooks.kind != '[' {
		return data, false, nil
	}
	indices := hookHandlerIndexes(data, groupHooks, record.Command)
	for index := len(indices) - 1; index >= 0; index-- {
		data = removeJSONArrayItem(data, groupHooks, indices[index])
	}
	changed := len(indices) > 0
	if !changed {
		return data, false, nil
	}
	root, err = parseJSONSpans(data)
	if err != nil {
		return nil, false, err
	}
	hooks, _ = root.member("hooks")
	start, _ = hooks.member(event)
	if record.CreatedGroup {
		if groupIndex >= len(start.elements) {
			return nil, false, fmt.Errorf("%s group disappeared during uninstall", event)
		}
		group = start.elements[groupIndex]
		groupHooks, _ = group.member("hooks")
		if groupHooks != nil && groupHooks.kind == '[' && len(groupHooks.elements) == 0 {
			data = removeJSONArrayItem(data, start, groupIndex)
		}
	}
	root, err = parseJSONSpans(data)
	if err != nil {
		return nil, false, err
	}
	hooks, _ = root.member("hooks")
	start, startIndex = hooks.member(event)
	if record.CreatedSessionKey && startIndex >= 0 && start.kind == '[' && len(start.elements) == 0 {
		data = removeJSONMember(data, hooks, startIndex)
	}
	root, err = parseJSONSpans(data)
	if err != nil {
		return nil, false, err
	}
	hooks, hooksIndex = root.member("hooks")
	if record.CreatedRoot && hooksIndex >= 0 && hooks.kind == '{' && len(hooks.members) == 0 {
		data = removeJSONMember(data, root, hooksIndex)
	}
	return data, true, nil
}

func groupHasCommand(data []byte, group *jsonSpan, command string) bool {
	if group == nil || group.kind != '{' {
		return false
	}
	hooks, _ := group.member("hooks")
	if hooks == nil || hooks.kind != '[' {
		return false
	}
	return len(hookHandlerIndexes(data, hooks, command)) > 0
}

func hookHandlerIndexes(data []byte, hooks *jsonSpan, command string) []int {
	var indices []int
	for index, handler := range hooks.elements {
		commandNode, _ := handler.member("command")
		if commandNode == nil {
			continue
		}
		var value string
		if json.Unmarshal(data[commandNode.start:commandNode.end], &value) == nil && value == command {
			indices = append(indices, index)
		}
	}
	return indices
}

func readJSONFile(path string) (map[string]any, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, true, err
	}
	if root == nil {
		root = map[string]any{}
	}
	return root, true, nil
}

func findPosseHookCommand(groups []any, spec hookSpec) string {
	for _, item := range groups {
		group, ok := item.(map[string]any)
		if !ok {
			continue
		}
		handlers, _ := group["hooks"].([]any)
		for _, value := range handlers {
			handler, ok := value.(map[string]any)
			if !ok {
				continue
			}
			command, _ := handler["command"].(string)
			if strings.Contains(command, "posse") && strings.HasSuffix(command, " "+spec.Subcommand) {
				return command
			}
		}
	}
	return ""
}

func fileMode(path string, fallback os.FileMode) os.FileMode {
	info, err := os.Stat(path)
	if err != nil {
		return fallback
	}
	return info.Mode().Perm()
}

func previousHook(records []setupHookRecord, path string, spec hookSpec) setupHookRecord {
	for _, record := range records {
		if record.spec() == spec && (record.Path == path || record.ConfiguredPath == path) {
			return record
		}
	}
	return setupHookRecord{Path: path}
}

func replaceHookRecord(records []setupHookRecord, record setupHookRecord) []setupHookRecord {
	for index, existing := range records {
		if existing.spec() != record.spec() {
			continue
		}
		if existing.Path == record.Path || existing.Path == record.ConfiguredPath || existing.ConfiguredPath == record.ConfiguredPath && record.ConfiguredPath != "" {
			records[index] = record
			return records
		}
	}
	return append(records, record)
}

func removeHookRecord(records []setupHookRecord, path string, spec hookSpec) []setupHookRecord {
	filtered := make([]setupHookRecord, 0, len(records))
	for _, record := range records {
		if record.spec() != spec || record.ConfiguredPath != path && record.Path != path {
			filtered = append(filtered, record)
		}
	}
	return filtered
}

func appendUnique(values []string, value string) []string {
	for _, item := range values {
		if item == value {
			return values
		}
	}
	return append(values, value)
}

func appendUniqueStrings(values []string, additions ...string) []string {
	for _, addition := range additions {
		found := false
		for _, value := range values {
			if value == addition {
				found = true
				break
			}
		}
		if !found {
			values = append(values, addition)
		}
	}
	return values
}

func makeSetupDirs(path string) ([]string, error) {
	path = filepath.Clean(path)
	var missing []string
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return nil, fmt.Errorf("%s is not a directory", current)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			return nil, fmt.Errorf("no existing parent for %s", path)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, err
	}
	for left, right := 0, len(missing)-1; left < right; left, right = left+1, right-1 {
		missing[left], missing[right] = missing[right], missing[left]
	}
	return missing, nil
}

func removeCreatedDirs(paths []string) {
	unique := appendUniqueStrings(nil, paths...)
	sort.Slice(unique, func(i, j int) bool { return len(unique[i]) > len(unique[j]) })
	for _, path := range unique {
		_ = os.Remove(path)
	}
}

func jsonHasFailure(data []byte) bool {
	var decoded any
	if json.Unmarshal(data, &decoded) != nil {
		return strings.Contains(strings.ToLower(string(data)), "error") || strings.Contains(strings.ToLower(string(data)), "failed")
	}
	var walk func(any) bool
	walk = func(value any) bool {
		switch item := value.(type) {
		case map[string]any:
			for key, nested := range item {
				if key == "error" && nested != nil && nested != "" {
					return true
				}
				if key == "status" {
					if text, ok := nested.(string); ok && (text == "failed" || text == "error") {
						return true
					}
				}
				if walk(nested) {
					return true
				}
			}
		case []any:
			for _, nested := range item {
				if walk(nested) {
					return true
				}
			}
		}
		return false
	}
	return walk(decoded)
}

type setupHookTarget struct {
	agent string
	path  string
	spec  hookSpec
}

// hookStepEvents maps each hook plan step to the hook event it installs.
var hookStepEvents = map[string]string{"session_start_hook": sessionStartHook.Event, "guard_hook": guardHook.Event}

func (t setupHookTarget) stepName() string {
	if t.spec == guardHook {
		return "guard"
	}
	return "session_start"
}

func (t setupHookTarget) step() string { return t.stepName() + "_hook" }

// setupHookTargets lists every agent hook posse installs: the SessionStart
// role hook and the PreToolUse Worker guard, for Claude Code and Codex.
func setupHookTargets(claudeDir, codexDir string) []setupHookTarget {
	targets := []setupHookTarget{}
	for _, spec := range setupHooks {
		targets = append(targets,
			setupHookTarget{agent: "Claude Code", path: filepath.Join(claudeDir, "settings.json"), spec: spec},
			setupHookTarget{agent: "Codex", path: filepath.Join(codexDir, "hooks.json"), spec: spec})
	}
	return targets
}
