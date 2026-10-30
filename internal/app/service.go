package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/runtime"
	"github.com/thanhbinh1905/posse/internal/store"
)

type Service struct {
	Home                 string
	Version              string
	updateURL            string
	updateClient         *http.Client
	Herdr                herdr.Adapter
	Progress             runtime.ProgressSource
	herdrContext         func() bool
	removeSandbox        func(context.Context, string, string) error
	pendingLead          *leadExecPlan
	confirm              confirmFunc
	updateConfirm        confirmFunc
	reexecUpdate         func(string, []string, []string) error
	updateSetup          func(context.Context, string, string, bool) ([]byte, error)
	maintenanceMu        sync.Mutex
	maintenance          map[string]bool
	maintenanceCancels   map[string]context.CancelFunc
	maintenanceWG        sync.WaitGroup
	teardownMu           sync.Mutex
	teardowns            map[string]chan struct{}
	taskContainmentSetup func(context.Context, store.Task, string, string) (string, error)
	// DetachQueuedLaunch runs each queued Task start in its own session. The
	// posse binary sets it; unit tests keep the start in-process.
	DetachQueuedLaunch bool
}

func New(home string, adapter herdr.Adapter) *Service {
	return &Service{
		Home: home, Herdr: adapter, Progress: runtime.SystemProgress{}, herdrContext: insideHerdr,
		removeSandbox: func(ctx context.Context, root, path string) error {
			_, err := gitOutput(ctx, root, "worktree", "remove", path)
			return err
		},
	}
}

func (s *Service) CLI() *axi.App {
	root := s.commands()
	s.guardWorkers(root, nil)
	addRemovedVocabularyCommands(root, s.legacyPollerAdapter)
	s.structureStoreErrors(root)
	return &axi.App{Name: "posse", Root: root}
}

// Apply storage error classification at every command boundary, including
// reconcile failures from preparation and failures after a Signal is recorded.
func (s *Service) structureStoreErrors(command *axi.Command) {
	if command.Handler != nil {
		handler := command.Handler
		name := command.Name
		command.Handler = func(ctx *axi.Context, args []string) error {
			ctx.Context = withObservationWarningWriter(ctx.Context, ctx.ErrOut)
			err := normalizeCommandError(handler(ctx, args))
			if err == nil {
				flushDeferredProjectObservation(ctx.Context)
				s.superviseWatchRuntimes(ctx, name, args)
			}
			return err
		}
	}
	for _, subcommand := range command.Subcommands {
		s.structureStoreErrors(subcommand)
	}
}

func normalizeCommandError(err error) error {
	var structured *axi.Error
	if errors.As(err, &structured) || !store.IsBusy(err) {
		return err
	}
	return axi.Failure("store_busy", err.Error(), true, "Retry the command after the store is available")
}

func deferObservationContention(ctx *axi.Context, err error, message string) error {
	// File-lock waits guard maintenance state, so keep them visible to callers.
	var lockWait *fileLockWaitError
	if err == nil || (!store.IsOnlyBusy(err) && !isOnlyStateRace(err)) || errors.As(err, &lockWait) {
		return err
	}
	fmt.Fprintln(ctx.ErrOut, message)
	return nil
}

func isOnlyStateRace(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !isOnlyStateRace(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return isOnlyStateRace(wrapped.Unwrap())
	}
	return err == store.ErrStateRace
}

func (s *Service) commands() *axi.Command {
	root := &axi.Command{Usage: "$ [--full] [--json] [--format human|toon|json]", Summary: "A local-first Lead and Worker CLI for Herdr. `posse --full` prints the full Project dashboard."}
	root.Handler = s.home
	root.Subcommands = []*axi.Command{
		{Name: "up", Usage: "$ up [--<kind>] [--replace] [--name <n>] [--yes] | up --all", Summary: "Register a repository or workspace folder after one confirmation, and start its Lead; --all restores the Lead, Workers and Watch of every Project that is not down.", Handler: s.up},
		{Name: "down", Usage: "$ down [--plan | --execute --user-approved <quote>]", Summary: "Stop this Project's Lead and Watch; nothing restarts them until `posse up`. --plan previews tearing down every Task; --execute does it.", Handler: s.down},
		{Name: "lead", Summary: "Print the Lead's instructions and Identity.", Handler: s.lead},
		{Name: "knowledge", Summary: "Inspect and curate opt-in User and Project knowledge.", Subcommands: []*axi.Command{
			{Name: "show", Usage: "$ knowledge show [<id>] [--project <name>|--user]", Summary: "Show knowledge entries with provenance and the startup budget, or one entry.", Handler: s.knowledgeShow},
			{Name: "set", Usage: "$ knowledge set <id> --file <file> --source <user|report:<task>|ref:<reference>> [--when <condition>] [--verified <YYYY-MM-DD>] [--project <name>|--user]", Summary: "Write one knowledge entry within the startup budget; Workers cannot write.", Handler: s.knowledgeSet},
			{Name: "archive", Usage: "$ knowledge archive <id> --reason <why> [--project <name>|--user]", Summary: "Move an entry to the never-loaded archive with provenance.", Handler: s.knowledgeArchive},
		}},
		{Name: "preferences", Summary: "Inspect and update layered User and Project preferences.", Subcommands: []*axi.Command{
			{Name: "show", Usage: "$ preferences show [--project <name>]", Summary: "Show effective Lead and Worker preferences with their sources.", Handler: s.preferencesShow},
			{Name: "path", Usage: "$ preferences path <lead|worker> [--project <name>]", Summary: "Show canonical and legacy source paths for a role.", Handler: s.preferencesPath},
			{Name: "set", Usage: "$ preferences set <lead|worker> --file <file> [--project <name>] [--user-approved <quote>]", Summary: "Write preferences atomically; a Lead needs a User quote.", Handler: s.preferencesSet},
			{Name: "move", Usage: "$ preferences move <lead|worker> [--project <name>] [--user-approved <quote>]", Summary: "Move legacy preferences without overwriting the destination.", Handler: s.preferencesMove},
		}},
		{Name: "lowkey", Usage: "$ lowkey on|off|status", Summary: "Toggle or inspect persisted Lead lowkey mode without restarting.", Handler: s.lowkey},
		{Name: "list", Usage: "$ list [--all] [--full]", Summary: "List Tasks in this Project or every Project.", Handler: s.ls},
		{Name: "show", Usage: "$ show <task> [--full]", Summary: "Inspect a Task, model-error recovery episode, undelivered messages, Signals and transitions.", Handler: s.show},
		{Name: "diff", Usage: "$ diff <task> [--stat]", Summary: "Print a Task's diff against its base, or a per-Member diff for a Workspace Task.", Handler: s.diff},
		{Name: "accept", Usage: "$ accept <task> [--user-approved <quote>]", Summary: "Record User acceptance of a publication-bound Research Report after independent review.", Handler: s.acceptReport},
		{Name: "ask", Usage: "$ ask <task> <question> --option <choice> --option <choice>...", Summary: "Put a Worker's question to the User as a Decision.", Handler: s.ask},
		{Name: "decisions", Usage: "$ decisions [--all]", Summary: "List pending Decisions, or include answered ones.", Handler: s.decisions},
		{Name: "decide", Usage: "$ decide <decision> <option> [--user-approved <quote>]", Summary: "Record the User's answer and notify the Lead.", Handler: s.decide},
		{Name: "apply", Usage: "$ apply <decision>", Summary: "Carry out an answered Orphan or closed-PR Decision.", Handler: s.applyDecision},
		{Name: "dispatch", Usage: "$ dispatch --brief <file> [--name <short>] [--profile <name>]", Summary: "Preview Dispatch Rule, Profile and Task Name selection.", Handler: s.dispatch},
		{Name: "stats", Summary: "Show each Profile's landed rate, median time to done, tokens per landed Task and relaunches over the last 30 days.", Handler: s.stats},
		{Name: "workflow", Summary: "Inspect and advance User-owned workflow runs.", Subcommands: []*axi.Command{
			{Name: "show", Usage: "$ workflow show <name> [--project <name>]", Summary: "Show a resolved workflow profile, its source and digest.", Handler: s.workflowShow},
			{Name: "status", Usage: "$ workflow status [<run>]", Summary: "Show workflow run position, stage Tasks, results and advance history.", Handler: s.workflowStatus},
			{Name: "advance", Usage: "$ workflow advance <run> --to <next-stage> --user-approved <quote> [--abandon <task>]...", Summary: "Record User approval to advance a run to its next profile stage.", Handler: s.workflowAdvance},
		}},
		{Name: "start", Usage: "$ start --brief <file> [--name <short>] [--profile p] [--from-orphan <decision>] [--user-approved <quote>] [--queue]", Summary: "Start a Worker from a Brief with an optional display name. `--queue` admits it durably until prerequisites and capacity permit dispatch.", Handler: s.spawn},
		{Name: "signal", Usage: "$ signal <working|needs-decision|done|failed> <note> [--turn <token>] [--findings <file>] [--resolutions <file>] [--report <file>] [--no-change]", Summary: "Record a Worker's Signal; done requires the current turn token and is refused while queued instructions remain undelivered.", Handler: s.signal},
		{Name: "publish", Usage: "$ publish [--repo <member>] [--refresh] [--rewrite --user-approved <User's words>] <summary> [--verify <command -> result>]... [--proof <markdown>]... [--risk <markdown>]...", Summary: "Publish or submit this PR-mode Change Task according to its frozen review mode. Repeated --verify, --proof and --risk values are retained in order.", Handler: s.publish},
		{Name: "validate", Usage: "$ validate <status|test|docs|lint> [--repo <member>] [stage evidence flags]", Summary: "Record ordered, exact-head full-pipeline evidence from the private candidate checkout.", Handler: s.validateCandidate},
		{Name: "candidate", Summary: "Manage a full-pipeline Task's private candidates.", Subcommands: []*axi.Command{
			{Name: "abandon", Usage: "$ candidate abandon <task> [--repo <member>] --reason <why>", Summary: "Discard the pending unpromoted candidate and return to the promoted head the Task branch is reset to; recorded in Task history.", Handler: s.abandonCandidate},
		}},
		{Name: "retitle", Usage: "$ retitle <task> \"<conventional title>\"", Summary: "Set the PR title that the Worker's next publish will use.", Handler: s.retitle},
		{Name: "pr", Summary: "Steward a Task's PR/MR as the Lead: inspect it, reply, refresh Posse-owned metadata or rerun an existing failed CI run. Each operation is recorded; merge keeps its own approval.", Subcommands: []*axi.Command{
			{Name: "inspect", Usage: "$ pr inspect <task> [--repo <member>]", Summary: "Read the current PR/MR state, failed checks, authors and recent steward operations.", Handler: s.prInspect},
			{Name: "reply", Usage: "$ pr reply <task> --body <text> [--repo <member>]", Summary: "Post a reply on the PR/MR once; an unconfirmed reply is checked on the forge before any repeat.", Handler: s.prReply},
			{Name: "refresh", Usage: "$ pr refresh <task> [--repo <member>]", Summary: "Rewrite the Posse-owned title and Validation block; other PR/MR text is unchanged.", Handler: s.prRefresh},
			{Name: "rerun", Usage: "$ pr rerun <task> (--run <id>|--job <id>) [--repo <member>]", Summary: "Rerun an existing failed CI run (GitHub) or job (GitLab) on the current head, at most twice per run and head.", Handler: s.prRerun},
		}},
		{Name: "brief", Summary: "Reprint this Worker's launch Brief.", Handler: s.brief},
		{Name: "status", Summary: "Read only this Worker's recorded status and admitted plan without maintenance.", Handler: s.status},
		{Name: "send", Usage: "$ send <task> <message> [--queue] [--reason <text>] | send <task> --file <path.md> [--queue] [--reason <text>] | send <task> --interrupt <message> [--reason <text>]", Summary: "Send a message or a Markdown instruction file, optionally queueing it or interrupting and redirecting a busy Worker.", Handler: s.send},
		{Name: "unsend", Usage: "$ unsend <message-id>", Summary: "Withdraw a queued Worker message before delivery begins.", Handler: s.unsend},
		{Name: "peek", Usage: "$ peek <task> [--lines n]", Summary: "Read the Worker's visible screen; --lines n reads n recent lines and scrolls a fullscreen Worker to the top and back.", Handler: s.peek},
		{Name: "keys", Usage: "$ keys <task> <key...>", Summary: "Send logical keys to a blocked Worker.", Handler: s.keys},
		{Name: "interrupt", Usage: "$ interrupt <task>", Summary: "Interrupt a working Worker and verify it stopped; report when no working turn can be interrupted.", Handler: s.interrupt},
		{Name: "relaunch", Usage: "$ relaunch <task> [--profile <name>]", Summary: "Restart the Worker in the same execution root, optionally with another Profile.", Handler: s.relaunch},
		{Name: "merge", Usage: "$ merge <task> [--execute] [--user-approved <quote>]", Summary: "Merge a completed Change Task; PR merges require the recorded head to include the target branch.", Handler: s.merge},
		{Name: "review", Usage: "$ review <change> [--full]", Summary: "Inspect Review Rounds, findings, carries and remaining budget.", Handler: s.reviewStatus, Subcommands: []*axi.Command{
			{Name: "start", Usage: "$ review start <change> [--profile <name>] [--name <short>]", Summary: "Admit the next numbered Review Round with its frozen delta scope.", Handler: s.reviewStart},
			{Name: "fix", Usage: "$ review fix <change> [--pattern P-n]...", Summary: "Queue one whole-pattern fix instruction for the Change Worker.", Handler: s.reviewFix},
			{Name: "carry", Usage: "$ review carry <change> <passed-head> <candidate-head>", Summary: "Record a Review-only carry for a tree-identical private candidate merge.", Handler: s.reviewCarry},
			{Name: "dispose", Usage: "$ review dispose <change> <F-n|P-n>... <fix|accept|defer|withdraw|reclassify> [--severity P0|P1|P2|P3] [--issue <url>] [--reason <text>] [--user-approved <quote>]", Summary: "Record a finding disposition or queue a whole-pattern fix.", Handler: s.reviewDispose},
			{Name: "merge", Usage: "$ review merge <change> <P-n> <P-n>...", Summary: "Merge duplicate Finding Patterns into the first pattern.", Handler: s.reviewMergePatterns},
			{Name: "followups", Usage: "$ review followups", Summary: "List the Project's non-blocking findings with no follow-up issue, grouped by repository and directory.", Handler: s.reviewFollowups, Subcommands: []*axi.Command{
				{Name: "defer", Usage: "$ review followups defer --area <dir> [--repo <repository>] --issue <url>", Summary: "Defer every listed finding of one area to its rolling follow-up issue.", Handler: s.reviewFollowupsDefer},
			}},
		}},
		{Name: "research", Summary: "Inspect and control bounded read-only native research children of the Lead or a Task.", Subcommands: []*axi.Command{
			{Name: "list", Usage: "$ research list [--all] [--task <task>]", Summary: "List active research attempts with parent, child identity, scope, usage and result pointers; --all includes finished ones.", Handler: s.researchList},
			{Name: "show", Usage: "$ research show <attempt> [--result]", Summary: "Show one attempt; --result prints its digest-checked result.", Handler: s.researchShow},
			{Name: "start", Usage: "$ research start --harness codex|pi --scope <dir> (--prompt <text>|--prompt-file <file>) [--model <m>] [--key <k>] [--wait] [--timeout ms]", Summary: "Register and launch a read-only Codex or Pi child in an explicit scope; --key makes a repeated start return the same attempt.", Handler: s.researchStart},
			{Name: "follow-up", Usage: "$ research follow-up <attempt> (--prompt <text>|--prompt-file <file>) [--key <k>] [--wait] [--timeout ms]", Summary: "Continue a completed or resumable interrupted child's own thread or session as a new linked attempt.", Handler: s.researchFollowUp},
			{Name: "wait", Usage: "$ research wait <attempt> [--timeout ms]", Summary: "Wait until an attempt settles and print it with its result.", Handler: s.researchWait},
			{Name: "cancel", Usage: "$ research cancel <attempt>", Summary: "Cancel the exact child of a registered or running attempt; later output is not accepted.", Handler: s.researchCancel},
		}},
		{Name: "sync", Summary: "Fast-forward the Project's default branch from origin when safe.", Handler: s.sync},
		{Name: "teardown", Summary: "Teardown finished Tasks or cancel Sandbox work with User approval and verified Worker shutdown.", Handler: s.teardown},
		{Name: "watch", Usage: "$ watch [--ack <ids>] [--timeout ms] [--quiet-routine] [--requeue <ids>] [--handoff --destination adapter:session] [--status --destination adapter:session] [--receipt <id> --receipt-outcome accepted|rejected|uncertain [--receipt-token token]] [--poll-only] [--repair]", Summary: "Wait for Notices; adapter handoffs are receiver-only, or perform a User-only repair of the Project watch.", Handler: s.wait},
		{Name: "pool", Summary: "List a Project's Pool or prune safe, Posse-owned orphan panes.", Handler: s.pool},
		{Name: "clean", Usage: "$ clean [--dry-run|--yes]", Summary: "Review or reclaim finished Tasks, their processes, scratch and held Sandboxes.", Handler: s.clean},
		{Name: "ack", Usage: "$ ack <id...|all>", Summary: "Acknowledge open Notices.", Handler: s.ack},
		{Name: "project", Summary: "Inspect or change a Project's settings.", Subcommands: []*axi.Command{
			{Name: "add", Usage: "$ project add [--name <n>]", Summary: "Register a Git repository or workspace without starting its Lead.", Handler: s.projectAdd},
			{Name: "scan", Usage: "$ project scan [<name>]", Summary: "Re-detect a workspace Project's member repositories.", Handler: s.projectScan},
			{Name: "show", Summary: "Show a Project's effective config.", Handler: s.projectShow},
			{Name: "move", Summary: "Update a moved Project's root path.", Handler: s.projectMove},
			{Name: "remove", Usage: "$ project remove <name> [--yes]", Summary: "Unregister a Project that has no Tasks or Sandboxes (dry run without --yes).", Handler: s.projectRemove},
		}},
		{Name: "config", Summary: "Inspect and update schema-validated configuration.", Subcommands: []*axi.Command{
			{Name: "schema", Summary: "List config keys, types, defaults and meanings.", Handler: s.configSchema},
			{Name: "show", Usage: "$ config show [--project <n>] [--effective]", Summary: "Show config or merged effective values.", Handler: s.configShow},
			{Name: "set", Usage: "$ config set <key> <value> [--project <n>] [--user-approved <quote>]", Summary: "Set one validated config value; Lead needs a User quote for user-only keys.", Handler: s.configSet},
			{Name: "unset", Usage: "$ config unset <key> [--project <n>] [--user-approved <quote>]", Summary: "Remove one config value; Lead needs a User quote for user-only keys.", Handler: s.configUnset},
		}},
		{Name: "skills", Usage: "$ skills [--project <n>]", Summary: "Show configured and recommended workflow skills per role; `skills add` installs the recommended set.", Handler: s.skillsShow, Subcommands: []*axi.Command{
			{Name: "add", Usage: "$ skills add [--all | <name>...] [--project <n>] [--user-approved <quote>]", Summary: "Add recommended skills to the [skills] config: pick on a terminal, or pass --all or names.", Handler: s.skillsAdd},
		}},
		{Name: "update", Usage: "$ update [--check] [--version vX.Y.Z] [--force] [--stop-watches]", Summary: "Check or install a verified GitHub release.", Handler: s.update},
		{Name: "_update-preflight", Hidden: true, Handler: s.updatePreflight},
		{Name: "setup", Summary: "Install or update the Herdr plugin and hooks; Posse skills load at launch, with optional global install/removal (--check previews; --human prints a checklist).", Handler: s.setup},
		{Name: "recover", Usage: "$ recover --rebuild", Summary: "Rebuild Projects, Tasks, Sandboxes, Decisions and Notices from snapshots.", Handler: s.recover},
		{Name: "_herdr_started", Summary: "Record a Herdr restart and tell the User how to restore.", Hidden: true, Handler: s.herdrStarted},
		{Name: "_context", Summary: "Print Lead or Worker context inside a posse pane.", Hidden: true, Handler: s.context},
		{Name: "doctor", Usage: "$ doctor [--full] [--verbose] [--json]", Summary: "Check Herdr, plugin, config, database and merging tools; --verbose expands successful checks.", Handler: s.doctor},
	}
	return root
}

func (s *Service) beginMaintenance(key string) bool {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	if s.maintenance == nil {
		s.maintenance = make(map[string]bool)
	}
	if s.maintenance[key] {
		return false
	}
	s.maintenance[key] = true
	s.maintenanceWG.Add(1)
	return true
}

func (s *Service) beginMaintenanceContext(key string, parent context.Context) (context.Context, bool) {
	ctx, cancel := context.WithCancel(parent)
	if !s.beginMaintenance(key) {
		cancel()
		return nil, false
	}
	s.maintenanceMu.Lock()
	if s.maintenanceCancels == nil {
		s.maintenanceCancels = make(map[string]context.CancelFunc)
	}
	s.maintenanceCancels[key] = cancel
	s.maintenanceMu.Unlock()
	return ctx, true
}

func (s *Service) endMaintenance(key string) {
	s.maintenanceMu.Lock()
	delete(s.maintenance, key)
	cancel := s.maintenanceCancels[key]
	delete(s.maintenanceCancels, key)
	s.maintenanceMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.maintenanceWG.Done()
}

func (s *Service) cancelWatchMaintenance() {
	s.maintenanceMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.maintenanceCancels))
	for _, cancel := range s.maintenanceCancels {
		cancels = append(cancels, cancel)
	}
	s.maintenanceMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (s *Service) waitForWatchMaintenance(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.maintenanceWG.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
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
	if project, found, err := s.projectForHomeFolder(ctx, db, dir); err != nil || found {
		return project, err
	}
	return store.Project{}, axi.Failure("project_unknown", "this folder is not registered as a Project", false, "Run the command from the Project root or from its Posse home folder (`~/.posse/projects/<name>/`)", "Run `posse up` here to register it and start its Lead, or `posse project add` for a Git repository or Workspace")
}

func (s *Service) projectForHomeFolder(ctx context.Context, db *store.DB, dir string) (store.Project, bool, error) {
	home, err := s.homePath()
	if err != nil {
		return store.Project{}, false, err
	}
	rel, err := filepath.Rel(filepath.Join(resolvedPath(home), "projects"), dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return store.Project{}, false, nil
	}
	project, err := db.ProjectByName(ctx, strings.SplitN(rel, string(filepath.Separator), 2)[0])
	if store.IsNotFound(err) {
		return store.Project{}, false, nil
	}
	return project, err == nil, err
}

func (s *Service) projectByName(ctx context.Context, db *store.DB, name string) (store.Project, error) {
	project, err := db.ProjectByName(ctx, name)
	if store.IsNotFound(err) {
		return store.Project{}, axi.Failure("project_unknown", fmt.Sprintf("unknown Project %q", name), false, "Run `posse` to list registered Projects", "Run `posse up` or `posse project add` in its folder to register it")
	}
	return project, err
}

// reconcileProject handles a real Herdr restart or group close before
// proceeding with a generation-mismatched snapshot. A delayed event from the
// old server instead retries against the current server, without rolling pane
// ids backward. Only userStart (`posse up`) restores the stopped agents; other
// callers record the episode, get errRecoveryHeld and must not reconcile the
// stale panes (ADR 0035).
func (s *Service) reconcileProject(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, userStart bool) (runtime.RunResult, error) {
	if err := ensureHeldSandboxLocks(ctx, db, project); err != nil {
		return runtime.RunResult{}, err
	}
	// No snapshot taken during a recovery claim is a complete project view.
	if err := waitProjectRecovery(ctx, db, project); err != nil {
		return runtime.RunResult{}, err
	}
	if !project.IsWorkspace() {
		if snapshot, err := s.snapshot(ctx); err == nil {
			if tasks, err := db.LiveTasks(ctx, project.ID); err == nil && workerGroupClosed(snapshot, project, tasks) {
				if !userStart {
					return runtime.RunResult{}, s.holdRestore(ctx, db, project, snapshot.ServerStartedAt+"/group/"+project.HerdrWorkspaceID+"/"+project.LeadPaneID)
				}
				home, err := s.homePath()
				if err != nil {
					return runtime.RunResult{}, err
				}
				if _, err := s.recoverProject(ctx, db, home, project); err != nil {
					if !userStart || !errors.Is(err, errRecoveryDeferred) {
						return runtime.RunResult{}, fmt.Errorf("recover Project %s after Herdr group close: %w", project.Name, err)
					}
					// Keep the Lead startup moving; userStart settles interrupted
					// relaunch intents after reconciling the current snapshot.
				}
				// Concurrent workspace.closed hooks wait for the recovery owner;
				// a partial snapshot would mark still-restoring Workers lost.
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
					if workerGroupClosed(fresh, current, currentTasks) {
						return runtime.RunResult{}, fmt.Errorf("lead still missing after Herdr group recovery for %s", project.Name)
					}
				}
			}
		}
	}
	run := func() (runtime.RunResult, error) {
		result, err := runtime.Run(ctx, db, s.Herdr, project.ID, duration(cfg.Defaults.StallAfter), duration(cfg.Defaults.IdleAfter), time.Now(), s.Progress)
		if err == nil && !result.GenerationMismatch {
			err = observeLeadAgent(ctx, db, project, result.Snapshot)
		}
		return result, err
	}
	result, err := run()
	if err != nil {
		return result, err
	}
	if !result.GenerationMismatch {
		if userStart {
			pendingGroupRecovery, err := pendingGroupRecoveryDue(ctx, db, project.ID)
			if err != nil {
				return result, err
			}
			if pendingGroupRecovery {
				home, err := s.homePath()
				if err != nil {
					return result, err
				}
				if _, err := s.recoverProject(ctx, db, home, project); err != nil {
					return result, fmt.Errorf("recover Project %s after group recovery backoff: %w", project.Name, err)
				}
				result, err = run()
				if err != nil {
					return result, err
				}
				if result.GenerationMismatch {
					return result, fmt.Errorf("project %s Herdr generation changed during recovery; retry reconcile", project.Name)
				}
			}
		}
		if err := s.appendQuotaNotices(ctx, db, project, cfg, &result); err != nil {
			return result, err
		}
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
	deferredRecovery := false
	if current.ServerStartedAt != "" && recorded != "" && current.ServerStartedAt != recorded {
		adopted, err := adoptCurrentGeneration(ctx, db, project, current)
		if err != nil {
			return result, err
		}
		if !adopted {
			if !userStart {
				return result, s.holdRestore(ctx, db, project, current.ServerStartedAt)
			}
			home, err := s.homePath()
			if err != nil {
				return result, err
			}
			if _, err := s.recoverProject(ctx, db, home, project); err != nil {
				if !errors.Is(err, errRecoveryDeferred) {
					return result, fmt.Errorf("recover Project %s after Herdr restart: %w", project.Name, err)
				}
				// Start the Lead while the interrupted Worker waits for its retry
				// backoff. Up settles explicit relaunch intents after reconciliation.
				deferredRecovery = true
			}
		}
	}
	result, err = run()
	if err == nil && result.GenerationMismatch && !(userStart && deferredRecovery) {
		return result, fmt.Errorf("project %s Herdr generation changed during recovery; retry reconcile", project.Name)
	}
	if err != nil {
		return result, err
	}
	if !result.GenerationMismatch {
		if err := s.appendQuotaNotices(ctx, db, project, cfg, &result); err != nil {
			return result, err
		}
	}
	return result, s.retryPendingLaunches(ctx, db, project, cfg, result.Snapshot)
}

// appendQuotaNotices handles Workers whose quota ran out. A Project that is
// down starts no Workers.
func (s *Service) appendQuotaNotices(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, result *runtime.RunResult) error {
	if project.IsDown() {
		return nil
	}
	notices, err := s.handleQuotaExhaustion(ctx, db, project, cfg, result.Snapshot)
	result.Notices = append(result.Notices, notices...)
	return err
}

// projectRecoveryWait bounds how long a command waits for another process's
// Project recovery before returning a retryable error.
var projectRecoveryWait = 60 * time.Second

func waitProjectRecovery(ctx context.Context, db *store.DB, project store.Project) error {
	deadline := time.Now().Add(projectRecoveryWait)
	for {
		claim, err := db.ProjectRecovery(ctx, project.ID)
		if err != nil {
			return err
		}
		if claim.OwnerPID == 0 || claim.OwnerPID == os.Getpid() || !processAlive(claim.OwnerPID) {
			return nil
		}
		if time.Now().After(deadline) {
			return axi.Failure("project_recovery_running", fmt.Sprintf("project recovery is still running for %s", project.Name), true, "Retry the command once recovery completes; check `posse status`")
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

func (s *Service) prepareTaskProject(ctx context.Context, db *store.DB, project store.Project, taskID int64) (config.Config, error) {
	return s.prepareProjectWithOptions(ctx, db, project, projectPreparationOptions{allowRecovery: true, pollRepositories: true, allowRunningLeadNetworkRecovery: true, deliverMessages: true, teardownTask: &taskID})
}

func (s *Service) prepareTaskProjectInspection(ctx context.Context, db *store.DB, project store.Project, taskID int64) (config.Config, error) {
	return s.prepareProjectWithOptions(ctx, db, project, projectPreparationOptions{pollRepositories: true, allowRunningLeadNetworkRecovery: true, deliverMessages: true, teardownTask: &taskID})
}

func (s *Service) prepareProjectForTask(ctx context.Context, db *store.DB, project store.Project) (config.Config, error) {
	return s.prepareProjectMode(ctx, db, project, true, false)
}

// PR inspections keep their refresh behavior without attempting launch recovery.
func (s *Service) prepareProjectInspection(ctx context.Context, db *store.DB, project store.Project) (config.Config, error) {
	return s.prepareProjectMode(ctx, db, project, false, true)
}

// Notice and inspection commands reconcile observations, but never start a
// Worker, retry a launch Brief, or wait for a launch-capable recovery owner.
func (s *Service) prepareProjectObservation(ctx context.Context, db *store.DB, project store.Project) (config.Config, error) {
	return s.prepareProjectMode(ctx, db, project, false, false)
}

func leadAgentStarted(project store.Project, snapshot herdr.Snapshot) bool {
	if project.LeadPaneID == "" {
		return false
	}
	for _, pane := range snapshot.Panes {
		if pane.PaneID == project.LeadPaneID && pane.WorkspaceID == project.HerdrWorkspaceID && pane.Agent != "" && pane.AgentStatus != "" && pane.AgentStatus != "unknown" && pane.AgentStatus != "exited" && pane.AgentStatus != "stopped" {
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

func (s *Service) prepareProjectWithoutMessageDelivery(ctx context.Context, db *store.DB, project store.Project) (config.Config, error) {
	return s.prepareProjectModeWithMessageDelivery(ctx, db, project, true, true, true, false)
}

func (s *Service) prepareProjectModeWithRecoveryPolicy(ctx context.Context, db *store.DB, project store.Project, allowRecovery, pollRepositories, allowRunningLeadNetworkRecovery bool) (config.Config, error) {
	return s.prepareProjectModeWithMessageDelivery(ctx, db, project, allowRecovery, pollRepositories, allowRunningLeadNetworkRecovery, true)
}

type projectPreparationOptions struct {
	allowRecovery                   bool
	pollRepositories                bool
	allowRunningLeadNetworkRecovery bool
	deliverMessages                 bool
	watchObservation                bool
	teardownTask                    *int64
	// admitted is set once the watch pass finished its queue refill or moved a
	// queued Task out of the queue.
	admitted *bool
}

func (s *Service) prepareProjectModeWithMessageDelivery(ctx context.Context, db *store.DB, project store.Project, allowRecovery, pollRepositories, allowRunningLeadNetworkRecovery, deliverMessages bool) (config.Config, error) {
	return s.prepareProjectWithOptions(ctx, db, project, projectPreparationOptions{
		allowRecovery:                   allowRecovery,
		pollRepositories:                pollRepositories,
		allowRunningLeadNetworkRecovery: allowRunningLeadNetworkRecovery,
		deliverMessages:                 deliverMessages,
	})
}

// The detached watch may deliver queued instructions and Notices when no
// extension or live Lead watch owns Notice delivery, and admits queued Tasks
// into free Worker slots. Command boundaries recover interrupted intents. A
// pass that a held, deferred or unavailable observation skipped returns why.
// blocked is the error that kept the pass from admitting queued Tasks; a
// failure after admission is returned only as err.
func (s *Service) prepareProjectWatchObservation(ctx context.Context, db *store.DB, project store.Project) (cfg config.Config, blocked error, err error) {
	admitted := false
	cfg, err = s.prepareProjectWithOptions(ctx, db, project, projectPreparationOptions{deliverMessages: true, watchObservation: true, admitted: &admitted})
	if admitted {
		return cfg, nil, err
	}
	return cfg, err, err
}

type observationWarningWriterKey struct{}

type observationWarningState struct {
	mu       sync.Mutex
	writer   io.Writer
	deferred bool
}

const deferredProjectObservationWarning = "Project reconciliation was deferred due to database contention; the displayed state may be stale."

func withObservationWarningWriter(ctx context.Context, writer io.Writer) context.Context {
	state := &observationWarningState{writer: writer}
	return context.WithValue(ctx, observationWarningWriterKey{}, state)
}

func warnDeferredProjectObservation(ctx context.Context) {
	state, ok := ctx.Value(observationWarningWriterKey{}).(*observationWarningState)
	if !ok || state == nil {
		return
	}
	state.mu.Lock()
	state.deferred = true
	state.mu.Unlock()
}

func flushDeferredProjectObservation(ctx context.Context) {
	state, ok := ctx.Value(observationWarningWriterKey{}).(*observationWarningState)
	if !ok || state == nil {
		return
	}
	state.mu.Lock()
	deferred, writer := state.deferred, state.writer
	state.deferred = false
	state.mu.Unlock()
	if deferred && writer != nil {
		fmt.Fprintln(writer, deferredProjectObservationWarning)
	}
}

func (s *Service) prepareProjectWithOptions(ctx context.Context, db *store.DB, project store.Project, options projectPreparationOptions) (config.Config, error) {
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
	var skipped error
	allowNetworkRecovery := options.allowRecovery
	if s.Herdr != nil {
		if err := s.Herdr.CheckProtocol(ctx); err != nil {
			if !isHerdrUnavailable(err) {
				return cfg, herdrError(err)
			}
			skipped = err
		} else {
			var result runtime.RunResult
			var runErr error
			if options.allowRecovery {
				result, runErr = s.reconcileProject(ctx, db, project, cfg, false)
			} else {
				claim, err := db.ProjectRecovery(ctx, project.ID)
				if err != nil {
					return cfg, err
				}
				if claim.OwnerPID != 0 && processAlive(claim.OwnerPID) {
					runErr = fmt.Errorf("%w: process %d is recovering the Project", errRecoveryDeferred, claim.OwnerPID)
				} else {
					result, runErr = runtime.Run(ctx, db, s.Herdr, project.ID, duration(cfg.Defaults.StallAfter), duration(cfg.Defaults.IdleAfter), time.Now(), s.Progress)
					if runErr == nil && result.GenerationMismatch {
						adopted, err := adoptCurrentGeneration(ctx, db, project, result.Snapshot)
						if err != nil {
							return cfg, err
						}
						if adopted {
							result, runErr = runtime.Run(ctx, db, s.Herdr, project.ID, duration(cfg.Defaults.StallAfter), duration(cfg.Defaults.IdleAfter), time.Now(), s.Progress)
						}
					}
					if runErr == nil && result.GenerationMismatch {
						runErr = heldGenerationError(ctx, db, project, result.Snapshot)
					}
					if runErr == nil {
						runErr = observeLeadAgent(ctx, db, project, result.Snapshot)
					}
					// A Worker stuck at its quota message may emit no further
					// events, and the Lead may share its exhausted account, so
					// the Watch's pass relaunches it with its fallback.
					if runErr == nil && options.watchObservation {
						runErr = s.appendQuotaNotices(ctx, db, project, cfg, &result)
					}
				}
			}
			if runErr != nil {
				// A held Project keeps its recorded panes for `posse up`. Pure
				// contention defers observations; Watch records it for maintenance
				// diagnostics, while foreground commands warn about stale output.
				if store.IsOnlyBusy(runErr) {
					if options.watchObservation {
						return cfg, runErr
					}
					warnDeferredProjectObservation(ctx)
				}
				if !store.IsOnlyBusy(runErr) && !isHerdrUnavailable(runErr) && !errors.Is(runErr, errRecoveryHeld) && !errors.Is(runErr, errRecoveryDeferred) {
					return cfg, herdrError(runErr)
				}
				skipped = runErr
			} else {
				herdrReady = true
				allowNetworkRecovery = allowNetworkRecovery || options.allowRunningLeadNetworkRecovery && leadAgentStarted(project, result.Snapshot)
				if err := s.reconcileTaskPanes(ctx, db, project, result.Snapshot); err != nil {
					return cfg, err
				}
				if options.watchObservation {
					if leadAgentStarted(project, result.Snapshot) {
						if err := s.reconcileWatchIntents(ctx, db, project); err != nil {
							return cfg, err
						}
					}
					count, err := s.refillQueuedTasks(ctx, db, project, cfg)
					if options.admitted != nil {
						*options.admitted = err == nil || count > 0
					}
					if err != nil {
						return cfg, err
					}
				} else if err := s.reconcileIntentsMode(ctx, db, project, cfg, result.Snapshot, options.allowRecovery, allowNetworkRecovery); err != nil {
					return cfg, err
				}
				fresh, err := db.ProjectByID(ctx, project.ID)
				if err != nil {
					return cfg, err
				}
				s.relabelProjectTabs(ctx, db, fresh)
				if options.deliverMessages {
					if err := s.deliverQueuedMessagesWithConfig(ctx, db, fresh, result.Snapshot, cfg); err != nil && !isHerdrUnavailable(err) {
						return cfg, herdrError(err)
					}
				}
				if err := s.regenerateProjects(ctx, db); err != nil {
					return cfg, err
				}
			}
		}
	}
	if options.pollRepositories {
		if err := s.pollProjectPullRequests(ctx, db, project, cfg, false); err != nil {
			return cfg, err
		}
	}
	if err := s.raiseNoticeDecisions(ctx, db, project); err != nil {
		return cfg, err
	}
	if options.pollRepositories {
		if _, err := s.syncProjectRoot(ctx, db, project, cfg, false); err != nil {
			return cfg, err
		}
		_, _ = s.availableUpdate(ctx, db, &project)
	}
	if herdrReady {
		if options.pollRepositories {
			if err := s.autoTeardownMergedTasks(ctx, db, project, cfg); err != nil {
				return cfg, err
			}
			var waitErr error
			if options.teardownTask != nil {
				waitErr = waitForTaskTeardown(ctx, db, project.ID, *options.teardownTask)
			} else {
				waitErr = waitForActiveTeardowns(ctx, db, project.ID)
			}
			if waitErr != nil {
				return cfg, waitErr
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
	if options.watchObservation {
		return cfg, skipped
	}
	return cfg, nil
}

// heldGenerationError names the Herdr restart that holds a Project until
// `posse up --all` advances its recorded generation.
func heldGenerationError(ctx context.Context, db *store.DB, project store.Project, snapshot herdr.Snapshot) error {
	recorded, err := db.ProjectServerStartedAt(ctx, project.ID)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: Herdr server %s replaced the recorded %s; run `posse up --all` to restore Project %s", errRecoveryHeld, snapshot.ServerStartedAt, recorded, project.Name)
}

func (s *Service) requireLead(ctx context.Context, db *store.DB, project store.Project) error {
	actor := s.configCaller(ctx, db)
	if actor.role == "agent" || s.herdrContext != nil && s.herdrContext() {
		if actor.role != "lead" || actor.projectID != project.ID {
			return axi.Failure("lead_only", "this command must run from the Project Lead pane", false, "Use `posse signal` from a Worker pane")
		}
	}
	if cwdInsideSandbox(ctx, db) {
		return axi.Failure("lead_only", "Lead-only commands cannot run from inside a Sandbox", false, "Use the Project Lead pane")
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
		return store.Task{}, axi.Failure("task_unknown", fmt.Sprintf("unknown Task %q", identifier), false, "Run `posse list` to list Tasks")
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

func gitStatusOutput(ctx context.Context, root string, args ...string) (string, error) {
	return gitOutput(ctx, root, append([]string{"--no-optional-locks", "status"}, args...)...)
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
		code := "config_invalid"
		var migration *config.MigrationError
		if errors.As(invalid, &migration) {
			code = "config_migration_required"
		}
		return axi.Failure(code, invalid.Error(), false, configRepairHelp(invalid)...)
	}
	return err
}

func configRepairHelp(invalid *config.InvalidError) []string {
	if _, ok := config.Lookup(invalid.Key); !ok {
		return []string{"Edit the named config file and retry"}
	}
	scope := ""
	projectDir := filepath.Dir(invalid.File)
	if filepath.Base(filepath.Dir(projectDir)) == "projects" {
		scope = " --project " + filepath.Base(projectDir)
	}
	key := invalid.Key
	var migration *config.MigrationError
	if errors.As(invalid, &migration) {
		help := []string{"Run `posse config set " + key + " " + migration.Replacement + scope + "` in " + invalid.File}
		for _, lighter := range migration.Lighter {
			help = append(help, "Or choose the lighter `posse config set "+key+" "+lighter+scope+"`")
		}
		if migration.Value == config.NoMistakes {
			help = append(help, "Keep no-mistakes commands.test and commands.lint in the repository CI workflow; a full-pipeline Worker records test evidence with `posse validate test`, and `full_pipeline.run_workflows = true` runs those workflow jobs locally at lint")
		}
		return append(help, "Posse never changes config for this migration", "Run `posse setup` after the config is valid")
	}
	value := "<valid-value>"
	if strings.Contains(invalid.Error(), "legacy review policy") {
		value = "none"
	}
	return []string{
		"Run `posse config set " + key + " " + value + scope + "` or `posse config unset " + key + scope + "`",
		"Run `posse setup` after the config is valid",
	}
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
			return axi.Failure(code, message, true, "The pane may have closed during this command. Run `posse up --all`; then inspect the Task and relaunch it if it still holds a Sandbox")
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
