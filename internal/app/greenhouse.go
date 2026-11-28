package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/thanhbinh1905/posse/internal/atomicfile"
	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/greenhouse"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

// greenhouseStaleRounds bounds the rounds one invocation starts after the head
// moves under a review.
const greenhouseStaleRounds = 3

type greenhouseMember struct {
	name, root, worktree, targetBranch string
}

// greenhouseReview requests one Greenhouse Verdict for the exact head of a
// Change Task Member and records the result. A Project Lead or its verified
// Watch may request it.
func (s *Service) greenhouseReview(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("_greenhouse review", args, map[string]flagSpec{"repo": {}, "head": {}, "follow-grant": {}, "rule": {repeatable: true}, "reviewer-backend": {}, "reviewer-model": {}, "timeout": {}, "greenhouse-binary": {}, "greenhouse-home": {}, "parallel-owner": {}, "parallel-lock-fd": {}, "watch-project-id": {}, "watch-token": {}, "watch-started-at": {}, "watch-pid": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("_greenhouse review requires one Task id")
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
	watchChild, err := s.verifiedWatchChild(ctx.Context, db, home, project, parsed.Flags)
	if err != nil {
		return err
	}
	if !watchChild {
		if err := s.requireLead(ctx.Context, db, project); err != nil {
			return err
		}
	}
	if !watchChild && s.workerCaller(ctx.Context, home) {
		return axi.Failure("lead_only", "a Worker never calls Greenhouse", false, "Run this from the Project Lead pane")
	}
	_, hasBinary := parsed.Flags["greenhouse-binary"]
	_, hasHome := parsed.Flags["greenhouse-home"]
	if watchChild && (hasBinary || hasHome) {
		return axi.Failure("lead_only", "only the Project Lead can select an isolated Greenhouse runtime", false)
	}
	override, err := parseGreenhouseRuntime(home, parsed.Flags)
	if err != nil {
		return err
	}
	if project.IsDirectory() {
		return executionUnavailable(project)
	}
	task, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	if task.Type != "change" {
		return axi.Failure("greenhouse_target_invalid", "Greenhouse reviews Change Tasks only", false)
	}
	member, err := greenhouseTaskMember(ctx.Context, db, project, task, parsed.Flags["repo"])
	if err != nil {
		return err
	}
	plan, err := task.AdmittedPlan()
	if err != nil {
		return admittedPlanFailure(err)
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return configError(err)
	}
	waitValue := cfg.ReviewLoop.GreenhouseTimeout
	if plan != nil && plan.ValidationLoops != nil && plan.ValidationLoops.Review.GreenhouseTimeout != "" {
		waitValue = plan.ValidationLoops.Review.GreenhouseTimeout
	}
	wait, err := time.ParseDuration(waitValue)
	if err != nil || wait <= 0 {
		return axi.Failure("greenhouse_timeout_invalid", "review_loop.greenhouse_timeout must be a positive duration", false)
	}
	if value := parsed.Flags["timeout"]; value != "" {
		millis, parseErr := strconv.Atoi(value)
		if parseErr != nil || millis < 1 {
			return axi.Usage("--timeout takes a positive number of milliseconds")
		}
		wait = time.Duration(millis) * time.Millisecond
	}
	parallelMode := plan != nil && plan.ValidationLoops != nil && plan.ValidationLoops.ReviewParallel == "greenhouse" || taskUsesGreenhouseReviewProvider(task, cfg, member.name)
	followGrant := parsed.Flags["follow-grant"]
	if followGrant != "" && (!parallelMode || parsed.Flags["head"] == "") {
		return axi.Usage("--follow-grant requires an exact parallel review head")
	}
	roundDir := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), "greenhouse")
	pendingPath := filepath.Join(roundDir, member.name+".pending.json")
	hasPending, err := parallelReviewPending(pendingPath)
	if err != nil {
		return err
	}
	ownerPath := parallelReviewOwnerPath(roundDir, member.name)
	requestOwner, ownerErr := readParallelReviewOwner(ownerPath)
	if ownerErr != nil {
		return ownerErr
	}
	notifyFailure := func(head, reason string) error {
		if !parallelMode {
			return nil
		}
		reason += ". Request or retry the Greenhouse Verdict with `posse review start " + taskIDString(task.Seq) + "`"
		_, err := db.CreateGreenhouseFailureNoticeOnce(ctx.Context, project.ID, task.ID, member.name, head, reason)
		return err
	}
	if value := parsed.Flags["head"]; value != "" {
		if !parallelMode || len(value) != 40 && len(value) != 64 {
			return axi.Usage("--head requires an internal parallel review commit SHA")
		}
		if _, err := hex.DecodeString(value); err != nil {
			return axi.Usage("--head requires an internal parallel review commit SHA")
		}
	}
	requestedAt := time.Now().UnixMilli()
	requestedHead := parsed.Flags["head"]
	if requestedHead == "" && hasPending && requestOwner != nil {
		requestedHead = requestOwner.Head
	}
	if requestedHead == "" {
		requestedHead, err = taskBranchAuthorHead(ctx.Context, member.root, task.Branch)
		if err != nil {
			return err
		}
	}
	requestStartedAt := requestedAt
	requestedHead, requestStartedAt = parallelRequestMetadata(requestedHead, requestStartedAt, hasPending, requestOwner)
	if followGrant != "" && requestedHead != parsed.Flags["head"] {
		return axi.Failure("greenhouse_head_mismatch", "the pending Greenhouse review does not match the requested head", false)
	}
	requestedHead, err = gitOutput(ctx.Context, member.root, "rev-parse", "--verify", requestedHead+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve Greenhouse review head: %w", err)
	}
	recordInitialFailure := func(reason string) error {
		if !parallelMode {
			return nil
		}
		grant, err := parallelPendingGrant(pendingPath)
		if err != nil {
			return err
		}
		if err := db.RecordGreenhouseReviewRequest(ctx.Context, store.GreenhouseReviewRequest{ProjectID: project.ID, TaskID: task.ID, Repository: member.name, RequestedHead: requestedHead, Provider: store.GreenhouseReviewProvider, RequestedAt: requestStartedAt, SettledAt: time.Now().UnixMilli(), Outcome: "failed", Grant: grant}); err != nil {
			return err
		}
		return notifyFailure(requestedHead, reason)
	}
	if err := os.MkdirAll(roundDir, 0o700); err != nil {
		if recordErr := recordInitialFailure(err.Error()); recordErr != nil {
			return recordErr
		}
		return err
	}
	round := greenhouse.Round{
		Worktree: member.worktree, MinimumGrantNumber: 1,
		Intent:   greenhouseIntent(task, plan),
		Settings: greenhouse.Settings{Rules: parsed.Repeated["rule"], ReviewerBackend: parsed.Flags["reviewer-backend"], ReviewerModel: parsed.Flags["reviewer-model"]},
		Pending:  pendingPath, Wait: wait,
		Checkpoint: func(step string) { crashIntentAt("greenhouse-review", "after", step) },
	}
	var parallelLock *os.File
	suppliedOwner := parsed.Flags["parallel-owner"]
	suppliedLockFD := parsed.Flags["parallel-lock-fd"]
	if !parallelMode && (suppliedOwner != "" || suppliedLockFD != "") {
		return axi.Usage("parallel owner flags require a frozen review_parallel setting")
	}
	if suppliedOwner != "" && suppliedOwner != ownerPath {
		return axi.Usage("invalid internal Greenhouse parallel owner path")
	}
	lockPath := parallelReviewLockPath(roundDir, member.name)
	if suppliedLockFD != "" {
		fd, parseErr := strconv.Atoi(suppliedLockFD)
		if parseErr != nil || fd != 3 {
			return axi.Usage("invalid internal Greenhouse parallel lock descriptor")
		}
		parallelLock, err = inheritedParallelReviewLock(fd, lockPath)
	} else {
		var busy bool
		parallelLock, busy, err = acquireParallelReviewLock(lockPath)
		if busy {
			return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "repo", Value: member.name}, {Key: "outcome", Value: "in_flight"}})
		}
	}
	if err != nil {
		if recordErr := recordInitialFailure(err.Error()); recordErr != nil {
			return recordErr
		}
		return err
	}
	defer releaseParallelReviewLock(parallelLock)
	if err := checkPendingGreenhouseRuntime(pendingPath, override); err != nil {
		return err
	}
	daemon, err := newGreenhouseDaemon(ctx.Context, db, home, override)
	if err != nil {
		if recordErr := recordInitialFailure(err.Error()); recordErr != nil {
			return recordErr
		}
		return err
	}
	round.Host = greenhouseReviewHost(project, task, member)
	round.GrantPrefix = daemon.GrantPrefix(round.Host)
	if followGrant != "" {
		pendingGrant, err := parallelPendingGrant(pendingPath)
		if err != nil {
			return err
		}
		if pendingGrant != followGrant {
			return axi.Failure("greenhouse_recovery_refused", "the restarted grant is no longer pending", false)
		}
	} else if daemon.Isolated {
		pendingGrant, err := parallelPendingGrant(pendingPath)
		if err != nil {
			return err
		}
		if pendingGrant != "" {
			restarted, err := db.GreenhouseReviewRestartedSince(ctx.Context, task.ID, member.name, requestedHead, pendingGrant, 0)
			if err != nil {
				return err
			}
			if restarted {
				followGrant = pendingGrant
			}
		}
	}
	if parallelMode {
		requests, err := db.GreenhouseReviewRequests(ctx.Context, project.ID)
		if err != nil {
			return err
		}
		round.MinimumGrantNumber = nextGreenhouseLedgerGrant(requests, task.ID, member.name, round.GrantPrefix)
		ownerHead, ownerStarted := parallelRequestMetadata(requestedHead, requestStartedAt, hasPending, requestOwner)
		if err := writeParallelReviewOwner(ownerPath, parallelReviewOwner{PID: os.Getpid(), Head: ownerHead, RequestedAt: ownerStarted}); err != nil {
			if recordErr := recordInitialFailure(err.Error()); recordErr != nil {
				return recordErr
			}
			return err
		}
		defer clearParallelReviewOwner(ownerPath, pendingPath, os.Getpid())
	}
	blockAt := "P1"
	if plan != nil && plan.ValidationLoops != nil && plan.ValidationLoops.Review.BlockAt != "" {
		blockAt = plan.ValidationLoops.Review.BlockAt
	}
	var refused []axi.Object
	parallelAttempt := 0
	rounds := greenhouseStaleRounds
	if followGrant != "" {
		rounds = 1
	}
	for range rounds {
		deadline := time.Now().Add(wait)
		requestHead, requestedAt := requestedHead, requestStartedAt
		if parallelAttempt > 0 {
			var headErr error
			requestHead, headErr = taskBranchAuthorHead(ctx.Context, member.root, task.Branch)
			if headErr != nil {
				return headErr
			}
			requestedAt = time.Now().UnixMilli()
		}
		if parallelMode {
			if err := writeParallelReviewOwner(ownerPath, parallelReviewOwner{PID: os.Getpid(), Head: requestHead, RequestedAt: requestedAt}); err != nil {
				return err
			}
		}
		reviewMember, err := pinGreenhouseReviewCheckout(ctx.Context, db, home, project, task, member, requestHead)
		if err != nil {
			return err
		}
		round.Worktree = reviewMember.worktree
		parallelAttempt++
		var requestSettledAt int64
		var result greenhouse.RoundResult
		recordRequest := func(outcome, grant string, settledAt int64) error {
			if !parallelMode && (result.Result == nil || result.Result.Usage == nil) {
				return nil
			}
			if outcome == "failed" || outcome == "refused" {
				if pendingGrant, err := parallelPendingGrant(pendingPath); err != nil {
					return err
				} else if grant == "" {
					grant = pendingGrant
				}
			}
			if requestSettledAt > 0 {
				settledAt = requestSettledAt
			}
			if grant == "" && result.Result != nil {
				grant = result.Result.Grant
			}
			if err := db.RecordGreenhouseReviewRequest(ctx.Context, store.GreenhouseReviewRequest{
				ProjectID: project.ID, TaskID: task.ID, Repository: member.name, RequestedHead: requestHead,
				Provider: store.GreenhouseReviewProvider, RequestedAt: requestedAt, SettledAt: settledAt,
				Outcome: outcome, Grant: grant,
			}); err != nil {
				return err
			}
			if result.Result == nil || result.Result.Usage == nil {
				return nil
			}
			usageJSON, err := json.Marshal(struct {
				Usage     *greenhouse.TokenUsage      `json:"usage"`
				Breakdown []greenhouse.UsageBreakdown `json:"usage_breakdown,omitempty"`
			}{Usage: result.Result.Usage, Breakdown: result.Result.UsageBreakdown})
			if err != nil {
				return err
			}
			u := result.Result.Usage
			return db.RecordTaskReviewUsage(ctx.Context, store.TaskReviewUsage{TaskID: task.ID, Repository: member.name, Head: requestHead, Grant: grant, Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.CachedInputTokens, CacheWrite: u.CacheWriteTokens, CostUSD: u.CostUSD, UsageJSON: string(usageJSON)})
		}
		requestAttempted := false
		for {
			var attempts int
			result, attempts, err = reviewGreenhouseWithRetries(ctx.Context, parallelMode, time.Until(deadline), func(attemptWait time.Duration) (greenhouse.RoundResult, error) {
				round.Wait = attemptWait
				if followGrant != "" {
					requestAttempted = true
					return daemon.FollowReview(ctx.Context, round, followGrant)
				}
				round.Target, err = greenhouseTarget(ctx.Context, member)
				if err != nil {
					return greenhouse.RoundResult{}, err
				}
				requestAttempted = true
				if s.greenhouseRound != nil {
					return s.greenhouseRound(ctx.Context, round)
				}
				return daemon.Review(ctx.Context, round)
			})
			if attempts > 0 && requestAttempted {
				requestSettledAt = time.Now().UnixMilli()
			}
			if ctx.Context.Err() != nil {
				return ctx.Context.Err()
			}
			if err != nil || !parallelMode || result.Outcome != greenhouse.Running && result.Outcome != greenhouse.Paused {
				break
			}
			if time.Until(deadline) <= 0 {
				break
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Context.Done():
				timer.Stop()
				return ctx.Context.Err()
			case <-timer.C:
			}
		}
		requestSettledAt = time.Now().UnixMilli()
		if errors.Is(err, greenhouse.ErrRuntimeMismatch) {
			return axi.Failure("greenhouse_runtime_conflict", err.Error(), false)
		}
		var protocolErr *greenhouse.ProtocolError
		if errors.As(err, &protocolErr) {
			if recordErr := recordRequest("refused", "", time.Now().UnixMilli()); recordErr != nil {
				return recordErr
			}
			if noticeErr := notifyFailure(requestHead, protocolErr.Error()); noticeErr != nil {
				return noticeErr
			}
			return axi.Failure("greenhouse_protocol", protocolErr.Error(), false, "No Verdict was requested")
		}
		if err != nil {
			if !requestAttempted {
				if noticeErr := notifyFailure(requestHead, err.Error()); noticeErr != nil {
					return noticeErr
				}
				var failure *axi.Error
				if errors.As(err, &failure) {
					return failure
				}
				return axi.Failure("greenhouse_target_unavailable", err.Error(), true, "Retry `posse review start "+taskIDString(task.Seq)+"` when the forge is reachable")
			}
			failureReason := err.Error()
			if parallelMode && transientGreenhouseFailure(err) {
				if recordErr := recordRequest("refused", result.Grant, time.Now().UnixMilli()); recordErr != nil {
					return recordErr
				}
				return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "repo", Value: member.name}, {Key: "outcome", Value: "retrying"}, {Key: "head", Value: requestHead}})
			}
			var terminalErr *greenhouse.TerminalReviewError
			if errors.As(err, &terminalErr) {
				failureReason = fmt.Sprintf("Greenhouse terminal status %q: %s", terminalErr.Status, terminalErr.Reason)
			}
			if recordErr := recordRequest("failed", result.Grant, time.Now().UnixMilli()); recordErr != nil {
				return recordErr
			}
			if clearErr := greenhouse.ClearPending(pendingPath); clearErr != nil {
				return clearErr
			}
			terminalFailure := reviewerSizeFailure(err)
			if parallelMode {
				requests, err := db.GreenhouseReviewRequests(ctx.Context, project.ID)
				if err != nil {
					return err
				}
				failures, _ := repeatedGreenhouseReviewFailures(requests, task.ID, member.name, requestHead)
				terminalFailure = terminalFailure || failures >= greenhouseReviewFailureLimit
			}
			if terminalFailure {
				reason := fmt.Sprintf("Needs a decision: split the change or land it another way. Latest failure: %s", failureReason)
				if _, err := db.CreateGreenhouseNeedsDecisionNoticeOnce(ctx.Context, project.ID, task.ID, member.name, requestHead, reason); err != nil {
					return err
				}
				return axi.Failure("greenhouse_failed", failureReason, false)
			}
			if noticeErr := notifyFailure(requestHead, failureReason); noticeErr != nil {
				return noticeErr
			}
			return axi.Failure("greenhouse_failed", failureReason, true)
		}
		if result.Outcome == greenhouse.Running || result.Outcome == greenhouse.Paused {
			if err := recordRequest("failed", result.Grant, time.Now().UnixMilli()); err != nil {
				return err
			}
			if noticeErr := notifyFailure(requestHead, "review did not settle before the configured timeout"); noticeErr != nil {
				return noticeErr
			}
			return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "repo", Value: member.name}, {Key: "outcome", Value: string(result.Outcome)}, {Key: "grant", Value: result.Grant}})
		}
		output := axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "repo", Value: member.name}, {Key: "outcome", Value: string(result.Outcome)}, {Key: "grant", Value: result.Grant}}
		switch result.Outcome {
		case greenhouse.Settled:
			crashIntentAt("greenhouse-review", "after", "settled")
			if result.Result == nil || result.Result.Head != requestHead {
				if err := recordRequest("head_mismatch", result.Grant, time.Now().UnixMilli()); err != nil {
					return err
				}
				if noticeErr := notifyFailure(requestHead, "Greenhouse settled on a different or non-current head"); noticeErr != nil {
					return noticeErr
				}
				if err := greenhouse.ClearPending(round.Pending); err != nil {
					return err
				}
				continue
			}
			recorded, verdict, err := recordGreenhouseVerdict(ctx.Context, db, task, reviewMember, roundDir, blockAt, cfg, result)
			if errors.Is(err, store.ErrVerdictHeadMoved) {
				if recordErr := recordRequest("head_mismatch", result.Grant, time.Now().UnixMilli()); recordErr != nil {
					return recordErr
				}
				if noticeErr := notifyFailure(requestHead, err.Error()); noticeErr != nil {
					return noticeErr
				}
				refused = append(refused, axi.Object{{Key: "grant", Value: result.Grant}, {Key: "reason", Value: err.Error()}})
				if err := greenhouse.ClearPending(round.Pending); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				if recordErr := recordRequest("failed", result.Grant, time.Now().UnixMilli()); recordErr != nil {
					return recordErr
				}
				if clearErr := greenhouse.ClearPending(round.Pending); clearErr != nil {
					return clearErr
				}
				if noticeErr := notifyFailure(requestHead, fmt.Sprintf("could not record Greenhouse run %s Verdict: %v", result.RunID, err)); noticeErr != nil {
					return noticeErr
				}
				return err
			}
			if err := recordRequest("verdict", result.Grant, time.Now().UnixMilli()); err != nil {
				return err
			}
			if len(result.Result.Uncovered) > 0 {
				reviewedHead := result.Result.Head
				summary := greenhousePartialDecisionQuestion(member.name, shortSHA(reviewedHead), *result.Result)
				if _, err := db.CreateGreenhousePartialNoticeOnce(ctx.Context, project.ID, task.ID, member.name, reviewedHead, summary, result.Result); err != nil {
					return err
				}
			}
			if err := s.applyGreenhouseVerdict(ctx.Context, db, project, task, verdict); err != nil {
				return err
			}
			if err := greenhouse.ClearPending(round.Pending); err != nil {
				return err
			}
			outputVerdict := result.Result.Verdict
			if len(result.Result.Uncovered) > 0 && outputVerdict == "clean" {
				outputVerdict = "partial"
			}
			output = append(output, axi.Field{Key: "head", Value: result.Result.Head}, axi.Field{Key: "verdict", Value: outputVerdict},
				axi.Field{Key: "blocking", Value: verdict.Blocking}, axi.Field{Key: "findings", Value: verdict.FindingsPath}, axi.Field{Key: "recorded", Value: recorded}, axi.Field{Key: "replayed", Value: result.Replayed})
		case greenhouse.Stale:
			if err := recordRequest("head_mismatch", result.Grant, time.Now().UnixMilli()); err != nil {
				return err
			}
			if noticeErr := notifyFailure(requestHead, "the head moved during the review"); noticeErr != nil {
				return noticeErr
			}
			refused = append(refused, axi.Object{{Key: "grant", Value: result.Grant}, {Key: "reason", Value: "the head moved during the review"}})
			if err := greenhouse.ClearPending(round.Pending); err != nil {
				return err
			}
			continue
		case greenhouse.RecoveryRequired:
			command := fmt.Sprintf("posse review recover %s --repo %s --head %s --recovery %s", taskIDString(task.Seq), member.name, requestHead, result.Recovery.ID)
			if daemon.Isolated {
				command += " --greenhouse-binary " + shellQuote(daemon.Binary) + " --greenhouse-home " + shellQuote(daemon.Home)
			}
			summary := fmt.Sprintf("%s: Greenhouse review %s of %s parked after an interruption (%s). Run `%s` after the interrupted executor exits", taskIDString(task.Seq), result.Grant, member.name, result.Recovery.Reason, command)
			created, err := db.CreateGreenhouseRecoveryNoticeOnce(ctx.Context, project.ID, task.ID, member.name, result.Recovery.ID, summary)
			if err != nil {
				return err
			}
			if created {
				if err := recordRequest("failed", result.Grant, time.Now().UnixMilli()); err != nil {
					return err
				}
			}
			output = append(output, axi.Field{Key: "recovery", Value: result.Recovery.ID}, axi.Field{Key: "head", Value: requestHead}, axi.Field{Key: "help", Value: []string{command}})
		}
		if len(refused) > 0 {
			output = append(output, axi.Field{Key: "refused", Value: refused})
		}
		return ctx.Print(output)
	}
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "repo", Value: member.name}, {Key: "outcome", Value: string(greenhouse.Stale)}, {Key: "refused", Value: refused}})
}

func (s *Service) reconcileGreenhousePrepublication(ctx context.Context, db *store.DB, home string, project store.Project) error {
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return configError(err)
	}
	if project.IsWorkspace() {
		return s.reconcileGreenhouseWorkspacePrepublication(ctx, db, home, project, cfg)
	}
	return s.reconcileGreenhouseRepositoryPrepublication(ctx, db, home, project, cfg)
}

func (s *Service) reconcileGreenhouseWorkspacePrepublication(ctx context.Context, db *store.DB, home string, project store.Project, cfg config.Config) error {
	tasks, err := db.TasksInState(ctx, project.ID, store.StateDone)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		signals, err := db.TaskSignals(ctx, task.ID, 1)
		if err != nil {
			return err
		}
		if len(signals) == 0 || signals[0].Verb != "done" {
			continue
		}
		var data struct {
			Members []greenhousePendingMember `json:"greenhouse_pending_members"`
		}
		if err := json.Unmarshal([]byte(signals[0].DataJSON), &data); err != nil || len(data.Members) == 0 {
			continue
		}
		current, err := greenhouseWorkspacePendingHeads(ctx, db, project, task)
		if err != nil {
			return err
		}
		currentByRepo := make(map[string]string, len(current))
		for _, member := range current {
			currentByRepo[member.Repository] = member.Head
		}
		targets := make([]store.ReviewTarget, 0, len(data.Members))
		moved := false
		for _, member := range data.Members {
			if currentByRepo[member.Repository] != member.Head {
				moved = true
				break
			}
			targets = append(targets, store.ReviewTarget{Repository: member.Repository, Commit: member.Head})
		}
		if moved {
			for _, member := range data.Members {
				currentHead := currentByRepo[member.Repository]
				if currentHead == member.Head {
					continue
				}
				reason := fmt.Sprintf("pending Member head %s moved to %s; run `posse review start %s` to request Verdicts for the current Member heads", shortSHA(member.Head), shortSHA(currentHead), taskIDString(task.Seq))
				if _, err := db.CreateGreenhouseFailureNoticeOnce(ctx, project.ID, task.ID, member.Repository, member.Head, reason); err != nil {
					return err
				}
			}
			continue
		}
		waivedMembers := make(map[string]bool)
		var pending []store.ReviewTarget
		for _, target := range targets {
			waived, err := hasReviewWaiver(ctx, db, task.ID, target.Repository, target.Commit)
			if err != nil {
				return err
			}
			if waived {
				waivedMembers[target.Repository] = true
			} else {
				pending = append(pending, target)
			}
		}
		if err := s.startParallelGreenhouseReviews(ctx, db, home, project, task, pending); err != nil {
			return err
		}
		verdicts, err := db.VerdictsFor(ctx, task.ID)
		if err != nil {
			return err
		}
		allPassed := true
		acceptedMembers := make(map[string]bool)
		var failed *store.Verdict
		for _, target := range pending {
			var latest *store.Verdict
			for index := len(verdicts) - 1; index >= 0; index-- {
				verdict := verdicts[index]
				if verdict.Repository == target.Repository && verdict.Mode == "gate" && verdict.Head == target.Commit {
					latest = &verdict
					break
				}
			}
			if latest == nil {
				allPassed = false
				break
			}
			partialReview, err := greenhousePartialReviewApplies(ctx, db, task, target.Repository, target.Commit)
			if err != nil {
				return err
			}
			if partialReview {
				accepted, err := greenhouseAcceptHeadDisposition(ctx, db, task, target.Repository, target.Commit)
				if err != nil {
					return err
				}
				if accepted && latest.Mode == "gate" && latest.Blocking == 0 {
					acceptedMembers[target.Repository] = true
					continue
				}
			} else if latest.Qualifies(target.Commit) {
				continue
			}
			allPassed = false
			failed = latest
			break
		}
		if !allPassed {
			if failed != nil && failed.Outcome == "findings" && failed.Blocking > 0 {
				pending, err := db.ReviewFixInstructionPending(ctx, task.ID)
				if err != nil {
					return err
				}
				if !pending {
					if _, err := s.queueGreenhouseReviewFix(ctx, db, project, task); err != nil {
						return err
					}
				}
			}
			if failed != nil {
				partialReview, err := greenhousePartialReviewApplies(ctx, db, task, failed.Repository, failed.Head)
				if err != nil {
					return err
				}
				if !partialReview || failed.Blocking > 0 {
					_, err := db.CreateGreenhouseFailureNoticeOnce(ctx, project.ID, task.ID, failed.Repository, failed.Head, fmt.Sprintf("Greenhouse returned %s for Member %s at %s; publication is blocked", failed.Outcome, failed.Repository, shortSHA(failed.Head)))
					if err != nil {
						return err
					}
				}
			}
			continue
		}
		var body strings.Builder
		summary := "Greenhouse passed all changed Member heads; Posse queued publication"
		intro := "Greenhouse passed or the User accepted coverage for these exact Member heads."
		if len(waivedMembers) > 0 {
			summary = "All changed Member heads cleared the publication gate; Posse queued publication"
			intro = "These exact Member heads are cleared for publication."
		}
		body.WriteString(intro + " Publish each unchanged head with the command shown, then signal done after all changed Members are published:\n\n")
		for _, target := range targets {
			status := "Greenhouse passed"
			if waivedMembers[target.Repository] {
				status = "User waived Greenhouse review; forge CI is still required"
			} else if acceptedMembers[target.Repository] {
				status = "User accepted coverage"
			}
			fmt.Fprintf(&body, "- %s: %s at %s: `posse publish --repo %s \"<summary>\"`\n", target.Repository, status, shortSHA(target.Commit), target.Repository)
		}
		_, queued, err := db.QueueMessageOnceAfter(ctx, task.ID, body.String(), true, signals[0].At-1)
		if err != nil {
			return err
		}
		if queued {
			if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "greenhouse_publish_queued", Summary: summary, DataJSON: marshalJSON(map[string]any{"members": targets})}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) noticeSkippedGreenhouseTask(ctx context.Context, db *store.DB, project store.Project, task store.Task, reason string) error {
	head, err := gitOutput(ctx, task.WorktreePath, "rev-parse", "HEAD")
	if err != nil {
		head = task.GatedSHA
		reason += "; current Task head could not be read: " + err.Error()
	}
	reason += "; run `posse review start " + taskIDString(task.Seq) + "` to request a Greenhouse Verdict"
	_, err = db.CreateGreenhouseFailureNoticeOnce(ctx, project.ID, task.ID, project.Name, head, reason)
	return err
}

func queueWaivedReviewPublication(ctx context.Context, db *store.DB, task store.Task, repository, head string, since int64) (bool, error) {
	waived, err := hasReviewWaiver(ctx, db, task.ID, repository, head)
	if err != nil || !waived {
		return waived, err
	}
	body := fmt.Sprintf("User waived Greenhouse review for Task head %s. Publish it unchanged with `posse publish \"<summary>\"`, then signal done with the PR URL. Forge CI is still required.", head)
	_, _, err = db.QueueMessageOnceAfter(ctx, task.ID, body, true, since)
	return true, err
}

func (s *Service) reconcileGreenhouseRepositoryPrepublication(ctx context.Context, db *store.DB, home string, project store.Project, cfg config.Config) error {
	tasks, err := db.TasksInState(ctx, project.ID, store.StateDone)
	if err != nil {
		return err
	}
	waiting, err := db.TasksInState(ctx, project.ID, store.StateNeedsDecision)
	if err != nil {
		return err
	}
	tasks = append(tasks, waiting...)
	for _, task := range tasks {
		signals, err := db.TaskSignals(ctx, task.ID, 1)
		if err != nil {
			return err
		}
		usesGreenhouse := taskUsesGreenhouseReviewProvider(task, cfg, "") && fullPipelineSelected(task, "")
		if usesGreenhouse && task.State == store.StateDone && len(signals) > 0 && signals[0].Verb == "done" {
			intent, intentErr := db.IntentByTask(ctx, task.ID)
			if intentErr != nil && !store.IsNotFound(intentErr) {
				return intentErr
			}
			var published struct {
				PRURL string `json:"pr_url"`
			}
			isPublished := json.Unmarshal([]byte(signals[0].DataJSON), &published) == nil && published.PRURL != ""
			if intentErr == nil && intent.Command == "pr-release" && !isPublished {
				head, err := taskBranchAuthorHead(ctx, project.Root, task.Branch)
				if err != nil {
					return err
				}
				reason := "publication is blocked by an unfinished pr-release intent; resume with `posse pr release " + taskIDString(task.Seq) + " --policy release` or discard with User approval"
				if _, err := db.CreateGreenhouseFailureNoticeOnce(ctx, project.ID, task.ID, project.Name, head, reason); err != nil {
					return err
				}
				continue
			}
		}
		if len(signals) > 0 && signals[0].Verb == "needs-decision" && usesGreenhouse {
			head, err := taskBranchAuthorHead(ctx, project.Root, task.Branch)
			if err != nil {
				return err
			}
			if waived, err := queueWaivedReviewPublication(ctx, db, task, project.Name, head, signals[0].At-1); err != nil {
				return err
			} else if waived {
				continue
			}
			verdicts, err := db.VerdictsFor(ctx, task.ID)
			if err != nil {
				return err
			}
			for index := len(verdicts) - 1; index >= 0; index-- {
				verdict := verdicts[index]
				if verdict.Repository != project.Name || verdict.Provider != store.GreenhouseReviewProvider || verdict.Mode != "gate" || !verdict.Qualifies(head) {
					continue
				}
				body := fmt.Sprintf("Greenhouse passed Task head %s. Publish this exact head with `posse publish \"%s\"`, then report done with the PR URL. Do not change the branch before publishing; a new head needs a new done Signal and Verdict.", shortSHA(head), task.Title)
				if _, _, err := db.QueueMessageOnceAfter(ctx, task.ID, body, true, signals[0].At-1); err != nil {
					return err
				}
				break
			}
			continue
		}
		if len(signals) == 0 || signals[0].Verb != "done" {
			if usesGreenhouse {
				if err := s.noticeSkippedGreenhouseTask(ctx, db, project, task, "the completed Task has no latest done Signal to anchor an exact-head Greenhouse review"); err != nil {
					return err
				}
			}
			continue
		}
		var data struct {
			Head string `json:"greenhouse_pending_head"`
		}
		if err := json.Unmarshal([]byte(signals[0].DataJSON), &data); err != nil || data.Head == "" {
			if usesGreenhouse {
				var published struct {
					PRURL string `json:"pr_url"`
				}
				if json.Unmarshal([]byte(signals[0].DataJSON), &published) == nil && published.PRURL != "" {
					head, err := taskBranchAuthorHead(ctx, project.Root, task.Branch)
					if err != nil {
						return err
					}
					verified, err := db.WasVerifiedPRHead(ctx, task.ID, published.PRURL, head)
					if err != nil {
						return err
					}
					passed, err := greenhouseGateSatisfied(ctx, db, task, project.Name, head, true)
					if err != nil {
						return err
					}
					if verified && passed {
						continue
					}
				}
				if task.Type == "change" {
					count, err := commitsAheadOfBase(ctx, task.WorktreePath, task.BaseRef)
					if err != nil {
						return err
					}
					if count == "0" {
						continue
					}
				}
				reason := "the latest done Signal has invalid or missing greenhouse_pending_head data"
				if err := s.noticeSkippedGreenhouseTask(ctx, db, project, task, reason); err != nil {
					return err
				}
			}
			continue
		}
		if !taskUsesGreenhouseReviewProvider(task, cfg, "") || !fullPipelineSelected(task, "") {
			reason := fmt.Sprintf("the done Signal requests Greenhouse review at %s, but the admitted Task plan does not select Greenhouse full-pipeline review; run `posse review start %s` to request a Verdict", shortSHA(data.Head), taskIDString(task.Seq))
			if _, err := db.CreateGreenhouseFailureNoticeOnce(ctx, project.ID, task.ID, project.Name, data.Head, reason); err != nil {
				return err
			}
			continue
		}
		head, err := taskBranchAuthorHead(ctx, project.Root, task.Branch)
		if err != nil {
			return err
		}
		if head != data.Head {
			reason := fmt.Sprintf("the Task head moved from %s to %s after its done Signal; run `posse review start %s` to request a Verdict for the current head", shortSHA(data.Head), shortSHA(head), taskIDString(task.Seq))
			if _, err := db.CreateGreenhouseFailureNoticeOnce(ctx, project.ID, task.ID, project.Name, data.Head, reason); err != nil {
				return err
			}
			continue
		}
		if waived, err := queueWaivedReviewPublication(ctx, db, task, project.Name, head, signals[0].At-1); err != nil {
			return err
		} else if waived {
			continue
		}
		if err := s.startParallelGreenhouseReviews(ctx, db, home, project, task, []store.ReviewTarget{{Repository: project.Name, Commit: head}}); err != nil {
			return err
		}
		verdicts, err := db.VerdictsFor(ctx, task.ID)
		if err != nil {
			return err
		}
		var latest *store.Verdict
		for index := len(verdicts) - 1; index >= 0; index-- {
			verdict := verdicts[index]
			if verdict.Repository == project.Name && verdict.Mode == "gate" && verdict.Head == head {
				latest = &verdict
				break
			}
		}
		if latest != nil {
			verdict := *latest
			partialReview, err := greenhousePartialReviewApplies(ctx, db, task, project.Name, head)
			if err != nil {
				return err
			}
			accepted := false
			if partialReview {
				accepted, err = greenhouseAcceptHeadDisposition(ctx, db, task, project.Name, head)
				if err != nil {
					return err
				}
			}
			if (accepted && verdict.Mode == "gate" && verdict.Blocking == 0) || (!partialReview && verdict.Qualifies(head)) {
				message := fmt.Sprintf("Greenhouse passed Task head %s", shortSHA(head))
				if accepted {
					message = fmt.Sprintf("User accepted Greenhouse coverage at Task head %s", shortSHA(head))
				}
				body := fmt.Sprintf("%s. Publish this exact head with `posse publish \"%s\"`, then report done with the PR URL. Do not change the branch before publishing; a new head needs a new done Signal and Verdict.", message, task.Title)
				_, queued, err := db.QueueMessageOnceAfter(ctx, task.ID, body, true, signals[0].At-1)
				if err != nil {
					return err
				}
				if queued {
					if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "greenhouse_publish_queued", Summary: fmt.Sprintf("%s: %s; Posse queued publication", task.Title, message), DataJSON: marshalJSON(map[string]string{"head": head})}); err != nil {
						return err
					}
				}
				continue
			}
			if partialReview {
				continue
			}
			reason := fmt.Sprintf("Greenhouse returned %s for Task head %s; Posse will not publish it. The review findings are at %s. A new head requires a new done Signal.", verdict.Outcome, shortSHA(head), verdict.FindingsPath)
			if verdict.Outcome == "findings" && verdict.Blocking > 0 {
				pending, err := db.ReviewFixInstructionPending(ctx, task.ID)
				if err != nil {
					return err
				}
				if !pending {
					if _, err := s.queueGreenhouseReviewFix(ctx, db, project, task); err != nil {
						_, _ = db.CreateGreenhouseFailureNoticeOnce(ctx, project.ID, task.ID, project.Name, head, reason)
						return err
					}
				}
			}
			if _, err := db.CreateGreenhouseFailureNoticeOnce(ctx, project.ID, task.ID, project.Name, head, reason); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) queueGreenhouseReviewFix(ctx context.Context, db *store.DB, project store.Project, task store.Task) (reviewFixQueued, error) {
	queued, err := s.queueReviewFix(ctx, db, project, task, nil, false)
	var failure *axi.Error
	if errors.As(err, &failure) && (failure.Code == "review_fix_empty" || failure.Code == "review_fix_duplicate" || failure.Code == "review_fix_pending") {
		return reviewFixQueued{}, nil
	}
	return queued, err
}

func (s *Service) applyGreenhouseVerdict(ctx context.Context, db *store.DB, project store.Project, task store.Task, verdict store.Verdict) error {
	if verdict.Outcome == "findings" && verdict.Blocking > 0 {
		return s.handleGreenhouseBlockingVerdict(ctx, db, project, task, verdict)
	}
	return nil
}

func (s *Service) handleGreenhouseBlockingVerdict(ctx context.Context, db *store.DB, project store.Project, task store.Task, verdict store.Verdict) error {
	if verdict.FindingsPath == "" || verdict.Head == "" {
		return nil
	}
	current, err := db.TaskByID(ctx, project.ID, task.ID)
	if err != nil {
		return err
	}
	if current.State != store.StateDone && current.State != store.StateMerging {
		return nil
	}
	if project.IsWorkspace() {
		heads, err := greenhouseWorkspacePendingHeads(ctx, db, project, current)
		if err != nil {
			return err
		}
		currentHead := ""
		for _, member := range heads {
			if member.Repository == verdict.Repository {
				currentHead = member.Head
				break
			}
		}
		if currentHead != verdict.Head {
			return nil
		}
	} else {
		head, err := taskBranchAuthorHead(ctx, project.Root, current.Branch)
		if err != nil {
			return err
		}
		if head != verdict.Head {
			return nil
		}
	}
	waived, err := hasReviewWaiver(ctx, db, current.ID, verdict.Repository, verdict.Head)
	if err != nil || waived {
		return err
	}
	if current.State == store.StateMerging {
		if err := db.Transition(ctx, current.ID, store.StateMerging, store.StateDone, "cli", "Greenhouse found blocking Review findings at "+shortSHA(verdict.Head)); err != nil {
			return err
		}
	}
	reason := fmt.Sprintf("Greenhouse found %d blocking finding(s) for head %s. Findings: %s", verdict.Blocking, shortSHA(verdict.Head), verdict.FindingsPath)
	signals, err := db.TaskSignals(ctx, current.ID, 1)
	if err != nil {
		return err
	}
	currentCycle := len(signals) > 0 && signals[0].Verb == "done"
	readinessSince := int64(0)
	if currentCycle {
		readinessSince = signals[0].At
	}
	readiness := currentCycle && current.State == store.StateMerging && current.GatedSHA == verdict.Head
	if readiness && current.UpdatedAt > readinessSince {
		readinessSince = current.UpdatedAt
	}
	decisions, err := db.PendingDecisions(ctx, project.ID)
	if err != nil {
		return err
	}
	for _, decision := range decisions {
		if currentCycle && decision.TaskID == current.ID && decision.Kind == "merge_ready" && current.GatedSHA == verdict.Head && decision.CreatedAt >= readinessSince {
			readiness = true
		}
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		return err
	}
	withdrawalAt := int64(0)
	for _, notice := range notices {
		if notice.TaskID != current.ID {
			continue
		}
		var data struct {
			HeadSHA   string            `json:"head_sha"`
			GatedHead string            `json:"gated_head"`
			PRHead    string            `json:"pr_head"`
			Repo      string            `json:"repository"`
			ShortRepo string            `json:"repo"`
			HeadSHAs  map[string]string `json:"head_shas"`
		}
		if json.Unmarshal([]byte(notice.DataJSON), &data) != nil {
			continue
		}
		noticeHead := data.HeadSHA
		if workspaceHead := data.HeadSHAs[verdict.Repository]; workspaceHead != "" {
			noticeHead = workspaceHead
		}
		if currentCycle && notice.Kind == "merge_ready" && noticeHead == verdict.Head && notice.CreatedAt >= readinessSince {
			readiness = true
		}
		withdrawalHead := data.HeadSHA
		if data.GatedHead != "" {
			withdrawalHead = data.GatedHead
		}
		if data.PRHead != "" && data.PRHead == verdict.Head {
			withdrawalHead = data.PRHead
		}
		noticeRepository := data.Repo
		if noticeRepository == "" {
			noticeRepository = data.ShortRepo
		}
		if notice.Kind == "merge_readiness_withdrawn" && withdrawalHead == verdict.Head && (noticeRepository == verdict.Repository || noticeRepository == "" && (!project.IsWorkspace() || verdict.Repository == project.Name)) && notice.CreatedAt > withdrawalAt {
			withdrawalAt = notice.CreatedAt
		}
	}
	withdrawalRecorded := currentCycle && withdrawalAt >= readinessSince && withdrawalAt > 0
	if readiness {
		if err := db.ObsoletePendingMergeReadyDecisions(ctx, current.ID, reason); err != nil {
			return err
		}
		if !withdrawalRecorded {
			if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: current.ID, Kind: "merge_readiness_withdrawn", Summary: current.Title + ": merge readiness withdrawn because Greenhouse found blocking findings at " + shortSHA(verdict.Head) + "; findings: " + verdict.FindingsPath, DataJSON: marshalJSON(map[string]string{"head_sha": verdict.Head, "findings": verdict.FindingsPath, "repository": verdict.Repository})}); err != nil {
				return err
			}
		}
	}
	if _, err := db.CreateGreenhouseFailureNoticeOnce(ctx, project.ID, current.ID, verdict.Repository, verdict.Head, reason); err != nil {
		return err
	}
	pending, err := db.ReviewFixInstructionPending(ctx, current.ID)
	if err != nil {
		return err
	}
	if !pending {
		if _, err := s.queueGreenhouseReviewFix(ctx, db, project, current); err != nil {
			return err
		}
	}
	if err := s.deliverNotices(ctx, db, project); err != nil && !isHerdrUnavailable(err) {
		return err
	}
	return nil
}

func greenhouseVerdictMode(task store.Task, cfg config.Config, repository string) string {
	if taskUsesGreenhouseReviewProvider(task, cfg, repository) {
		return "gate"
	}
	return "parallel"
}

func recordGreenhouseVerdict(ctx context.Context, db *store.DB, task store.Task, member greenhouseMember, roundDir, blockAt string, cfg config.Config, result greenhouse.RoundResult) (bool, store.Verdict, error) {
	hr := result.Result
	reviewBase, err := greenhouseReviewBase(ctx, member.worktree, hr.Target, hr.Head)
	if err != nil {
		return false, store.Verdict{}, fmt.Errorf("compute review base for Greenhouse run %s: %w", result.RunID, err)
	}
	if hr.ReviewBase != "" && hr.ReviewBase != reviewBase {
		return false, store.Verdict{}, fmt.Errorf("greenhouse run %s review_base %s differs from Posse-computed review base %s", result.RunID, hr.ReviewBase, reviewBase)
	}
	findings, err := json.MarshalIndent(hr.Findings, "", "  ")
	if err != nil {
		return false, store.Verdict{}, err
	}
	path := filepath.Join(roundDir, strings.ReplaceAll(result.Grant, "/", "_")+".findings.json")
	if err := atomicfile.Write(path, findings, 0o600); err != nil {
		return false, store.Verdict{}, err
	}
	digest := sha256.Sum256(findings)
	verdict := store.Verdict{
		TaskID: task.ID, Repository: member.name, Provider: store.GreenhouseReviewProvider, Protocol: strconv.Itoa(greenhouse.Protocol),
		Grant: result.Grant, Head: hr.Head, Target: hr.Target, ReviewBase: reviewBase, Outcome: hr.Verdict,
		RulesDigest: hr.RulesDigest, ConfigDigest: hr.ConfigDigest, InputDigest: hr.InputDigest, Backend: hr.Backend, Model: hr.Model,
		FindingsHash: hex.EncodeToString(digest[:]), FindingsPath: path, Mode: greenhouseVerdictMode(task, cfg, member.name),
	}
	for _, finding := range hr.Findings {
		verdict.Findings = append(verdict.Findings, store.ProviderFinding{
			Key: finding.ID, Severity: finding.Priority, Origin: finding.Origin, Path: finding.File, Line: finding.Start,
			Excerpt: finding.Explanation, Guidance: finding.Action, Rule: finding.Rule, Head: finding.Head,
		})
	}
	if hr.Verdict == "partial" || (len(hr.Uncovered) > 0 && hr.Verdict != "findings") {
		verdict.Outcome = "inconclusive"
	}
	if hr.Verdict == "findings" {
		verdict.Blocking = greenhouseBlocking(hr.Findings, blockAt)
		acceptedHead, err := acceptedGreenhouseHead(ctx, db, task.ID, member.root, verdict.Head)
		if err != nil {
			return false, store.Verdict{}, err
		}
		if acceptedHead != "" && acceptedHead != verdict.Head {
			var followups []store.ProviderFinding
			for index, finding := range verdict.Findings {
				if !greenhouseFindingBlocks(finding.Severity, blockAt) {
					continue
				}
				rangeFinding := hr.Findings[index]
				changed, err := greenhouseFindingChangedSinceAccepted(ctx, member.root, acceptedHead, verdict.Head, finding.Path, rangeFinding.Start, rangeFinding.End)
				if err != nil {
					return false, store.Verdict{}, err
				}
				if changed {
					followups = nil
					break
				}
				followups = append(followups, finding)
			}
			if verdict.Blocking > 0 && len(followups) == verdict.Blocking {
				if err := createAcceptedFindingFollowup(ctx, member, task, acceptedHead, followups, cfg); err != nil {
					return false, store.Verdict{}, err
				}
				verdict.Blocking = 0
			}
		}
	}
	recorded, err := db.RecordGreenhouseVerdict(ctx, verdict, func(ctx context.Context) (string, error) {
		return taskBranchAuthorHead(ctx, member.root, task.Branch)
	})
	if err != nil || recorded {
		return recorded, verdict, err
	}
	verdicts, err := db.VerdictsFor(ctx, task.ID)
	if err != nil {
		return false, store.Verdict{}, err
	}
	for _, persisted := range verdicts {
		if persisted.Grant == result.Grant && persisted.Repository == member.name && persisted.Provider == store.GreenhouseReviewProvider {
			return false, persisted, nil
		}
	}
	return false, store.Verdict{}, fmt.Errorf("greenhouse verdict for already-recorded grant %s was not found", result.Grant)
}

func acceptedGreenhouseHead(ctx context.Context, db *store.DB, taskID int64, repository, current string) (string, error) {
	rows, err := db.QueryContext(ctx, `SELECT rd.head_sha FROM review_dispositions rd JOIN decisions d ON d.id=rd.decision_id WHERE rd.ship_task_id=? AND d.kind='review_cap' AND d.answer='accept-head' AND d.answer_actor='user' AND d.answered_at>0 AND d.obsolete_at=0 ORDER BY rd.id DESC`, taskID)
	if err != nil {
		return "", err
	}
	var heads []string
	for rows.Next() {
		var head string
		if err := rows.Scan(&head); err != nil {
			rows.Close()
			return "", err
		}
		heads = append(heads, head)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	for _, head := range heads {
		if _, err := exec.CommandContext(ctx, "git", "-C", repository, "cat-file", "-e", head+"^{commit}").Output(); err != nil {
			continue
		}
		command := exec.CommandContext(ctx, "git", "-C", repository, "merge-base", "--is-ancestor", head, current)
		if err := command.Run(); err == nil {
			return head, nil
		} else {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				continue
			}
			return "", fmt.Errorf("check accepted Greenhouse head %s ancestry in repository: %w", head, err)
		}
	}
	return "", nil
}

func greenhouseFindingBlocks(severity, blockAt string) bool {
	rank := map[string]int{"P0": 0, "P1": 1, "P2": 2, "P3": 3}
	findingRank, findingKnown := rank[severity]
	threshold, thresholdKnown := rank[blockAt]
	return !findingKnown || !thresholdKnown || findingRank <= threshold
}

func createAcceptedFindingFollowup(ctx context.Context, member greenhouseMember, task store.Task, acceptedHead string, findings []store.ProviderFinding, cfg config.Config) error {
	forge, err := forgeForRepository(ctx, member.root, cfg, member.name)
	if err != nil {
		return fmt.Errorf("resolve forge for accepted Greenhouse findings: %w", err)
	}
	title := fmt.Sprintf("Follow up Greenhouse findings from Task t%d accepted at %s", task.Seq, shortSHA(acceptedHead))
	var body strings.Builder
	fmt.Fprintf(&body, "Blocking Greenhouse findings were reported on lines unchanged since the User accepted head `%s` for Task t%d. They do not block this target-merge re-review.\n\n", acceptedHead, task.Seq)
	for _, finding := range findings {
		fmt.Fprintf(&body, "- **%s** `%s:%d` (%s): %s\n  - %s\n", finding.Severity, finding.Path, finding.Line, finding.Key, finding.Excerpt, finding.Guidance)
	}
	if forge.Kind == "github" {
		args := []string{"issue", "list", "--repo", forge.Host + "/" + forge.Path, "--state", "open", "--search", "in:title " + title, "--json", "number,title", "--limit", "100"}
		output, err := commandOutputArgs(ctx, member.root, "gh", args...)
		if err != nil {
			return fmt.Errorf("find follow-up issue for accepted Greenhouse findings: %w", err)
		}
		var issues []struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
		}
		if err := json.Unmarshal([]byte(output), &issues); err != nil {
			return fmt.Errorf("decode follow-up issues for accepted Greenhouse findings: %w", err)
		}
		for _, issue := range issues {
			if issue.Title == title {
				_, err = commandOutputArgs(ctx, member.root, "gh", "issue", "edit", strconv.Itoa(issue.Number), "--body", body.String())
				if err != nil {
					return fmt.Errorf("update follow-up issue for accepted Greenhouse findings: %w", err)
				}
				return nil
			}
		}
		_, err = commandOutputArgs(ctx, member.root, "gh", "issue", "create", "--repo", forge.Host+"/"+forge.Path, "--title", title, "--body", body.String())
		if err != nil {
			return fmt.Errorf("file follow-up issue for accepted Greenhouse findings: %w", err)
		}
		return nil
	}
	endpoint := "projects/" + url.PathEscape(forge.Path) + "/issues?state=opened&search=" + url.QueryEscape(title)
	output, err := commandOutputArgs(ctx, member.root, "glab", "api", "--hostname", forge.Host, "--paginate", endpoint)
	if err != nil {
		return fmt.Errorf("find follow-up issue for accepted Greenhouse findings: %w", err)
	}
	var issues []struct {
		IID   int    `json:"iid"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(output), &issues); err != nil {
		return fmt.Errorf("decode follow-up issues for accepted Greenhouse findings: %w", err)
	}
	for _, issue := range issues {
		if issue.Title == title {
			_, err = commandOutputArgs(ctx, member.root, "glab", "api", "--hostname", forge.Host, "--method", "PUT", "--raw-field", "description="+body.String(), "projects/"+url.PathEscape(forge.Path)+"/issues/"+strconv.Itoa(issue.IID))
			if err != nil {
				return fmt.Errorf("update follow-up issue for accepted Greenhouse findings: %w", err)
			}
			return nil
		}
	}
	_, err = commandOutputArgs(ctx, member.root, "glab", "api", "--hostname", forge.Host, "--method", "POST", "--raw-field", "title="+title, "--raw-field", "description="+body.String(), "projects/"+url.PathEscape(forge.Path)+"/issues")
	if err != nil {
		return fmt.Errorf("file follow-up issue for accepted Greenhouse findings: %w", err)
	}
	return nil
}

func greenhouseFindingChangedSinceAccepted(ctx context.Context, repository, accepted, current, file string, start, end int) (bool, error) {
	if accepted == "" || current == "" || file == "" || filepath.IsAbs(file) || start < 1 {
		return true, nil
	}
	if end < start {
		end = start
	}
	for _, part := range strings.Split(filepath.ToSlash(file), "/") {
		if part == ".." {
			return true, nil
		}
	}
	ancestor := exec.CommandContext(ctx, "git", "-C", repository, "merge-base", "--is-ancestor", accepted, current)
	if err := ancestor.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return true, nil
		}
		return false, fmt.Errorf("check accepted Greenhouse head ancestry: %w", err)
	}
	command := exec.CommandContext(ctx, "git", "-C", repository, "diff", "--no-ext-diff", "--no-renames", "--unified=0", accepted, current, "--", filepath.ToSlash(file))
	output, err := command.Output()
	if err != nil {
		return false, fmt.Errorf("diff accepted Greenhouse head %s against %s: %w", accepted, current, err)
	}
	for _, match := range greenhouseDiffHunk.FindAllSubmatch(output, -1) {
		hunkStart, err := strconv.Atoi(string(match[1]))
		if err != nil {
			return false, fmt.Errorf("parse Greenhouse diff hunk: %w", err)
		}
		count := 1
		if len(match[2]) > 0 {
			count, err = strconv.Atoi(string(match[2]))
			if err != nil {
				return false, fmt.Errorf("parse Greenhouse diff hunk: %w", err)
			}
		}
		if count > 0 && hunkStart <= end && hunkStart+count-1 >= start || count == 0 && hunkStart >= start && hunkStart <= end+1 {
			return true, nil
		}
	}
	return false, nil
}

var greenhouseDiffHunk = regexp.MustCompile(`@@ -[0-9]+(?:,[0-9]+)? \+([0-9]+)(?:,([0-9]+))? @@`)

func greenhouseReviewBase(ctx context.Context, worktree, target, candidate string) (string, error) {
	base, err := exec.CommandContext(ctx, "git", "-C", worktree, "merge-base", target, candidate).Output()
	if err == nil {
		return strings.TrimSpace(string(base)), nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return target, nil
	}
	return "", fmt.Errorf("git merge-base %s %s: %w", target, candidate, err)
}

// greenhouseBlocking counts findings at or above blockAt. An unknown priority blocks.
func greenhouseBlocking(findings []greenhouse.Finding, blockAt string) int {
	blocking := 0
	for _, finding := range findings {
		if greenhouseFindingBlocks(finding.Priority, blockAt) {
			blocking++
		}
	}
	return blocking
}

func greenhouseIntent(task store.Task, plan *taskplan.Plan) string {
	title, doneWhen := task.Title, ""
	if plan != nil {
		for _, input := range plan.Inputs {
			if input.Role != "brief" {
				continue
			}
			if brief, err := dispatch.ParseBriefText(string(input.Bytes)); err == nil {
				if brief.Title != "" {
					title = brief.Title
				}
				doneWhen = brief.DoneWhen
			}
			break
		}
	}
	if doneWhen != "" {
		title += "\n\nDone when: " + doneWhen
	}
	intent := strings.ToValidUTF8(title, "")
	var clean strings.Builder
	for _, r := range intent {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			clean.WriteRune(r)
		}
	}
	intent = clean.String()
	if strings.TrimSpace(intent) == "" {
		intent = "Greenhouse review"
	}
	const maxBytes = 8192
	const marker = "[truncated]"
	if len(intent) > maxBytes {
		end := maxBytes - len(marker)
		for end > 0 && !utf8.RuneStart(intent[end]) {
			end--
		}
		intent = intent[:end] + marker
	}
	return intent
}

func greenhouseTaskMember(ctx context.Context, db *store.DB, project store.Project, task store.Task, repo string) (greenhouseMember, error) {
	if !project.IsWorkspace() {
		if repo != "" && repo != project.Name {
			return greenhouseMember{}, axi.Usage("this Project has no Member " + repo)
		}
		return greenhouseMember{name: project.Name, root: project.Root, worktree: task.WorktreePath, targetBranch: taskTargetBranch(project, task)}, nil
	}
	if repo == "" {
		return greenhouseMember{}, axi.Usage("a workspace Task needs --repo <member>")
	}
	projectRepos, err := db.ProjectRepos(ctx, project.ID)
	if err != nil {
		return greenhouseMember{}, err
	}
	taskRepos, err := db.TaskRepos(ctx, task.ID)
	if err != nil {
		return greenhouseMember{}, err
	}
	for _, taskRepo := range taskRepos {
		if taskRepo.Repo != repo {
			continue
		}
		for _, projectRepo := range projectRepos {
			if projectRepo.Name == repo {
				target := taskRepo.TargetBranch
				if target == "" {
					target = projectRepo.DefaultBranch
				}
				return greenhouseMember{name: repo, root: filepath.Join(project.Root, projectRepo.Path), worktree: taskRepo.WorktreePath, targetBranch: target}, nil
			}
		}
	}
	return greenhouseMember{}, axi.Usage("Task " + taskIDString(task.Seq) + " has no Member " + repo)
}

func greenhouseBinary() (string, error) {
	if binary, err := exec.LookPath("greenhouse"); err == nil {
		return binary, nil
	}
	candidates := []string{}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "greenhouse"))
	}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), "greenhouse"))
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("greenhouse is not installed on PATH or in ~/.local/bin")
}

func greenhouseTarget(ctx context.Context, member greenhouseMember) (string, error) {
	ref := "refs/heads/" + member.targetBranch
	if _, err := gitOutput(ctx, member.root, "remote", "get-url", "origin"); err == nil {
		ref = "refs/remotes/origin/" + member.targetBranch
		if _, err := gitFetch(ctx, member.root, "origin", member.targetBranch+":"+ref); err != nil {
			return "", axi.Failure("greenhouse_target_unavailable", "could not fetch target origin/"+member.targetBranch, true, err.Error())
		}
	}
	commit, err := gitOutput(ctx, member.root, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", axi.Failure("greenhouse_target_unavailable", "cannot resolve target "+member.targetBranch+" of "+member.name, false, err.Error())
	}
	return commit, nil
}

func greenhouseReviewHost(project store.Project, task store.Task, member greenhouseMember) greenhouse.Host {
	return greenhouse.Host{Name: "posse", ID: project.Name + "/" + taskIDString(task.Seq) + "/" + member.name}
}

func newGreenhouseDaemon(ctx context.Context, db *store.DB, home string, runtime greenhouse.ReviewRuntime) (*greenhouse.Daemon, error) {
	isolated := runtime != (greenhouse.ReviewRuntime{})
	if !isolated {
		binary, err := greenhouseBinary()
		if err != nil {
			return nil, axi.Failure("greenhouse_missing", err.Error(), false, "Install Greenhouse, then retry")
		}
		runtime = greenhouse.ReviewRuntime{Binary: binary, Home: greenhouse.HomeFor(home)}
	}
	tasks, err := posseTasks(ctx, db)
	if err != nil {
		return nil, err
	}
	return &greenhouse.Daemon{Binary: runtime.Binary, Home: runtime.Home, Isolated: isolated, Env: os.Environ(), InTask: func(pid int) bool {
		for _, task := range tasks {
			if processInTaskContainment(pid, task) {
				return true
			}
		}
		return false
	}}, nil
}

// posseTasks lists every Task of this Posse home whose processes may still run.
func posseTasks(ctx context.Context, db *store.DB) ([]store.Task, error) {
	projects, err := db.Projects(ctx)
	if err != nil {
		return nil, err
	}
	var tasks []store.Task
	for _, project := range projects {
		projectTasks, err := db.Tasks(ctx, project.ID, true)
		if err != nil {
			return nil, err
		}
		for _, task := range projectTasks {
			if task.State != store.StateTornDown {
				tasks = append(tasks, task)
			}
		}
	}
	return tasks, nil
}

// greenhouseDaemonCheck reports the protocol range Posse recorded when it placed
// the daemon. It never calls Greenhouse.
func greenhouseDaemonCheck(home string) (name, status, detail, action string) {
	name = "Greenhouse daemon"
	greenhouseHome := greenhouse.HomeFor(home)
	pid, live := greenhouse.LivePID(greenhouseHome)
	if !live {
		return name, "ok", "not running; Posse starts it in " + greenhouseHome + " when it requests a Greenhouse review", ""
	}
	binary := "unknown executable"
	if running, err := greenhouse.RunningExecutable(pid); err == nil {
		binary = running
	}
	state, err := greenhouse.ReadState(greenhouseHome)
	if err != nil || state.PID != pid {
		return name, "warn", fmt.Sprintf("PID %d runs %s and serves %s, but Posse has no protocol record for it", pid, binary, greenhouseHome), "Request a Greenhouse review to probe it again"
	}
	if !state.Range.Includes(greenhouse.Protocol) {
		return name, "fail", fmt.Sprintf("PID %d runs %s and speaks protocol %s; Posse needs protocol %s", pid, binary, state.Range, greenhouse.PosseRange), "Let its work settle, stop it with SIGTERM to PID " + strconv.Itoa(pid) + " and install a Greenhouse that speaks protocol " + strconv.Itoa(greenhouse.Protocol)
	}
	return name, "ok", fmt.Sprintf("PID %d runs %s and speaks protocol %s; Posse uses %d", pid, binary, state.Range, greenhouse.Protocol), ""
}

const greenhouseStopWait = 10 * time.Second

// greenhouseAfterDown stops the shared Greenhouse daemon once no Project of
// this Posse home is up. A daemon with a review in progress is handed to a
// detached `posse _greenhouse stop`, which stops it after the review settles.
func greenhouseAfterDown(ctx context.Context, db *store.DB, home string) (string, error) {
	greenhouseHome := greenhouse.HomeFor(home)
	if _, live := greenhouse.LivePID(greenhouseHome); !live {
		return "", nil
	}
	up, err := upProject(ctx, db)
	if err != nil {
		return "", err
	}
	if up != "" {
		return "kept: Project " + up + " is up", nil
	}
	idle, err := greenhouse.Idle(greenhouseHome)
	if err != nil {
		return "", err
	}
	if idle {
		if err := greenhouse.Stop(greenhouseHome, greenhouseStopWait); err != nil {
			return "", err
		}
		return "stopped", nil
	}
	log, err := os.OpenFile(filepath.Join(greenhouseHome, "posse-daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer log.Close()
	waiter, err := selfExecCommand("_greenhouse", "stop")
	if err != nil {
		return "", err
	}
	waiter.Dir = greenhouseHome
	for _, entry := range os.Environ() {
		if key, _, _ := strings.Cut(entry, "="); !watchRuntimeDropsEnvironmentVariable(key) {
			waiter.Env = append(waiter.Env, entry)
		}
	}
	waiter.Env = append(waiter.Env, "POSSE_HOME="+home)
	waiter.Stdout, waiter.Stderr = log, log
	waiter.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := waiter.Start(); err != nil {
		return "", fmt.Errorf("start the Greenhouse stop waiter: %w", err)
	}
	return "stops after its reviews settle", waiter.Process.Release()
}

// greenhouseStop waits until the Greenhouse daemon is idle, then stops it,
// unless a Project comes up first.
func (s *Service) greenhouseStop(ctx *axi.Context, args []string) error {
	if len(args) != 0 {
		return axi.Usage("_greenhouse stop takes no arguments")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if s.workerCaller(ctx.Context, home) {
		return axi.Failure("lead_only", "a Worker never calls Greenhouse", false)
	}
	greenhouseHome := greenhouse.HomeFor(home)
	for {
		if _, live := greenhouse.LivePID(greenhouseHome); !live {
			return ctx.Print(axi.Object{{Key: "greenhouse", Value: "not running"}})
		}
		up, err := upProject(ctx.Context, db)
		if err != nil {
			return err
		}
		if up != "" {
			return ctx.Print(axi.Object{{Key: "greenhouse", Value: "kept: Project " + up + " is up"}})
		}
		idle, err := greenhouse.Idle(greenhouseHome)
		if err != nil {
			return err
		}
		if idle {
			if err := greenhouse.Stop(greenhouseHome, greenhouseStopWait); err != nil {
				return err
			}
			return ctx.Print(axi.Object{{Key: "greenhouse", Value: "stopped"}})
		}
		select {
		case <-ctx.Context.Done():
			return ctx.Context.Err()
		case <-time.After(time.Second):
		}
	}
}

// upProject names a Project of this Posse home whose Lead is up.
func upProject(ctx context.Context, db *store.DB) (string, error) {
	projects, err := db.Projects(ctx)
	if err != nil {
		return "", err
	}
	for _, project := range projects {
		if !project.IsDown() && project.LeadPaneID != "" {
			return project.Name, nil
		}
	}
	return "", nil
}
