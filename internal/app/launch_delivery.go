package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

const launchDeliveryTimeout = 2 * time.Minute

// retryPendingLaunches runs on focus events and normal Project reconciliation
// (including watch ticks). Only a finished start intent can be retried here.
func (s *Service) retryPendingLaunches(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, snapshot herdr.Snapshot) error {
	if err := s.refillQueuedTasks(ctx, db, project, cfg); err != nil {
		return err
	}
	home, err := s.homePath()
	if err != nil {
		return err
	}
	tasks, err := db.TasksInState(ctx, project.ID, store.StateSpawning)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.PaneID == "" {
			continue
		}
		if _, err := db.IntentByTask(ctx, task.ID); err == nil {
			continue
		} else if !store.IsNotFound(err) {
			return err
		}
		launchPath := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), "launch.md")
		if _, err := os.Stat(launchPath); err != nil {
			continue
		}
		pane, found := findAppPane(snapshot.Panes, task.PaneID, task.PaneLabel)
		if found && pane.Agent != "" && pane.AgentStatus != "blocked" && !pane.Focused && snapshot.FocusedPaneID != pane.PaneID {
			err := s.deliverLaunchPrompt(ctx, pane.PaneID, "Read "+launchPath+" and follow it.")
			if err == nil {
				if err := s.refreshWorkerDisplay(ctx, db, project, task.ID, taskKind(cfg, task)); err != nil {
					return err
				}
				if err := db.Transition(ctx, task.ID, store.StateSpawning, store.StateWorking, "cli", "Delivered the queued launch Brief"); err != nil && err != store.ErrStateRace {
					return err
				}
				continue
			}
			var failure *axi.Error
			if !errors.As(err, &failure) || failure.Code != "pane_focused" && failure.Code != "agent_blocked" {
				return err
			}
		}
		if time.Since(time.UnixMilli(task.CreatedAt)) >= launchDeliveryTimeout {
			if err := createNoticeOnce(ctx, db, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "spawn_waiting", Summary: task.Title + " is waiting for its launch Brief to be delivered", DataJSON: `{}`}); err != nil {
				return err
			}
		}
	}
	return nil
}

// refillQueuedTasks reuses the admitted plan and the normal spawn-intent
// launcher. ClaimQueuedTask reserves both capacity and the Task branch
// atomically before the launcher can allocate scratch, a Sandbox or a pane.
func queuedTaskBlockerWaitReason(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, plan *taskplan.Plan) (string, error) {
	if plan == nil {
		return "", nil
	}
	var briefBytes []byte
	for _, input := range plan.Inputs {
		if input.Role == "brief" {
			briefBytes = input.Bytes
			break
		}
	}
	if len(briefBytes) == 0 {
		return "", fmt.Errorf("queued Task has no frozen Brief input")
	}
	brief, err := dispatch.ParseHistoricalBriefText(string(briefBytes))
	if err != nil {
		return "", err
	}
	if brief.Type != "change" || len(issueReferences(brief)) == 0 {
		return "", nil
	}
	targets, err := projectTargets(ctx, db, project)
	if err != nil {
		return "", err
	}
	if project.IsWorkspace() {
		selected := make(map[string]bool, len(plan.Execution.Members))
		for _, member := range plan.Execution.Members {
			selected[member.Name] = true
		}
		filtered := targets[:0]
		for _, target := range targets {
			if selected[target.Name] {
				filtered = append(filtered, target)
			}
		}
		targets = filtered
	}
	if err := validateTicketBlockers(ctx, cfg, brief, targets); err != nil {
		var failure *axi.Error
		if errors.As(err, &failure) {
			return failure.Message, nil
		}
		return err.Error(), nil
	}
	return "", nil
}

func (s *Service) refillQueuedTasks(ctx context.Context, db *store.DB, project store.Project, cfg config.Config) error {
	home, err := s.homePath()
	if err != nil {
		return err
	}
	for {
		tasks, err := db.Tasks(ctx, project.ID, false)
		if err != nil {
			return err
		}
		launched := false
		for _, queued := range tasks {
			if queued.State != store.StateQueued {
				continue
			}
			plan, err := queued.AdmittedPlan()
			if err != nil {
				return admittedPlanFailure(err)
			}
			if plan == nil {
				continue
			}
			unmet, err := db.UnmetTaskDependencies(ctx, project.ID, plan.Dependencies)
			if err != nil {
				return err
			}
			if len(unmet) > 0 {
				for _, dependency := range plan.Dependencies {
					predecessor, taskErr := db.Task(ctx, project.ID, dependency.TaskID)
					if taskErr != nil {
						return taskErr
					}
					if predecessor.State != store.StateFailed && predecessor.State != store.StateLost && predecessor.State != store.StateTornDown {
						continue
					}
					ready, readyErr := db.UnmetTaskDependencies(ctx, project.ID, []dispatch.Dependency{dependency})
					if readyErr != nil {
						return readyErr
					}
					if len(ready) == 0 {
						continue
					}
					origin := fmt.Sprintf("dependency:%s:%s:%s", taskIDString(queued.Seq), dependency.TaskID, dependency.Milestone)
					_, decisionErr := db.RaiseDecision(ctx, store.DecisionRequest{
						ProjectID: project.ID, TaskID: queued.ID, Origin: origin, Kind: "dependency",
						Question: fmt.Sprintf("Prerequisite %s for queued Task %s is %s and cannot satisfy %s. Cancel the queued Task, or drop this prerequisite and run it?", dependency.TaskID, taskIDString(queued.Seq), predecessor.State, dependency.Milestone),
						Options:  []string{"cancel", "drop-prerequisite-and-run"},
					})
					if decisionErr != nil {
						return decisionErr
					}
				}
				continue
			}
			launchedTask, err := s.admitQueuedTask(ctx, db, home, project, cfg, queued, plan, false)
			if err != nil {
				return err
			}
			if !launchedTask {
				continue
			}
			launched = true
			break
		}
		if !launched {
			return nil
		}
	}
}

// frozenTaskBrief returns the Brief bytes admitted with the Task plan.
func frozenTaskBrief(task store.Task, plan *taskplan.Plan) ([]byte, dispatch.Brief, error) {
	var briefBytes []byte
	for _, input := range plan.Inputs {
		if input.Role == "brief" {
			briefBytes = append([]byte(nil), input.Bytes...)
			break
		}
	}
	if len(briefBytes) == 0 {
		return nil, dispatch.Brief{}, fmt.Errorf("frozen Brief input missing for Task %s", taskIDString(task.Seq))
	}
	brief, err := dispatch.ParseHistoricalBriefText(string(briefBytes))
	if err != nil {
		return nil, dispatch.Brief{}, fmt.Errorf("parse Task %s Brief: %w", taskIDString(task.Seq), err)
	}
	return briefBytes, brief, nil
}

// frozenTaskMembers maps the Members admitted with a Workspace Task plan back
// to the currently registered repositories.
func frozenTaskMembers(project store.Project, task store.Task, plan *taskplan.Plan, targets []repoTarget) ([]taskMember, error) {
	members := make([]taskMember, 0, len(plan.Execution.Members))
	if !project.IsWorkspace() {
		return members, nil
	}
	byName := make(map[string]repoTarget, len(targets))
	for _, target := range targets {
		byName[target.Name] = target
	}
	for _, frozen := range plan.Execution.Members {
		target, ok := byName[frozen.Name]
		if !ok {
			return nil, fmt.Errorf("workspace member %q of Task %s is no longer registered", frozen.Name, taskIDString(task.Seq))
		}
		members = append(members, taskMember{repoTarget: target, Path: frozen.Path, MergeMode: frozen.Delivery, BaseRef: frozen.Base, TargetBranch: frozen.Target})
	}
	return members, nil
}

// frozenLaunchConfig overlays the admitted Profile, kind and Pool onto cfg so a
// launch never depends on config edited after admission.
func frozenLaunchConfig(cfg config.Config, plan *taskplan.Plan) (config.Config, dispatch.Resolution) {
	frozen := cfg
	frozen.Kinds = make(map[string]config.Kind, len(cfg.Kinds)+1)
	for name, kind := range cfg.Kinds {
		frozen.Kinds[name] = kind
	}
	frozen.Profiles = make(map[string]config.Profile, len(cfg.Profiles)+1)
	for name, profile := range cfg.Profiles {
		frozen.Profiles[name] = profile
	}
	frozen.Kinds[plan.Launch.Profile.Kind] = plan.Launch.Kind
	frozen.Profiles[plan.Launch.ProfileName] = plan.Launch.Profile
	frozen.Pool = plan.Pool
	return frozen, dispatch.Resolution{Profile: plan.Launch.ProfileName, Rule: plan.Launch.DispatchRule, Resolved: plan.Launch.Profile}
}

// admitQueuedTask launches one queued Task once capacity allows. A foreground
// command is killed with its caller, so with DetachQueuedLaunch the launch runs
// in its own session and the caller only waits for it; an interrupted start
// would otherwise leave a Sandbox and a pane without a Worker.
func (s *Service) admitQueuedTask(ctx context.Context, db *store.DB, home string, project store.Project, cfg config.Config, queued store.Task, plan *taskplan.Plan, inline bool) (bool, error) {
	briefBytes, brief, err := frozenTaskBrief(queued, plan)
	if err != nil {
		return false, err
	}
	targets, err := projectTargets(ctx, db, project)
	if err != nil {
		return false, err
	}
	members, err := frozenTaskMembers(project, queued, plan, targets)
	if err != nil {
		return false, err
	}
	if project.IsWorkspace() {
		targets = targets[:0]
		for _, member := range members {
			targets = append(targets, member.repoTarget)
		}
	}
	if err := validateTicketBlockers(ctx, cfg, brief, targets); err != nil {
		return false, nil
	}
	branch := store.TaskBranchName(queued.Seq)
	if err := taskBranchAvailableForBranch(ctx, db, project, branch, members); err != nil {
		if errors.Is(err, store.ErrTaskBranchExists) {
			return false, nil
		}
		return false, err
	}
	if !inline && s.DetachQueuedLaunch {
		return s.launchQueuedTaskDetached(ctx, db, home, project, queued)
	}
	claimed, err := db.ClaimQueuedTask(ctx, project.ID, queued.ID, cfg.Defaults.MaxWorkers, branch, os.Getpid())
	if err != nil {
		if errors.Is(err, store.ErrTaskBranchExists) {
			return false, nil
		}
		return false, err
	}
	if !claimed {
		return false, nil
	}
	task, err := db.TaskByID(ctx, project.ID, queued.ID)
	if err != nil {
		return false, err
	}
	intent, err := db.IntentByTask(ctx, task.ID)
	if err != nil {
		return false, err
	}
	frozenCfg, resolution := frozenLaunchConfig(cfg, plan)
	launchCtx := &axi.Context{Context: ctx}
	launchErr := s.launchAdmittedTask(launchCtx, db, home, project, frozenCfg, task, task.ID, task.Seq, intent, *plan, brief, briefBytes, resolution, plan.Delivery, task.ShortName, members, nil, nil, false)
	finishErr := db.FinishIntent(ctx, intent.ID, intent.ProcessID)
	if launchErr != nil {
		return false, launchErr
	}
	if finishErr != nil && !errors.Is(finishErr, store.ErrIntentOwned) {
		return false, finishErr
	}
	return true, nil
}

// launchQueuedTaskDetached runs `posse _launch-queued` in a new session, so the
// start survives the caller being interrupted. The detached process owns the
// start intent. It reports whether the Task left the queue.
func (s *Service) launchQueuedTaskDetached(ctx context.Context, db *store.DB, home string, project store.Project, queued store.Task) (bool, error) {
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	logPath := filepath.Join(home, "projects", project.Name, "launch.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return false, err
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	defer logFile.Close()
	command := exec.Command(executable, "_launch-queued", strconv.FormatInt(project.ID, 10), strconv.FormatInt(queued.ID, 10))
	command.Stdout, command.Stderr = logFile, logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return false, fmt.Errorf("start queued Task %s launch: %w", taskIDString(queued.Seq), err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	select {
	case waitErr := <-exited:
		if waitErr != nil {
			fmt.Fprintf(os.Stderr, "queued Task %s launch stopped: %v (see %s)\n", taskIDString(queued.Seq), waitErr, logPath)
		}
	case <-ctx.Done():
		return false, ctx.Err()
	}
	current, err := db.TaskByID(ctx, project.ID, queued.ID)
	if err != nil {
		return false, err
	}
	return current.State != store.StateQueued, nil
}

// RunQueuedLaunch is the detached `_launch-queued` entry point.
func (s *Service) RunQueuedLaunch(args []string, errOut io.Writer) int {
	if err := s.runQueuedLaunch(args); err != nil {
		fmt.Fprintln(errOut, "posse: queued Task launch failed:", err)
		return 1
	}
	return 0
}

func (s *Service) runQueuedLaunch(args []string) error {
	if len(args) != 2 {
		return errors.New("_launch-queued takes a Project id and a Task id")
	}
	projectID, projectErr := strconv.ParseInt(args[0], 10, 64)
	taskID, taskErr := strconv.ParseInt(args[1], 10, 64)
	if projectErr != nil || taskErr != nil {
		return errors.New("_launch-queued takes a Project id and a Task id")
	}
	ctx := context.Background()
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := db.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return err
	}
	queued, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		return err
	}
	if queued.State != store.StateQueued {
		return nil
	}
	plan, err := queued.AdmittedPlan()
	if err != nil {
		return admittedPlanFailure(err)
	}
	if plan == nil {
		return nil
	}
	_, err = s.admitQueuedTask(ctx, db, home, project, cfg, queued, plan, true)
	return err
}
