package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func fullPipelineSelected(task store.Task, repo string) bool {
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil {
		return false
	}
	if repo == "" {
		return plan.Review == "full-pipeline"
	}
	for _, member := range plan.Execution.Members {
		if member.Name == repo {
			return member.Review == "full-pipeline"
		}
	}
	return false
}

func candidateRefName(task store.Task) string {
	return fmt.Sprintf("refs/posse/candidate/t%d", task.Seq)
}

func candidateArchiveRefName(task store.Task, candidate store.ValidationCandidate) string {
	return fmt.Sprintf("refs/posse/candidate/archive/t%d/%d", task.Seq, candidate.Generation)
}

func candidateCheckoutPath(home string, project store.Project, task store.Task, repo string, generation int) (string, error) {
	if project.Name == "" || project.Name == "." || project.Name == ".." || filepath.Base(project.Name) != project.Name || strings.ContainsAny(project.Name, `/\\`) {
		return "", fmt.Errorf("unsafe Project name %q for candidate checkout", project.Name)
	}
	if task.Seq < 1 || generation < 1 {
		return "", fmt.Errorf("invalid Task or candidate generation")
	}
	if repo == "" {
		repo = project.Name
	}
	if repo == "." || repo == ".." || filepath.Base(repo) != repo || strings.ContainsAny(repo, `/\\`) {
		return "", fmt.Errorf("unsafe repository name %q for candidate checkout", repo)
	}
	root, err := filepath.Abs(home)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return filepath.Join(root, "projects", project.Name, "tasks", taskIDString(task.Seq), "candidates", repo, "g"+strconv.Itoa(generation)), nil
}

func ensureCandidateParents(home, candidatePath string) error {
	root, err := filepath.Abs(home)
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	} else if !os.IsNotExist(err) {
		return err
	}
	candidatePath, err = filepath.Abs(candidatePath)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, filepath.Dir(candidatePath))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("candidate checkout escapes POSSE_HOME")
	}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("candidate path component is not a private directory: %s", current)
			}
			continue
		}
		if !os.IsNotExist(err) {
			return err
		}
		if err := os.Mkdir(current, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

func isFullPipelineTask(task store.Task, repo string) bool {
	return fullPipelineSelected(task, repo)
}

func authorBranchHead(ctx context.Context, task store.Task) (string, string, error) {
	if task.Branch == "" || task.WorktreePath == "" {
		return "", "", fmt.Errorf("task has no author branch or checkout")
	}
	branch, branchErr := gitOutput(ctx, task.WorktreePath, "symbolic-ref", "--quiet", "HEAD")
	head, err := gitOutput(ctx, task.WorktreePath, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	branchHead, err := gitOutput(ctx, task.WorktreePath, "rev-parse", "refs/heads/"+task.Branch)
	if err != nil {
		return "", "", err
	}
	if branchErr != nil {
		return head, "detached HEAD", nil
	}
	if branch != "refs/heads/"+task.Branch {
		return head, branch, nil
	}
	if head != branchHead {
		return branchHead, branch, nil
	}
	return head, branch, nil
}

func candidateSubmissionError(task store.Task, candidate store.ValidationCandidate, code, message string, retryable bool, help ...string) error {
	return axi.Failure(code, fmt.Sprintf("candidate %s for %s: %s", shortSHA(candidate.CandidateSHA), taskIDString(task.Seq), message), retryable, help...)
}

func candidateAuthorMovedFailure(task store.Task, candidate store.ValidationCandidate, actual, branch string) error {
	return axi.Failure("candidate_author_moved",
		fmt.Sprintf("Task branch for t%d moved after candidate submission: submitted %s, current %s (checkout %s)", task.Seq, candidate.SubmittedSHA, actual, branch),
		false, "Run `posse publish` again to submit a fresh candidate; the previous candidate remains preserved")
}

// allowResubmission is true only for publish, which archives a stale candidate
// before creating a fresh one. A Signal must keep refusing until publish.
func (s *Service) refuseMovedCandidateAuthor(ctx context.Context, db *store.DB, task store.Task, allowResubmission bool) error {
	candidates, err := db.LiveValidationCandidates(ctx, task.ID)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		memberTask := task
		if candidate.Repo != "" {
			repos, err := db.TaskRepos(ctx, task.ID)
			if err != nil {
				return err
			}
			found := false
			for _, repo := range repos {
				if repo.Repo == candidate.Repo {
					memberTask.WorktreePath = repo.WorktreePath
					memberTask.Branch = task.Branch
					found = true
					break
				}
			}
			if !found {
				return axi.Failure("candidate_author_moved", "candidate names a Member that is no longer part of the Task", false)
			}
		}
		actual, branch, err := authorBranchHead(ctx, memberTask)
		if err != nil {
			return err
		}
		if candidate.State == store.ValidationCandidatePromoting && actual == candidate.CandidateSHA && branch == "refs/heads/"+task.Branch {
			continue
		}
		if candidate.State == store.ValidationCandidateStale && allowResubmission {
			continue
		}
		if candidate.State == store.ValidationCandidateStale || actual != candidate.SubmittedSHA || branch != "refs/heads/"+task.Branch {
			canMarkStale := candidate.State == store.ValidationCandidatePreparing || candidate.State == store.ValidationCandidateActive
			if candidate.State == store.ValidationCandidatePromoting {
				remoteHead, err := remoteBranchHead(ctx, memberTask.WorktreePath, "refs/heads/"+task.Branch)
				if err != nil {
					return err
				}
				canMarkStale = remoteHead != candidate.CandidateSHA
			}
			if canMarkStale {
				if err := db.MarkValidationCandidateStale(ctx, candidate.ID, actual); err != nil && err != store.ErrStateRace {
					return err
				}
			}
			return candidateAuthorMovedFailure(memberTask, candidate, actual, branch)
		}
	}
	return nil
}

func (s *Service) submitPrivateCandidate(out *axi.Context, db *store.DB, home string, project store.Project, task store.Task, repoName, summary, verification, proof, risk string, refresh bool) (returnErr error) {
	ctx := out.Context
	intent, err := s.startTaskIntent(ctx, db, project.ID, task.ID, "publish")
	if err != nil {
		return axi.Failure("intent_active", "Task already has an unfinished command", true, err.Error())
	}
	defer func() {
		if finishErr := db.FinishIntent(ctx, intent.ID, intent.ProcessID); returnErr == nil && finishErr != nil {
			returnErr = finishErr
		}
	}()

	submittedSHA, branch, err := authorBranchHead(ctx, task)
	if err != nil {
		return axi.Failure("candidate_author_moved", "could not verify the Task branch checkout", false, err.Error())
	}
	if branch != "refs/heads/"+task.Branch {
		return axi.Failure("publish_refused", "Sandbox must be checked out on its Task branch", false)
	}
	if err := validateChangeSignal(ctx, task); err != nil {
		return axi.Failure("publish_refused", err.Error(), false)
	}
	refName := candidateRefName(task)
	candidate, err := db.LatestValidationCandidate(ctx, task.ID, repoName)
	if err != nil && !store.IsNotFound(err) {
		return err
	}
	if err == nil {
		switch candidate.State {
		case store.ValidationCandidatePreparing, store.ValidationCandidateActive:
			if candidate.SubmittedSHA != submittedSHA {
				if markErr := db.MarkValidationCandidateStale(ctx, candidate.ID, submittedSHA); markErr != nil && markErr != store.ErrStateRace {
					return markErr
				}
				return candidateAuthorMovedFailure(task, candidate, submittedSHA, branch)
			}
			if err := s.ensureCandidateCheckout(ctx, db, home, task, candidate); err != nil {
				return err
			}
			candidate, err = db.LatestValidationCandidate(ctx, task.ID, repoName)
			if err != nil {
				return err
			}
			return printCandidateSubmission(out, task, candidate, true)
		case store.ValidationCandidatePromoting:
			return axi.Failure("candidate_promotion_in_progress", fmt.Sprintf("candidate for t%d is being promoted", task.Seq), true, "Resume its internal promotion before submitting another candidate")
		case store.ValidationCandidatePromoted:
			if submittedSHA == candidate.PromotedSHA {
				return printPromotedCandidate(out, db, task, candidate, repoName)
			}
			if err := s.archiveCandidate(ctx, db, task, candidate); err != nil {
				return err
			}
		case store.ValidationCandidateStale:
			if err := s.archiveCandidate(ctx, db, task, candidate); err != nil {
				return err
			}
		}
	}

	targetBranch := taskTargetBranch(project, task)
	targetSHA, err := remoteBranchHead(ctx, task.WorktreePath, "refs/heads/"+targetBranch)
	if err != nil {
		return axi.Failure("candidate_target_head_failed", "could not inspect the frozen target before candidate submission", true, err.Error())
	}
	if targetSHA == "" {
		return axi.Failure("candidate_target_missing", "the frozen target branch is not present on origin", false, targetBranch)
	}
	remoteSHA, err := remoteBranchHead(ctx, task.WorktreePath, "refs/heads/"+task.Branch)
	if err != nil {
		return axi.Failure("candidate_remote_head_failed", "could not inspect the Task branch on origin before candidate submission", true, err.Error())
	}
	generation, err := db.NextValidationCandidateGeneration(ctx, task.ID, repoName)
	if err != nil {
		return err
	}
	path, err := candidateCheckoutPath(home, project, task, repoName, generation)
	if err != nil {
		return err
	}
	candidate, err = db.CreateValidationCandidate(ctx, store.ValidationCandidate{
		TaskID: task.ID, Repo: repoName, SubmittedSHA: submittedSHA, CandidateSHA: submittedSHA,
		TargetSHA: targetSHA, ExpectedRemoteSHA: remoteSHA, RefName: refName, WorktreePath: path,
		Summary: summary, Verification: verification, Proof: proof, Risk: risk, Refresh: refresh,
	})
	if err != nil {
		if err == store.ErrStateRace {
			return axi.Failure("candidate_submit_race", "another candidate submission won the custody race", true, "Retry `posse publish` after inspecting the active candidate")
		}
		return err
	}
	if err := s.ensureCandidateCheckout(ctx, db, home, task, candidate); err != nil {
		return err
	}
	candidate, err = db.LatestValidationCandidate(ctx, task.ID, repoName)
	if err != nil {
		return err
	}
	return printCandidateSubmission(out, task, candidate, false)
}

func printCandidateSubmission(out *axi.Context, task store.Task, candidate store.ValidationCandidate, resumed bool) error {
	message := fmt.Sprintf("submitted: candidate %s at %s; validation required before publication", taskIDString(task.Seq), shortSHA(candidate.CandidateSHA))
	if resumed {
		message = fmt.Sprintf("resumed: candidate %s at %s; validation required before publication", taskIDString(task.Seq), shortSHA(candidate.CandidateSHA))
	}
	return out.Print(axi.Object{
		{Key: "task", Value: taskIDString(task.Seq)}, {Key: "submitted_head", Value: candidate.SubmittedSHA},
		{Key: "candidate_head", Value: candidate.CandidateSHA}, {Key: "target_head", Value: candidate.TargetSHA},
		{Key: "expected_remote_head", Value: candidate.ExpectedRemoteSHA}, {Key: "candidate_checkout", Value: candidate.WorktreePath},
		{Key: "result", Value: message},
		{Key: "help", Value: []any{"A later validation pass may promote this exact candidate; no remote ref was changed by submission"}},
	})
}

func printPromotedCandidate(out *axi.Context, db *store.DB, task store.Task, candidate store.ValidationCandidate, repoName string) error {
	prURL := task.PRURL
	if repoName != "" {
		repos, err := db.TaskRepos(out.Context, task.ID)
		if err != nil {
			return err
		}
		for _, repo := range repos {
			if repo.Repo == repoName {
				prURL = repo.PRURL
			}
		}
	}
	return out.Print(axi.Object{
		{Key: "task", Value: taskIDString(task.Seq)}, {Key: "candidate_head", Value: candidate.PromotedSHA},
		{Key: "pr_url", Value: prURL}, {Key: "result", Value: fmt.Sprintf("already promoted: candidate %s at %s", taskIDString(task.Seq), shortSHA(candidate.PromotedSHA))},
	})
}

func (s *Service) archiveCandidate(ctx context.Context, db *store.DB, task store.Task, candidate store.ValidationCandidate) error {
	if candidate.State != store.ValidationCandidateStale && candidate.State != store.ValidationCandidatePromoted {
		return axi.Failure("candidate_archive_refused", "only a stale or promoted candidate can be superseded", false)
	}
	candidateHead, found, err := readGitRef(ctx, task.WorktreePath, candidate.RefName)
	if err != nil {
		return err
	}
	if !found {
		candidateHead = candidate.CandidateSHA
	} else if candidateHead != candidate.CandidateSHA {
		if !gitAncestor(ctx, task.WorktreePath, candidate.CandidateSHA, candidateHead) {
			return axi.Failure("candidate_ref_moved", "private candidate ref no longer descends from its recorded head", false, candidate.RefName)
		}
		if err := db.AdvanceValidationCandidate(ctx, candidate.ID, candidate.CandidateSHA, candidateHead); err != nil {
			return err
		}
		candidate.CandidateSHA = candidateHead
	}
	archiveRef := candidateArchiveRefName(task, candidate)
	archivedHead, archiveFound, err := readGitRef(ctx, task.WorktreePath, archiveRef)
	if err != nil {
		return err
	}
	if archiveFound && archivedHead != candidateHead {
		return axi.Failure("candidate_archive_conflict", "the candidate archive ref already names another commit", false, archiveRef)
	}
	if !archiveFound {
		if err := updateGitRef(ctx, task.WorktreePath, archiveRef, candidateHead, strings.Repeat("0", len(candidateHead))); err != nil {
			return err
		}
	}
	if info, statErr := os.Lstat(candidate.WorktreePath); statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return axi.Failure("candidate_path_invalid", "candidate checkout path is not a directory", false, candidate.WorktreePath)
		}
		authorCommonDir, err := gitCommonDir(ctx, task.WorktreePath)
		if err != nil {
			return err
		}
		candidateCommonDir, err := gitCommonDir(ctx, candidate.WorktreePath)
		if err != nil || !samePath(authorCommonDir, candidateCommonDir) {
			return axi.Failure("candidate_checkout_invalid", "candidate checkout is not linked to the Task repository", false, candidate.WorktreePath)
		}
		checkoutHead, headErr := gitOutput(ctx, candidate.WorktreePath, "rev-parse", "HEAD")
		if headErr != nil {
			return headErr
		}
		if checkoutHead != candidateHead {
			return axi.Failure("candidate_checkout_moved", "candidate checkout does not match its recorded head", false, candidate.WorktreePath)
		}
		checkoutRef, refErr := gitOutput(ctx, candidate.WorktreePath, "symbolic-ref", "--quiet", "HEAD")
		if refErr == nil && checkoutRef == candidate.RefName {
			if _, err := gitOutput(ctx, candidate.WorktreePath, "symbolic-ref", "HEAD", archiveRef); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	if current, ok, err := readGitRef(ctx, task.WorktreePath, candidate.RefName); err != nil {
		return err
	} else if ok {
		if err := deleteGitRef(ctx, task.WorktreePath, candidate.RefName, current); err != nil {
			return err
		}
	}
	return db.SupersedeValidationCandidate(ctx, candidate.ID, candidate.State, archiveRef, candidateHead)
}

func (s *Service) ensureCandidateCheckout(ctx context.Context, db *store.DB, home string, task store.Task, candidate store.ValidationCandidate) error {
	if candidate.RefName != candidateRefName(task) {
		return axi.Failure("candidate_ref_invalid", "active candidate does not use its Task-private ref", false, candidate.RefName)
	}
	if err := ensureCandidateParents(home, candidate.WorktreePath); err != nil {
		return axi.Failure("candidate_path_invalid", "candidate checkout path is not owned by POSSE_HOME", false, err.Error())
	}
	refHead, refExists, err := readGitRef(ctx, task.WorktreePath, candidate.RefName)
	if err != nil {
		return err
	}
	if !refExists {
		if err := updateGitRef(ctx, task.WorktreePath, candidate.RefName, candidate.CandidateSHA, strings.Repeat("0", len(candidate.CandidateSHA))); err != nil {
			return axi.Failure("candidate_ref_create_failed", "could not restore the private candidate ref", true, err.Error())
		}
	} else if refHead != candidate.CandidateSHA {
		if gitAncestor(ctx, task.WorktreePath, candidate.CandidateSHA, refHead) {
			if err := db.AdvanceValidationCandidate(ctx, candidate.ID, candidate.CandidateSHA, refHead); err != nil {
				return err
			}
			candidate.CandidateSHA = refHead
		} else {
			return axi.Failure("candidate_ref_moved", "private candidate ref differs from its recorded head", false, candidate.RefName)
		}
	}
	if info, err := os.Lstat(candidate.WorktreePath); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return axi.Failure("candidate_path_invalid", "candidate checkout path is not a directory", false, candidate.WorktreePath)
		}
		root, rootErr := gitTop(ctx, candidate.WorktreePath)
		if rootErr != nil || !samePath(root, candidate.WorktreePath) {
			return axi.Failure("candidate_checkout_invalid", "candidate checkout path is occupied by unrelated data", false, candidate.WorktreePath)
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		if err := addCandidateWorktree(ctx, task.WorktreePath, candidate.WorktreePath, candidate.RefName); err != nil {
			return axi.Failure("candidate_checkout_create_failed", "could not create the private candidate checkout", true, err.Error())
		}
	}
	authorCommonDir, err := gitCommonDir(ctx, task.WorktreePath)
	if err != nil {
		return err
	}
	candidateCommonDir, err := gitCommonDir(ctx, candidate.WorktreePath)
	if err != nil || !samePath(authorCommonDir, candidateCommonDir) {
		return axi.Failure("candidate_checkout_invalid", "candidate checkout is not linked to the Task repository", false, candidate.WorktreePath)
	}
	checkoutHead, err := gitOutput(ctx, candidate.WorktreePath, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	refHead, _, err = readGitRef(ctx, task.WorktreePath, candidate.RefName)
	if err != nil {
		return err
	}
	if checkoutHead != refHead {
		if !gitAncestor(ctx, task.WorktreePath, refHead, checkoutHead) {
			return axi.Failure("candidate_checkout_moved", "candidate checkout is not a fast-forward of its private ref", false, candidate.WorktreePath)
		}
		if err := updateGitRef(ctx, task.WorktreePath, candidate.RefName, checkoutHead, refHead); err != nil {
			return axi.Failure("candidate_ref_advance_failed", "could not record the recovered candidate commit", true, err.Error())
		}
		if err := db.AdvanceValidationCandidate(ctx, candidate.ID, refHead, checkoutHead); err != nil {
			return err
		}
		refHead = checkoutHead
	}
	checkoutRef, err := gitOutput(ctx, candidate.WorktreePath, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || checkoutRef != candidate.RefName {
		if _, err := gitOutput(ctx, candidate.WorktreePath, "symbolic-ref", "HEAD", candidate.RefName); err != nil {
			return axi.Failure("candidate_checkout_attach_failed", "could not attach the candidate checkout to its private ref", true, err.Error())
		}
	}
	if candidate.State == store.ValidationCandidatePreparing {
		if err := db.ActivateValidationCandidate(ctx, candidate.ID, refHead); err != nil {
			return err
		}
	}
	return nil
}

func gitCommonDir(ctx context.Context, path string) (string, error) {
	commonDir, err := gitOutput(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.Abs(strings.TrimSpace(commonDir))
}

func addCandidateWorktree(ctx context.Context, repoRoot, path, ref string) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("candidate path is not a directory")
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return fmt.Errorf("candidate path is occupied")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	_, err := gitOutput(ctx, repoRoot, "worktree", "add", "--quiet", "--detach", path, ref)
	return err
}

func readGitRef(ctx context.Context, root, ref string) (string, bool, error) {
	command := exec.CommandContext(ctx, "git", "-C", root, "show-ref", "--verify", "--hash", ref)
	output, err := command.CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(output)), true, nil
	}
	if exit, ok := err.(*exec.ExitError); ok && (exit.ExitCode() == 1 || exit.ExitCode() == 128 && strings.Contains(string(output), "not a valid ref")) {
		return "", false, nil
	}
	return "", false, fmt.Errorf("git show-ref %s: %w: %s", ref, err, strings.TrimSpace(string(output)))
}

func updateGitRef(ctx context.Context, root, ref, next, expected string) error {
	_, err := gitOutput(ctx, root, "update-ref", ref, next, expected)
	return err
}

func deleteGitRef(ctx context.Context, root, ref, expected string) error {
	_, err := gitOutput(ctx, root, "update-ref", "-d", ref, expected)
	return err
}

func gitAncestor(ctx context.Context, root, ancestor, descendant string) bool {
	_, err := gitOutput(ctx, root, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}
