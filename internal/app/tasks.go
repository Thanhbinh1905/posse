package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/prepare"
	"github.com/thanhbinh1905/posse/internal/runtime"
	"github.com/thanhbinh1905/posse/internal/sessionref"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) dispatch(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("dispatch", args, map[string]flagSpec{"brief": {}, "profile": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 || parsed.Flags["brief"] == "" {
		return axi.Usage("dispatch requires --brief <file>")
	}
	db, _, err := s.openDB()
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
	cfg, err := s.prepareProject(ctx.Context, db, project)
	if err != nil {
		return err
	}
	brief, err := dispatch.ParseBrief(parsed.Flags["brief"])
	if err != nil {
		return briefError(err)
	}
	slug := taskTitleSlug(brief.Title)
	if err := validateWorkerName(slug); err != nil {
		return axi.Failure("brief_invalid", "Task title cannot produce a descriptive branch slug", false, "Rewrite the title to state the work")
	}
	resolution, err := dispatch.Resolve(cfg, brief.Type, parsed.Flags["profile"])
	if err != nil {
		var failure *axi.Error
		if errors.As(profileError(err), &failure) {
			failure.Help = append([]string{"Task Name: " + slug, "Run `posse ride --brief " + parsed.Flags["brief"] + " --name " + slug + " --profile <name>` after choosing a Profile"}, failure.Help...)
			return failure
		}
		return err
	}
	rideCommand := "posse ride --brief " + parsed.Flags["brief"] + " --name " + slug
	if parsed.Flags["profile"] != "" {
		rideCommand += " --profile " + parsed.Flags["profile"]
	}
	return ctx.Print(axi.Object{
		{Key: "task_type", Value: brief.Type},
		{Key: "name", Value: slug},
		{Key: "profile", Value: resolution.Profile},
		{Key: "dispatch_rule", Value: resolution.Rule},
		{Key: "help", Value: []any{"Run `" + rideCommand + "` to start this Rider"}},
	})
}

func (s *Service) spawn(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("ride", args, map[string]flagSpec{"brief": {}, "name": {}, "profile": {}, "from-leftover": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 || parsed.Flags["brief"] == "" {
		return axi.Usage("ride requires --brief <file> --name <short>")
	}
	if parsed.Flags["name"] == "" {
		return axi.Usage("ride requires --name <short>")
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
	cfg, err := s.prepareProject(ctx.Context, db, project)
	if err != nil {
		return err
	}
	briefData, err := os.ReadFile(parsed.Flags["brief"])
	if err != nil {
		return axi.Failure("brief_invalid", "cannot read Brief: "+err.Error(), false, "Check the Brief path and retry")
	}
	brief, err := dispatch.ParseBriefText(string(briefData))
	if err != nil {
		return briefError(err)
	}
	slug := taskTitleSlug(brief.Title)
	if err := validateWorkerName(slug); err != nil {
		return axi.Failure("brief_invalid", "Task title cannot produce a descriptive branch slug", false, "Rewrite the title to state the work")
	}
	if err := validateWorkerName(parsed.Flags["name"]); err != nil {
		return axi.Failure("name_invalid", err.Error(), false, "Use --name "+slug+" (derived from the Task title)")
	}
	if parsed.Flags["name"] != slug {
		allowed, err := retryNameAllowed(ctx.Context, db, project, brief.Title, slug, parsed.Flags["name"])
		if err != nil {
			return err
		}
		if !allowed {
			return axi.Failure("name_invalid", fmt.Sprintf("Rider name must come from the Task title %q", brief.Title), false, "Use --name "+slug+" or, when retrying a failed or discarded Task, append a short suffix to its slug")
		}
	}
	count, err := db.ActiveWorkerCount(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	if count >= cfg.Defaults.MaxWorkers {
		return axi.Failure("worker_limit", fmt.Sprintf("%s already runs %d Riders (max_workers=%d)", project.Name, count, cfg.Defaults.MaxWorkers), false, "Wait for a Task to finish or raise max_workers in the Project config")
	}
	resolution, err := dispatch.Resolve(cfg, brief.Type, parsed.Flags["profile"])
	if err != nil {
		return profileError(err)
	}
	tightening := brief
	if project.IsWorkspace() {
		// Workspace members tighten one by one in planTaskMembers.
		tightening.LandingMode = ""
	}
	mode, autonomy, err := dispatch.ValidateTightening(tightening, cfg.Defaults.LandingMode, cfg.Autonomy)
	if err != nil {
		return briefError(err)
	}
	if mode == "no-mistakes" && !project.IsWorkspace() {
		if err := ensureNoMistakesInitialized(ctx.Context, project.Root); err != nil {
			return err
		}
	}
	baseRef := project.DefaultBranch
	var reviewsTaskID int64
	var reviewedTask *store.Task
	if brief.Type == "review" {
		reviewed, err := s.currentTask(ctx.Context, db, project, brief.ReviewOf)
		if err != nil {
			return err
		}
		if reviewed.Type != "ship" {
			return axi.Failure("brief_invalid", "review_of must identify a Ship Task", false)
		}
		if reviewed.Branch == "" {
			return axi.Failure("brief_invalid", "the reviewed Task has no branch to inspect", false)
		}
		reviewsTaskID = reviewed.ID
		baseRef = reviewed.Branch
		reviewedTask = &reviewed
	}
	members, workspaceMode, err := s.planTaskMembers(ctx.Context, db, project, cfg, brief, reviewedTask)
	if err != nil {
		return err
	}
	if project.IsWorkspace() {
		// Each member records its own base ref and Landing Mode; the Task keeps the strictest.
		baseRef, mode = "", workspaceMode
	}
	if from := parsed.Flags["from-leftover"]; from != "" {
		id, parseErr := strconv.ParseInt(from, 10, 64)
		if parseErr != nil || id < 1 {
			return axi.Usage("--from-leftover requires a Decision id")
		}
		decision, lookupErr := db.Decision(ctx.Context, project.ID, id)
		if lookupErr != nil || decision.Kind != "leftover" || decision.Answer != "open-task" {
			return axi.Failure("leftover_refused", "an answered open-task Leftover Decision is required", false)
		}
		origin := strings.TrimPrefix(decision.Origin, "leftover:")
		source, err := db.TaskByID(ctx.Context, project.ID, decision.TaskID)
		if err != nil {
			return err
		}
		if project.IsWorkspace() {
			memberName, ref, found := strings.Cut(origin, ":")
			if !found {
				return axi.Failure("leftover_refused", "Leftover has no workspace Member", false)
			}
			matched := false
			for i := range members {
				if members[i].Name == memberName {
					if _, err := gitOutput(ctx.Context, members[i].Root, "rev-parse", "--verify", "refs/heads/"+ref); err != nil {
						return err
					}
					merged := members[i].BaseRef
					repos, err := db.TaskRepos(ctx.Context, source.ID)
					if err != nil {
						return err
					}
					for _, repo := range repos {
						if repo.Repo == memberName && repo.LandedRef != "" {
							merged = repo.LandedRef
							break
						}
					}
					base, err := leftoverBase(ctx.Context, members[i].Root, "refs/heads/"+ref, merged, members[i].BaseRef)
					if err != nil {
						return axi.Failure("leftover_conflict", err.Error(), false, "Repair the Leftover on the current default branch before retrying")
					}
					members[i].BaseRef = base
					matched = true
				}
			}
			if !matched {
				return axi.Failure("leftover_refused", "Brief does not include the Leftover Member", false)
			}
		} else {
			snapshot := "refs/heads/" + origin
			merged := source.LandedRef
			if merged == "" {
				merged = source.BaseRef
			}
			baseRef, err = leftoverBase(ctx.Context, project.Root, snapshot, merged, baseRef)
			if err != nil {
				return axi.Failure("leftover_conflict", err.Error(), false, "Repair the Leftover on the current default branch before retrying")
			}
		}
	}
	name := parsed.Flags["name"]
	if err := taskBranchAvailable(ctx.Context, db, project, name); err != nil {
		return err
	}
	if project.HerdrWorkspaceID == "" {
		return axi.Failure("lead_missing", "Project has no Herdr workspace recorded", false, "Run `posse up` to restart the Lead")
	}
	task := store.Task{
		Type: brief.Type, ReviewsTaskID: reviewsTaskID, Title: brief.Title, ShortName: name, State: store.StateSpawning,
		Profile: resolution.Profile, DispatchRule: resolution.Rule, LandingMode: mode,
		AutonomyReview: defaultValue(autonomy.Review, "ask"), AutonomyLand: defaultValue(autonomy.Land, "ask"),
		BaseRef: baseRef,
	}
	crashIntentAt("ride", "before", "task.create")
	taskID, sequence, intent, err := createTaskWithSequenceAndIntent(ctx.Context, db, project, home, task)
	if errors.Is(err, store.ErrTaskBranchExists) {
		return branchNameTaken(name)
	}
	if err != nil {
		return err
	}
	defer func() { _ = db.FinishIntent(ctx.Context, intent.ID, intent.ProcessID) }()
	crashIntentAt("ride", "after", "task.create")
	task, err = db.TaskByID(ctx.Context, project.ID, taskID)
	if err != nil {
		return err
	}
	worktreeLabel := task.PaneLabel
	workerCount, err := db.ActiveWorkerCount(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	if workerCount > cfg.Defaults.MaxWorkers {
		_ = db.Transition(ctx.Context, taskID, store.StateSpawning, store.StateFailed, "cli", "Rider limit changed during spawn")
		return axi.Failure("worker_limit", "Rider limit was reached during spawn", false)
	}
	kind := taskKind(cfg, task)
	var mount store.Mount
	err = s.runIntentStep(ctx.Context, db, intent, "mount.acquire", func() error {
		var acquireErr error
		if project.IsWorkspace() {
			mount, acquireErr = s.acquireWorkspaceMount(ctx.Context, db, project, task, members, home, cfg.Remuda.Clean, cfg.Remuda.Setup)
		} else {
			mount, acquireErr = acquireMount(ctx.Context, db, project, task, home, cfg.Remuda.Clean, cfg.Remuda.Setup)
		}
		return acquireErr
	})
	if err != nil {
		_ = s.failSpawn(ctx.Context, db, project, taskID, task.Title, err.Error())
		return err
	}
	task.MountID = mount.ID
	task.WorktreePath = mount.Path
	var opened openedTab
	err = s.runIntentStep(ctx.Context, db, intent, "pane.open", func() error {
		var callErr error
		opened, callErr = s.openRiderTab(ctx.Context, home, project, task, mount.Path, kind)
		return callErr
	})
	if err != nil {
		_ = s.failSpawn(ctx.Context, db, project, taskID, task.Title, err.Error())
		return err
	}
	if err := s.runIntentStep(ctx.Context, db, intent, "pane.record", func() error {
		return db.UpdateTaskWorkspace(ctx.Context, taskID, opened.WorkspaceID, opened.PaneID)
	}); err != nil {
		_ = s.failSpawn(ctx.Context, db, project, taskID, task.Title, err.Error())
		return err
	}
	s.relabelProjectTabs(ctx.Context, db, project)
	worktreePath := filepath.Clean(mount.Path)
	if kindConfig, ok := cfg.Kinds[cfg.Profiles[resolution.Profile].Kind]; ok {
		prepareErr := s.runIntentStep(ctx.Context, db, intent, "repository.prepare", func() error {
			if project.IsWorkspace() && kindConfig.Prepare == "claude-trust" {
				worktrees := map[string]string{}
				for _, member := range members {
					worktrees[filepath.Join(worktreePath, member.Path)] = member.Root
				}
				return prepare.ClaudeTrustWorkspace(worktreePath, project.Root, worktrees)
			}
			return prepareMountForKind(kindConfig, worktreePath, project.Root)
		})
		if prepareErr != nil {
			_ = s.failSpawn(ctx.Context, db, project, taskID, task.Title, prepareErr.Error())
			return axi.Failure("prepare_failed", prepareErr.Error(), false, "Inspect the Rider worktree and config before retrying")
		}
	}
	launch := 0
	err = s.runIntentStep(ctx.Context, db, intent, "agent.sequence", func() error {
		var launchErr error
		launch, launchErr = db.NextTaskLaunch(ctx.Context, taskID)
		return launchErr
	})
	if err != nil {
		_ = s.failSpawn(ctx.Context, db, project, taskID, task.Title, err.Error())
		return err
	}
	workerName := agentName(project.Name, sequence, launch)
	if err := s.runIntentStep(ctx.Context, db, intent, "agent.record", func() error {
		return db.UpdateTaskLaunch(ctx.Context, taskID, worktreePath, opened.WorkspaceID, opened.PaneID, worktreeLabel, workerName)
	}); err != nil {
		_ = s.failSpawn(ctx.Context, db, project, taskID, task.Title, err.Error())
		return err
	}
	if err := s.runIntentStep(ctx.Context, db, intent, "pane.label", func() error {
		_, callErr := s.herdrCall(ctx.Context, "pane.rename", map[string]any{"pane_id": opened.PaneID, "label": worktreeLabel})
		return callErr
	}); err != nil {
		_ = s.failSpawn(ctx.Context, db, project, taskID, task.Title, err.Error())
		return err
	}
	kindConfig := cfg.Kinds[kind]
	taskHome := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(sequence))
	briefPath := filepath.Join(taskHome, "brief.md")
	launchPath := filepath.Join(taskHome, "launch.md")
	profile := cfg.Profiles[resolution.Profile]
	argsForAgent := workerAgentArgs(profile, kindConfig, "")
	if err := s.runIntentStep(ctx.Context, db, intent, "brief.write", func() error { return writeFile(briefPath, briefData) }); err != nil {
		_ = s.failSpawn(ctx.Context, db, project, taskID, task.Title, err.Error())
		return err
	}
	launchContents := workerProtocol(project, task, brief, launchPath) + workspaceProtocol(project, members) + workerWaitRules(kindConfig) + "\n\n" + brief.Body + "\n"
	if err := s.runIntentStep(ctx.Context, db, intent, "launch.write", func() error { return writeFile(launchPath, []byte(launchContents)) }); err != nil {
		_ = s.failSpawn(ctx.Context, db, project, taskID, task.Title, err.Error())
		return err
	}
	// These harnesses accept an opening turn without touching the composer.
	openingPrompt := kind == "codex" || kind == "opencode"
	if kind == "codex" {
		argsForAgent = append(argsForAgent, "Read "+launchPath+" and follow it.")
	} else if kind == "opencode" {
		argsForAgent = append(argsForAgent, "--prompt", "Read "+launchPath+" and follow it.")
	}
	var started json.RawMessage
	err = s.runIntentStep(ctx.Context, db, intent, "agent.start", func() error {
		var callErr error
		started, callErr = s.startAgent(ctx.Context, map[string]any{"name": workerName, "kind": kind, "pane_id": opened.PaneID, "args": argsForAgent})
		return callErr
	})
	if err != nil {
		_ = s.failSpawn(ctx.Context, db, project, taskID, task.Title, err.Error())
		return err
	}
	_ = started
	if openingPrompt {
		err = s.waitAgentLaunched(ctx.Context, opened.PaneID)
	} else {
		err = s.waitAgentReady(ctx.Context, opened.PaneID)
	}
	if err != nil {
		_ = s.failSpawn(ctx.Context, db, project, taskID, task.Title, err.Error())
		return err
	}
	if !openingPrompt {
		err = s.runIntentStep(ctx.Context, db, intent, "agent.prompt", func() error {
			return s.deliverLaunchPrompt(ctx.Context, opened.PaneID, "Read "+launchPath+" and follow it.")
		})
		var focused *axi.Error
		if errors.As(err, &focused) && focused.Code == "pane_focused" {
			// The next focus event or lookout tick will submit the Brief.
			err = nil
			if finishErr := db.FinishIntent(ctx.Context, intent.ID, os.Getpid()); finishErr != nil {
				return finishErr
			}
			if refreshErr := s.regenerateProjects(ctx.Context, db); refreshErr != nil {
				return refreshErr
			}
			return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(sequence)}, {Key: "state", Value: "spawning"}, {Key: "worker", Value: workerName}, {Key: "help", Value: []any{"Brief delivery waits for the Rider pane to become unfocused"}}})
		}
		if err != nil {
			_ = s.failSpawn(ctx.Context, db, project, taskID, task.Title, err.Error())
			return err
		}
	}
	if err := s.runIntentStep(ctx.Context, db, intent, "pane.metadata", func() error {
		return s.refreshWorkerDisplay(ctx.Context, db, project, taskID, kind)
	}); err != nil {
		_ = s.failSpawn(ctx.Context, db, project, taskID, task.Title, err.Error())
		return err
	}
	if err := s.runIntentStep(ctx.Context, db, intent, "task.working", func() error {
		return db.Transition(ctx.Context, taskID, store.StateSpawning, store.StateWorking, "cli", "Herdr started the Rider and saw it begin the Brief prompt")
	}); err != nil {
		return err
	}
	if err := db.FinishIntent(ctx.Context, intent.ID, os.Getpid()); err != nil {
		return err
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return err
	}
	return ctx.Print(axi.Object{
		{Key: "task", Value: taskIDString(sequence)},
		{Key: "type", Value: brief.Type},
		{Key: "state", Value: string(store.StateWorking)},
		{Key: "worker", Value: workerName},
		{Key: "profile", Value: resolution.Profile},
		{Key: "help", Value: []any{"Run `posse show " + taskIDString(sequence) + "` to inspect the Rider", "Run `posse peek " + taskIDString(sequence) + "` to read Rider output"}},
	})
}

// Herdr otherwise prefers the unique agent.start name over its detected
// harness in the Agents row. Use the resolved harness before start and Herdr's
// detected identity afterward. Pane title and tokens remain available to other
// UIs without changing canonical pane or agent identity.
func workerDisplayMetadata(task store.Task, detectedAgent, expectedAgent string) map[string]any {
	name := taskDisplayName(task)
	metadata := map[string]any{
		"pane_id": task.PaneID, "source": "posse",
		"title":  task.Title + " · " + name + " · " + task.WorktreePath,
		"tokens": map[string]string{"posse_title": task.Title, "posse_branch": name, "posse_mount": filepath.Base(task.WorktreePath)},
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

func workerTabLabel(task store.Task) string {
	return taskDisplayName(task)
}

// refreshWorkerDisplay reports a Rider's pane title and tokens. Riders are
// tabs of the Lead's workspace, so posse never renames a workspace for them.
func (s *Service) refreshWorkerDisplay(ctx context.Context, db *store.DB, project store.Project, taskID int64, expectedAgent string) error {
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		return err
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return err
	}
	pane, found := findTaskPane(snapshot.Panes, task)
	if !found {
		return nil
	}
	task.PaneID = pane.PaneID
	_, err = s.herdrCall(ctx, "pane.report_metadata", workerDisplayMetadata(task, pane.Agent, expectedAgent))
	return err
}

var workerNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var titleWordPattern = regexp.MustCompile(`[a-z0-9]+`)

// A visible Rider name is derived from the first three meaningful title words.
// This keeps the branch brief and recognizable without accepting an unrelated nickname.
func taskTitleSlug(title string) string {
	var words []string
	for _, word := range titleWordPattern.FindAllString(strings.ToLower(title), -1) {
		if titleStopWords[word] {
			continue
		}
		candidate := strings.Join(append(words, word), "-")
		if len(candidate) > 24 {
			break
		}
		words = append(words, word)
		if len(words) == 3 {
			break
		}
	}
	return strings.Join(words, "-")
}

var titleStopWords = map[string]bool{"a": true, "an": true, "and": true, "for": true, "in": true, "is": true, "of": true, "on": true, "the": true, "to": true, "while": true, "with": true}
var taskIDNamePattern = regexp.MustCompile(`^t[0-9]+$`)

func validateWorkerName(name string) error {
	if len([]rune(name)) > 24 || !workerNamePattern.MatchString(name) || taskIDNamePattern.MatchString(name) {
		return fmt.Errorf("invalid Rider name: must be lowercase kebab-case, at most 24 characters, and not a Task id")
	}
	return nil
}

func createTaskWithSequence(ctx context.Context, db *store.DB, project store.Project, home string, task store.Task) (int64, int, error) {
	return db.CreateTaskWithSequence(ctx, project.ID, project.Name, task, taskSequenceOccupied(ctx, db, project, home))
}

func createTaskWithSequenceAndIntent(ctx context.Context, db *store.DB, project store.Project, home string, task store.Task) (int64, int, store.Intent, error) {
	taskID, sequence, err := db.CreateTaskWithSequenceAndIntent(ctx, project.ID, project.Name, task, "ride", os.Getpid(), taskSequenceOccupied(ctx, db, project, home))
	if err != nil {
		return 0, 0, store.Intent{}, err
	}
	intent, err := db.IntentByTask(ctx, taskID)
	return taskID, sequence, intent, err
}

func retryNameAllowed(ctx context.Context, db *store.DB, project store.Project, title, slug, name string) (bool, error) {
	prefix := slug
	if len(prefix) > 18 {
		prefix = strings.TrimRight(prefix[:18], "-")
	}
	if !strings.HasPrefix(name, prefix+"-") {
		return false, nil
	}
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return false, err
	}
	for _, task := range tasks {
		if task.Title == title && (task.State == store.StateFailed || task.State == store.StateLost || task.State == store.StateTornDown) {
			return true, nil
		}
	}
	return false, nil
}

func branchNameTaken(name string) error {
	prefix := name
	if len(prefix) > 18 {
		prefix = strings.TrimRight(prefix[:18], "-")
	}
	return axi.Failure("branch_exists", "Task branch posse/"+name+" already exists or was used", false, "If retrying a failed or discarded Task, keep the Brief title and use --name "+prefix+"-retry (at most 24 characters); otherwise rewrite the Task title")
}

// Check every repository before inserting the Task, including members a
// workspace Brief did not request. A later checkout -b still guards races.
func taskBranchAvailable(ctx context.Context, db *store.DB, project store.Project, name string) error {
	branch := "posse/" + name
	used, err := db.TaskBranchExists(ctx, project.ID, branch)
	if err != nil {
		return err
	}
	if used {
		return branchNameTaken(name)
	}
	roots := []string{project.Root}
	if project.IsWorkspace() {
		roots = nil
		targets, err := workspaceMountTargets(ctx, db, project)
		if err != nil {
			return err
		}
		for _, target := range targets {
			roots = append(roots, target.Root)
		}
	}
	for _, root := range roots {
		for _, ref := range []string{"refs/heads/" + branch, "refs/remotes/origin/" + branch} {
			found, err := gitOutput(ctx, root, "for-each-ref", "--format=%(refname)", ref)
			if err != nil {
				return err
			}
			if found != "" {
				return branchNameTaken(name)
			}
		}
		if _, err := gitOutput(ctx, root, "remote", "get-url", "origin"); err == nil {
			found, err := gitOutput(ctx, root, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
			if err != nil {
				return axi.Failure("branch_check_failed", "could not check origin for "+branch, true, err.Error())
			}
			if found != "" {
				return branchNameTaken(name)
			}
		}
	}
	return nil
}

// A sequence is an internal identity, independent of Git branch names.
// Historical Task folders prevent reuse after the database is rebuilt.
func taskSequenceOccupied(_ context.Context, _ *store.DB, project store.Project, home string) func(int) (bool, error) {
	return func(sequence int) (bool, error) {
		path := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(sequence))
		_, err := os.Stat(path)
		if err == nil {
			return true, nil
		}
		if !os.IsNotExist(err) {
			return false, err
		}
		return false, nil
	}
}

var agentReadyWait = 30 * time.Second

// waitAgentReady polls until the started agent is idle at its input. Herdr can
// stall a single request while it launches the agent, so a failed poll is
// retried until the overall deadline.
func (s *Service) waitAgentReady(ctx context.Context, paneID string) error {
	return s.waitAgent(ctx, paneID, false)
}

// waitAgentLaunched waits until Herdr detects the agent in any status. It is
// for agents started with their own first prompt, which are busy at once.
func (s *Service) waitAgentLaunched(ctx context.Context, paneID string) error {
	return s.waitAgent(ctx, paneID, true)
}

func (s *Service) waitAgent(ctx context.Context, paneID string, anyStatus bool) error {
	readyCtx, cancel := context.WithTimeout(ctx, agentReadyWait)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	lastStatus := ""
	var lastErr error
	for {
		callCtx, callCancel := context.WithTimeout(readyCtx, 3*time.Second)
		raw, err := s.Herdr.Call(callCtx, "agent.get", map[string]any{"target": paneID})
		callCancel()
		if err == nil {
			var response struct {
				Agent struct {
					AgentStatus      string `json:"agent_status"`
					LaunchPending    bool   `json:"launch_pending"`
					InteractiveReady bool   `json:"interactive_ready"`
				} `json:"agent"`
			}
			if json.Unmarshal(raw, &response) == nil {
				lastStatus = response.Agent.AgentStatus
				// Herdr keeps launch_pending set while a start prompt runs.
				if anyStatus && lastStatus != "" && lastStatus != "unknown" {
					return nil
				}
				if response.Agent.InteractiveReady && !response.Agent.LaunchPending && (lastStatus == "idle" || lastStatus == "done") {
					return nil
				}
			}
		} else if failure, ok := err.(*herdr.Error); readyCtx.Err() == nil && (!ok || (failure.Code != "agent_not_found" && failure.Code != "agent_not_ready" && failure.Code != "herdr_unavailable")) {
			return axi.Failure("herdr_unavailable", "Herdr agent.get readiness poll failed: "+err.Error(), true, "Run `posse doctor` to inspect Herdr connectivity")
		} else if ok && failure.Code == "herdr_unavailable" {
			lastErr = err
		}
		select {
		case <-readyCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if lastStatus == "blocked" {
				return axi.Failure("agent_blocked", "the started agent in pane "+paneID+" is waiting at a dialog, such as a folder trust prompt", true, "Answer the dialog in pane "+paneID+", then retry")
			}
			message := "Herdr did not detect the started agent as ready within " + agentReadyWait.String()
			if lastStatus != "" {
				message += "; last status " + lastStatus
			} else if lastErr != nil {
				message += "; last error: " + lastErr.Error()
			}
			return axi.Failure("agent_start_timeout", message, true, "Run `posse doctor` and inspect the agent pane")
		case <-ticker.C:
		}
	}
}

func (s *Service) failSpawn(ctx context.Context, db *store.DB, project store.Project, id int64, title, reason string) error {
	if task, err := db.TaskByID(ctx, project.ID, id); err == nil {
		if s.Herdr != nil && (task.HerdrWorkspaceID != "" || task.PaneID != "" || task.PaneLabel != "") {
			if _, closeErr := s.closeTaskPanes(ctx, project, task); closeErr != nil {
				reason += "; Task pane cleanup failed: " + closeErr.Error()
			} else {
				s.relabelProjectTabs(ctx, db, project)
			}
		}
		// A written launch Brief and an initialized Mount let relaunch resume
		// the same Task and branch. Earlier failures still release the Mount.
		home, homeErr := s.homePath()
		launchPath := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), "launch.md")
		_, launchErr := os.Stat(launchPath)
		if task.Branch == "" || task.WorktreePath == "" || launchErr != nil || homeErr != nil {
			cfg, _ := config.Load(home, project.Name)
			if _, releaseErr := releaseMount(ctx, db, project, task, cfg.Remuda.Clean); releaseErr != nil {
				reason += "; Mount release failed: " + releaseErr.Error()
			}
		}
	}
	if err := db.Transition(ctx, id, store.StateSpawning, store.StateFailed, "cli", reason); err != nil {
		return err
	}
	if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: id, Kind: "task_failed", Summary: title + " failed to start", DataJSON: `{}`}); err != nil {
		return err
	}
	if err := s.deliverNotices(ctx, db, project); err != nil {
		return err
	}
	if err := s.regenerateProjects(ctx, db); err != nil {
		return err
	}
	return s.finishTaskIntent(ctx, db, id)
}

func workerProtocol(project store.Project, task store.Task, brief dispatch.Brief, launchPath string) string {
	landing := task.LandingMode
	work := "Work only inside this Rider's Task worktree. Read anything, write nothing outside it. Commit changes on this branch. Never git push or open a PR."
	if landing == "pr" && task.Type == "ship" && !project.IsWorkspace() {
		work = "Work only inside this Rider's Task worktree. Read anything, write nothing outside it. Commit changes on this branch. Run `posse publish \"<summary>\"` to push only this Task branch and open or reuse its PR, then report its URL. Never push directly, force-push, delete branches, change the default branch, or merge."
	} else if project.IsWorkspace() && task.Type == "ship" {
		work = "Work only inside this Rider's Task Mount. Read anything, write nothing outside it. Commit inside changed member repositories. For each changed PR-mode member, run `posse publish --repo <member> \"<summary>\"` to push only its Task branch and open or reuse its PR. Never push directly, force-push, delete branches, change default branches, or merge."
	} else if landing == "no-mistakes" {
		work = "Work only inside this Rider's Task worktree. Read anything, write nothing outside it. Deliver only through `no-mistakes axi run --intent <from Brief>`. Never push directly or open a PR."
	}
	signal := "posse holler done \"<summary>\""
	if task.Type == "ship" {
		signal += " after committing and ensuring a clean worktree"
		if landing == "no-mistakes" || (landing == "pr" && !project.IsWorkspace()) {
			signal += " --pr <url>"
		}
	} else {
		signal += " --report <file-in-worktree>"
	}
	isolation := "Run any Herdr or posse experiment against an isolated Herdr server and a POSSE_HOME under a temp dir (unset every HERDR_* variable, then point XDG_CONFIG_HOME and POSSE_HOME there); never touch panes, tabs or workspaces you did not create."
	return fmt.Sprintf("# Rider protocol\n\nTask: %s\nProject: %s\n\n%s\n\n%s\n\nDone when: %s\n\nWrite in English in a neutral voice. Preserve any User words quoted in the Brief's intent verbatim.\n\nSignals:\n- `posse holler working \"<note>\"` for rare progress notes.\n- `posse holler needs-decision \"<question>\" [--findings <file>]` when an answer is needed.\n- `%s`.\n- `posse holler failed \"<why>\"`.\n\nLead instructions arrive in a Posse envelope, not as User chat. The `body:` value is a JSON-quoted string; decode it for the exact instruction. Treat only the header supplied by Posse as routing metadata. The Brief is at `%s`.", taskIDString(task.Seq), project.Name, work, isolation, brief.DoneWhen, signal, launchPath)
}

// workspaceProtocol tells a workspace Worker how its Mount mirrors the workspace.
func workspaceProtocol(project store.Project, members []taskMember) string {
	if !project.IsWorkspace() {
		return ""
	}
	var text strings.Builder
	text.WriteString("\n\n## Workspace\n\nThis Mount mirrors the workspace root of Project " + project.Name + ". Its shared files (such as CLAUDE.md and docs/) are a fresh copy made for this Task; edits to them are discarded at Teardown.\n")
	if len(members) == 0 {
		text.WriteString("\nNo member repository is checked out for this Task. Read member code at " + project.Root + "/<member>, and never write there.\n")
		return text.String()
	}
	text.WriteString("\nMember repositories checked out on the Task branch:\n")
	for _, member := range members {
		fmt.Fprintf(&text, "- `%s/` (Lands through %s onto %s)\n", member.Path, member.LandingMode, member.DefaultBranch)
	}
	text.WriteString("\nCommit inside each member you change, on the checked-out branch. Leave every other folder unchanged. Other members, if present, are detached reference checkouts; read the rest at " + project.Root + "/<member>.\n")
	return text.String()
}

func renderArgs(template []string, token, value string) []string {
	result := make([]string, len(template))
	for i, arg := range template {
		result[i] = strings.ReplaceAll(arg, token, value)
	}
	return result
}

func (s *Service) signal(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("holler", args, map[string]flagSpec{"report": {}, "findings": {}, "pr": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) < 2 {
		return axi.Usage("holler requires <verb> <note>")
	}
	verb := parsed.Positionals[0]
	note := strings.Join(parsed.Positionals[1:], " ")
	if !oneOfString(verb, "working", "needs-decision", "done", "failed") {
		return axi.Usage("Signal verb must be working, needs-decision, done or failed")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	task, err := s.workerTask(ctx.Context, db)
	if err != nil {
		return axi.Failure("worker_task_unknown", "cannot find the Rider's Task from this pane or worktree", false, "Run `posse holler` from the Rider's Herdr pane")
	}
	project, err := db.ProjectByID(ctx.Context, task.ProjectID)
	if err != nil {
		return err
	}
	_, err = s.prepareProject(ctx.Context, db, project)
	if err != nil {
		return err
	}
	task, err = db.TaskByID(ctx.Context, project.ID, task.ID)
	if err != nil {
		return err
	}
	if task.State != store.StateWorking && task.State != store.StateStalled {
		return axi.Failure("signal_state_invalid", fmt.Sprintf("Task %s is %s and cannot accept a %s Signal", taskIDString(task.Seq), task.State, verb), false, "Run `posse show "+taskIDString(task.Seq)+"` to inspect its state")
	}
	if parsed.Flags["pr"] != "" && (verb != "done" || task.Type != "ship" || project.IsWorkspace() || (task.LandingMode != "no-mistakes" && task.LandingMode != "pr")) {
		return axi.Failure("signal_invalid", "--pr is only valid on a PR or no-mistakes Ship Task done Signal", false)
	}
	data := map[string]string{}
	if parsed.Flags["pr"] != "" {
		data["pr_url"] = parsed.Flags["pr"]
	}
	if parsed.Flags["report"] != "" {
		data["report"] = parsed.Flags["report"]
	}
	if parsed.Flags["findings"] != "" {
		data["findings"] = parsed.Flags["findings"]
	}
	if verb == "done" {
		if task.Type == "ship" && project.IsWorkspace() {
			if err := validateWorkspaceShipSignal(ctx.Context, db, task); err != nil {
				return axi.Failure("signal_invalid", err.Error(), false, "Commit the change in each member you changed and leave every member worktree clean before signalling done")
			}
			cfg, err := config.Load(home, project.Name)
			if err != nil {
				return configError(err)
			}
			if err := s.validateWorkspacePublishedPRs(ctx.Context, db, project, task, cfg); err != nil {
				return err
			}
		} else if task.Type == "ship" {
			if err := validateShipSignal(ctx.Context, task); err != nil {
				return axi.Failure("signal_invalid", err.Error(), false, "Commit the change and leave the worktree clean before signalling done")
			}
			if (task.LandingMode == "no-mistakes" || task.LandingMode == "pr") && parsed.Flags["pr"] == "" {
				return axi.Failure("signal_invalid", "a PR-mode Ship Task requires --pr <url>", false)
			}
			if task.LandingMode == "no-mistakes" {
				if err := validatePullRequestOrigin(ctx.Context, project.Root, parsed.Flags["pr"]); err != nil {
					return err
				}
			} else if task.LandingMode == "pr" {
				cfg, err := config.Load(home, project.Name)
				if err != nil {
					return configError(err)
				}
				forge, err := forgeForRepository(ctx.Context, project.Root, cfg, "")
				if err != nil {
					return err
				}
				sha, err := gitOutput(ctx.Context, task.WorktreePath, "rev-parse", "HEAD")
				if err != nil {
					return err
				}
				validationErr := validateWorkerPullRequest(ctx.Context, project, task, forge, parsed.Flags["pr"], sha)
				verifiedCurrentHead := validationErr == nil
				if validationErr != nil && task.PRURL == parsed.Flags["pr"] {
					// A merged PR retains its verified head even if the local
					// branch was advanced by merging the Project default branch.
					observation, observationErr := db.LatestPRObservation(ctx.Context, task.ID)
					if observationErr == nil && observation.State == "MERGED" && observation.PRURL == task.PRURL {
						verified, verifyErr := db.WasVerifiedPRHead(ctx.Context, task.ID, task.PRURL, observation.HeadSHA)
						if verifyErr != nil {
							return verifyErr
						}
						candidate := task
						candidate.LandedRef = observation.MergeCommit
						safe, safetyErr := safeMergedPRWorktree(ctx.Context, db, project, candidate, observation)
						if safetyErr != nil {
							return safetyErr
						}
						if verified && safe && validateWorkerPullRequestWithRemote(ctx.Context, project, task, forge, task.PRURL, observation.HeadSHA, false) == nil {
							validationErr = nil
						}
					} else if observationErr != nil && !store.IsNotFound(observationErr) {
						return observationErr
					}
				}
				if validationErr != nil {
					return validationErr
				}
				if verifiedCurrentHead {
					if err := db.RecordVerifiedPRHead(ctx.Context, task.ID, parsed.Flags["pr"], sha); err != nil {
						return err
					}
				}
			}
		} else if parsed.Flags["report"] == "" {
			return axi.Failure("signal_invalid", "a Scout or Review Task requires --report <file-in-worktree>", false)
		}
	}
	if verb == "needs-decision" && parsed.Flags["findings"] != "" {
		if _, err := copyWorktreeFile(task, parsed.Flags["findings"], filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), "findings.toon")); err != nil {
			return axi.Failure("signal_invalid", err.Error(), false)
		}
	}
	if verb == "done" && task.Type != "ship" {
		if _, err := copyWorktreeFile(task, parsed.Flags["report"], filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), "report.md")); err != nil {
			return axi.Failure("signal_invalid", err.Error(), false)
		}
	}
	noticeKind := map[string]string{"needs-decision": "needs_decision", "failed": "task_failed", "done": "task_done"}[verb]
	state, err := db.RecordWorkerSignal(ctx.Context, task, verb, note, data, noticeKind)
	if err != nil {
		if store.IsBusy(err) {
			return axi.Failure("store_busy", "the store was busy and this Signal was not recorded", true, "Run `posse holler` again with the same Signal")
		}
		return err
	}
	if noticeKind != "" {
		if err := s.deliverNotices(ctx.Context, db, project); err != nil && !isHerdrUnavailable(err) {
			return err
		}
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "signal", Value: verb}, {Key: "state", Value: string(state)}, {Key: "help", Value: []any{"Run `posse brief` to reread the Rider protocol"}}})
}

func (s *Service) brief(ctx *axi.Context, args []string) error {
	if len(args) != 0 {
		return axi.Usage("brief does not take arguments")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	task, err := s.workerTask(ctx.Context, db)
	if err != nil {
		return axi.Failure("worker_task_unknown", "cannot find this Rider's Task", false)
	}
	project, err := db.ProjectByID(ctx.Context, task.ProjectID)
	if err != nil {
		return err
	}
	if _, err := s.prepareProject(ctx.Context, db, project); err != nil {
		return err
	}
	path := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), "launch.md")
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "brief", Value: string(contents)}, {Key: "help", Value: []any{"Use `posse holler` to report this Rider's state"}}})
}

func (s *Service) send(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("send", args, map[string]flagSpec{"queue": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) < 2 {
		return axi.Usage("send requires <task> <message>")
	}
	db, _, err := s.openDB()
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
	cfg, err := s.prepareProject(ctx.Context, db, project)
	if err != nil {
		return err
	}
	task, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	refreshPR := task.PRURL != "" && (task.LandingMode == "pr" || task.LandingMode == "no-mistakes")
	if project.IsWorkspace() {
		repos, err := db.TaskRepos(ctx.Context, task.ID)
		if err != nil {
			return err
		}
		for _, repo := range repos {
			refreshPR = refreshPR || repo.PRURL != "" && repo.LandingMode == "pr"
		}
	}
	if refreshPR {
		if err := s.pollProjectPullRequests(ctx.Context, db, project, cfg, true); err != nil {
			return err
		}
		if err := s.autoTeardownLandedTasks(ctx.Context, db, project, cfg); err != nil {
			return err
		}
		if task, err = db.TaskByID(ctx.Context, project.ID, task.ID); err != nil {
			return err
		}
		if task.State == store.StateLanded || task.State == store.StateTornDown {
			return axi.Failure("pr_merged", "pull request merged; Task Landed before the message could be sent", false)
		}
	}
	if task.State == store.StateDone && task.Type != "ship" {
		return axi.Failure("message_refused", "Task is not accepting Rider instructions in state done because it is not a Ship Task", false, "Create a new Ship Task with `posse ride --brief <file> --name <short>` to continue the work")
	}
	if task.State == store.StateReported || task.State == store.StateLanded || task.State == store.StateTornDown || task.State == store.StateFailed || task.State == store.StateLost {
		nextStep := "Create a new Ship Task with `posse ride --brief <file> --name <short>` to continue the work"
		if task.State == store.StateFailed || task.State == store.StateLost {
			nextStep = "Run `posse relaunch " + taskIDString(task.Seq) + "` to resume this Task"
		}
		return axi.Failure("message_refused", "Task is not accepting Rider instructions in state "+string(task.State), false, nextStep)
	}
	if task.State == store.StateLanding {
		if task.Type != "ship" {
			return axi.Failure("message_refused", "Task is not accepting Rider instructions in state landing because it is not a Ship Task", false, "Run `posse show "+taskIDString(task.Seq)+"` to inspect this Task")
		}
		intent, intentErr := db.IntentByTask(ctx.Context, task.ID)
		if intentErr == nil && intent.Command == "land --merge" && intentProcessAlive(intent) {
			id := taskIDString(task.Seq)
			return axi.Failure("intent_active", "Task has an active land --merge command", true, "Let `posse land "+id+" --merge` finish before retrying `posse send "+id+" <message>`")
		}
		if intentErr != nil && !store.IsNotFound(intentErr) {
			return intentErr
		}
	}
	body := strings.Join(parsed.Positionals[1:], " ")
	messageID, err := db.QueueMessage(ctx.Context, task.ID, body, parsed.Bool("queue"))
	if err != nil {
		return err
	}
	delivered := false
	snapshot, err := s.snapshot(ctx.Context)
	if err != nil {
		return err
	}
	message, err := db.OldestQueuedMessage(ctx.Context, task.ID)
	if err != nil {
		return err
	}
	reason := "agent_not_ready"
	if pane, found := findAppPane(snapshot.Panes, task.PaneID, task.PaneLabel); found {
		reason = queuedMessageReason(task, message, pane, snapshot, cfg)
		if reason == "" {
			wasDelivered, deliveryReason, err := s.deliverClaimedMessage(ctx.Context, db, task, message, pane.PaneID, cfg)
			if err != nil {
				return err
			}
			if wasDelivered {
				if task.State == store.StateNeedsDecision {
					if err := db.Transition(ctx.Context, task.ID, store.StateNeedsDecision, store.StateWorking, "lead", "Lead delivered a response"); err != nil {
						return err
					}
				} else if task.State == store.StateDone && task.Type == "ship" {
					if err := db.Transition(ctx.Context, task.ID, store.StateDone, store.StateWorking, "lead", "Lead delivered a fix instruction"); err != nil {
						return err
					}
					if err := db.ClearTaskGatedSHA(ctx.Context, task.ID); err != nil {
						return err
					}
				} else if task.State == store.StateLanding && task.Type == "ship" {
					if err := db.Transition(ctx.Context, task.ID, store.StateLanding, store.StateWorking, "lead", "Lead delivered a pull request fix instruction"); err != nil {
						return err
					}
					if err := db.ClearTaskGatedSHA(ctx.Context, task.ID); err != nil {
						return err
					}
				}
				delivered = message.ID == messageID
				if !delivered {
					reason = "earlier_message_delivered_first"
				}
			} else {
				reason = deliveryReason
			}
		}
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return err
	}
	state := "queued"
	if delivered {
		state = "delivered"
	} else {
		help := "The message will be delivered when the Rider is ready and unfocused"
		if reason == "blocked" || reason == "agent_ui_unknown" {
			help = "Run `posse peek " + taskIDString(task.Seq) + "` to inspect the Rider's dialog before retrying; use `posse keys " + taskIDString(task.Seq) + " <key...>` for a blocked permission prompt"
		}
		return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "message", Value: messageID}, {Key: "state", Value: state}, {Key: "reason", Value: reason}, {Key: "help", Value: []any{help}}})
	}
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "message", Value: messageID}, {Key: "state", Value: state}, {Key: "reason", Value: "agent_ready_and_unfocused"}, {Key: "help", Value: []any{"Run `posse peek " + taskIDString(task.Seq) + "` to inspect the Rider's response"}}})
}

func (s *Service) peek(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("peek", args, map[string]flagSpec{"lines": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("peek requires one Task id")
	}
	lines := 80
	if value := parsed.Flags["lines"]; value != "" {
		lines, err = strconv.Atoi(value)
		if err != nil || lines < 1 || lines > 2000 {
			return axi.Usage("--lines must be between 1 and 2000")
		}
	}
	db, _, err := s.openDB()
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
	if _, err := s.prepareProject(ctx.Context, db, project); err != nil {
		return err
	}
	task, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	output, err := (runtime.SystemProgress{}).ReadPane(ctx.Context, s.Herdr, task.PaneID, lines)
	if err != nil {
		return herdrError(err)
	}
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "output", Value: output}, {Key: "help", Value: []any{"Run `posse send " + taskIDString(task.Seq) + " <message>` to steer the Rider"}}})
}

func (s *Service) keys(ctx *axi.Context, args []string) error {
	if len(args) < 2 {
		return axi.Usage("keys requires <task> <key...>")
	}
	db, _, err := s.openDB()
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
	if _, err := s.prepareProject(ctx.Context, db, project); err != nil {
		return err
	}
	task, err := s.currentTask(ctx.Context, db, project, args[0])
	if err != nil {
		return err
	}
	if task.State != store.StateBlocked {
		return axi.Failure("worker_not_blocked", taskIDString(task.Seq)+" is not blocked", false, "Use `posse send` for normal instructions")
	}
	snapshot, err := s.snapshot(ctx.Context)
	if err != nil {
		return err
	}
	for _, pane := range snapshot.Panes {
		if pane.PaneID == task.PaneID && (pane.Focused || snapshot.FocusedPaneID == task.PaneID) {
			return axi.Failure("pane_focused", "refusing to send keys to a focused pane", false, "Unfocus the pane, then retry")
		}
	}
	_, err = s.herdrCall(ctx.Context, "agent.send_keys", map[string]any{"target": task.PaneID, "keys": args[1:]})
	if err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "keys", Value: args[1:]}, {Key: "state", Value: "sent"}, {Key: "help", Value: []any{"Run `posse peek " + taskIDString(task.Seq) + "` to inspect the pane"}}})
}

func (s *Service) interrupt(ctx *axi.Context, args []string) error {
	if len(args) != 1 {
		return axi.Usage("interrupt requires one Task id")
	}
	db, _, err := s.openDB()
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
	if _, err := s.prepareProject(ctx.Context, db, project); err != nil {
		return err
	}
	task, err := s.currentTask(ctx.Context, db, project, args[0])
	if err != nil {
		return err
	}
	snapshot, err := s.snapshot(ctx.Context)
	if err != nil {
		return err
	}
	for _, pane := range snapshot.Panes {
		if pane.PaneID == task.PaneID && (pane.Focused || snapshot.FocusedPaneID == task.PaneID) {
			return axi.Failure("pane_focused", "refusing to interrupt a focused pane", false, "Unfocus the pane, then retry")
		}
	}
	_, err = s.herdrCall(ctx.Context, "agent.send_keys", map[string]any{"target": task.PaneID, "keys": []string{"ctrl+c"}})
	if err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "interrupted", Value: true}, {Key: "help", Value: []any{"Run `posse show " + taskIDString(task.Seq) + "` to inspect its state"}}})
}

func (s *Service) relaunch(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("relaunch", args, map[string]flagSpec{"profile": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("relaunch requires one Task id", "Run `posse relaunch <task> [--profile <name>]`")
	}
	if profile, found := parsed.Flags["profile"]; found && profile == "" {
		return axi.Usage("--profile requires a non-empty Profile name")
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
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return configError(err)
	}
	task, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	result, err := s.relaunchTask(ctx.Context, db, home, project, cfg, task, parsed.Flags["profile"])
	if err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "working"}, {Key: "worker", Value: result.Worker}, {Key: "pane", Value: result.Pane}, {Key: "mount", Value: task.WorktreePath}, {Key: "help", Value: []any{"Run `posse show " + taskIDString(task.Seq) + "` to inspect the relaunched Rider"}}})
}

type relaunchResult struct {
	Worker string
	Pane   string
}

func (s *Service) relaunchTask(ctx context.Context, db *store.DB, home string, project store.Project, cfg config.Config, task store.Task, requestedProfile string) (relaunchResult, error) {
	failure := func(err error) (relaunchResult, error) { return relaunchResult{}, err }
	if !recoverableTaskState(task.State) {
		return failure(axi.Failure("relaunch_refused", "Task in state "+string(task.State)+" cannot be relaunched", false))
	}
	if task.Profile == "" {
		return failure(axi.Failure("config_invalid", "Task has no Profile", false))
	}
	profileName := task.Profile
	if requestedProfile != "" {
		profileName = requestedProfile
	}
	resolution, err := dispatch.Resolve(cfg, task.Type, profileName)
	if err != nil {
		return failure(profileError(err))
	}
	profileName = resolution.Profile
	profile, ok := cfg.Profiles[profileName]
	if !ok {
		return failure(axi.Failure("config_invalid", "Task Profile "+profileName+" no longer exists", false))
	}
	kind := profile.Kind
	kindConfig, ok := cfg.Kinds[kind]
	if !ok {
		return failure(axi.Failure("config_invalid", "Task Profile uses unknown agent kind "+kind, false))
	}
	kindChanged := kind != taskKind(cfg, task)
	mount, err := db.MountByTask(ctx, task.ID)
	if err != nil || mount.State != "held" || mount.TaskID != task.ID {
		return failure(axi.Failure("relaunch_mount_missing", "Task Mount is not held", false, "Inspect the Task and its Remuda Mount before relaunching"))
	}
	if task.State == store.StateFailed && (task.MountID == 0 || task.MountID != mount.ID) {
		return failure(axi.Failure("relaunch_mount_missing", "failed Task no longer owns its Mount", false))
	}
	if task.WorktreePath == "" || task.Branch == "" {
		return failure(axi.Failure("relaunch_mount_missing", "Task has no recorded Mount or branch", false))
	}
	intent, err := s.startTaskIntent(ctx, db, project.ID, task.ID, "relaunch")
	if err != nil {
		return failure(axi.Failure("intent_active", "Task already has an unfinished command", true, err.Error()))
	}
	defer func() { _ = db.FinishIntent(ctx, intent.ID, intent.ProcessID) }()
	track := func(step string, action func() error) error { return s.runIntentStep(ctx, db, intent, step, action) }
	gitStateReport := ""
	if err := track("git.inspect", func() error {
		var inspectErr error
		gitStateReport, inspectErr = inspectMountGitState(ctx, task.WorktreePath)
		return inspectErr
	}); err != nil {
		return failure(err)
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return failure(err)
	}
	pane, found := findTaskPane(snapshot.Panes, task)
	if !found {
		var opened openedTab
		if err := track("pane.open", func() error {
			var callErr error
			opened, callErr = s.openRiderTab(ctx, home, project, task, task.WorktreePath, kind)
			return callErr
		}); err != nil {
			return failure(err)
		}
		pane = herdr.Pane{PaneID: opened.PaneID, WorkspaceID: opened.WorkspaceID, TabID: opened.TabID, Label: task.PaneLabel}
	}
	if pane.Agent != "" {
		if err := track("agent.stop", func() error {
			return s.stopPaneAgent(ctx, pane.PaneID)
		}); err != nil {
			return failure(err)
		}
	}
	if kindChanged {
		if err := track("repository.prepare", func() error {
			if project.IsWorkspace() && kindConfig.Prepare == "claude-trust" {
				members, err := s.workspaceMembers(ctx, db, project, task)
				if err != nil {
					return err
				}
				worktrees := map[string]string{}
				for _, member := range members {
					worktrees[member.repo.WorktreePath] = member.target.Root
				}
				return prepare.ClaudeTrustWorkspace(filepath.Clean(task.WorktreePath), project.Root, worktrees)
			}
			return prepareMountForKind(kindConfig, filepath.Clean(task.WorktreePath), project.Root)
		}); err != nil {
			return failure(axi.Failure("prepare_failed", err.Error(), false, "Inspect the Rider Mount and config before retrying"))
		}
	}
	if err := track("pane.label", func() error {
		if _, callErr := s.herdrCall(ctx, "pane.rename", map[string]any{"pane_id": pane.PaneID, "label": task.PaneLabel}); callErr != nil {
			return callErr
		}
		task.PaneID = pane.PaneID
		_, callErr := s.herdrCall(ctx, "pane.report_metadata", workerDisplayMetadata(task, "", kind))
		return callErr
	}); err != nil {
		return failure(err)
	}
	s.relabelProjectTabs(ctx, db, project)
	launch := 0
	if err := track("agent.sequence", func() error {
		var launchErr error
		launch, launchErr = db.NextTaskLaunch(ctx, task.ID)
		return launchErr
	}); err != nil {
		return failure(err)
	}
	session := ""
	if !kindChanged {
		session = agentSessionID(kind, task.AgentSession)
	}
	startArgs := workerAgentArgs(profile, kindConfig, session)
	workerName := agentName(project.Name, task.Seq, launch)
	if err := track("agent.record", func() error {
		return db.UpdateTaskLaunch(ctx, task.ID, task.WorktreePath, pane.WorkspaceID, pane.PaneID, task.PaneLabel, workerName)
	}); err != nil {
		return failure(err)
	}
	if profileName != task.Profile {
		if err := track("task.profile", func() error {
			return db.ChangeTaskProfile(ctx, task.ID, task.Profile, profileName)
		}); err != nil {
			return failure(err)
		}
	}
	if err := track("agent.start", func() error {
		_, callErr := s.startAgent(ctx, map[string]any{"name": workerName, "kind": kind, "pane_id": pane.PaneID, "args": startArgs})
		return callErr
	}); err != nil {
		return failure(err)
	}
	if err := s.waitAgentReady(ctx, pane.PaneID); err != nil {
		return failure(err)
	}
	if err := track("pane.metadata", func() error {
		return s.refreshWorkerDisplay(ctx, db, project, task.ID, kind)
	}); err != nil {
		return failure(err)
	}
	messages, err := db.TaskMessagesAfter(ctx, task.ID, task.CreatedAt, maxRelaunchMessages+1)
	if err != nil {
		return failure(err)
	}
	relaunchPath := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), "relaunch.md")
	if err := track("relaunch.write", func() error { return writeFile(relaunchPath, relaunchInstructions(gitStateReport, messages)) }); err != nil {
		return failure(err)
	}
	if err := track("agent.prompt", func() error { return s.deliverLaunchPrompt(ctx, pane.PaneID, "Read "+relaunchPath+" and follow it.") }); err != nil {
		return failure(err)
	}
	if err := s.markRelaunchedWorking(ctx, db, project.ID, task.ID, track); err != nil {
		return failure(err)
	}
	if err := db.FinishIntent(ctx, intent.ID, intent.ProcessID); err != nil {
		return failure(err)
	}
	if err := s.regenerateProjects(ctx, db); err != nil {
		return failure(err)
	}
	return relaunchResult{Worker: workerName, Pane: pane.PaneID}, nil
}

const (
	maxRelaunchMessages     = 32
	maxRelaunchMessageBytes = 1200
)

// markRelaunchedWorking records the relaunched Worker as working. It reads the
// Task again because reconcile can mark it lost while the relaunch waits for the
// new agent, and a transition from the stale state would leave it lost.
func (s *Service) markRelaunchedWorking(ctx context.Context, db *store.DB, projectID, taskID int64, track func(string, func() error) error) error {
	for attempt := 0; ; attempt++ {
		current, err := db.TaskByID(ctx, projectID, taskID)
		if err != nil {
			return err
		}
		if current.State == store.StateWorking {
			return track("task.progress", func() error { return db.ResetTaskProgress(ctx, taskID) })
		}
		if !recoverableTaskState(current.State) {
			return nil
		}
		source := "cli"
		if current.State == store.StateNeedsDecision {
			source = "lead"
		} else if current.State == store.StateBlocked {
			source = "herdr"
		}
		err = track("task.working", func() error {
			return db.Transition(ctx, taskID, current.State, store.StateWorking, source, "Rider relaunched in the same Mount and branch")
		})
		if err != nil {
			if !errors.Is(err, store.ErrStateRace) || attempt >= 2 {
				return err
			}
			continue
		}
		return track("task.progress", func() error { return db.ResetTaskProgress(ctx, taskID) })
	}
}

func relaunchInstructions(gitStateReport string, messageSets ...[]store.Message) []byte {
	var messages []store.Message
	if len(messageSets) > 0 {
		messages = messageSets[0]
	}
	text := "You were interrupted. Re-read `launch.md`, inspect `git status` and `git log`, continue the Task, and report with `posse holler`.\n"
	if gitStateReport != "" {
		text += "\n" + gitStateReport + "\n"
	}
	if len(messages) > maxRelaunchMessages {
		text += fmt.Sprintf("\nEarlier delivered Lead messages are omitted to keep this prompt bounded; the most recent %d follow in original order.\n", maxRelaunchMessages)
		messages = messages[len(messages)-maxRelaunchMessages:]
	}
	if len(messages) > 0 {
		text += "\nLead messages delivered after the Brief, in order. Later messages supersede earlier ones:\n"
		for index, message := range messages {
			text += fmt.Sprintf("%d. %s\n", index+1, truncateRelaunchMessage(message.Body))
		}
	}
	return []byte(text)
}

func truncateRelaunchMessage(message string) string {
	if len(message) <= maxRelaunchMessageBytes {
		return message
	}
	cut := maxRelaunchMessageBytes
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return message[:cut] + "... [truncated]"
}

func prepareMountForKind(kind config.Kind, mountPath, projectRoot string) error {
	switch kind.Prepare {
	case "claude-trust":
		return prepare.ClaudeTrust(mountPath, projectRoot)
	case "codex-trust":
		return prepare.CodexTrust(mountPath)
	default:
		return nil
	}
}

func taskKind(cfg config.Config, task store.Task) string {
	if profile, ok := cfg.Profiles[task.Profile]; ok && profile.Kind != "" {
		return profile.Kind
	}
	return "claude"
}

func workerAgentArgs(profile config.Profile, kind config.Kind, session string) []string {
	args := make([]string, 0, len(kind.AutoApproveArgs)+len(profile.Args)+len(kind.ModelArgs)+len(kind.EffortArgs)+len(kind.ResumeArgs))
	args = append(args, kind.AutoApproveArgs...)
	if profile.Model != "" {
		args = append(args, renderArgs(kind.ModelArgs, "{model}", profile.Model)...)
	}
	if profile.Effort != "" {
		args = append(args, renderArgs(kind.EffortArgs, "{effort}", profile.Effort)...)
	}
	args = append(args, profile.Args...)
	if session != "" && len(kind.ResumeArgs) > 0 {
		args = append(args, renderArgs(kind.ResumeArgs, "{session}", session)...)
	}
	return args
}

func agentSessionID(agent, session string) string {
	return sessionref.Sanitize(agent, session)
}

// inspectMountGitState reports interrupted Git state in a Mount, per member
// worktree for a workspace Mount.
func inspectMountGitState(ctx context.Context, mountPath string) (string, error) {
	if _, err := os.Stat(filepath.Join(mountPath, ".git")); err == nil {
		return inspectWorktreeGitState(ctx, mountPath)
	}
	entries, err := os.ReadDir(mountPath)
	if err != nil {
		return "", err
	}
	var reports []string
	for _, entry := range entries {
		worktree := filepath.Join(mountPath, entry.Name())
		if _, statErr := os.Stat(filepath.Join(worktree, ".git")); !entry.IsDir() || statErr != nil {
			continue
		}
		report, err := inspectWorktreeGitState(ctx, worktree)
		if err != nil {
			return "", fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if report != "" {
			reports = append(reports, entry.Name()+": "+report)
		}
	}
	return strings.Join(reports, "\n"), nil
}

func inspectWorktreeGitState(ctx context.Context, mountPath string) (string, error) {
	gitDirText, err := gitOutput(ctx, mountPath, "rev-parse", "--git-dir")
	if err != nil {
		return "", err
	}
	gitDir := strings.TrimSpace(gitDirText)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(mountPath, gitDir)
	}
	for _, state := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(gitDir, state)); err == nil {
			return "An in-progress Git operation (" + state + ") was found in this Mount. Leave it intact and continue the Task without aborting or resetting it.", nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	processIDs, err := mountProcessIDs(mountPath)
	if err != nil {
		return "", err
	}
	for _, pid := range processIDs {
		procPath := filepath.Join("/proc", strconv.Itoa(pid))
		comm, _ := os.ReadFile(filepath.Join(procPath, "comm"))
		commandLine, _ := os.ReadFile(filepath.Join(procPath, "cmdline"))
		executable, _ := os.Readlink(filepath.Join(procPath, "exe"))
		name := strings.ToLower(strings.TrimSpace(string(comm)))
		executable = strings.ToLower(filepath.Base(strings.TrimSuffix(executable, " (deleted)")))
		argv0 := ""
		if len(commandLine) > 0 {
			argv0 = strings.ToLower(filepath.Base(strings.TrimSuffix(strings.SplitN(string(commandLine), "\x00", 2)[0], " (deleted)")))
		}
		if isGitProcessName(name) || isGitProcessName(executable) || isGitProcessName(argv0) {
			return "A Git process is still running in this Mount. Leave it running and continue the Task without interfering with it.", nil
		}
	}
	indexLock := filepath.Join(gitDir, "index.lock")
	if _, err := os.Stat(indexLock); os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	if err := os.Remove(indexLock); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return "", nil
}

func isGitProcessName(name string) bool {
	return name == "git" || strings.HasPrefix(name, "git-")
}

func (s *Service) workerTask(ctx context.Context, db *store.DB) (store.Task, error) {
	if paneID := os.Getenv("HERDR_PANE_ID"); paneID != "" {
		if task, err := db.TaskByPane(ctx, paneID); err == nil {
			return task, nil
		}
	}
	dir, err := currentDir()
	if err != nil {
		return store.Task{}, err
	}
	return taskForPath(ctx, db, dir)
}

func validateShipSignal(ctx context.Context, task store.Task) error {
	if task.WorktreePath == "" || task.BaseRef == "" {
		return fmt.Errorf("ship Task has no worktree or base ref")
	}
	count, err := gitOutput(ctx, task.WorktreePath, "rev-list", "--count", task.BaseRef+"..HEAD")
	if err != nil {
		return err
	}
	ahead, err := strconv.Atoi(count)
	if err != nil || ahead < 1 {
		return fmt.Errorf("branch has no commits ahead of %s", task.BaseRef)
	}
	status, err := gitOutput(ctx, task.WorktreePath, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("worktree is not clean")
	}
	return nil
}

func copyWorktreeFile(task store.Task, supplied, destination string) (string, error) {
	root, err := filepath.Abs(task.WorktreePath)
	if err != nil {
		return "", err
	}
	path := supplied
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("report and findings files must stay inside the Rider's worktree")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := writeFile(destination, contents); err != nil {
		return "", err
	}
	return path, nil
}

func profileError(err error) error {
	var profileErr *dispatch.ProfileError
	if !errors.As(err, &profileErr) {
		return err
	}
	help := []string{}
	for _, candidate := range profileErr.Candidates {
		help = append(help, fmt.Sprintf("when=%q profile=%s", candidate.When, candidate.Profile))
	}
	if profileErr.Code == "profile_required" && len(profileErr.Candidates) == 0 {
		help = append(help,
			"Run `posse config set dispatch.default.use <profile>` to choose a default Profile",
			"Run `/posse-setup` to configure available Profiles",
		)
	} else {
		help = append(help, "Pass `--profile <name>` after the Lead chooses based on the Brief")
	}
	return axi.Failure(profileErr.Code, profileErr.Message, false, help...)
}

func briefError(err error) error {
	var briefErr *dispatch.BriefError
	if !errors.As(err, &briefErr) {
		return axi.Failure("brief_invalid", err.Error(), false)
	}
	return axi.Failure("brief_invalid", briefErr.Field+": "+briefErr.Reason, false)
}

func taskIDString(sequence int) string { return fmt.Sprintf("t%d", sequence) }

func defaultValue(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
