package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/runtime"
	"github.com/thanhbinh1905/posse/internal/store"
)

type Service struct {
	Home          string
	Version       string
	updateURL     string
	updateClient  *http.Client
	Herdr         herdr.Adapter
	Progress      runtime.ProgressSource
	herdrContext  func() bool
	removeMount   func(context.Context, string, string) error
	pendingLead   *leadExecPlan
	confirm       confirmFunc
	updateConfirm confirmFunc
	reexecUpdate  func(string, []string, []string) error
}

func New(home string, adapter herdr.Adapter) *Service {
	return &Service{
		Home: home, Herdr: adapter, Progress: runtime.SystemProgress{}, herdrContext: insideHerdr,
		removeMount: func(ctx context.Context, root, path string) error {
			_, err := gitOutput(ctx, root, "worktree", "remove", path)
			return err
		},
	}
}

func (s *Service) CLI() *axi.App {
	root := s.commands()
	s.guardWorkers(root, nil)
	structureStoreErrors(root)
	return &axi.App{Name: "posse", Root: root}
}

// Apply storage error classification at every command boundary, including
// reconcile failures from preparation and failures after a Signal is recorded.
func structureStoreErrors(command *axi.Command) {
	if command.Handler != nil {
		handler := command.Handler
		command.Handler = func(ctx *axi.Context, args []string) error {
			return normalizeCommandError(handler(ctx, args))
		}
	}
	for _, subcommand := range command.Subcommands {
		structureStoreErrors(subcommand)
	}
}

func normalizeCommandError(err error) error {
	var structured *axi.Error
	if errors.As(err, &structured) || !store.IsBusy(err) {
		return err
	}
	return axi.Failure("store_busy", err.Error(), true, "Retry the command after the store is available")
}

func (s *Service) commands() *axi.Command {
	root := &axi.Command{Usage: "$ [--full] [--json]", Summary: "A local-first Lead and Rider CLI for Herdr. `posse --full` prints the full Project dashboard."}
	root.Handler = s.home
	root.Subcommands = []*axi.Command{
		{Name: "up", Usage: "$ up [--<kind>] [--replace] [--name <n>] [--yes]", Summary: "Register a repository or workspace folder after one confirmation, and start its Lead.", Handler: s.up},
		{Name: "down", Usage: "$ down", Summary: "Stop this Project's Lead and Lookout; nothing restarts them until `posse up`.", Handler: s.down},
		{Name: "lead", Summary: "Print the Lead's instructions and Identity.", Handler: s.lead},
		{Name: "preferences", Summary: "Inspect and update layered User and Project preferences.", Subcommands: []*axi.Command{
			{Name: "show", Usage: "$ preferences show [--project <name>]", Summary: "Show effective Lead and Rider preferences with their sources.", Handler: s.preferencesShow},
			{Name: "path", Usage: "$ preferences path <lead|rider> [--project <name>]", Summary: "Show canonical and legacy source paths for a role.", Handler: s.preferencesPath},
			{Name: "set", Usage: "$ preferences set <lead|rider> --file <file> [--project <name>] [--user-approved <quote>]", Summary: "Write preferences atomically; a Lead needs a User quote.", Handler: s.preferencesSet},
			{Name: "move", Usage: "$ preferences move <lead|rider> [--project <name>] [--user-approved <quote>]", Summary: "Move one legacy Playbook file without overwriting existing preferences.", Handler: s.preferencesMove},
		}},
		{Name: "playbook", Summary: "Deprecated alias for `posse preferences`; legacy files remain readable.", Subcommands: []*axi.Command{
			{Name: "show", Usage: "$ playbook show [--project <name>]", Summary: "Deprecated alias for `posse preferences show`.", Handler: s.playbookShow},
			{Name: "path", Usage: "$ playbook path <lead|rider> [--project <name>]", Summary: "Deprecated alias for `posse preferences path`.", Handler: s.playbookPath},
			{Name: "set", Usage: "$ playbook set <lead|rider> --file <file> [--project <name>] [--user-approved <quote>]", Summary: "Deprecated alias for `posse preferences set`.", Handler: s.playbookSet},
		}},
		{Name: "lowkey", Usage: "$ lowkey on|off|status", Summary: "Toggle or inspect persisted Lead lowkey mode without restarting.", Handler: s.lowkey},
		{Name: "roster", Usage: "$ roster [--all] [--full]", Summary: "List Tasks in this Project or every Project.", Handler: s.ls},
		{Name: "show", Usage: "$ show <task> [--full]", Summary: "Inspect a Task, undelivered messages, Signals and transitions.", Handler: s.show},
		{Name: "ask", Usage: "$ ask <task> <question> --option <choice> --option <choice>...", Summary: "Put a Rider's question to the User as a Decision.", Handler: s.ask},
		{Name: "decisions", Usage: "$ decisions [--all]", Summary: "List pending Decisions, or include answered ones.", Handler: s.decisions},
		{Name: "decide", Usage: "$ decide <decision> <option> [--user-approved <quote>]", Summary: "Record the User's answer and notify the Lead.", Handler: s.decide},
		{Name: "apply", Usage: "$ apply <decision>", Summary: "Carry out an answered Leftover or closed-PR Decision.", Handler: s.applyDecision},
		{Name: "dispatch", Usage: "$ dispatch --brief <file> [--profile <name>]", Summary: "Preview Dispatch Rule and Profile selection.", Handler: s.dispatch},
		{Name: "ride", Usage: "$ ride --brief <file> --name <short> [--profile p] [--from-leftover <decision>]", Summary: "Start a Rider from a Brief with a short name.", Handler: s.spawn},
		{Name: "holler", Usage: "$ holler <working|needs-decision|done|failed> <note>", Summary: "Record a Rider's Signal.", Handler: s.signal},
		{Name: "publish", Usage: "$ publish [--repo <member>] <summary> [--verify <command -> result>] [--proof <markdown>] [--risk <markdown>]", Summary: "Push this PR-mode Ship Task's branch and open or reuse its pull request.", Handler: s.publish},
		{Name: "brief", Summary: "Reprint this Rider's launch Brief.", Handler: s.brief},
		{Name: "send", Usage: "$ send <task> <message> [--queue]", Summary: "Deliver to an unfocused ready Rider, steering supported kinds mid-turn; --queue waits for idle.", Handler: s.send},
		{Name: "peek", Usage: "$ peek <task> [--lines n]", Summary: "Read recent Rider output.", Handler: s.peek},
		{Name: "keys", Usage: "$ keys <task> <key...>", Summary: "Send logical keys to a blocked Rider.", Handler: s.keys},
		{Name: "interrupt", Summary: "Interrupt the Rider's current turn.", Handler: s.interrupt},
		{Name: "relaunch", Usage: "$ relaunch <task> [--profile <name>]", Summary: "Restart the Rider in the same Mount, optionally with another Profile.", Handler: s.relaunch},
		{Name: "land", Summary: "Land a completed Ship Task.", Handler: s.land},
		{Name: "sync", Summary: "Fast-forward the Project's default branch from origin when safe.", Handler: s.sync},
		{Name: "unsaddle", Summary: "Teardown a finished Task and release its Mount.", Handler: s.teardown},
		{Name: "lookout", Usage: "$ lookout [--ack <ids>] [--timeout ms] [--quiet-routine] [--requeue <ids>] [--poll-only]", Summary: "Acknowledge requested Notices and wait; unrelated maintenance failures arrive as Notices.", Handler: s.wait},
		{Name: "remuda", Summary: "List a Project's Remuda or prune idle Mounts.", Handler: s.remuda},
		{Name: "sweep", Usage: "$ sweep [--yes]", Summary: "Review or close orphan Task panes.", Handler: s.sweep},
		{Name: "ack", Usage: "$ ack <id...|all>", Summary: "Acknowledge open Notices.", Handler: s.ack},
		{Name: "project", Summary: "Inspect or change a Project's settings.", Subcommands: []*axi.Command{
			{Name: "add", Usage: "$ project add [--name <n>]", Summary: "Register this repository or workspace folder without starting its Lead.", Handler: s.projectAdd},
			{Name: "scan", Usage: "$ project scan [<name>]", Summary: "Re-detect a workspace Project's member repositories.", Handler: s.projectScan},
			{Name: "show", Summary: "Show a Project's effective config.", Handler: s.projectShow},
			{Name: "move", Summary: "Update a moved Project's root path.", Handler: s.projectMove},
			{Name: "remove", Usage: "$ project remove <name> [--yes]", Summary: "Unregister a Project that has no Tasks or Mounts (dry run without --yes).", Handler: s.projectRemove},
		}},
		{Name: "config", Summary: "Inspect and update schema-validated configuration.", Subcommands: []*axi.Command{
			{Name: "schema", Summary: "List config keys, types, defaults and meanings.", Handler: s.configSchema},
			{Name: "show", Usage: "$ config show [--project <n>] [--effective]", Summary: "Show config or merged effective values.", Handler: s.configShow},
			{Name: "set", Usage: "$ config set <key> <value> [--project <n>] [--user-approved <quote>]", Summary: "Set one validated config value; Lead needs a User quote for user-only keys.", Handler: s.configSet},
			{Name: "unset", Usage: "$ config unset <key> [--project <n>] [--user-approved <quote>]", Summary: "Remove one config value; Lead needs a User quote for user-only keys.", Handler: s.configUnset},
		}},
		{Name: "update", Usage: "$ update [--check] [--version vX.Y.Z] [--force] [--stop-lookouts]", Summary: "Check or install a verified GitHub release.", Handler: s.update},
		{Name: "_update-preflight", Hidden: true, Handler: s.updatePreflight},
		{Name: "setup", Summary: "Install or update the Herdr plugin, skills and hooks; optionally offer the Agents sidebar layout (--check previews; --exit-code returns 3 for required changes; --human prints a checklist).", Handler: s.setup},
		{Name: "recover", Usage: "$ recover [--all|--rebuild]", Summary: "Recover Riders after a Herdr restart or rebuild state from Task snapshots.", Handler: s.recover},
		{Name: "_context", Summary: "Print Lead or Rider context inside a posse pane.", Hidden: true, Handler: s.context},
		{Name: "doctor", Summary: "Check Herdr, plugin, config, database and landing tools.", Handler: s.doctor},
	}
	return root
}

func (s *Service) homePath() (string, error) {
	if s.Home != "" {
		return filepath.Clean(s.Home), nil
	}
	return config.DefaultHome()
}

func (s *Service) openDB() (*store.DB, string, error) {
	home, err := s.homePath()
	if err != nil {
		return nil, "", err
	}
	db, err := store.Open(home)
	return db, home, schemaFailure(err)
}

func (s *Service) projectForCWD(ctx context.Context, db *store.DB) (store.Project, error) {
	dir, err := currentDir()
	if err != nil {
		return store.Project{}, err
	}
	project, found, err := registeredProjectFor(ctx, db, dir)
	if err != nil || found {
		return project, err
	}
	if paneID := os.Getenv("HERDR_PANE_ID"); paneID != "" {
		if task, taskErr := db.TaskByPane(ctx, paneID); taskErr == nil {
			return db.ProjectByID(ctx, task.ProjectID)
		}
	}
	return store.Project{}, axi.Failure("project_unknown", "this folder is not registered as a Project", false, "Run `posse up` here to register it and start its Lead, or `posse project add` to register it only")
}

func (s *Service) projectByName(ctx context.Context, db *store.DB, name string) (store.Project, error) {
	project, err := db.ProjectByName(ctx, name)
	if store.IsNotFound(err) {
		return store.Project{}, axi.Failure("project_unknown", fmt.Sprintf("unknown Project %q", name), false, "Run `posse` to list registered Projects", "Run `posse up` or `posse project add` in its folder to register it")
	}
	return project, err
}

// reconcileProject recovers a real Herdr restart before proceeding with a
// generation-mismatched snapshot. A delayed event from the old server instead
// retries against the current server, without rolling pane ids backward.
// Only userStart (`posse up`) may recover a Project whose recovery is held;
// other callers get errRecoveryHeld and must not reconcile its stale panes.
func (s *Service) reconcileProject(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, userStart bool) (runtime.RunResult, error) {
	if err := ensureHeldMountLocks(ctx, db, project); err != nil {
		return runtime.RunResult{}, err
	}
	// No snapshot taken during a recovery claim is a complete project view.
	if err := waitProjectRecovery(ctx, db, project); err != nil {
		return runtime.RunResult{}, err
	}
	if !project.IsWorkspace() {
		if snapshot, err := s.snapshot(ctx); err == nil {
			if tasks, err := db.LiveTasks(ctx, project.ID); err == nil && riderGroupClosed(snapshot, project, tasks) {
				if !userStart && recoveryHeld(project, cfg) {
					return runtime.RunResult{}, errRecoveryHeld
				}
				home, err := s.homePath()
				if err != nil {
					return runtime.RunResult{}, err
				}
				if _, err := s.recoverProject(ctx, db, home, project); err != nil {
					return runtime.RunResult{}, fmt.Errorf("recover Project %s after Herdr group close: %w", project.Name, err)
				}
				// Concurrent workspace.closed hooks wait for the recovery owner;
				// a partial snapshot would mark still-restoring Riders lost.
				if err := waitProjectRecovery(ctx, db, project); err != nil {
					return runtime.RunResult{}, err
				}
				// `posse up` holds the Lead start claim, so recovery leaves the
				// Lead to it and the Lead is still missing here.
				if !userStart {
					fresh, err := s.snapshot(ctx)
					if err != nil {
						return runtime.RunResult{}, err
					}
					current, err := db.ProjectByID(ctx, project.ID)
					if err != nil {
						return runtime.RunResult{}, err
					}
					currentTasks, err := db.LiveTasks(ctx, project.ID)
					if err != nil {
						return runtime.RunResult{}, err
					}
					if riderGroupClosed(fresh, current, currentTasks) {
						return runtime.RunResult{}, fmt.Errorf("lead still missing after Herdr group recovery for %s", project.Name)
					}
				}
			}
		}
	}
	run := func() (runtime.RunResult, error) {
		return runtime.Run(ctx, db, s.Herdr, project.ID, duration(cfg.Defaults.StallAfter), duration(cfg.Defaults.IdleAfter), time.Now(), s.Progress)
	}
	result, err := run()
	if err != nil {
		return result, err
	}
	if !result.GenerationMismatch {
		return result, s.retryPendingLaunches(ctx, db, project, cfg, result.Snapshot)
	}
	current, err := s.snapshot(ctx)
	if err != nil {
		return result, err
	}
	recorded, err := db.ProjectServerStartedAt(ctx, project.ID)
	if err != nil {
		return result, err
	}
	if current.ServerStartedAt != "" && recorded != "" && current.ServerStartedAt != recorded {
		if !userStart && recoveryHeld(project, cfg) {
			return result, errRecoveryHeld
		}
		home, err := s.homePath()
		if err != nil {
			return result, err
		}
		if _, err := s.recoverProject(ctx, db, home, project); err != nil {
			return result, fmt.Errorf("recover Project %s after Herdr restart: %w", project.Name, err)
		}
	}
	result, err = run()
	if err == nil && result.GenerationMismatch {
		return result, fmt.Errorf("project %s Herdr generation changed during recovery; retry reconcile", project.Name)
	}
	if err != nil {
		return result, err
	}
	return result, s.retryPendingLaunches(ctx, db, project, cfg, result.Snapshot)
}

func waitProjectRecovery(ctx context.Context, db *store.DB, project store.Project) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		claim, err := db.ProjectRecovery(ctx, project.ID)
		if err != nil {
			return err
		}
		if claim.OwnerPID == 0 || claim.OwnerPID == os.Getpid() || !processAlive(claim.OwnerPID) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("project recovery is still running for %s", project.Name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (s *Service) prepareProject(ctx context.Context, db *store.DB, project store.Project) (config.Config, error) {
	return s.prepareProjectMode(ctx, db, project, true, true)
}

func (s *Service) prepareProjectForTask(ctx context.Context, db *store.DB, project store.Project) (config.Config, error) {
	return s.prepareProjectMode(ctx, db, project, true, false)
}

// PR inspections keep their refresh behavior without attempting launch recovery.
func (s *Service) prepareProjectInspection(ctx context.Context, db *store.DB, project store.Project) (config.Config, error) {
	return s.prepareProjectMode(ctx, db, project, false, true)
}

// Notice and inspection commands reconcile observations, but never start a
// Rider, retry a launch Brief, or wait for a launch-capable recovery owner.
func (s *Service) prepareProjectObservation(ctx context.Context, db *store.DB, project store.Project) (config.Config, error) {
	return s.prepareProjectMode(ctx, db, project, false, false)
}

func leadAgentStarted(project store.Project, snapshot herdr.Snapshot) bool {
	if project.LeadPaneID == "" {
		return false
	}
	for _, pane := range snapshot.Panes {
		if pane.PaneID == project.LeadPaneID && pane.WorkspaceID == project.HerdrWorkspaceID && pane.Agent != "" && pane.AgentStatus != "unknown" {
			return true
		}
	}
	return false
}

func (s *Service) prepareProjectMode(ctx context.Context, db *store.DB, project store.Project, allowRecovery, pollRepositories bool) (config.Config, error) {
	return s.prepareProjectModeWithRecoveryPolicy(ctx, db, project, allowRecovery, pollRepositories, true)
}

// prepareProjectLocalRecovery reconciles durable local state without invoking
// forge CLIs or refreshing repositories before the Lead is running.
func (s *Service) prepareProjectLocalRecovery(ctx context.Context, db *store.DB, project store.Project) (config.Config, error) {
	return s.prepareProjectModeWithRecoveryPolicy(ctx, db, project, false, false, false)
}

func (s *Service) prepareProjectModeWithRecoveryPolicy(ctx context.Context, db *store.DB, project store.Project, allowRecovery, pollRepositories, allowRunningLeadNetworkRecovery bool) (config.Config, error) {
	if _, err := os.Stat(project.Root); err != nil {
		_ = db.UpdateProjectStatus(ctx, project.ID, "missing")
		_ = s.regenerateProjects(ctx, db)
		return config.Config{}, axi.Failure("project_missing", fmt.Sprintf("Project %s path no longer exists: %s", project.Name, project.Root), false, "Run `posse project move "+project.Name+" <new-root>`")
	}
	home, err := s.homePath()
	if err != nil {
		return config.Config{}, err
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return config.Config{}, configError(err)
	}
	if _, err := s.raiseExpiredMessageDeliveryNotices(ctx, db, project); err != nil {
		return cfg, err
	}
	herdrReady := false
	allowNetworkRecovery := allowRecovery
	if s.Herdr != nil {
		if err := s.Herdr.CheckProtocol(ctx); err != nil {
			if !isHerdrUnavailable(err) {
				return cfg, herdrError(err)
			}
		} else {
			var result runtime.RunResult
			var runErr error
			if allowRecovery {
				result, runErr = s.reconcileProject(ctx, db, project, cfg, false)
			} else {
				claim, err := db.ProjectRecovery(ctx, project.ID)
				if err != nil {
					return cfg, err
				}
				if claim.OwnerPID != 0 && processAlive(claim.OwnerPID) {
					runErr = errRecoveryDeferred
				} else {
					result, runErr = runtime.Run(ctx, db, s.Herdr, project.ID, duration(cfg.Defaults.StallAfter), duration(cfg.Defaults.IdleAfter), time.Now(), s.Progress)
					if runErr == nil && result.GenerationMismatch {
						runErr = errRecoveryDeferred
					}
				}
			}
			if runErr != nil {
				// A held Project keeps its recorded panes for `posse up`.
				if !isHerdrUnavailable(runErr) && !errors.Is(runErr, errRecoveryHeld) && !errors.Is(runErr, errRecoveryDeferred) {
					return cfg, herdrError(runErr)
				}
			} else {
				herdrReady = true
				allowNetworkRecovery = allowNetworkRecovery || allowRunningLeadNetworkRecovery && leadAgentStarted(project, result.Snapshot)
				if err := s.reconcileTaskPanes(ctx, db, project, result.Snapshot); err != nil {
					return cfg, err
				}
				if err := s.reconcileIntentsMode(ctx, db, project, cfg, result.Snapshot, allowRecovery, allowNetworkRecovery); err != nil {
					return cfg, err
				}
				fresh, err := db.ProjectByID(ctx, project.ID)
				if err != nil {
					return cfg, err
				}
				s.relabelProjectTabs(ctx, db, fresh)
				if err := s.deliverQueuedMessagesWithConfig(ctx, db, fresh, result.Snapshot, cfg); err != nil && !isHerdrUnavailable(err) {
					return cfg, herdrError(err)
				}
				if err := s.regenerateProjects(ctx, db); err != nil {
					return cfg, err
				}
			}
		}
	}
	if pollRepositories {
		if err := s.pollProjectPullRequests(ctx, db, project, cfg, false); err != nil {
			return cfg, err
		}
	}
	if err := s.raiseNoticeDecisions(ctx, db, project); err != nil {
		return cfg, err
	}
	if pollRepositories {
		if _, err := s.syncProjectRoot(ctx, db, project, cfg, false); err != nil {
			return cfg, err
		}
		_, _ = s.availableUpdate(ctx, db, &project)
	}
	if herdrReady {
		if pollRepositories {
			if err := s.autoTeardownLandedTasks(ctx, db, project, cfg); err != nil {
				return cfg, err
			}
			if err := waitForActiveTeardowns(ctx, db, project.ID); err != nil {
				return cfg, err
			}
		}
		fresh, err := db.ProjectByID(ctx, project.ID)
		if err != nil {
			return cfg, err
		}
		if err := s.deliverNotices(ctx, db, fresh); err != nil && !isHerdrUnavailable(err) {
			return cfg, herdrError(err)
		}
		if err := s.regenerateProjects(ctx, db); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func (s *Service) requireLead(ctx context.Context, db *store.DB, project store.Project) error {
	if s.herdrContext != nil && s.herdrContext() {
		paneID := os.Getenv("HERDR_PANE_ID")
		if paneID == "" || paneID != project.LeadPaneID {
			return axi.Failure("lead_only", "this command must run from the Project Lead pane", false, "Use `posse holler` from a Rider pane")
		}
	}
	if cwdInsideMount(ctx, db) {
		return axi.Failure("lead_only", "Lead-only commands cannot run from inside a Mount", false, "Use the Project Lead pane")
	}
	return nil
}

func insideHerdr() bool {
	return hasHerdrEnvironment() || hasHerdrAncestor(os.Getpid())
}

func hasHerdrEnvironment() bool {
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(key, "HERDR_") {
			return true
		}
	}
	return false
}

func hasHerdrAncestor(pid int) bool {
	return hasHerdrAncestorAt("/proc", pid)
}

func hasHerdrAncestorAt(procRoot string, pid int) bool {
	for depth := 0; depth < 64 && pid > 1; depth++ {
		stat, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
		if err != nil {
			return false
		}
		end := strings.LastIndexByte(string(stat), ')')
		if end < 0 {
			return false
		}
		fields := strings.Fields(string(stat)[end+1:])
		if len(fields) < 2 {
			return false
		}
		parent, err := strconv.Atoi(fields[1])
		if err != nil || parent <= 1 {
			return false
		}
		processDir := filepath.Join(procRoot, strconv.Itoa(parent))
		if executable, err := os.Readlink(filepath.Join(processDir, "exe")); err == nil && strings.EqualFold(filepath.Base(strings.TrimSuffix(executable, " (deleted)")), "herdr") {
			return true
		}
		if name, err := os.ReadFile(filepath.Join(processDir, "comm")); err == nil && strings.EqualFold(strings.TrimSpace(string(name)), "herdr") {
			return true
		}
		pid = parent
	}
	return false
}

func (s *Service) currentTask(ctx context.Context, db *store.DB, project store.Project, identifier string) (store.Task, error) {
	task, err := db.Task(ctx, project.ID, identifier)
	var ambiguous *store.AmbiguousTaskName
	if errors.As(err, &ambiguous) {
		help := make([]string, 0, len(ambiguous.IDs))
		for _, id := range ambiguous.IDs {
			help = append(help, "Run `posse show "+id+"` to inspect this Task")
		}
		return store.Task{}, axi.Failure("task_ambiguous", err.Error(), false, help...)
	}
	if store.IsNotFound(err) {
		return store.Task{}, axi.Failure("task_unknown", fmt.Sprintf("unknown Task %q", identifier), false, "Run `posse roster` to list Tasks")
	}
	return task, err
}

func gitTop(ctx context.Context, path string) (string, error) {
	args := []string{"rev-parse", "--show-toplevel"}
	if path != "" {
		args = append([]string{"-C", path}, args...)
	}
	output, err := commandOutputArgs(ctx, "", "git", args...)
	if err != nil {
		return "", axi.Failure("not_in_git_repository", "current directory is not inside a Git repository", false, "Run this command from a Project checkout")
	}
	root, err := filepath.Abs(strings.TrimSpace(string(output)))
	if err != nil {
		return "", err
	}
	return filepath.Clean(root), nil
}

func gitOutput(ctx context.Context, root string, args ...string) (string, error) {
	commandArgs := append([]string{"-C", root}, args...)
	output, err := commandStdoutArgs(ctx, "", "git", commandArgs...)
	if err != nil {
		if details := strings.TrimSpace(output); details != "" {
			err = fmt.Errorf("%w: %s", err, details)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(output), nil
}

func gitOutputRaw(ctx context.Context, root string, args ...string) (string, error) {
	commandArgs := append([]string{"-C", root}, args...)
	output, err := commandStdoutArgs(ctx, "", "git", commandArgs...)
	if err != nil {
		if details := strings.TrimSpace(output); details != "" {
			err = fmt.Errorf("%w: %s", err, details)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return output, nil
}

func defaultBranch(ctx context.Context, root string) (string, error) {
	branch, err := gitOutput(ctx, root, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if err == nil {
		return strings.TrimPrefix(branch, "origin/"), nil
	}
	branch, err = gitOutput(ctx, root, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	if branch == "" {
		return "", errors.New("cannot determine the Project default branch from a detached checkout")
	}
	return branch, nil
}

func configError(err error) error {
	var invalid *config.InvalidError
	if errors.As(err, &invalid) {
		return axi.Failure("config_invalid", invalid.Error(), false, "Edit the named config file and retry")
	}
	return err
}

func herdrError(err error) error {
	var apiError *herdr.Error
	if errors.As(err, &apiError) {
		code := apiError.Code
		if code == "" {
			code = "herdr_error"
		}
		message := apiError.Message
		if apiError.Cause != nil {
			message = apiError.Error()
		}
		if code == "pane_not_found" || code == "agent_pane_not_found" {
			return axi.Failure(code, message, true, "The pane may have closed during this command. Run `posse recover --all`; then inspect the Task and relaunch it if it still holds a Mount")
		}
		return axi.Failure(code, message, code == "herdr_unavailable", "Run `posse doctor` to inspect Herdr connectivity")
	}
	return err
}

func isHerdrUnavailable(err error) bool {
	var apiError *herdr.Error
	if errors.As(err, &apiError) {
		return apiError.Code == "herdr_unavailable"
	}
	var commandError *axi.Error
	return errors.As(err, &commandError) && commandError.Code == "herdr_unavailable"
}

func duration(value string) time.Duration {
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0
	}
	return parsed
}

func resolvedProfileArgs(profile config.Profile, kind config.Kind) []string {
	args := make([]string, 0, len(profile.Args)+len(kind.ModelArgs)+len(kind.EffortArgs))
	args = append(args, profile.Args...)
	if profile.Model != "" {
		args = append(args, substituteArgument(kind.ModelArgs, "{model}", profile.Model)...)
	}
	if profile.Effort != "" {
		args = append(args, substituteArgument(kind.EffortArgs, "{effort}", profile.Effort)...)
	}
	return args
}

func substituteArgument(arguments []string, token, value string) []string {
	if value == "" {
		return nil
	}
	result := make([]string, len(arguments))
	for index, argument := range arguments {
		result[index] = strings.ReplaceAll(argument, token, value)
	}
	return result
}

var safeName = regexp.MustCompile(`[^a-z0-9_-]+`)

func projectName(value string) string {
	name := strings.ToLower(value)
	name = safeName.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-_ ")
	if len(name) > 32 {
		name = name[:32]
	}
	return name
}
