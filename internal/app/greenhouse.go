package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

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
// Change Task Member and records it as a parallel-mode Verdict. A Project Lead
// or its verified Watch may request it; no gate reads Greenhouse Verdicts yet.
func (s *Service) greenhouseReview(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("_greenhouse review", args, map[string]flagSpec{"repo": {}, "rule": {repeatable: true}, "reviewer-backend": {}, "reviewer-model": {}, "timeout": {}, "parallel-owner": {}, "parallel-lock-fd": {}, "watch-project-id": {}, "watch-token": {}, "watch-started-at": {}, "watch-pid": {}})
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
	requestedAt := time.Now().UnixMilli()
	requestedHead, err := gitOutput(ctx.Context, member.worktree, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	requestStartedAt := requestedAt
	requestedHead, requestStartedAt = parallelRequestMetadata(requestedHead, requestStartedAt, hasPending, requestOwner)
	target, err := greenhouseTarget(ctx.Context, member)
	if err != nil {
		if parallelMode {
			grant, grantErr := parallelPendingGrant(pendingPath)
			if grantErr != nil {
				return grantErr
			}
			if recordErr := db.RecordGreenhouseReviewRequest(ctx.Context, store.GreenhouseReviewRequest{ProjectID: project.ID, TaskID: task.ID, Repository: member.name, RequestedHead: requestedHead, Provider: store.GreenhouseReviewProvider, RequestedAt: requestStartedAt, SettledAt: time.Now().UnixMilli(), Outcome: "failed", Grant: grant}); recordErr != nil {
				return recordErr
			}
			if noticeErr := notifyFailure(requestedHead, err.Error()); noticeErr != nil {
				return noticeErr
			}
		}
		return err
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
	binary, err := exec.LookPath("greenhouse")
	if err != nil {
		if recordErr := recordInitialFailure(err.Error()); recordErr != nil {
			return recordErr
		}
		return axi.Failure("greenhouse_missing", "greenhouse is not installed on PATH", false, "Install Greenhouse, then retry")
	}
	tasks, err := posseTasks(ctx.Context, db)
	if err != nil {
		if recordErr := recordInitialFailure(err.Error()); recordErr != nil {
			return recordErr
		}
		return err
	}
	daemon := &greenhouse.Daemon{Binary: binary, Home: greenhouse.HomeFor(home), Env: os.Environ(), InTask: func(pid int) bool {
		for _, task := range tasks {
			if processInTaskContainment(pid, task) {
				return true
			}
		}
		return false
	}}
	hostID := project.Name + "/" + taskIDString(task.Seq) + "/" + member.name
	if err := os.MkdirAll(roundDir, 0o700); err != nil {
		if recordErr := recordInitialFailure(err.Error()); recordErr != nil {
			return recordErr
		}
		return err
	}
	round := greenhouse.Round{
		Worktree: member.worktree, Host: greenhouse.Host{Name: "posse", ID: hostID}, GrantPrefix: hostID,
		Intent: greenhouseIntent(task, plan), Target: target,
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
	if parallelMode {
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
		ownerHead, ownerStarted := parallelRequestMetadata(requestedHead, requestStartedAt, hasPending, requestOwner)
		if err := writeParallelReviewOwner(ownerPath, parallelReviewOwner{PID: os.Getpid(), Head: ownerHead, RequestedAt: ownerStarted}); err != nil {
			_ = releaseParallelReviewLock(parallelLock)
			if recordErr := recordInitialFailure(err.Error()); recordErr != nil {
				return recordErr
			}
			return err
		}
		defer func() {
			_ = clearParallelReviewOwner(ownerPath, pendingPath, os.Getpid())
			_ = releaseParallelReviewLock(parallelLock)
		}()
	}
	blockAt := "P1"
	if plan != nil && plan.ValidationLoops != nil && plan.ValidationLoops.Review.BlockAt != "" {
		blockAt = plan.ValidationLoops.Review.BlockAt
	}
	var refused []axi.Object
	parallelAttempt := 0
	for range greenhouseStaleRounds {
		deadline := time.Now().Add(wait)
		requestedAt := time.Now().UnixMilli()
		requestHead, headErr := gitOutput(ctx.Context, member.worktree, "rev-parse", "HEAD")
		if headErr != nil {
			return headErr
		}
		if parallelLock != nil {
			requestHead, requestedAt = parallelRequestMetadata(requestHead, requestedAt, parallelAttempt == 0 && hasPending, requestOwner)
			if err := writeParallelReviewOwner(ownerPath, parallelReviewOwner{PID: os.Getpid(), Head: requestHead, RequestedAt: requestedAt}); err != nil {
				return err
			}
		}
		parallelAttempt++
		var requestSettledAt int64
		recordRequest := func(outcome, grant string, settledAt int64) error {
			if !parallelMode {
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
			return db.RecordGreenhouseReviewRequest(ctx.Context, store.GreenhouseReviewRequest{
				ProjectID: project.ID, TaskID: task.ID, Repository: member.name, RequestedHead: requestHead,
				Provider: store.GreenhouseReviewProvider, RequestedAt: requestedAt, SettledAt: settledAt,
				Outcome: outcome, Grant: grant,
			})
		}
		var result greenhouse.RoundResult
		for {
			var attempts int
			result, attempts, err = reviewGreenhouseWithRetries(ctx.Context, parallelMode, time.Until(deadline), func(attemptWait time.Duration) (greenhouse.RoundResult, error) {
				round.Wait = attemptWait
				if s.greenhouseRound != nil {
					return s.greenhouseRound(ctx.Context, round)
				}
				return daemon.Review(ctx.Context, round)
			})
			if attempts > 0 {
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
			failureReason := err.Error()
			var terminalErr *greenhouse.TerminalReviewError
			if errors.As(err, &terminalErr) {
				failureReason = fmt.Sprintf("Greenhouse terminal status %q: %s", terminalErr.Status, terminalErr.Reason)
			}
			if recordErr := recordRequest("failed", result.Grant, time.Now().UnixMilli()); recordErr != nil {
				return recordErr
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
			if err := greenhouse.ClearPending(round.Pending); err != nil {
				return err
			}
			return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "repo", Value: member.name}, {Key: "outcome", Value: string(result.Outcome)}, {Key: "grant", Value: result.Grant}})
		}
		output := axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "repo", Value: member.name}, {Key: "outcome", Value: string(result.Outcome)}, {Key: "grant", Value: result.Grant}}
		switch result.Outcome {
		case greenhouse.Settled:
			crashIntentAt("greenhouse-review", "after", "settled")
			if result.Result == nil || !result.Result.Current || result.Result.Head != requestHead {
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
			recorded, verdict, err := recordGreenhouseVerdict(ctx.Context, db, task, member, roundDir, blockAt, cfg, result)
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
			if err := greenhouse.ClearPending(round.Pending); err != nil {
				return err
			}
			output = append(output, axi.Field{Key: "head", Value: verdict.Head}, axi.Field{Key: "verdict", Value: verdict.Outcome},
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
			if err := recordRequest("failed", result.Grant, time.Now().UnixMilli()); err != nil {
				return err
			}
			if plan == nil || plan.ValidationLoops == nil || plan.ValidationLoops.ReviewParallel != "greenhouse" && plan.ValidationLoops.ReviewProvider != store.GreenhouseReviewProvider {
				summary := fmt.Sprintf("%s: Greenhouse review %s of %s parked after an interruption (%s); Posse issues no new grant until it recovers", taskIDString(task.Seq), result.Grant, member.name, result.Recovery.Reason)
				if _, err := db.CreateGreenhouseRecoveryNoticeOnce(ctx.Context, project.ID, task.ID, member.name, result.Recovery.ID, summary); err != nil {
					return err
				}
			} else {
				if noticeErr := notifyFailure(requestHead, result.Recovery.Reason); noticeErr != nil {
					return noticeErr
				}
			}
			output = append(output, axi.Field{Key: "recovery", Value: result.Recovery.ID})
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
		current, err := greenhouseWorkspacePendingHeads(ctx, db, task)
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
			continue
		}
		if err := s.startParallelGreenhouseReviews(ctx, db, home, project, task, targets); err != nil {
			return err
		}
		verdicts, err := db.VerdictsFor(ctx, task.ID)
		if err != nil {
			return err
		}
		allPassed := true
		var failed *store.Verdict
		for _, target := range targets {
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
			if !latest.Qualifies(target.Commit) {
				allPassed = false
				failed = latest
				break
			}
		}
		if !allPassed {
			if failed != nil && failed.Outcome == "findings" && failed.Blocking > 0 {
				pending, err := db.ReviewFixInstructionPending(ctx, task.ID)
				if err != nil {
					return err
				}
				if !pending {
					if _, err := s.queueReviewFix(ctx, db, project, task, nil, false); err != nil {
						return err
					}
				}
			}
			if failed != nil {
				_, err := db.CreateGreenhouseFailureNoticeOnce(ctx, project.ID, task.ID, failed.Repository, failed.Head, fmt.Sprintf("Greenhouse returned %s for Member %s at %s; publication is blocked", failed.Outcome, failed.Repository, shortSHA(failed.Head)))
				if err != nil {
					return err
				}
			}
			continue
		}
		var body strings.Builder
		body.WriteString("Greenhouse passed these exact Member heads. Publish each unchanged head with the command shown, then signal done after all changed Members are published:\n\n")
		for _, target := range targets {
			fmt.Fprintf(&body, "- %s at %s: `posse publish --repo %s \"<summary>\"`\n", target.Repository, shortSHA(target.Commit), target.Repository)
		}
		_, queued, err := db.QueueMessageOnceAfter(ctx, task.ID, body.String(), true, signals[0].At-1)
		if err != nil {
			return err
		}
		if queued {
			if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "greenhouse_publish_queued", Summary: "Greenhouse passed all changed Member heads; Posse queued publication", DataJSON: marshalJSON(map[string]any{"members": targets})}); err != nil {
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

func (s *Service) reconcileGreenhouseRepositoryPrepublication(ctx context.Context, db *store.DB, home string, project store.Project, cfg config.Config) error {
	tasks, err := db.TasksInState(ctx, project.ID, store.StateDone)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		signals, err := db.TaskSignals(ctx, task.ID, 1)
		if err != nil {
			return err
		}
		usesGreenhouse := taskUsesGreenhouseReviewProvider(task, cfg, "") && fullPipelineSelected(task, "")
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
		head, err := gitOutput(ctx, task.WorktreePath, "rev-parse", "HEAD")
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
			if verdict.Qualifies(head) {
				body := fmt.Sprintf("Greenhouse passed Task head %s. Publish this exact head with `posse publish \"%s\"`, then report done with the PR URL. Do not change the branch before publishing; a new head needs a new done Signal and Verdict.", shortSHA(head), task.Title)
				_, queued, err := db.QueueMessageOnceAfter(ctx, task.ID, body, true, signals[0].At-1)
				if err != nil {
					return err
				}
				if queued {
					if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "greenhouse_publish_queued", Summary: fmt.Sprintf("%s: Greenhouse passed %s; Posse queued publication", task.Title, shortSHA(head)), DataJSON: marshalJSON(map[string]string{"head": head})}); err != nil {
						return err
					}
				}
				continue
			}
			reason := fmt.Sprintf("Greenhouse returned %s for Task head %s; Posse will not publish it. The review findings are at %s. A new head requires a new done Signal.", verdict.Outcome, shortSHA(head), verdict.FindingsPath)
			if verdict.Outcome == "findings" && verdict.Blocking > 0 {
				pending, err := db.ReviewFixInstructionPending(ctx, task.ID)
				if err != nil {
					return err
				}
				if !pending {
					if _, err := s.queueReviewFix(ctx, db, project, task, nil, false); err != nil {
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
		return false, store.Verdict{}, fmt.Errorf("Greenhouse run %s review_base %s differs from Posse-computed review base %s", result.RunID, hr.ReviewBase, reviewBase)
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
	if hr.Verdict == "findings" {
		verdict.Blocking = greenhouseBlocking(hr.Findings, blockAt)
	}
	recorded, err := db.RecordGreenhouseVerdict(ctx, verdict, func(ctx context.Context) (string, error) {
		return gitOutput(ctx, member.worktree, "rev-parse", "HEAD")
	})
	return recorded, verdict, err
}

// greenhouseBlocking counts findings at or above blockAt. An unknown priority blocks.
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

func greenhouseBlocking(findings []greenhouse.Finding, blockAt string) int {
	rank := map[string]int{"P0": 0, "P1": 1, "P2": 2, "P3": 3}
	threshold := rank[blockAt]
	blocking := 0
	for _, finding := range findings {
		if priority, known := rank[finding.Priority]; !known || priority <= threshold {
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
	if doneWhen == "" {
		return title
	}
	return title + "\n\nDone when: " + doneWhen
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
	state, err := greenhouse.ReadState(greenhouseHome)
	if err != nil || state.PID != pid {
		return name, "warn", fmt.Sprintf("PID %d serves %s, but Posse has no protocol record for it", pid, greenhouseHome), "Request a Greenhouse review to probe it again"
	}
	if !state.Range.Includes(greenhouse.Protocol) {
		return name, "fail", fmt.Sprintf("PID %d speaks protocol %s; Posse needs protocol %s", pid, state.Range, greenhouse.PosseRange), "Let its work settle, stop it with SIGTERM to PID " + strconv.Itoa(pid) + " and install a Greenhouse that speaks protocol " + strconv.Itoa(greenhouse.Protocol)
	}
	return name, "ok", fmt.Sprintf("PID %d speaks protocol %s; Posse uses %d", pid, state.Range, greenhouse.Protocol), ""
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
