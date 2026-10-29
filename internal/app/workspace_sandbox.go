package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/execution"
	"github.com/thanhbinh1905/posse/internal/store"
)

// sharedFilesLimit caps the shared workspace files copied into each Sandbox, so a
// large artifact at the workspace root fails loudly instead of filling the disk.
const (
	sharedFilesLimit   = 64 << 20
	sharedEntriesLimit = 10000
)

var mergeRank = map[string]int{"local": 0, "pr": 1}

// taskMember is one member repository a workspace Task checks out.
type taskMember struct {
	repoTarget
	Path         string // relative to the workspace root and to the Sandbox
	MergeMode    string
	BaseRef      string
	TargetBranch string
}

// planTaskMembers resolves a Brief's `repos:` against the Project and returns the
// members to check out, each with its Merge Mode after the Brief's tightening.
func (s *Service) planTaskMembers(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, brief dispatch.Brief, reviewed *store.Task) ([]taskMember, string, error) {
	return s.planTaskMembersWithChoice(ctx, db, project, cfg, brief, reviewed, false)
}

func (s *Service) planTaskMembersWithChoice(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, brief dispatch.Brief, reviewed *store.Task, userChoice bool) ([]taskMember, string, error) {
	if !project.IsWorkspace() {
		if brief.RootContext != "" {
			return nil, "", axi.Failure("brief_invalid", "root_context: is valid only in a workspace Project", false, "Remove `root_context:` from the Brief")
		}
		if len(brief.Repos) > 0 {
			return nil, "", axi.Failure("brief_invalid", "repos: is valid only in a workspace Project", false, "Remove `repos:` from the Brief")
		}
		if len(brief.BaseByRepo) > 0 || len(brief.TargetByRepo) > 0 {
			return nil, "", axi.Failure("brief_invalid", "base and target member maps are valid only in a workspace Project", false, "Use a scalar `base:` or `target:` branch in a repository Project")
		}
		return nil, "", nil
	}
	requested := brief.Repos
	if reviewed != nil {
		reviewedRepos, err := db.TaskRepos(ctx, reviewed.ID)
		if err != nil {
			return nil, "", err
		}
		if len(requested) == 0 {
			for _, repo := range reviewedRepos {
				requested = append(requested, repo.Repo)
			}
		}
		for _, name := range requested {
			found := false
			for _, repo := range reviewedRepos {
				found = found || repo.Repo == name
			}
			if !found {
				return nil, "", axi.Failure("brief_invalid", "the reviewed Task does not touch member "+name, false, "List only members of "+taskIDString(reviewed.Seq)+" in `repos:`")
			}
		}
	}
	if len(requested) == 0 && brief.Type == "change" {
		return nil, "", axi.Failure("brief_invalid", "a Change Brief in a workspace Project must list the members it changes in `repos:`", false, "Add `repos: [<member>, ...]` to the Brief frontmatter; `posse project show` lists members")
	}
	if err := validateBranchMapMembers("base", brief.BaseByRepo, requested); err != nil {
		return nil, "", err
	}
	if err := validateBranchMapMembers("target", brief.TargetByRepo, requested); err != nil {
		return nil, "", err
	}
	repos, err := db.ProjectRepos(ctx, project.ID)
	if err != nil {
		return nil, "", err
	}
	byName := map[string]store.ProjectRepo{}
	for _, repo := range repos {
		byName[repo.Name] = repo
	}
	members := make([]taskMember, 0, len(requested))
	taskMode := "local"
	for _, name := range requested {
		repo, found := byName[name]
		if !found || repo.Status != store.RepoActive {
			return nil, "", axi.Failure("brief_invalid", fmt.Sprintf("repos: %s is not a member of Project %s", name, project.Name), false, "Run `posse project show` to list members, or `posse project scan` after adding one")
		}
		member := taskMember{repoTarget: repoTarget{Name: repo.Name, Root: filepath.Join(project.Root, repo.Path), DefaultBranch: repo.DefaultBranch}, Path: repo.Path}
		remote := originHost(ctx, member.Root)
		member.MergeMode = memberMergeMode(cfg, repo.Name, remote)
		if brief.MergeMode != "" {
			if !userChoice && mergeRank[brief.MergeMode] < mergeRank[member.MergeMode] {
				return nil, "", axi.Failure("brief_invalid", fmt.Sprintf("merge_mode %s would loosen member %s from %s", brief.MergeMode, name, member.MergeMode), false, "Remove merge_mode or tighten it")
			}
			member.MergeMode = brief.MergeMode
		}
		if member.MergeMode == "pr" && remote == "" {
			return nil, "", axi.Failure("brief_invalid", "member "+name+" has no origin and can only Merge locally", false, "Remove merge_mode: pr from the Brief")
		}
		member.BaseRef = memberBaseRef(member.repoTarget, remote != "")
		members = append(members, member)
	}
	// Validate every Member's compatibility before resolving any branch.
	for i := range members {
		member := &members[i]
		repo := byName[member.Name]
		if brief.Type == "change" {
			baseBranch, baseSelected := branchSelection(brief.Base, brief.BaseByRepo, repo.Name, "")
			if baseSelected {
				member.BaseRef, err = selectedBranchRef(ctx, member.Root, baseBranch, "base")
				if err != nil {
					return nil, "", err
				}
			}
			var targetSelected bool
			member.TargetBranch, targetSelected = targetBranchForTask(cfg, brief, repo.Name, repo.DefaultBranch)
			if targetSelected {
				targetRef, err := selectedBranchRef(ctx, member.Root, member.TargetBranch, "target")
				if err != nil {
					return nil, "", err
				}
				if baseFromTarget(cfg, brief, repo.Name, repo.DefaultBranch) {
					member.BaseRef = targetRef
				}
			}
		}
		if reviewed != nil {
			member.BaseRef = reviewed.Branch
			if isFullPipelineTask(*reviewed, member.Name) {
				candidate, err := db.LatestValidationCandidate(ctx, reviewed.ID, member.Name)
				if err != nil {
					return nil, "", axi.Failure("candidate_missing", "private candidate disappeared before Review admission for Member "+member.Name, false)
				}
				member.BaseRef = candidate.RefName
			}
		}
		if mergeRank[member.MergeMode] > mergeRank[taskMode] {
			taskMode = member.MergeMode
		}
	}
	return members, taskMode, nil
}

func memberBaseRef(target repoTarget, hasOrigin bool) string {
	if hasOrigin {
		return "refs/remotes/origin/" + target.DefaultBranch
	}
	return "refs/heads/" + target.DefaultBranch
}

// acquireWorkspaceSandbox prepares a workspace Sandbox with only selected Member
// worktrees and the shared-root context admitted in the Task plan.
func (s *Service) acquireWorkspaceSandbox(ctx context.Context, db *store.DB, project store.Project, task store.Task, members []taskMember, home string, cleanMode string, setup []string, rootContext string) (store.Sandbox, error) {
	base := poolProjectPath(home, project.Name)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return store.Sandbox{}, err
	}
	targets, err := workspaceSandboxTargets(ctx, db, project)
	if err != nil {
		return store.Sandbox{}, err
	}
	if rootContext == "" {
		rootContext = "legacy"
	}
	for attempts := 0; attempts < 64; attempts++ {
		var sandbox store.Sandbox
		err := withSandboxStateLock(ctx, db, func() error {
			manager := execution.NewManager(db)
			if err := manager.EnsureSandboxLeases(ctx); err != nil {
				return resourceLeaseError(err)
			}
			var err error
			sandbox, err = db.AcquireSandbox(ctx, project.ID, task.ID, base)
			if err != nil {
				return err
			}
			if err := manager.EnsureSandboxLeases(ctx); err != nil {
				return errors.Join(resourceLeaseError(err), abandonUnpreparedSandbox(ctx, db, project, task, sandbox))
			}
			return nil
		})
		if err != nil {
			return sandbox, err
		}
		if err := os.MkdirAll(sandbox.Path, 0o700); err != nil {
			return sandbox, err
		}
		if err := s.prepareWorkspaceSandbox(ctx, project, targets, members, sandbox.Path, cleanMode, rootContext); err != nil {
			if breakErr := breakSandbox(ctx, db, project, task, sandbox, err.Error(), nil); breakErr != nil {
				return sandbox, errors.Join(err, breakErr)
			}
			var failure *axi.Error
			if errors.As(err, &failure) && (failure.Code == "sandbox_shared_too_large" || failure.Code == "sandbox_root_context_unsafe") {
				return sandbox, err
			}
			continue
		}
		rows := make([]store.TaskRepo, 0, len(members))
		for _, member := range members {
			worktree := filepath.Join(sandbox.Path, member.Path)
			base, err := taskCheckoutBase(ctx, member.Root, task, member.Name, member.BaseRef)
			if err != nil {
				return sandbox, err
			}
			if _, err := gitOutput(ctx, worktree, "checkout", "-b", task.Branch, base); err != nil {
				ref := "refs/heads/" + task.Branch
				found, refErr := gitOutput(ctx, worktree, "for-each-ref", "--format=%(refname)", ref)
				if refErr == nil && found != "" {
					return sandbox, branchTaken(task.Branch, "in repository "+member.Name+" at ref "+found)
				}
				return sandbox, axi.Failure("branch_check_failed", "could not create "+ref+" in repository "+member.Name, true, err.Error())
			}
			rows = append(rows, store.TaskRepo{Repo: member.Name, WorktreePath: worktree, BaseRef: base, TargetBranch: member.TargetBranch, MergeMode: member.MergeMode})
		}
		if err := db.CreateTaskRepos(ctx, task.ID, rows); err != nil {
			return sandbox, err
		}
		if err := db.UpdateTaskSandboxPath(ctx, task.ID, sandbox.ID, sandbox.Path); err != nil {
			return sandbox, err
		}
		for _, command := range setup {
			if output, err := commandOutput(ctx, sandbox.Path, command); err != nil {
				return sandbox, axi.Failure("sandbox_setup_failed", fmt.Sprintf("setup %q failed: %s", command, truncate(strings.TrimSpace(output), 1200)), false, "Fix the Pool setup command, then retry the Task")
			}
		}
		if rootContext == "read-only" {
			reserved := map[string]bool{}
			for _, target := range targets {
				reserved[target.Path] = true
			}
			if err := makeWorkspaceSharedFilesReadOnly(sandbox.Path, reserved); err != nil {
				return sandbox, err
			}
		}
		if err := writeWorkspaceRootBaseline(ctx, home, project, task, sandbox.Path, targets, rootContext); err != nil {
			return sandbox, fmt.Errorf("capture workspace root at Sandbox acquisition: %w", err)
		}
		return sandbox, nil
	}
	return store.Sandbox{}, fmt.Errorf("no clean Sandbox could be acquired")
}

// workspaceSandboxTargets lists every member, including missing ones, so stale
// worktrees of a removed member are still reset and never copied as shared files.
func workspaceSandboxTargets(ctx context.Context, db *store.DB, project store.Project) ([]taskMember, error) {
	repos, err := db.ProjectRepos(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	targets := make([]taskMember, 0, len(repos))
	for _, repo := range repos {
		if !safeWorkspaceRootPath(repo.Path) {
			return nil, fmt.Errorf("unsafe workspace Member path %q", repo.Path)
		}
		targets = append(targets, taskMember{repoTarget: repoTarget{Name: repo.Name, Root: filepath.Join(project.Root, repo.Path), DefaultBranch: repo.DefaultBranch}, Path: repo.Path})
	}
	return targets, nil
}

func (s *Service) prepareWorkspaceSandbox(ctx context.Context, project store.Project, targets, members []taskMember, path, cleanMode, rootContext string) error {
	if err := validateWorkspaceSandboxRoot(path); err != nil {
		return err
	}
	reserved := map[string]bool{}
	requested := map[string]bool{}
	for _, target := range targets {
		reserved[target.Path] = true
	}
	for _, member := range members {
		requested[member.Name] = true
	}
	if err := removeSharedCopies(path, reserved); err != nil {
		return err
	}
	switch rootContext {
	case "legacy":
		if err := copySharedFiles(project.Root, path, reserved); err != nil {
			return err
		}
	case "read-only":
		if err := copySharedFilesReadOnly(project.Root, path, reserved); err != nil {
			return err
		}
	}
	if err := s.drainUnrequestedWorkspaceWorktrees(ctx, targets, requested, path); err != nil {
		return err
	}
	for _, member := range members {
		if err := validateWorkspaceSandboxMemberPath(path, member.Path); err != nil {
			return fmt.Errorf("unsafe selected Member %s Sandbox path: %w", member.Name, err)
		}
		worktree := filepath.Join(path, member.Path)
		if _, err := os.Stat(member.Root); err != nil {
			return fmt.Errorf("%s: member checkout is missing at %s", member.Name, member.Root)
		}
		present, err := workspaceSandboxWorktreePresent(worktree)
		if err != nil {
			return fmt.Errorf("inspect Member %s worktree: %w", member.Name, err)
		}
		if !present {
			if err := addMemberWorktree(ctx, member.repoTarget, worktree); err != nil {
				return fmt.Errorf("%s: %w", member.Name, err)
			}
		}
		if err := resetWorktree(ctx, worktree, member.repoTarget, cleanMode, true); err != nil {
			return fmt.Errorf("%s: %w", member.Name, err)
		}
	}
	return nil
}

func (s *Service) drainUnrequestedWorkspaceWorktrees(ctx context.Context, targets []taskMember, requested map[string]bool, sandboxPath string) error {
	for _, target := range targets {
		if requested[target.Name] {
			continue
		}
		if err := validateWorkspaceSandboxMemberPath(sandboxPath, target.Path); err != nil {
			return fmt.Errorf("unsafe legacy Member %s Sandbox path: %w", target.Name, err)
		}
		worktree := filepath.Join(sandboxPath, target.Path)
		present, err := workspaceSandboxWorktreePresent(worktree)
		if err != nil {
			return fmt.Errorf("inspect legacy Member %s worktree: %w", target.Name, err)
		}
		if !present {
			continue
		}
		if _, err := os.Stat(target.Root); err != nil {
			return fmt.Errorf("cannot safely drain legacy Member %s worktree: repository is unavailable: %w", target.Name, err)
		}
		if !sandboxWorktreeClean(ctx, worktree) {
			return fmt.Errorf("cannot safely drain legacy Member %s worktree because it is not clean", target.Name)
		}
		branch, err := gitOutput(ctx, worktree, "branch", "--show-current")
		if err != nil {
			return fmt.Errorf("inspect legacy Member %s worktree: %w", target.Name, err)
		}
		if branch != "" {
			return fmt.Errorf("cannot safely drain legacy Member %s worktree because it is on branch %s", target.Name, branch)
		}
		if err := s.removeSandbox(ctx, target.Root, worktree); err != nil {
			return fmt.Errorf("drain legacy Member %s worktree: %w", target.Name, err)
		}
	}
	return nil
}

func validateWorkspaceSandboxRoot(sandboxPath string) error {
	info, err := os.Lstat(sandboxPath)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("sandbox root is not a real directory")
	}
	return nil
}

func validateWorkspaceSandboxMemberPath(sandboxPath, memberPath string) error {
	if !safeWorkspaceRootPath(memberPath) {
		return fmt.Errorf("unsafe relative path %q", memberPath)
	}
	if err := validateWorkspaceSandboxRoot(sandboxPath); err != nil {
		return err
	}
	current := sandboxPath
	parts := strings.Split(memberPath, string(os.PathSeparator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("sandbox path component %q is a symlink", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("sandbox path component %q is not a directory", current)
		}
		if index == len(parts)-1 {
			return nil
		}
	}
	return nil
}

func workspaceSandboxWorktreePresent(path string) (bool, error) {
	info, err := os.Lstat(filepath.Join(path, ".git"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf(".git entry is a symlink")
	}
	return true, nil
}

func removeGitWorktree(ctx context.Context, root, path string) error {
	_, err := gitOutput(ctx, root, "worktree", "remove", path)
	return err
}

func addMemberWorktree(ctx context.Context, target repoTarget, worktree string) error {
	ref, remote := sandboxDefaultRef(ctx, target)
	if remote {
		if _, err := gitFetch(ctx, target.Root, "origin"); err != nil {
			return err
		}
	}
	_, err := gitOutput(ctx, target.Root, "worktree", "add", "--detach", worktree, ref)
	return err
}

// releaseWorkspaceSandbox preserves pre-scope cleanup for legacy plans. New scoped
// Tasks reset selected Members and remove clean legacy extras without resetting them.
func releaseWorkspaceSandbox(ctx context.Context, db *store.DB, project store.Project, sandbox store.Sandbox, clean string, removeSandbox func(context.Context, string, string) error) error {
	if err := validateWorkspaceSandboxRoot(sandbox.Path); err != nil {
		return err
	}
	targets, err := workspaceSandboxTargets(ctx, db, project)
	if err != nil {
		return err
	}
	task, err := db.TaskByID(ctx, project.ID, sandbox.TaskID)
	if err != nil {
		return err
	}
	plan, err := task.AdmittedPlan()
	if err != nil {
		return err
	}
	legacyScope := plan == nil || plan.Execution.RootContext == ""
	taskRepos, err := db.TaskRepos(ctx, sandbox.TaskID)
	if err != nil {
		return err
	}
	selected := make(map[string]bool, len(taskRepos))
	for _, repo := range taskRepos {
		selected[repo.Repo] = true
	}
	reserved := map[string]bool{}
	for _, target := range targets {
		reserved[target.Path] = true
		if err := validateWorkspaceSandboxMemberPath(sandbox.Path, target.Path); err != nil {
			return fmt.Errorf("unsafe Member %s Sandbox path: %w", target.Name, err)
		}
		worktree := filepath.Join(sandbox.Path, target.Path)
		present, err := workspaceSandboxWorktreePresent(worktree)
		if err != nil {
			return fmt.Errorf("inspect Member %s worktree: %w", target.Name, err)
		}
		if !present {
			continue
		}
		if selected[target.Name] || legacyScope {
			if _, err := os.Stat(target.Root); err != nil {
				continue
			}
			if err := resetWorktree(ctx, worktree, target.repoTarget, clean, false); err != nil {
				return fmt.Errorf("%s: %w", target.Name, err)
			}
			continue
		}
		if _, err := os.Stat(target.Root); err != nil {
			return fmt.Errorf("cannot safely drain legacy Member %s worktree: repository is unavailable: %w", target.Name, err)
		}
		if !sandboxWorktreeClean(ctx, worktree) {
			return fmt.Errorf("cannot safely drain legacy Member %s worktree because it is not clean", target.Name)
		}
		branch, err := gitOutput(ctx, worktree, "branch", "--show-current")
		if err != nil {
			return fmt.Errorf("inspect legacy Member %s worktree: %w", target.Name, err)
		}
		if branch != "" {
			return fmt.Errorf("cannot safely drain legacy Member %s worktree because it is on branch %s", target.Name, branch)
		}
		if err := removeSandbox(ctx, target.Root, worktree); err != nil {
			return fmt.Errorf("drain legacy Member %s worktree: %w", target.Name, err)
		}
	}
	return removeSharedCopies(sandbox.Path, reserved)
}

// removeWorkspaceSandbox deletes a pruned workspace Sandbox: each member worktree
// through Git, then the folder.
func (s *Service) removeWorkspaceSandbox(ctx context.Context, db *store.DB, project store.Project, path string) error {
	targets, err := workspaceSandboxTargets(ctx, db, project)
	if err != nil {
		return err
	}
	for _, target := range targets {
		worktree := filepath.Join(path, target.Path)
		if _, err := os.Stat(filepath.Join(worktree, ".git")); err != nil {
			continue
		}
		if err := s.removeSandbox(ctx, target.Root, worktree); err != nil {
			return fmt.Errorf("%s: %w", target.Name, err)
		}
	}
	return os.RemoveAll(path)
}

// workspaceSandboxClean reports whether every member worktree in a Sandbox is clean.
func workspaceSandboxClean(ctx context.Context, path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		worktree := filepath.Join(path, entry.Name())
		if _, err := os.Stat(filepath.Join(worktree, ".git")); !entry.IsDir() || err != nil {
			continue
		}
		if !sandboxWorktreeClean(ctx, worktree) {
			return false
		}
	}
	return true
}

// removeSharedCopies deletes everything in a workspace Sandbox except member worktrees.
func removeSharedCopies(sandboxPath string, reserved map[string]bool) error {
	entries, err := os.ReadDir(sandboxPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if reserved[entry.Name()] {
			if _, err := os.Stat(filepath.Join(sandboxPath, entry.Name(), ".git")); err == nil {
				continue
			}
		}
		path := filepath.Join(sandboxPath, entry.Name())
		if err := makeSandboxTreeWritable(path); err != nil {
			return err
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

// copySharedFiles copies the workspace root into the Sandbox, except member folders
// and any folder that is a Git repository or worktree (such as a .worktrees folder
// of extra checkouts). A copy, unlike a link, keeps a Worker's edits away from the
// User's real files and cannot dangle when the Sandbox moves between Tasks.
func copySharedFiles(root, sandboxPath string, reserved map[string]bool) error {
	return copySharedFilesWithMode(root, sandboxPath, reserved, false)
}

func copySharedFilesReadOnly(root, sandboxPath string, reserved map[string]bool) error {
	return copySharedFilesWithMode(root, sandboxPath, reserved, true)
}

func copySharedFilesWithMode(root, sandboxPath string, reserved map[string]bool, readOnly bool) error {
	type entry struct {
		rel  string
		size int64
		dir  fs.DirEntry
	}
	var entries []entry
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		if d.IsDir() {
			if reserved[rel] || d.Name() == ".git" {
				return filepath.SkipDir
			}
			if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
				return filepath.SkipDir
			}
		}
		if readOnly && d.Type()&fs.ModeSymlink != 0 {
			return axi.Failure("sandbox_root_context_unsafe", fmt.Sprintf("read-only workspace root context contains symlink %s", rel), false, "Remove the symlink or omit root_context from the Brief")
		}
		if readOnly && !d.IsDir() && !d.Type().IsRegular() {
			return axi.Failure("sandbox_root_context_unsafe", fmt.Sprintf("read-only workspace root context contains unsupported file type %s", rel), false, "Remove the special file or omit root_context from the Brief")
		}
		if readOnly && len(entries) >= sharedEntriesLimit {
			return axi.Failure("sandbox_shared_too_large", fmt.Sprintf("the shared workspace context contains more than %d entries", sharedEntriesLimit), false, "Reduce the selected shared-root context")
		}
		var size int64
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			size = info.Size()
			total += size
		}
		entries = append(entries, entry{rel: rel, size: size, dir: d})
		return nil
	})
	if err != nil {
		return err
	}
	if total > sharedFilesLimit {
		sort.Slice(entries, func(i, j int) bool { return entries[i].size > entries[j].size })
		largest := make([]string, 0, 3)
		for index := 0; index < len(entries) && index < 3; index++ {
			largest = append(largest, entries[index].rel+" ("+strconv.FormatInt(entries[index].size>>20, 10)+" MiB)")
		}
		return axi.Failure("sandbox_shared_too_large", fmt.Sprintf("the shared workspace files add up to %d MiB, over the %d MiB a Sandbox copies", total>>20, sharedFilesLimit>>20), false, "Move large files out of "+root+"; the largest are "+strings.Join(largest, ", "))
	}
	for _, item := range entries {
		if err := copyEntry(filepath.Join(root, item.rel), filepath.Join(sandboxPath, item.rel), item.dir); err != nil {
			return err
		}
	}
	return nil
}

func makeWorkspaceSharedFilesReadOnly(sandboxPath string, reserved map[string]bool) error {
	var paths []string
	var total int64
	err := filepath.WalkDir(sandboxPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(sandboxPath, path)
		if err != nil || rel == "." {
			return err
		}
		if entry.IsDir() && (reserved[rel] || entry.Name() == ".git") {
			return filepath.SkipDir
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("unexpected symlink in copied read-only context: %s", rel)
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported file type in copied read-only context: %s", rel)
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			total += info.Size()
			if total > sharedFilesLimit {
				return axi.Failure("sandbox_shared_too_large", fmt.Sprintf("the read-only workspace context exceeds %d MiB after setup", sharedFilesLimit>>20), false, "Reduce shared-root context or setup output")
			}
		}
		if entry.IsDir() || entry.Type().IsRegular() {
			if len(paths) >= sharedEntriesLimit {
				return axi.Failure("sandbox_shared_too_large", fmt.Sprintf("the read-only workspace context exceeds %d entries after setup", sharedEntriesLimit), false, "Reduce shared-root context or setup output")
			}
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for index := len(paths) - 1; index >= 0; index-- {
		path := paths[index]
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() &^ 0o222
		if info.IsDir() {
			mode |= 0o555
		} else {
			mode |= 0o444
		}
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
	}
	return nil
}

func copyEntry(source, target string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	switch {
	case d.IsDir():
		return os.MkdirAll(target, info.Mode().Perm()|0o700)
	case d.Type()&fs.ModeSymlink != 0:
		link, err := os.Readlink(source)
		if err != nil {
			return err
		}
		return os.Symlink(link, target)
	case d.Type().IsRegular():
		in, err := os.Open(source)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	default:
		return nil
	}
}

// validateWorkspaceChangeSignal checks a workspace Change Task before it may report
// done: every requested member is clean, at least one has commits, and no other
// member worktree in the Sandbox was changed.
func validateWorkspaceChangeSignal(ctx context.Context, db *store.DB, task store.Task) error {
	repos, err := db.TaskRepos(ctx, task.ID)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		return fmt.Errorf("workspace Task has no member repositories")
	}
	requested := map[string]bool{}
	changed := 0
	for _, repo := range repos {
		requested[filepath.Clean(repo.WorktreePath)] = true
		status, err := gitStatusOutput(ctx, repo.WorktreePath, "--porcelain")
		if err != nil {
			return fmt.Errorf("%s: %w", repo.Repo, err)
		}
		if status != "" {
			return fmt.Errorf("%s: worktree has uncommitted changes", repo.Repo)
		}
		ahead, err := commitsAhead(ctx, repo.WorktreePath, repo.BaseRef, "refs/heads/"+task.Branch)
		if err != nil {
			return fmt.Errorf("%s: %w", repo.Repo, err)
		}
		if ahead > 0 {
			changed++
		}
	}
	if changed == 0 {
		return fmt.Errorf("no member has commits on %s", task.Branch)
	}
	entries, err := os.ReadDir(task.WorktreePath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		worktree := filepath.Clean(filepath.Join(task.WorktreePath, entry.Name()))
		if _, statErr := os.Stat(filepath.Join(worktree, ".git")); !entry.IsDir() || statErr != nil || requested[worktree] {
			continue
		}
		status, err := gitStatusOutput(ctx, worktree, "--porcelain")
		if err != nil {
			return fmt.Errorf("%s: %w", entry.Name(), err)
		}
		branch, _ := gitOutput(ctx, worktree, "branch", "--show-current")
		if status != "" || branch != "" {
			return fmt.Errorf("%s is not in this Task's repos but has changes; list it in the Brief's repos or revert it", entry.Name())
		}
	}
	return nil
}

func commitsAhead(ctx context.Context, worktree, base, head string) (int, error) {
	count, err := gitOutput(ctx, worktree, "rev-list", "--count", base+".."+head)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(count)
}
