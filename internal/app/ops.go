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

func (s *Service) recordWatchMaintenanceFailure(ctx context.Context, db *store.DB, project store.Project, cause error) error {
	summary := "Watch maintenance failed (retryable): " + truncate(normalizeCommandError(cause).Error(), 200)
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

const (
	memberPRPollPhase       = "pr_poll"
	memberCheckoutSyncPhase = "checkout_sync"
)

func (s *Service) recordMemberWatchFailure(ctx context.Context, db *store.DB, project store.Project, repo, phase string, cause error) error {
	failure := truncate(normalizeCommandError(cause).Error(), 240)
	writeErr := db.RecordMemberWatchError(ctx, project.ID, repo, phase, failure, time.Now().UnixMilli())
	if store.IsOnlyBusy(cause) {
		return writeErr
	}
	noticeErr := s.recordWatchMaintenanceFailure(ctx, db, project, fmt.Errorf("member %s %s: %w", repo, phase, cause))
	return errors.Join(writeErr, noticeErr)
}

func (s *Service) startWorkspacePRPolls(ctx context.Context, db *store.DB, project store.Project, cfg config.Config) error {
	now := time.Now()
	projectClaim, err := db.ClaimPRPoll(ctx, project.ID, now, parseDurationOr(cfg.Defaults.PRPoll, 2*time.Minute), false)
	if err != nil || projectClaim == "" {
		return err
	}
	defer releaseProjectPRPollWithRetry(db, project.ID, projectClaim)
	targets, err := projectTargets(ctx, db, project)
	if err != nil {
		return err
	}
	interval := parseDurationOr(cfg.Defaults.PRPoll, 2*time.Minute)
	var failures []error
	for _, target := range targets {
		repo := target.Name
		key := fmt.Sprintf("pr:%d:%s", project.ID, repo)
		memberCtx, started := s.beginMaintenanceContext(key, ctx)
		if !started {
			continue
		}
		token, err := db.ClaimMemberPRPoll(memberCtx, project.ID, repo, now, interval, false)
		if err != nil {
			s.endMaintenance(key)
			if errors.Is(err, context.Canceled) {
				continue
			}
			memberErr := fmt.Errorf("member %s PR polling claim: %w", repo, err)
			_ = db.RecordMemberWatchError(context.Background(), project.ID, repo, memberPRPollPhase, truncate(normalizeCommandError(memberErr).Error(), 240), now.UnixMilli())
			failures = append(failures, memberErr)
			continue
		}
		if token == "" {
			s.endMaintenance(key)
			continue
		}
		go func(repo, key, token string, memberCtx context.Context) {
			defer s.endMaintenance(key)
			mergedMember := false
			onMerged := func() {
				mergedMember = true
				s.startMergedMemberSync(ctx, db, project, repo)
			}
			pollErr := s.pollWorkspacePullRequestsForRepo(memberCtx, db, project, cfg, false, onMerged, repo, false, false, token)
			if decisionErr := s.raiseNoticeDecisions(memberCtx, db, project); decisionErr != nil {
				pollErr = errors.Join(pollErr, fmt.Errorf("PR watch Decisions: %w", decisionErr))
			}
			if mergedMember {
				s.startMergedTaskTeardown(ctx, db, project, cfg)
			}
			if pollErr != nil && !errors.Is(pollErr, context.Canceled) {
				_ = s.recordMemberWatchFailure(context.Background(), db, project, repo, "PR polling", pollErr)
			}
		}(repo, key, token, memberCtx)
	}
	return errors.Join(failures...)
}

func (s *Service) startWorkspaceCheckoutSync(ctx context.Context, db *store.DB, project store.Project, cfg config.Config) error {
	targets, err := projectTargets(ctx, db, project)
	if err != nil {
		return err
	}
	now := time.Now()
	interval := parseDurationOr(cfg.Defaults.PRPoll, 2*time.Minute)
	var failures []error
	for _, target := range targets {
		state, err := db.ProjectRepoWatchState(ctx, project.ID, target.Name)
		if err != nil {
			memberErr := fmt.Errorf("member %s checkout sync state: %w", target.Name, err)
			_ = db.RecordMemberWatchError(context.Background(), project.ID, target.Name, memberCheckoutSyncPhase, truncate(normalizeCommandError(memberErr).Error(), 240), now.UnixMilli())
			failures = append(failures, memberErr)
			continue
		}
		if !store.ProjectWatchInterval(state.CheckoutCheckedAt, interval, now) {
			continue
		}
		target := target
		key := workspaceCheckoutMaintenanceKey(project.ID, target.Name)
		memberCtx, started := s.beginMaintenanceContext(key, ctx)
		if !started {
			continue
		}
		go func() {
			defer s.endMaintenance(key)
			result, syncErr := s.syncRepository(memberCtx, db, project, target, now, false)
			if syncErr == nil && result.Status != "skipped" {
				reason := result.Reason
				if reason == "" && result.Err != nil {
					reason = truncate(strings.TrimSpace(result.Err.Error()), 240)
				}
				syncErr = db.RecordRepoCheckout(memberCtx, project.ID, target.Name, now.UnixMilli(), result.Status, reason)
			}
			if syncErr == nil && result.Status != "skipped" {
				syncErr = db.RecordCheckoutAttempt(memberCtx, project.ID, now.UnixMilli())
			}
			if syncErr != nil && !errors.Is(syncErr, context.Canceled) {
				_ = s.recordMemberWatchFailure(context.Background(), db, project, target.Name, "checkout sync", syncErr)
			} else if syncErr == nil && result.Err != nil && (result.Status == "fetch_failed" || result.Status == "no_origin") {
				_ = db.RecordMemberWatchError(context.Background(), project.ID, target.Name, memberCheckoutSyncPhase, truncate(normalizeCommandError(result.Err).Error(), 240), now.UnixMilli())
			} else if syncErr == nil && result.Status != "skipped" {
				_ = db.ClearMemberWatchError(context.Background(), project.ID, target.Name, memberCheckoutSyncPhase)
			}
		}()
	}
	return errors.Join(failures...)
}

func workspaceCheckoutMaintenanceKey(projectID int64, repo string) string {
	return fmt.Sprintf("checkout:%d:%s", projectID, repo)
}

func (s *Service) startMergedMemberSync(ctx context.Context, db *store.DB, project store.Project, repo string) {
	key := workspaceCheckoutMaintenanceKey(project.ID, repo)
	memberCtx, started := s.beginMaintenanceContext(key, ctx)
	if !started {
		return
	}
	go func() {
		defer s.endMaintenance(key)
		var result projectSyncResult
		target, err := projectTarget(memberCtx, db, project, repo)
		if err == nil {
			now := time.Now()
			result, err = s.syncRepository(memberCtx, db, project, target, now, true)
			if err == nil && result.Status != "skipped" {
				reason := result.Reason
				if reason == "" && result.Err != nil {
					reason = truncate(strings.TrimSpace(result.Err.Error()), 240)
				}
				err = db.RecordRepoCheckout(memberCtx, project.ID, repo, now.UnixMilli(), result.Status, reason)
			}
		}
		if err == nil && result.Status != "skipped" {
			err = db.RecordCheckoutAttempt(memberCtx, project.ID, time.Now().UnixMilli())
		}
		if err != nil && !store.IsBusy(err) && !errors.Is(err, context.Canceled) {
			_ = s.recordWatchMaintenanceFailure(context.Background(), db, project, fmt.Errorf("member %s merged checkout sync: %w", repo, err))
		}
	}()
}

func (s *Service) startMergedTaskTeardown(ctx context.Context, db *store.DB, project store.Project, cfg config.Config) {
	key := fmt.Sprintf("teardown:%d", project.ID)
	maintenanceCtx, started := s.beginMaintenanceContext(key, ctx)
	if !started {
		return
	}
	go func() {
		defer s.endMaintenance(key)
		if err := s.autoTeardownMergedTasks(maintenanceCtx, db, project, cfg); err != nil && !store.IsBusy(err) && !errors.Is(err, context.Canceled) {
			_ = s.recordWatchMaintenanceFailure(context.Background(), db, project, fmt.Errorf("merged Task Teardown: %w", err))
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
	s.startWorkerGoCacheTrim(ctx, db, project, home)
	if project.IsWorkspace() {
		var failures []error
		if err := s.autoTeardownMergedTasks(ctx, db, project, cfg); err != nil {
			failures = append(failures, fmt.Errorf("merged Task Teardown: %w", err))
		}
		if err := s.startWorkspacePRPolls(ctx, db, project, cfg); err != nil {
			failures = append(failures, fmt.Errorf("PR polling: %w", err))
		}
		if err := s.startWorkspaceCheckoutSync(ctx, db, project, cfg); err != nil {
			failures = append(failures, fmt.Errorf("checkout sync: %w", err))
		}
		_, _ = s.availableUpdate(ctx, db, &project)
		return errors.Join(failures...)
	}
	var failures []error
	mergedTasks := make(chan struct{}, 1)
	pollResult := make(chan error, 1)
	go func() {
		pollResult <- s.pollProjectPullRequestsWithMergedCallback(ctx, db, project, cfg, false, func() {
			select {
			case mergedTasks <- struct{}{}:
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
		if err := s.autoTeardownMergedTasks(ctx, db, project, cfg); err != nil {
			failures = append(failures, fmt.Errorf("merged Task Teardown: %w", err))
		}
	}
	runTeardown()
	pollPending, checkoutPending := true, true
	for pollPending || checkoutPending {
		select {
		case <-mergedTasks:
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
	case <-mergedTasks:
		runTeardown()
	default:
	}
	if s.Herdr != nil {
		if err := waitForActiveTeardowns(ctx, db, project.ID); err != nil {
			failures = append(failures, fmt.Errorf("active Task Teardown: %w", err))
		}
	}
	if err := s.raiseNoticeDecisions(ctx, db, project); err != nil {
		failures = append(failures, fmt.Errorf("PR watch Decisions: %w", err))
	}
	_, _ = s.availableUpdate(ctx, db, &project)
	return errors.Join(failures...)
}

func (s *Service) wait(ctx *axi.Context, args []string) error {
	stopSignals := make(chan os.Signal, 1)
	signal.Notify(stopSignals, syscall.SIGTERM)
	defer signal.Stop(stopSignals)
	parsed, err := parseArgs("watch", args, map[string]flagSpec{"timeout": {}, "ack": {}, "requeue": {}, "destination": {}, "receipt": {}, "receipt-outcome": {}, "receipt-token": {}, "handoff": {boolean: true}, "quiet-routine": {boolean: true}, "poll-only": {boolean: true}, "repair": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("watch does not take positional arguments")
	}
	timeout := time.Duration(0)
	if value := parsed.Flags["timeout"]; value != "" {
		milliseconds, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil || milliseconds < 1 {
			return axi.Usage("--timeout must be a positive number of milliseconds")
		}
		timeout = time.Duration(milliseconds) * time.Millisecond
	}
	if parsed.Bool("repair") && (parsed.Flags["timeout"] != "" || parsed.Flags["ack"] != "" || parsed.Flags["requeue"] != "" || parsed.Flags["destination"] != "" || parsed.Flags["receipt"] != "" || parsed.Flags["receipt-outcome"] != "" || parsed.Flags["receipt-token"] != "" || parsed.Bool("handoff") || parsed.Bool("quiet-routine") || parsed.Bool("poll-only")) {
		return axi.Usage("--repair cannot be combined with Watch wait or delivery options")
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
	if parsed.Bool("repair") {
		return s.repairWatch(ctx, db, home, project)
	}
	// Adapter-owned receivers must not trigger Project poller repair. Their
	// adapter lifecycle owns waiting and re-arming independently of observation.
	if !parsed.Bool("poll-only") && !parsed.Bool("handoff") && parsed.Flags["receipt"] == "" {
		if err := s.ensureWatchRuntime(ctx.Context, db, home, project); err != nil {
			fmt.Fprintf(ctx.ErrOut, "Watch supervision for %s failed: %v\n", project.Name, err)
		}
	}
	if parsed.Bool("poll-only") {
		if parsed.Flags["ack"] != "" || parsed.Flags["requeue"] != "" || parsed.Flags["destination"] != "" || parsed.Flags["receipt"] != "" || parsed.Flags["receipt-outcome"] != "" || parsed.Flags["receipt-token"] != "" || parsed.Bool("quiet-routine") || parsed.Bool("handoff") {
			return axi.Usage("--poll-only cannot deliver or acknowledge Notices")
		}
		return s.runProjectWatchRuntime(ctx, db, project, timeout, stopSignals)
	}
	if err := s.requireLead(ctx.Context, db, project); err != nil {
		return err
	}
	if parsed.Flags["receipt"] != "" {
		if parsed.Flags["receipt-outcome"] == "" || parsed.Flags["ack"] != "" || parsed.Flags["requeue"] != "" || parsed.Bool("handoff") || parsed.Flags["destination"] != "" || timeout != 0 {
			return axi.Usage("--receipt requires --receipt-outcome and cannot be combined with other delivery options")
		}
		var err error
		if parsed.Flags["receipt-token"] == "" {
			err = db.ResolveUncertainNoticeDelivery(ctx.Context, project.ID, parsed.Flags["receipt"], parsed.Flags["receipt-outcome"], currentTime())
		} else {
			err = db.ResolveNoticeDelivery(ctx.Context, project.ID, parsed.Flags["receipt"], parsed.Flags["receipt-token"], parsed.Flags["receipt-outcome"], currentTime())
		}
		if err != nil {
			return err
		}
		if err := s.regenerateProjects(ctx.Context, db); err != nil {
			return err
		}
		return ctx.Print(axi.Object{{Key: "delivery_id", Value: parsed.Flags["receipt"]}, {Key: "receipt", Value: parsed.Flags["receipt-outcome"]}})
	}
	if parsed.Flags["receipt-outcome"] != "" || parsed.Flags["receipt-token"] != "" {
		return axi.Usage("--receipt-outcome and --receipt-token require --receipt")
	}
	if parsed.Bool("handoff") != (parsed.Flags["destination"] != "") {
		return axi.Usage("--handoff requires --destination <adapter:session>")
	}
	if parsed.Bool("handoff") && (parsed.Flags["ack"] != "" || parsed.Flags["requeue"] != "") {
		return axi.Usage("--handoff cannot be combined with --ack or --requeue")
	}
	if parsed.Flags["requeue"] != "" && parsed.Flags["ack"] != "" {
		return axi.Usage("--requeue and --ack cannot be combined")
	}
	acknowledged := -1
	if ids := parsed.Flags["ack"]; ids != "" {
		var err error
		acknowledged, _, err = s.ackNoticesWithCommit(ctx, db, project, strings.Split(ids, ","), func(count int) {
			fmt.Fprintf(ctx.ErrOut, "Watch acknowledgement committed: %d\n", count)
		})
		if err != nil {
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
	lastReexecCheck := time.Time{}
	for {
		select {
		case <-stopSignals:
			s.cancelWatchMaintenance()
			if !s.waitForWatchMaintenance(4 * time.Second) {
				fmt.Fprintln(ctx.ErrOut, "Watch maintenance shutdown timed out; unfinished poll claims will expire")
			}
			return s.reportWatchStopped(ctx, project, false)
		default:
		}
		var retryDelivery *store.NoticeDelivery
		var notices []store.Notice
		if parsed.Bool("handoff") {
			existing, found, err := db.NoticeDeliveryForDestinationRetry(ctx.Context, project.ID, parsed.Flags["destination"], currentTime())
			if err != nil {
				return err
			}
			if found && existing.State == "uncertain" {
				return ctx.Print(noticeDeliveryUncertaintyResult(existing, "Inspect that Lead session before retrying"))
			}
			prior, priorFound, err := db.NoticeDeliveryForOtherSession(ctx.Context, project.ID, parsed.Flags["destination"])
			if err != nil {
				return err
			}
			if priorFound {
				switch prior.State {
				case "uncertain":
					return ctx.Print(noticeDeliveryUncertaintyResult(prior, "The previous session outcome is unresolved; inspect its history or effect before retrying"))
				case "printed":
					if err := db.MarkNoticeDeliveryUncertain(ctx.Context, prior.DeliveryID, currentTime()); err != nil {
						return err
					}
					prior, err = db.NoticeDelivery(ctx.Context, prior.DeliveryID)
					if err != nil {
						return err
					}
					return ctx.Print(noticeDeliveryUncertaintyResult(prior, "The previous session may have received this batch; inspect its history or effect before retrying"))
				case "claimed":
					if prior.LeaseUntil > currentTime() {
						if err := waitNoticeDeliveryRetry(ctx.Context); err != nil {
							return err
						}
						continue
					}
					if err := db.RejectExpiredNoticeDeliveryClaim(ctx.Context, prior.DeliveryID, currentTime()); err != nil && !errors.Is(err, store.ErrStateRace) {
						return err
					}
					continue
				}
			}
			if found {
				retryDelivery = &existing
				notices, err = db.NoticesByIDs(ctx.Context, project.ID, existing.NoticeIDs)
				if err != nil {
					return err
				}
			}
		}
		if retryDelivery == nil {
			var err error
			notices, err = db.UndeliveredNotices(ctx.Context, project.ID)
			if err != nil {
				return err
			}
		}
		if len(notices) == 0 {
			if !parsed.Bool("handoff") && (lastHerdrReconcile.IsZero() || time.Since(lastHerdrReconcile) >= time.Minute) {
				if _, err := s.prepareProjectObservation(ctx.Context, db, project); err != nil {
					if store.IsBusy(err) {
						// Contention is transient. Keep this Watch armed, and retry
						// rather than requiring the Lead to restart it after release.
						fmt.Fprintf(ctx.ErrOut, "Watch maintenance deferred: %v\n", normalizeCommandError(err))
					} else if noticeErr := s.recordWatchMaintenanceFailure(ctx.Context, db, project, err); noticeErr != nil {
						fmt.Fprintf(ctx.ErrOut, "Watch maintenance failure could not be recorded: %v\n", noticeErr)
					}
				} else {
					lastHerdrReconcile = time.Now()
				}
			}
			if !parsed.Bool("handoff") {
				if err := s.maintainProjectWatch(ctx.Context, db, project); err != nil {
					if store.IsBusy(err) {
						fmt.Fprintf(ctx.ErrOut, "Watch maintenance deferred: %v\n", normalizeCommandError(err))
					} else if noticeErr := s.recordWatchMaintenanceFailure(ctx.Context, db, project, err); noticeErr != nil {
						return noticeErr
					}
				}
			}
			if retryDelivery == nil {
				notices, err = db.UndeliveredNotices(ctx.Context, project.ID)
				if err != nil {
					return err
				}
			}
		}
		if len(notices) > 0 {
			// Only the Pi integration requests quiet handling. Recheck the current
			// preference for every batch so switching lowkey off takes effect now.
			if _, quiet := parsed.Flags["quiet-routine"]; quiet && retryDelivery == nil {
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
			var handoff *store.NoticeDelivery
			if parsed.Bool("handoff") {
				generation, err := s.noticeDeliveryGeneration(ctx.Context)
				if err != nil {
					return err
				}
				delivery, claimed, err := db.ClaimNoticeDelivery(ctx.Context, project.ID, ids, parsed.Flags["destination"], generation, token, currentTime(), noticeDeliveryLeaseTimeout.Milliseconds())
				if err != nil {
					return err
				}
				if !claimed {
					if err := waitNoticeDeliveryRetry(ctx.Context); err != nil {
						return err
					}
					continue
				}
				printedAt := currentTime()
				if err := db.MarkNoticeDeliveryPrinted(ctx.Context, delivery.DeliveryID, token, printedAt); err != nil {
					return err
				}
				delivery.State, delivery.UpdatedAt = "printed", printedAt
				handoff = &delivery
			} else {
				claimed, err := db.ClaimNoticeBatch(ctx.Context, project.ID, ids, token, currentTime())
				if err != nil {
					return err
				}
				if !claimed {
					continue
				}
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
			if acknowledged >= 0 {
				result = append(result, axi.Field{Key: "acknowledged", Value: acknowledged})
			}
			if handoff != nil {
				result = append(result, axi.Field{Key: "delivery", Value: noticeDeliveryRow(*handoff, true)})
			}
			if cfg.Lowkey.Lead {
				result = append(result, axi.Field{Key: "lowkey", Value: true}, axi.Field{Key: "reporting_rule", Value: shortReportingRule(true)})
			} else {
				result = append(result, axi.Field{Key: "lowkey", Value: false}, axi.Field{Key: "reporting_rule", Value: normalReportingRule})
			}
			if ctx.JSON || cfg.Lowkey.Lead {
				result = append(result, axi.Field{Key: "wake", Value: noticeWakeMessage(ctx.Context, db, notices, cfg.Lowkey.Lead)})
			}
			result = append(result, axi.Field{Key: "help", Value: []any{"Run `posse show <task>` for Task details", "Run `posse watch --ack <ids>` to acknowledge and keep waiting (or `posse ack all`)"}})
			if err := ctx.Print(result); err != nil {
				if handoff != nil {
					return errors.Join(err, db.ResolveNoticeDelivery(ctx.Context, project.ID, handoff.DeliveryID, token, "uncertain", currentTime()))
				}
				return errors.Join(err, db.RollbackNoticeClaim(ctx.Context, project.ID, ids, token))
			}
			if handoff != nil {
				return nil
			}
			if err := db.MarkClaimedNoticesDelivered(ctx.Context, project.ID, ids, token, currentTime()); err != nil {
				return err
			}
			return s.regenerateProjects(ctx.Context, db)
		}
		if time.Since(lastReexecCheck) >= time.Second {
			home, err := s.homePath()
			if err != nil {
				return err
			}
			if len(os.Args) > 1 && os.Args[1] == "_claude-stop" {
				// The hook cannot re-exec after consuming stdin. Its setup-manifest binary may differ, so only an explicit marker can request one rewake.
				if watchReexecRequested(home, os.Getpid()) {
					return s.reportWatchStopped(ctx, project, false)
				}
			} else if err := s.reexecWatchIfUpdated(project.ID, nil); err != nil {
				return err
			}
			lastReexecCheck = time.Now()
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
			result := axi.Object{{Key: "project", Value: project.Name}, {Key: "notices", Value: []any{}}, {Key: "state", Value: "timeout"}, {Key: "lowkey", Value: cfg.Lowkey.Lead}, {Key: "reporting_rule", Value: reportingRule(cfg.Lowkey.Lead)}}
			if acknowledged >= 0 {
				result = append(result, axi.Field{Key: "acknowledged", Value: acknowledged})
			}
			result = append(result, axi.Field{Key: "help", Value: []any{"Run `posse watch` to keep waiting for a Notice"}})
			return ctx.Print(result)
		}
		select {
		case <-stopSignals:
			return s.reportWatchStopped(ctx, project, false)
		case <-ctx.Context.Done():
			return ctx.Context.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (s *Service) reportWatchStopped(ctx *axi.Context, project store.Project, pollOnly bool) error {
	home, err := s.homePath()
	if err != nil {
		return err
	}
	reason := watchStopReason(home, os.Getpid())
	help := "The Lead should restart `posse watch` on its next wake"
	if pollOnly {
		help = "The Project watch supervisor repairs this process on the next posse command"
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
	count, autoTornDown, err := s.ackNotices(ctx, db, project, args)
	if err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "acknowledged", Value: count}, {Key: "auto_torn_down", Value: autoTornDown}, {Key: "help", Value: []any{"Run `posse` to see remaining open Notices"}}})
}

func (s *Service) ackNotices(ctx *axi.Context, db *store.DB, project store.Project, args []string) (int, []string, error) {
	return s.ackNoticesWithCommit(ctx, db, project, args, nil)
}

func (s *Service) ackNoticesWithCommit(ctx *axi.Context, db *store.DB, project store.Project, args []string, onCommit func(int)) (int, []string, error) {
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
	if onCommit != nil && (err == nil || count > 0) {
		onCommit(count)
	}
	if err != nil {
		return count, nil, axi.Failure("notice_invalid", err.Error(), false)
	}
	autoTornDown := []string{}
	if autoTeardownMode(cfg) == "finished" {
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
			if _, err := s.teardownTask(ctx.Context, db, project, cfg, task, false, ""); err != nil {
				return 0, nil, err
			}
			autoTornDown = append(autoTornDown, taskDisplayName(task))
		}
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return 0, nil, err
	}
	return count, autoTornDown, nil
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
	err = deferObservationContention(ctx, err, "Project observations were deferred; showing the last recorded state.")
	if err != nil {
		return err
	}
	if project.IsWorkspace() {
		repos, err := s.projectRepoRows(ctx.Context, db, project, cfg)
		if err != nil {
			return err
		}
		return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "kind", Value: project.Kind}, {Key: "root", Value: project.Root}, {Key: "status", Value: project.Status}, {Key: "repos", Value: repos}, {Key: "autonomy", Value: map[string]any{"review": defaultValue(cfg.Autonomy.Review, "ask"), "merge": defaultValue(cfg.Autonomy.Merge, "ask")}}, {Key: "lead", Value: s.leadStatus(ctx.Context, project)}, {Key: "help", Value: []any{"Run `posse config set repositories.<repo>.merge_mode pr --project " + project.Name + "` to change one member's Merge Mode", "Run `posse config set repositories.<repo>.target_branch <branch> --project " + project.Name + "` to set one member's PR/MR target", "Run `posse project scan` after adding or removing a member repository"}}})
	}
	if project.IsDirectory() {
		return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "kind", Value: project.Kind}, {Key: "root", Value: project.Root}, {Key: "status", Value: project.Status}, {Key: "autonomy", Value: map[string]any{"review": defaultValue(cfg.Autonomy.Review, "ask"), "merge": defaultValue(cfg.Autonomy.Merge, "ask")}}, {Key: "lead", Value: s.leadStatus(ctx.Context, project)}, {Key: "help", Value: []any{"Directory Projects are legacy records and do not admit new Tasks", "Register a Git repository with `posse project add`"}}})
	}
	targetBranch, _ := configTargetBranch(cfg, "", project.DefaultBranch)
	return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "root", Value: project.Root}, {Key: "default_branch", Value: project.DefaultBranch}, {Key: "target_branch", Value: targetBranch}, {Key: "status", Value: project.Status}, {Key: "merge_mode", Value: projectMergeMode(project, cfg)}, {Key: "autonomy", Value: map[string]any{"review": defaultValue(cfg.Autonomy.Review, "ask"), "merge": defaultValue(cfg.Autonomy.Merge, "ask")}}, {Key: "lead", Value: s.leadStatus(ctx.Context, project)}, {Key: "help", Value: []any{"Run `posse config set defaults.merge_mode \"local\" --project " + project.Name + "` to update Project settings", "Run `posse config set defaults.target_branch <branch> --project " + project.Name + "` to change the PR/MR target"}}})
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
		fmt.Fprintf(&out, "- Path: `%s`\n- Kind: %s\n- Merge Mode: %s\n- Autonomy: review=%s merge=%s\n- Lead: %s\n", project.Root, project.Kind, projectMergeMode(project, cfg), defaultValue(cfg.Autonomy.Review, "ask"), defaultValue(cfg.Autonomy.Merge, "ask"), s.leadStatus(ctx, project))
		if project.IsDirectory() {
			out.WriteString("- Admission: legacy Directory Project; new Tasks are unsupported\n")
		}
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
	return writeFile(filepath.Join(home, "projects.md"), []byte(out.String()))
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
