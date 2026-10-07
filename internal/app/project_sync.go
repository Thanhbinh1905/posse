package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
	"golang.org/x/sys/unix"
)

type projectSyncResult struct {
	Status        string
	UpstreamHead  string
	CommitsBehind int
	Reason        string
	Err           error
}

func (s *Service) syncProjectRoot(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, force bool) (projectSyncResult, error) {
	results, err := s.syncProjectRepos(ctx, db, project, cfg, force)
	if err != nil || len(results) == 0 {
		return projectSyncResult{Status: "current"}, err
	}
	for _, result := range results {
		if result.Err != nil || result.Status == "root_behind" {
			return result.projectSyncResult, nil
		}
	}
	return results[0].projectSyncResult, nil
}

type repoSyncResult struct {
	Repo string
	projectSyncResult
}

func repositorySyncKey(project store.Project, target repoTarget) string {
	return fmt.Sprintf("sync:%d:%s", project.ID, target.Name)
}

// syncProjectRepos fast-forwards the default branch of every repository of the
// Project (the Project itself, or each member of a workspace) when that is safe.
func (s *Service) syncProjectRepos(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, force bool) ([]repoSyncResult, error) {
	now := time.Now()
	state, err := db.ProjectWatchState(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	interval := parseDurationOr(cfg.Defaults.PRPoll, 2*time.Minute)
	targets, err := s.projectTargets(ctx, db, project)
	if err != nil {
		return nil, err
	}
	if !project.IsWorkspace() && !force && !store.ProjectWatchInterval(state.CheckoutCheckedAt, interval, now) {
		return []repoSyncResult{{projectSyncResult: projectSyncResult{Status: "skipped"}}}, nil
	}
	results := make([]repoSyncResult, len(targets))
	due := make([]bool, len(targets))
	for index, target := range targets {
		due[index] = true
		if project.IsWorkspace() && !force {
			repoState, stateErr := db.ProjectRepoWatchState(ctx, project.ID, target.Name)
			if stateErr != nil {
				return nil, stateErr
			}
			due[index] = store.ProjectWatchInterval(repoState.CheckoutCheckedAt, interval, now)
			if !due[index] {
				results[index] = repoSyncResult{Repo: target.Name, projectSyncResult: projectSyncResult{Status: "skipped", Reason: repoState.CheckoutReason}}
			}
		}
	}
	var workers sync.WaitGroup
	var errorMu sync.Mutex
	var firstErr error
	attempted := false
	for index, target := range targets {
		if !due[index] {
			continue
		}
		workers.Add(1)
		go func(index int, target repoTarget) {
			defer workers.Done()
			result, syncErr := s.syncRepository(ctx, db, project, target, now)
			if syncErr == nil && result.Status != "skipped" {
				errorMu.Lock()
				attempted = true
				errorMu.Unlock()
			}
			if syncErr == nil && project.IsWorkspace() && result.Status != "skipped" {
				reason := result.Reason
				if reason == "" && result.Err != nil {
					reason = truncate(strings.TrimSpace(result.Err.Error()), 240)
				}
				syncErr = db.RecordRepoCheckout(ctx, project.ID, target.Name, now.UnixMilli(), result.Status, reason)
			}
			results[index] = repoSyncResult{Repo: target.Name, projectSyncResult: result}
			if syncErr != nil {
				errorMu.Lock()
				if firstErr == nil {
					firstErr = syncErr
				}
				errorMu.Unlock()
			}
		}(index, target)
	}
	workers.Wait()
	if firstErr != nil {
		return results, firstErr
	}
	if !attempted {
		return results, nil
	}
	return results, db.RecordCheckoutAttempt(ctx, project.ID, now.UnixMilli())
}

func (s *Service) syncRepository(ctx context.Context, db *store.DB, project store.Project, target repoTarget, now time.Time) (projectSyncResult, error) {
	key := repositorySyncKey(project, target)
	if !s.beginMaintenance(key) {
		return projectSyncResult{Status: "skipped", Reason: "repository sync already running"}, nil
	}
	defer s.endMaintenance(key)
	result := projectSyncResult{Status: "current"}
	if _, err := gitOutput(ctx, target.Root, "remote", "get-url", "origin"); err != nil {
		result.Status = "no_origin"
		result.Err = err
		result.Reason = "origin remote unavailable: " + truncate(strings.TrimSpace(err.Error()), 240)
		return result, nil
	}
	lock, err := acquireRepositorySyncLock(ctx, target.Root)
	if err != nil {
		if store.IsBusy(err) {
			return projectSyncResult{Status: "skipped", Reason: "repository sync already running"}, nil
		}
		return result, err
	}
	defer lock.Close()
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	if _, err := gitFetch(ctx, target.Root, "origin"); err != nil {
		// Local contention says nothing about whether origin is ahead. Let
		// the caller retry instead of creating a misleading root_behind Notice.
		if store.IsBusy(err) || ctx.Err() != nil {
			return result, err
		}
		result.Status = "root_behind"
		result.Err = err
		result.UpstreamHead, _ = gitOutput(ctx, target.Root, "rev-parse", "refs/remotes/origin/"+target.DefaultBranch)
		result.Reason = "could not fetch origin: " + truncate(strings.TrimSpace(err.Error()), 240)
		_, recordErr := db.RecordRootBehind(ctx, project.ID, target.Name, now.UnixMilli(), result.UpstreamHead, 0, result.Reason)
		return result, recordErr
	}
	upstreamRef := "refs/remotes/origin/" + target.DefaultBranch
	upstreamHead, err := gitOutput(ctx, target.Root, "rev-parse", upstreamRef)
	if err != nil {
		if _, fetchErr := gitFetch(ctx, target.Root, "origin", target.DefaultBranch+":"+upstreamRef); fetchErr != nil {
			if store.IsBusy(fetchErr) || ctx.Err() != nil {
				return result, fetchErr
			}
			result.Status = "fetch_failed"
			result.Err = fetchErr
			return result, nil
		}
		upstreamHead, err = gitOutput(ctx, target.Root, "rev-parse", upstreamRef)
		if err != nil {
			result.Status = "fetch_failed"
			result.Err = err
			return result, nil
		}
	}
	result.UpstreamHead = upstreamHead

	counts, countErr := gitOutput(ctx, target.Root, "rev-list", "--left-right", "--count", upstreamRef+"...refs/heads/"+target.DefaultBranch)
	if countErr != nil {
		result.Status = "root_behind"
		result.Reason = "could not compare the local default branch with origin"
		result.Err = countErr
		_, recordErr := db.RecordRootBehind(ctx, project.ID, target.Name, now.UnixMilli(), upstreamHead, 0, result.Reason)
		return result, recordErr
	}
	parts := strings.Fields(counts)
	if len(parts) != 2 {
		return result, fmt.Errorf("git rev-list returned an invalid comparison: %q", counts)
	}
	result.CommitsBehind, err = strconv.Atoi(parts[0])
	if err != nil {
		return result, err
	}
	localAhead, err := strconv.Atoi(parts[1])
	if err != nil {
		return result, err
	}
	if localAhead > 0 && result.CommitsBehind > 0 {
		result.Status = "root_behind"
		result.Reason = "origin history diverged from the local default branch"
		_, recordErr := db.RecordRootBehind(ctx, project.ID, target.Name, now.UnixMilli(), upstreamHead, result.CommitsBehind, result.Reason)
		return result, recordErr
	}
	if localAhead > 0 && result.CommitsBehind == 0 {
		result.Status = "local_ahead"
		if err := db.ClearRootBehind(ctx, project.ID, target.Name, now.UnixMilli()); err != nil {
			return result, err
		}
		return result, nil
	}
	currentBranch, branchErr := gitOutput(ctx, target.Root, "branch", "--show-current")
	if branchErr != nil {
		result.Status = "root_behind"
		result.Reason = "could not determine the root checkout branch"
		result.Err = branchErr
		_, recordErr := db.RecordRootBehind(ctx, project.ID, target.Name, now.UnixMilli(), upstreamHead, result.CommitsBehind, result.Reason)
		return result, recordErr
	}
	rootStatus, statusErr := gitOutput(ctx, target.Root, "status", "--porcelain", "--untracked-files=all")
	if statusErr != nil {
		result.Status = "root_behind"
		result.Reason = "could not verify that the root checkout is clean"
		result.Err = statusErr
		_, recordErr := db.RecordRootBehind(ctx, project.ID, target.Name, now.UnixMilli(), upstreamHead, result.CommitsBehind, result.Reason)
		return result, recordErr
	}
	worktrees, worktreeErr := gitOutput(ctx, target.Root, "worktree", "list", "--porcelain")
	if worktreeErr != nil {
		result.Status = "root_behind"
		result.Reason = "could not determine whether the default branch is checked out"
		result.Err = worktreeErr
		_, recordErr := db.RecordRootBehind(ctx, project.ID, target.Name, now.UnixMilli(), upstreamHead, result.CommitsBehind, result.Reason)
		return result, recordErr
	}
	defaultCheckedOut := worktreeHasBranch(worktrees, target.DefaultBranch)
	trackedChanges := []string(nil)
	if rootStatus != "" {
		trackedOutput, trackedErr := gitOutputRaw(ctx, target.Root, "diff", "--name-only", "-z", "HEAD")
		if trackedErr != nil {
			result.Status = "root_behind"
			result.Reason = "could not identify tracked local changes: " + truncate(trackedErr.Error(), 200)
			result.Err = trackedErr
			_, recordErr := db.RecordRootBehind(ctx, project.ID, target.Name, now.UnixMilli(), upstreamHead, result.CommitsBehind, result.Reason)
			return result, recordErr
		}
		trackedChanges = nulPaths(trackedOutput)
	}
	var branchUpdateErr error
	if currentBranch == target.DefaultBranch && localAhead == 0 && (rootStatus == "" || result.CommitsBehind > 0) {
		if result.CommitsBehind > 0 {
			if reason := rootSyncBlockReason(trackedChanges, nil); reason != "" {
				result.Status = "root_behind"
				result.Reason = reason
				_, recordErr := db.RecordRootBehind(ctx, project.ID, target.Name, now.UnixMilli(), upstreamHead, result.CommitsBehind, result.Reason)
				return result, recordErr
			}
			collisions, collisionErr := untrackedPathCollisions(ctx, target.Root, "refs/heads/"+target.DefaultBranch, upstreamRef)
			if collisionErr != nil {
				result.Status = "root_behind"
				result.Reason = "could not verify local ignored and untracked paths: " + truncate(collisionErr.Error(), 200)
				result.Err = collisionErr
				_, recordErr := db.RecordRootBehind(ctx, project.ID, target.Name, now.UnixMilli(), upstreamHead, result.CommitsBehind, result.Reason)
				return result, recordErr
			}
			if reason := rootSyncBlockReason(nil, collisions); reason != "" {
				result.Status = "root_behind"
				result.Reason = reason
				_, recordErr := db.RecordRootBehind(ctx, project.ID, target.Name, now.UnixMilli(), upstreamHead, result.CommitsBehind, result.Reason)
				return result, recordErr
			}
			if _, err := gitOutput(ctx, target.Root, "merge", "--ff-only", upstreamRef); err != nil {
				result.Status = "root_behind"
				result.Reason = "the root default branch could not fast-forward"
				result.Err = err
				_, recordErr := db.RecordRootBehind(ctx, project.ID, target.Name, now.UnixMilli(), upstreamHead, result.CommitsBehind, result.Reason)
				return result, recordErr
			}
		}
		result.Status = "updated"
		if err := db.ClearRootBehind(ctx, project.ID, target.Name, now.UnixMilli()); err != nil {
			return result, err
		}
		return result, nil
	}
	if !defaultCheckedOut && localAhead == 0 {
		if _, err := gitFetch(ctx, target.Root, "origin", target.DefaultBranch+":refs/heads/"+target.DefaultBranch); err == nil {
			result.Status = "updated"
			if err := db.ClearRootBehind(ctx, project.ID, target.Name, now.UnixMilli()); err != nil {
				return result, err
			}
			return result, nil
		} else {
			result.Err = err
			branchUpdateErr = err
		}
	}
	if result.CommitsBehind == 0 {
		result.Status = "current"
		if err := db.ClearRootBehind(ctx, project.ID, target.Name, now.UnixMilli()); err != nil {
			return result, err
		}
		return result, nil
	}
	switch {
	case branchUpdateErr != nil:
		result.Reason = "the default branch could not be advanced: " + truncate(strings.TrimSpace(branchUpdateErr.Error()), 240)
	case rootStatus != "" && len(trackedChanges) > 0:
		result.Reason = "tracked local changes: " + formatGitPaths(trackedChanges)
	case rootStatus != "":
		result.Reason = "the root checkout has uncommitted changes"
	case localAhead > 0:
		result.Reason = fmt.Sprintf("the local default branch has %d commit(s) not on origin", localAhead)
	case defaultCheckedOut && currentBranch != target.DefaultBranch:
		result.Reason = "the default branch is checked out in another worktree"
	case currentBranch != target.DefaultBranch:
		result.Reason = "another branch is checked out in the root worktree"
	default:
		result.Reason = "the default branch could not be fast-forwarded"
	}
	result.Status = "root_behind"
	_, err = db.RecordRootBehind(ctx, project.ID, target.Name, now.UnixMilli(), upstreamHead, result.CommitsBehind, result.Reason)
	return result, err
}

func untrackedPathCollisions(ctx context.Context, root, localRef, upstreamRef string) ([]string, error) {
	changedOutput, err := gitOutputRaw(ctx, root, "diff", "--name-only", "-z", localRef+".."+upstreamRef)
	if err != nil {
		return nil, err
	}
	changed := nulPaths(changedOutput)
	if len(changed) == 0 {
		return nil, nil
	}
	localOutput, err := gitOutputRaw(ctx, root, "ls-files", "--others", "-z", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	ignoredOutput, err := gitOutputRaw(ctx, root, "ls-files", "--others", "--ignored", "-z", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	local := append(nulPaths(localOutput), nulPaths(ignoredOutput)...)
	return gitPathCollisions(changed, local), nil
}

func gitPathCollisions(upstreamPaths, localPaths []string) []string {
	var collisions []string
	seen := make(map[string]bool)
	for _, localPath := range localPaths {
		for _, upstreamPath := range upstreamPaths {
			if gitPathsOverlap(upstreamPath, localPath) && !seen[localPath] {
				collisions = append(collisions, localPath)
				seen[localPath] = true
				break
			}
		}
	}
	return collisions
}

func rootSyncBlockReason(trackedChanges, untrackedCollisions []string) string {
	if len(trackedChanges) > 0 {
		return "tracked local changes: " + formatGitPaths(trackedChanges)
	}
	if len(untrackedCollisions) > 0 {
		return "local ignored or untracked paths conflict with incoming changes: " + formatGitPaths(untrackedCollisions)
	}
	return ""
}

func formatGitPaths(paths []string) string {
	const maxPaths = 3
	shown := make([]string, 0, min(len(paths), maxPaths)+1)
	for _, path := range paths[:min(len(paths), maxPaths)] {
		shown = append(shown, strconv.Quote(path))
	}
	if len(paths) > maxPaths {
		shown = append(shown, fmt.Sprintf("and %d more", len(paths)-maxPaths))
	}
	return strings.Join(shown, ", ")
}

func nulPaths(output string) []string {
	output = strings.TrimSuffix(output, "\x00")
	if output == "" {
		return nil
	}
	return strings.Split(output, "\x00")
}

func gitPathsOverlap(left, right string) bool {
	left, right = strings.TrimSuffix(left, "/"), strings.TrimSuffix(right, "/")
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

func worktreeHasBranch(output, branch string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "branch refs/heads/"+branch {
			return true
		}
	}
	return false
}

func (s *Service) sync(ctx *axi.Context, args []string) error {
	if len(args) != 0 {
		return axi.Usage("sync takes no arguments")
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
	home, err := s.homePath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return configError(err)
	}
	if project.IsWorkspace() {
		results, err := s.syncProjectRepos(ctx.Context, db, project, cfg, true)
		if err != nil {
			return err
		}
		rows := make([]any, 0, len(results))
		for _, result := range results {
			rows = append(rows, axi.Object{{Key: "repo", Value: result.Repo}, {Key: "status", Value: result.Status}, {Key: "commits_behind", Value: result.CommitsBehind}, {Key: "reason", Value: syncReason(result.projectSyncResult)}})
		}
		return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "repos", Value: rows}, {Key: "help", Value: []any{"Run `posse sync` again after each member checkout is ready"}}})
	}
	result, err := s.syncProjectRoot(ctx.Context, db, project, cfg, true)
	if err != nil {
		return err
	}
	if result.Err != nil {
		if store.IsOnlyBusy(result.Err) {
			return result.Err
		}
		return axi.Failure("project_sync_failed", "could not sync the Project default branch", true, result.Err.Error())
	}
	return ctx.Print(axi.Object{
		{Key: "project", Value: project.Name}, {Key: "default_branch", Value: project.DefaultBranch},
		{Key: "status", Value: result.Status}, {Key: "upstream_head", Value: result.UpstreamHead},
		{Key: "commits_behind", Value: result.CommitsBehind}, {Key: "reason", Value: result.Reason},
		{Key: "help", Value: []any{"Run `posse sync` again after the root checkout is ready"}},
	})
}

func syncReason(result projectSyncResult) string {
	if result.Reason == "" && result.Err != nil && result.Status != "no_origin" {
		return truncate(result.Err.Error(), 240)
	}
	return result.Reason
}
