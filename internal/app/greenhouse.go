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
// Change Task Member and records it as a parallel-mode Verdict. Only the Lead
// side calls it; no gate reads Greenhouse Verdicts yet.
func (s *Service) greenhouseReview(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("_greenhouse review", args, map[string]flagSpec{"repo": {}, "rule": {repeatable: true}, "reviewer-backend": {}, "reviewer-model": {}, "timeout": {}, "parallel-owner": {}, "parallel-lock-fd": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("_greenhouse review requires one Task id")
	}
	wait := 30 * time.Minute
	if value := parsed.Flags["timeout"]; value != "" {
		millis, err := strconv.Atoi(value)
		if err != nil || millis < 1 {
			return axi.Usage("--timeout takes a positive number of milliseconds")
		}
		wait = time.Duration(millis) * time.Millisecond
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
	if s.workerCaller(ctx.Context, home) {
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
	parallelMode := plan != nil && plan.ValidationLoops != nil && plan.ValidationLoops.ReviewParallel == "greenhouse"
	roundDir := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), "greenhouse")
	pendingPath := filepath.Join(roundDir, member.name+".pending.json")
	_, pendingErr := os.Stat(pendingPath)
	hasPending := pendingErr == nil
	if pendingErr != nil && !errors.Is(pendingErr, os.ErrNotExist) {
		return pendingErr
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
			if recordErr := db.RecordGreenhouseReviewRequest(ctx.Context, store.GreenhouseReviewRequest{ProjectID: project.ID, TaskID: task.ID, Repository: member.name, RequestedHead: requestedHead, Provider: store.GreenhouseReviewProvider, RequestedAt: requestStartedAt, SettledAt: time.Now().UnixMilli(), Outcome: "failed"}); recordErr != nil {
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
		if err := db.RecordGreenhouseReviewRequest(ctx.Context, store.GreenhouseReviewRequest{ProjectID: project.ID, TaskID: task.ID, Repository: member.name, RequestedHead: requestedHead, Provider: store.GreenhouseReviewProvider, RequestedAt: requestStartedAt, SettledAt: time.Now().UnixMilli(), Outcome: "failed"}); err != nil {
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
		retries := 0
		for {
			round.Wait = time.Until(deadline)
			result, err = daemon.Review(ctx.Context, round)
			if err != nil {
				var protocolErr *greenhouse.ProtocolError
				if parallelMode && !errors.As(err, &protocolErr) && retries < 3 {
					if recordErr := recordRequest("failed", "", time.Now().UnixMilli()); recordErr != nil {
						return recordErr
					}
					timer := time.NewTimer(time.Duration(1<<retries) * time.Second)
					retries++
					select {
					case <-ctx.Context.Done():
						timer.Stop()
						return ctx.Context.Err()
					case <-timer.C:
					}
					deadline = time.Now().Add(wait)
					continue
				}
				break
			}
			if result.Outcome != greenhouse.Running && result.Outcome != greenhouse.Paused {
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
			if recordErr := recordRequest("failed", "", time.Now().UnixMilli()); recordErr != nil {
				return recordErr
			}
			if noticeErr := notifyFailure(requestHead, err.Error()); noticeErr != nil {
				return noticeErr
			}
			return axi.Failure("greenhouse_failed", err.Error(), true)
		}
		if result.Outcome == greenhouse.Running || result.Outcome == greenhouse.Paused {
			if err := recordRequest("failed", result.Grant, time.Now().UnixMilli()); err != nil {
				return err
			}
			if noticeErr := notifyFailure(requestHead, "review did not settle before the timeout"); noticeErr != nil {
				return noticeErr
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
			recorded, verdict, err := recordGreenhouseVerdict(ctx.Context, db, task, member, roundDir, blockAt, result)
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
			if plan == nil || plan.ValidationLoops == nil || plan.ValidationLoops.ReviewParallel != "greenhouse" {
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

func recordGreenhouseVerdict(ctx context.Context, db *store.DB, task store.Task, member greenhouseMember, roundDir, blockAt string, result greenhouse.RoundResult) (bool, store.Verdict, error) {
	hr := result.Result
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
		Grant: result.Grant, Head: hr.Head, Target: hr.Target, ReviewBase: hr.ReviewBase, Outcome: hr.Verdict,
		RulesDigest: hr.RulesDigest, ConfigDigest: hr.ConfigDigest, InputDigest: hr.InputDigest, Backend: hr.Backend, Model: hr.Model,
		FindingsHash: hex.EncodeToString(digest[:]), FindingsPath: path, Mode: "parallel",
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
	binary, err := os.Executable()
	if err != nil {
		return "", err
	}
	log, err := os.OpenFile(filepath.Join(greenhouseHome, "posse-daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer log.Close()
	waiter := exec.Command(binary, "_greenhouse", "stop")
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
