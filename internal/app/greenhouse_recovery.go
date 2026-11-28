package app

import (
	"encoding/hex"
	"errors"
	"path/filepath"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/greenhouse"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) reviewRecover(out *axi.Context, args []string) error {
	parsed, err := parseArgs("review recover", args, map[string]flagSpec{"repo": {}, "head": {}, "recovery": {}, "greenhouse-binary": {}, "greenhouse-home": {}})
	if err != nil {
		return err
	}
	head, recovery := parsed.Flags["head"], parsed.Flags["recovery"]
	if len(parsed.Positionals) != 1 || recovery == "" {
		return axi.Usage("review recover requires one Task id, --head <full-sha> and --recovery <id>")
	}
	if _, err := hex.DecodeString(head); err != nil || len(head) != 40 && len(head) != 64 {
		return axi.Usage("--head requires a full commit SHA")
	}
	ctx := out.Context
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx, db)
	if err != nil {
		return err
	}
	if err := s.requireLead(ctx, db, project); err != nil {
		return err
	}
	if s.workerCaller(ctx, home) {
		return axi.Failure("lead_only", "a Worker never calls Greenhouse", false, "Run this from the Project Lead pane")
	}
	override, err := parseGreenhouseRuntime(home, parsed.Flags)
	if err != nil {
		return err
	}
	if project.IsDirectory() {
		return executionUnavailable(project)
	}
	task, err := s.currentTask(ctx, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	if task.Type != "change" {
		return axi.Failure("greenhouse_target_invalid", "Greenhouse reviews Change Tasks only", false)
	}
	member, err := greenhouseTaskMember(ctx, db, project, task, parsed.Flags["repo"])
	if err != nil {
		return err
	}
	roundDir := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), "greenhouse")
	lock, busy, err := acquireParallelReviewLock(parallelReviewLockPath(roundDir, member.name))
	if err != nil {
		return err
	}
	if busy {
		return axi.Failure("greenhouse_in_flight", "Posse is already handling this Member's Greenhouse review", true)
	}
	defer releaseParallelReviewLock(lock)
	if fenced, err := greenhouseReviewFenced(ctx, db, project, task); err != nil {
		return err
	} else if fenced || task.State == store.StateMerged {
		return axi.Failure("greenhouse_target_invalid", "this Task no longer accepts Greenhouse reviews", false)
	}
	branchLock, err := acquireTaskBranchAuthorLock(ctx, member.root, task.Branch)
	if err != nil {
		return err
	}
	defer branchLock.Close()
	current, err := taskBranchAuthorHead(ctx, member.root, task.Branch)
	if err != nil {
		return err
	}
	if head != current {
		return axi.Failure("greenhouse_head_mismatch", "the requested head is not the current Task Member head", false)
	}
	pending := filepath.Join(roundDir, member.name+".pending.json")
	if hasPending, err := parallelReviewPending(pending); err != nil {
		return err
	} else if !hasPending {
		return axi.Failure("greenhouse_recovery_refused", "this Task Member has no pending Greenhouse review", false)
	}
	if err := checkPendingGreenhouseRuntime(pending, override); err != nil {
		return err
	}
	scratch, err := validatedTaskScratchRoot(home, project, task)
	if err != nil {
		return err
	}
	checkout := filepath.Join(scratch, "greenhouse-review", member.name, "worktree")
	resolved, err := filepath.EvalSymlinks(checkout)
	if err != nil || resolved != checkout {
		return axi.Failure("greenhouse_recovery_refused", "the pinned Greenhouse checkout is missing or unsafe", false)
	}
	checkoutRepo, err := gitCommonDir(ctx, checkout)
	if err != nil {
		return err
	}
	memberRepo, err := gitCommonDir(ctx, member.root)
	if err != nil {
		return err
	}
	if checkoutRepo != memberRepo {
		return axi.Failure("greenhouse_recovery_refused", "the pinned Greenhouse checkout belongs to another repository", false)
	}
	pinnedHead, err := gitOutput(ctx, checkout, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if pinnedHead != head {
		return axi.Failure("greenhouse_head_mismatch", "the pinned Greenhouse review does not match the requested head", false)
	}
	daemon, err := newGreenhouseDaemon(ctx, db, home, override)
	if err != nil {
		return err
	}
	round := greenhouse.Round{
		Worktree: checkout, Host: greenhouseReviewHost(project, task, member), Pending: pending,
	}
	result, err := daemon.RecoverReview(ctx, round, head, recovery)
	if err != nil {
		return axi.Failure("greenhouse_recovery_refused", err.Error(), errors.Is(err, greenhouse.ErrRecoveryNotReady))
	}
	if err := db.RecordGreenhouseReviewRecovery(ctx, project.ID, task.ID, store.GreenhouseRecoveryOutcome{
		Repository: member.name, Head: head, RecoveryID: recovery, Grant: result.Grant, Run: result.RunID, Status: result.Status, Detail: result.Detail,
	}); err != nil {
		return err
	}
	help := "Posse Watch checks the result. To request it now, run `posse review start " + taskIDString(task.Seq) + "`"
	if daemon.Isolated {
		help = "Continue the review with `posse _greenhouse review " + taskIDString(task.Seq) + " --repo " + shellQuote(member.name) + " --greenhouse-binary " + shellQuote(daemon.Binary) + " --greenhouse-home " + shellQuote(daemon.Home) + "`"
	}
	return out.Print(axi.Object{
		{Key: "task", Value: taskIDString(task.Seq)}, {Key: "repo", Value: member.name}, {Key: "head", Value: head},
		{Key: "run", Value: result.RunID}, {Key: "recovery", Value: recovery}, {Key: "grant", Value: result.Grant},
		{Key: "outcome", Value: result.Status}, {Key: "detail", Value: result.Detail},
		{Key: "help", Value: []string{help}},
	})
}
