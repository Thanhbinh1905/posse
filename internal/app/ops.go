package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/execgroup"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) recordLookoutMaintenanceFailure(ctx context.Context, db *store.DB, project store.Project, cause error) error {
	summary := "Lookout maintenance failed (retryable): " + truncate(normalizeCommandError(cause).Error(), 200)
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notices WHERE project_id=? AND kind='pr_watch_failing' AND summary=? AND acked_at IS NULL)`, project.ID, summary).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "pr_watch_failing", Summary: summary, DataJSON: `{}`}); err != nil {
		return err
	}
	return s.regenerateProjects(ctx, db)
}

func (s *Service) startWorkspacePRPolls(ctx context.Context, db *store.DB, project store.Project, cfg config.Config) error {
	now := time.Now()
	projectClaim, err := db.ClaimPRPoll(ctx, project.ID, now, parseDurationOr(cfg.Defaults.PRPoll, 2*time.Minute), false)
	if err != nil || projectClaim == "" {
		return err
	}
	defer func() { _ = db.ReleasePRPoll(context.Background(), project.ID, projectClaim) }()
	targets, err := s.projectTargets(ctx, db, project)
	if err != nil {
		return err
	}
	interval := parseDurationOr(cfg.Defaults.PRPoll, 2*time.Minute)
	for _, target := range targets {
		repo := target.Name
		key := fmt.Sprintf("pr:%d:%s", project.ID, repo)
		if !s.beginMaintenance(key) {
			continue
		}
		token, err := db.ClaimMemberPRPoll(ctx, project.ID, repo, now, interval, false)
		if err != nil {
			s.endMaintenance(key)
			return err
		}
		if token == "" {
			s.endMaintenance(key)
			continue
		}
		go func(repo, key, token string) {
			defer s.endMaintenance(key)
			onLanded := func() {
				s.startLandedTaskTeardown(ctx, db, project, cfg)
				s.startLandedMemberSync(ctx, db, project, repo)
			}
			pollErr := s.pollWorkspacePullRequestsForRepo(ctx, db, project, cfg, false, onLanded, repo, false, false, token)
			if pollErr != nil && !store.IsBusy(pollErr) {
				_ = s.recordLookoutMaintenanceFailure(context.Background(), db, project, fmt.Errorf("member %s PR polling: %w", repo, pollErr))
			}
		}(repo, key, token)
	}
	return nil
}

func (s *Service) startWorkspaceCheckoutSync(ctx context.Context, db *store.DB, project store.Project, cfg config.Config) error {
	targets, err := s.projectTargets(ctx, db, project)
	if err != nil {
		return err
	}
	now := time.Now()
	interval := parseDurationOr(cfg.Defaults.PRPoll, 2*time.Minute)
	for _, target := range targets {
		state, err := db.ProjectRepoWatchState(ctx, project.ID, target.Name)
		if err != nil {
			return err
		}
		if !store.ProjectWatchInterval(state.CheckoutCheckedAt, interval, now) {
			continue
		}
		target := target
		key := fmt.Sprintf("checkout:%d:%s", project.ID, target.Name)
		if !s.beginMaintenance(key) {
			continue
		}
		go func() {
			defer s.endMaintenance(key)
			result, syncErr := s.syncRepository(ctx, db, project, target, now)
			if syncErr == nil {
				reason := result.Reason
				if reason == "" && result.Err != nil {
					reason = truncate(strings.TrimSpace(result.Err.Error()), 240)
				}
				syncErr = db.RecordRepoCheckout(ctx, project.ID, target.Name, now.UnixMilli(), result.Status, reason)
			}
			if syncErr == nil {
				syncErr = db.RecordCheckoutAttempt(ctx, project.ID, now.UnixMilli())
			}
			if syncErr != nil && !store.IsBusy(syncErr) {
				_ = s.recordLookoutMaintenanceFailure(context.Background(), db, project, fmt.Errorf("member %s checkout sync: %w", target.Name, syncErr))
			}
		}()
	}
	return nil
}

func (s *Service) startLandedMemberSync(ctx context.Context, db *store.DB, project store.Project, repo string) {
	key := fmt.Sprintf("land-sync:%d:%s", project.ID, repo)
	if !s.beginMaintenance(key) {
		return
	}
	go func() {
		defer s.endMaintenance(key)
		target, err := s.projectTarget(ctx, db, project, repo)
		if err == nil {
			now := time.Now()
			var result projectSyncResult
			result, err = s.syncRepository(ctx, db, project, target, now)
			if err == nil {
				reason := result.Reason
				if reason == "" && result.Err != nil {
					reason = truncate(strings.TrimSpace(result.Err.Error()), 240)
				}
				err = db.RecordRepoCheckout(ctx, project.ID, repo, now.UnixMilli(), result.Status, reason)
			}
		}
		if err == nil {
			err = db.RecordCheckoutAttempt(ctx, project.ID, time.Now().UnixMilli())
		}
		if err != nil && !store.IsBusy(err) {
			_ = s.recordLookoutMaintenanceFailure(context.Background(), db, project, fmt.Errorf("member %s landed checkout sync: %w", repo, err))
		}
	}()
}

func (s *Service) startLandedTaskTeardown(ctx context.Context, db *store.DB, project store.Project, cfg config.Config) {
	key := fmt.Sprintf("teardown:%d", project.ID)
	if !s.beginMaintenance(key) {
		return
	}
	go func() {
		defer s.endMaintenance(key)
		if err := s.autoTeardownLandedTasks(ctx, db, project, cfg); err != nil && !store.IsBusy(err) {
			_ = s.recordLookoutMaintenanceFailure(context.Background(), db, project, fmt.Errorf("landed Task Teardown: %w", err))
		}
	}()
}

func (s *Service) maintainProjectWatch(ctx context.Context, db *store.DB, project store.Project) error {
	home, err := s.homePath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return configError(err)
	}
	if project.IsWorkspace() {
		if err := s.startWorkspacePRPolls(ctx, db, project, cfg); err != nil {
			return err
		}
		if err := s.startWorkspaceCheckoutSync(ctx, db, project, cfg); err != nil {
			return err
		}
		if err := s.autoTeardownLandedTasks(ctx, db, project, cfg); err != nil {
			return err
		}
		_, _ = s.availableUpdate(ctx, db, &project)
		return nil
	}
	var failures []error
	landedTasks := make(chan struct{}, 1)
	pollResult := make(chan error, 1)
	go func() {
		pollResult <- s.pollProjectPullRequestsWithLandedCallback(ctx, db, project, cfg, false, func() {
			select {
			case landedTasks <- struct{}{}:
			default:
			}
		})
	}()
	checkoutResult := make(chan error, 1)
	go func() {
		_, err := s.syncProjectRoot(ctx, db, project, cfg, false)
		if err != nil {
			err = fmt.Errorf("checkout sync: %w", err)
		}
		checkoutResult <- err
	}()
	runTeardown := func() {
		if err := s.autoTeardownLandedTasks(ctx, db, project, cfg); err != nil {
			failures = append(failures, fmt.Errorf("landed Task Teardown: %w", err))
		}
	}
	runTeardown()
	pollPending, checkoutPending := true, true
	for pollPending || checkoutPending {
		select {
		case <-landedTasks:
			runTeardown()
		case err := <-pollResult:
			pollPending = false
			pollResult = nil
			if err != nil {
				failures = append(failures, fmt.Errorf("PR polling: %w", err))
			}
		case err := <-checkoutResult:
			checkoutPending = false
			checkoutResult = nil
			if err != nil {
				failures = append(failures, err)
			}
		}
	}
	select {
	case <-landedTasks:
		runTeardown()
	default:
	}
	if s.Herdr != nil {
		if err := waitForActiveTeardowns(ctx, db, project.ID); err != nil {
			failures = append(failures, fmt.Errorf("active Task Teardown: %w", err))
		}
	}
	_, _ = s.availableUpdate(ctx, db, &project)
	return errors.Join(failures...)
}

func (s *Service) wait(ctx *axi.Context, args []string) error {
	stopSignals := make(chan os.Signal, 1)
	signal.Notify(stopSignals, syscall.SIGTERM)
	defer signal.Stop(stopSignals)
	parsed, err := parseArgs("lookout", args, map[string]flagSpec{"timeout": {}, "ack": {}, "requeue": {}, "quiet-routine": {boolean: true}, "poll-only": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("lookout does not take positional arguments")
	}
	timeout := time.Duration(0)
	if value := parsed.Flags["timeout"]; value != "" {
		milliseconds, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil || milliseconds < 1 {
			return axi.Usage("--timeout must be a positive number of milliseconds")
		}
		timeout = time.Duration(milliseconds) * time.Millisecond
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
	if parsed.Bool("poll-only") {
		if parsed.Flags["ack"] != "" || parsed.Flags["requeue"] != "" || parsed.Bool("quiet-routine") {
			return axi.Usage("--poll-only cannot deliver or acknowledge Notices")
		}
		return s.watchPullRequestsInLookoutTab(ctx, db, project, timeout, stopSignals)
	}
	if err := s.requireLead(ctx.Context, db, project); err != nil {
		return err
	}
	if parsed.Flags["requeue"] != "" && parsed.Flags["ack"] != "" {
		return axi.Usage("--requeue and --ack cannot be combined")
	}
	if ids := parsed.Flags["ack"]; ids != "" {
		if _, _, err := s.ackNotices(ctx, db, project, strings.Split(ids, ",")); err != nil {
			return err
		}
	}
	if ids := parsed.Flags["requeue"]; ids != "" {
		if _, quiet := parsed.Flags["quiet-routine"]; !quiet {
			return axi.Usage("--requeue requires --quiet-routine")
		}
		values := strings.Split(ids, ",")
		noticeIDs := make([]int64, 0, len(values))
		for _, value := range values {
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil || id < 1 {
				return axi.Usage("--requeue requires comma-separated Notice ids")
			}
			noticeIDs = append(noticeIDs, id)
		}
		if err := db.RequeueNotices(ctx.Context, project.ID, noticeIDs); err != nil {
			return err
		}
	}
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	lastHerdrReconcile := time.Time{}
	lookoutPaneID := ""
	for {
		select {
		case <-stopSignals:
			return s.reportLookoutStopped(ctx, project, false)
		default:
		}
		notices, err := db.UndeliveredNotices(ctx.Context, project.ID)
		if err != nil {
			return err
		}
		if len(notices) == 0 {
			if lastHerdrReconcile.IsZero() || time.Since(lastHerdrReconcile) >= time.Minute {
				if _, err := s.prepareProjectObservation(ctx.Context, db, project); err != nil {
					if store.IsBusy(err) {
						// Contention is transient. Keep this Lookout armed, and retry
						// rather than requiring the Lead to restart it after release.
						fmt.Fprintf(ctx.ErrOut, "Lookout maintenance deferred: %v\n", normalizeCommandError(err))
					} else if noticeErr := s.recordLookoutMaintenanceFailure(ctx.Context, db, project, err); noticeErr != nil {
						fmt.Fprintf(ctx.ErrOut, "Lookout maintenance failure could not be recorded: %v\n", noticeErr)
					}
				} else {
					lastHerdrReconcile = time.Now()
					if snapshot, err := s.snapshot(ctx.Context); err == nil {
						for _, pane := range snapshot.Panes {
							if pane.Label == lookoutTabLabel(project) && pane.WorkspaceID == project.HerdrWorkspaceID {
								lookoutPaneID = pane.PaneID
								break
							}
						}
					}
				}
			} else {
				// Poll PRs at their configured cadence, without a full Herdr
				// reconciliation on every tick. Recheck the Lookout tab when its
				// process is absent or its pane has not been observed yet.
				home, err := s.homePath()
				if err != nil {
					return err
				}
				processRunning := lookoutPaneID != "" && lookoutProcessRunning(lookoutPaneID, home)
				if processRunning {
					if err := markLookoutRunning(ctx.Context, db, project.ID, lookoutPaneID); err != nil {
						return err
					}
				} else {
					needsSnapshot := lookoutPaneID != ""
					if !needsSnapshot {
						state, err := db.LookoutRecovery(ctx.Context, project.ID)
						if err != nil {
							return err
						}
						needsSnapshot = state.PaneID != ""
					}
					if needsSnapshot {
						if snapshot, err := s.snapshot(ctx.Context); err == nil {
							paneID := ""
							for _, pane := range snapshot.Panes {
								if pane.Label == lookoutTabLabel(project) && pane.WorkspaceID == project.HerdrWorkspaceID {
									paneID = pane.PaneID
									break
								}
							}
							lookoutPaneID = paneID
							if paneID == "" {
								// A closed tab is not a failed startup. Recreate it at once
								// and begin a fresh startup grace period.
								if err := resetLookoutRecovery(ctx.Context, db, project.ID); err != nil {
									return err
								}
							} else if lookoutProcessRunning(paneID, home) {
								if err := markLookoutRunning(ctx.Context, db, project.ID, paneID); err != nil {
									return err
								}
								processRunning = true
							}
							due := paneID == ""
							if !due {
								due, err = lookoutRecoveryDue(ctx.Context, db, project.ID, paneID, time.Now())
								if err != nil {
									return err
								}
							}
							if !processRunning && due {
								if err := s.ensureLookoutTab(ctx.Context, db, project, snapshot, false); err != nil {
									return err
								}
								if fresh, err := s.snapshot(ctx.Context); err == nil {
									for _, pane := range fresh.Panes {
										if pane.Label == lookoutTabLabel(project) && pane.WorkspaceID == project.HerdrWorkspaceID {
											lookoutPaneID = pane.PaneID
											break
										}
									}
								}
								failures, noticeRaised, err := lookoutStartFailureNotice(ctx.Context, db, project.ID)
								if err != nil {
									return err
								}
								if failures >= lookoutStartFailureNoticeAfter && !noticeRaised {
									if _, err := db.CreateNotice(ctx.Context, store.Notice{ProjectID: project.ID, Kind: "pr_watch_failing", Summary: "Lookout failed to start after repeated retries; recovery will continue with backoff", DataJSON: `{}`}); err != nil {
										return err
									}
									if err := markLookoutStartFailureNotice(ctx.Context, db, project.ID); err != nil {
										return err
									}
								}
							}
						}
					}
				}
			}
			if err := s.maintainProjectWatch(ctx.Context, db, project); err != nil {
				if store.IsBusy(err) {
					fmt.Fprintf(ctx.ErrOut, "Lookout maintenance deferred: %v\n", normalizeCommandError(err))
				} else if noticeErr := s.recordLookoutMaintenanceFailure(ctx.Context, db, project, err); noticeErr != nil {
					return noticeErr
				}
			}
			notices, err = db.UndeliveredNotices(ctx.Context, project.ID)
			if err != nil {
				return err
			}
		}
		if len(notices) > 0 {
			// Only the Pi integration requests quiet handling. Recheck the current
			// preference for every batch so switching lowkey off takes effect now.
			if _, quiet := parsed.Flags["quiet-routine"]; quiet {
				home, err := s.homePath()
				if err != nil {
					return err
				}
				cfg, err := config.Load(home, project.Name)
				if err != nil {
					return configError(err)
				}
				if cfg.Lowkey.Lead {
					acked, err := db.AckUndeliveredPROpened(ctx.Context, project.ID, currentTime())
					if err != nil {
						return err
					}
					// Re-read after the atomic ack; mixed batches retain every
					// actionable/unknown Notice and competing claims stay untouched.
					if acked > 0 {
						continue
					}
				}
			}
			ids := noticeIDs(notices)
			token, err := newDeliveryClaimToken()
			if err != nil {
				return err
			}
			claimed, err := db.ClaimNoticeBatch(ctx.Context, project.ID, ids, token, currentTime())
			if err != nil {
				return err
			}
			if !claimed {
				continue
			}
			home, err := s.homePath()
			if err != nil {
				return err
			}
			cfg, err := config.Load(home, project.Name)
			if err != nil {
				return configError(err)
			}
			result := axi.Object{{Key: "project", Value: project.Name}, {Key: "notices", Value: noticeRows(ctx.Context, db, notices)}}
			if cfg.Lowkey.Lead {
				result = append(result, axi.Field{Key: "lowkey", Value: true}, axi.Field{Key: "reporting_rule", Value: shortReportingRule(true)})
			} else {
				result = append(result, axi.Field{Key: "lowkey", Value: false}, axi.Field{Key: "reporting_rule", Value: normalReportingRule})
			}
			if ctx.JSON || cfg.Lowkey.Lead {
				result = append(result, axi.Field{Key: "wake", Value: noticeWakeMessage(ctx.Context, db, notices, cfg.Lowkey.Lead)})
			}
			result = append(result, axi.Field{Key: "help", Value: []any{"Run `posse show <task>` for Task details", "Run `posse lookout --ack <ids>` to acknowledge and keep waiting (or `posse ack all`)"}})
			if err := ctx.Print(result); err != nil {
				return errors.Join(err, db.RollbackNoticeClaim(ctx.Context, project.ID, ids, token))
			}
			if err := db.MarkClaimedNoticesDelivered(ctx.Context, project.ID, ids, token, currentTime()); err != nil {
				return err
			}
			return s.regenerateProjects(ctx.Context, db)
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			home, err := s.homePath()
			if err != nil {
				return err
			}
			cfg, err := config.Load(home, project.Name)
			if err != nil {
				return configError(err)
			}
			return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "notices", Value: []any{}}, {Key: "state", Value: "timeout"}, {Key: "lowkey", Value: cfg.Lowkey.Lead}, {Key: "reporting_rule", Value: reportingRule(cfg.Lowkey.Lead)}, {Key: "help", Value: []any{"Run `posse lookout` to keep waiting for a Notice"}}})
		}
		select {
		case <-stopSignals:
			return s.reportLookoutStopped(ctx, project, false)
		case <-ctx.Context.Done():
			return ctx.Context.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (s *Service) reportLookoutStopped(ctx *axi.Context, project store.Project, pollOnly bool) error {
	home, err := s.homePath()
	if err != nil {
		return err
	}
	reason := lookoutStopReason(home, os.Getpid())
	help := "The Lead should restart `posse lookout` on its next wake"
	if pollOnly {
		help = "The Lookout tab owner will restart this process on its next tick"
	}
	return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "state", Value: "stopped"}, {Key: "reason", Value: reason}, {Key: "help", Value: []any{help}}})
}

func (s *Service) ack(ctx *axi.Context, args []string) error {
	if len(args) == 0 {
		return axi.Usage("ack requires one or more Notice ids, or all")
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
	count, autoUnsaddled, err := s.ackNotices(ctx, db, project, args)
	if err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "acknowledged", Value: count}, {Key: "auto_unsaddled", Value: autoUnsaddled}, {Key: "help", Value: []any{"Run `posse` to see remaining open Notices"}}})
}

func (s *Service) ackNotices(ctx *axi.Context, db *store.DB, project store.Project, args []string) (int, []string, error) {
	home, err := s.homePath()
	if err != nil {
		return 0, nil, err
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return 0, nil, configError(err)
	}
	identifiers := []string{}
	for _, arg := range args {
		identifiers = append(identifiers, strings.Split(arg, ",")...)
	}
	count, err := db.AckNotices(ctx.Context, project.ID, identifiers)
	if err != nil {
		return 0, nil, axi.Failure("notice_invalid", err.Error(), false)
	}
	autoUnsaddled := []string{}
	if autoUnsaddleMode(cfg) == "finished" {
		notices, err := db.Notices(ctx.Context, project.ID, false)
		if err != nil {
			return 0, nil, err
		}
		for _, notice := range notices {
			if notice.Kind != "task_done" || notice.TaskID == 0 || notice.AckedAt == 0 || !ackIncludes(identifiers, notice.ID) {
				continue
			}
			task, err := db.TaskByID(ctx.Context, project.ID, notice.TaskID)
			if err != nil {
				return 0, nil, err
			}
			if task.State != store.StateReported {
				continue
			}
			if _, err := s.unsaddleTask(ctx.Context, db, project, cfg, task, false, ""); err != nil {
				return 0, nil, err
			}
			autoUnsaddled = append(autoUnsaddled, taskDisplayName(task))
		}
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return 0, nil, err
	}
	return count, autoUnsaddled, nil
}

func ackIncludes(identifiers []string, noticeID int64) bool {
	for _, identifier := range identifiers {
		if identifier == "all" {
			return true
		}
		for _, part := range strings.Split(identifier, ",") {
			if part == strconv.FormatInt(noticeID, 10) {
				return true
			}
		}
	}
	return false
}

func (s *Service) projectShow(ctx *axi.Context, args []string) error {
	if len(args) > 1 {
		return axi.Usage("project show takes at most one Project name")
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	var project store.Project
	if len(args) == 1 {
		project, err = s.projectByName(ctx.Context, db, args[0])
	} else {
		project, err = s.projectForCWD(ctx.Context, db)
	}
	if err != nil {
		return err
	}
	var cfg config.Config
	if project.IsWorkspace() {
		cfg, err = s.prepareProjectObservation(ctx.Context, db, project)
	} else {
		cfg, err = s.prepareProjectInspection(ctx.Context, db, project)
	}
	if err != nil {
		return err
	}
	if project.IsWorkspace() {
		repos, err := s.projectRepoRows(ctx.Context, db, project, cfg)
		if err != nil {
			return err
		}
		return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "kind", Value: project.Kind}, {Key: "root", Value: project.Root}, {Key: "status", Value: project.Status}, {Key: "repos", Value: repos}, {Key: "gate", Value: cfg.Defaults.Gate}, {Key: "autonomy", Value: map[string]any{"review": defaultValue(cfg.Autonomy.Review, "ask"), "land": defaultValue(cfg.Autonomy.Land, "ask")}}, {Key: "lead", Value: s.leadStatus(ctx.Context, project)}, {Key: "help", Value: []any{"Run `posse config set repositories.<repo>.landing_mode pr --project " + project.Name + "` to change one member's Landing Mode", "Run `posse project scan` after adding or removing a member repository"}}})
	}
	return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "root", Value: project.Root}, {Key: "default_branch", Value: project.DefaultBranch}, {Key: "status", Value: project.Status}, {Key: "landing_mode", Value: cfg.Defaults.LandingMode}, {Key: "gate", Value: cfg.Defaults.Gate}, {Key: "autonomy", Value: map[string]any{"review": defaultValue(cfg.Autonomy.Review, "ask"), "land": defaultValue(cfg.Autonomy.Land, "ask")}}, {Key: "lead", Value: s.leadStatus(ctx.Context, project)}, {Key: "help", Value: []any{"Run `posse config set defaults.landing_mode \"local\" --project " + project.Name + "` to update Project settings"}}})
}

func (s *Service) projectMove(ctx *axi.Context, args []string) error {
	if len(args) != 2 {
		return axi.Usage("project move requires <name> <new-root>")
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectByName(ctx.Context, db, args[0])
	if err != nil {
		return err
	}
	if err := s.requireLead(ctx.Context, db, project); err != nil {
		return err
	}
	newRoot, err := resolveProjectPath(args[1])
	if err != nil {
		return axi.Failure("project_move_invalid", "new Project path does not exist", false, err.Error())
	}
	if existing, lookupErr := db.ProjectByRoot(ctx.Context, newRoot); lookupErr == nil && existing.ID != project.ID {
		return axi.Failure("project_root_taken", "another Project already uses the new root", false)
	}
	if project.IsWorkspace() {
		if _, err := gitTop(ctx.Context, newRoot); err == nil {
			return axi.Failure("project_move_invalid", "a workspace Project root cannot be inside a Git repository", false, "Pass the workspace folder that holds the member repositories")
		}
		if err := db.MoveProject(ctx.Context, project.ID, newRoot, ""); err != nil {
			return err
		}
		project.Root = newRoot
		result, err := s.rescanWorkspace(ctx.Context, db, project)
		if err != nil {
			return err
		}
		if err := s.regenerateProjects(ctx.Context, db); err != nil {
			return err
		}
		return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "root", Value: newRoot}, {Key: "added", Value: result.Added}, {Key: "missing", Value: result.Missing}, {Key: "help", Value: []any{"Run `posse project show` to inspect its members"}}})
	}
	actualRoot, err := gitTop(ctx.Context, newRoot)
	if err != nil || actualRoot != newRoot {
		return axi.Failure("project_move_invalid", "new path must be the top-level Git checkout", false, "Pass the repository root")
	}
	branch, err := defaultBranch(ctx.Context, newRoot)
	if err != nil {
		return err
	}
	if err := db.MoveProject(ctx.Context, project.ID, newRoot, branch); err != nil {
		return err
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "root", Value: newRoot}, {Key: "default_branch", Value: branch}, {Key: "help", Value: []any{"Run `posse` from the moved checkout to inspect the Project"}}})
}

func (s *Service) regenerateProjects(ctx context.Context, db *store.DB) error {
	home, err := s.homePath()
	if err != nil {
		return err
	}
	projects, err := db.Projects(ctx)
	if err != nil {
		return err
	}
	var out strings.Builder
	out.WriteString("<!-- generated by posse; edit config.toml instead -->\n\n")
	for _, project := range projects {
		out.WriteString("## " + project.Name + "\n\n")
		if _, err := os.Stat(project.Root); err != nil {
			fmt.Fprintf(&out, "- Error: Project path is unavailable: %s\n\n", project.Root)
			continue
		}
		cfg, err := config.Load(home, project.Name)
		if err != nil {
			fmt.Fprintf(&out, "- Error: %s\n\n", err)
			continue
		}
		tasks, err := db.Tasks(ctx, project.ID, false)
		if err != nil {
			fmt.Fprintf(&out, "- Error: %s\n\n", err)
			continue
		}
		notices, err := db.Notices(ctx, project.ID, true)
		if err != nil {
			fmt.Fprintf(&out, "- Error: %s\n\n", err)
			continue
		}
		fmt.Fprintf(&out, "- Path: `%s`\n- Landing Mode: %s\n- Autonomy: review=%s land=%s\n- Lead: %s\n", project.Root, cfg.Defaults.LandingMode, defaultValue(cfg.Autonomy.Review, "ask"), defaultValue(cfg.Autonomy.Land, "ask"), s.leadStatus(ctx, project))
		if len(tasks) == 0 {
			out.WriteString("- Open Tasks: none\n")
		} else {
			out.WriteString("- Open Tasks:\n")
			for _, task := range tasks {
				fmt.Fprintf(&out, "  - %s/%s (%s) [%s] %s\n", project.Name, taskIDString(task.Seq), taskDisplayName(task), task.State, task.Title)
			}
		}
		if len(notices) == 0 {
			out.WriteString("- Open Notices: none\n")
		} else {
			fmt.Fprintf(&out, "- Open Notices: %d\n", len(notices))
		}
		fmt.Fprintf(&out, "- Last activity: %s\n\n", time.UnixMilli(project.LastActivityAt).Local().Format(time.RFC3339))
	}
	return writeAtomic(filepath.Join(home, "projects.md"), []byte(out.String()), 0o600)
}

func writeAtomic(path string, contents []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".posse-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func jsonRaw(data []byte) any {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return string(data)
	}
	return value
}

func commandOutput(ctx context.Context, cwd string, command string) (string, error) {
	process := execgroup.CommandContext(ctx, "/bin/sh", "-c", command)
	process.Dir = cwd
	output, err := process.CombinedOutput()
	if ctx.Err() != nil {
		return string(output), ctx.Err()
	}
	return string(output), err
}
