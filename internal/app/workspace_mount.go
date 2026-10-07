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
	"github.com/thanhbinh1905/posse/internal/store"
)

// sharedFilesLimit caps the shared workspace files copied into each Mount, so a
// large artifact at the workspace root fails loudly instead of filling the disk.
const sharedFilesLimit = 64 << 20

var landingRank = map[string]int{"local": 0, "pr": 1, "no-mistakes": 2}

// taskMember is one member repository a workspace Task checks out.
type taskMember struct {
	repoTarget
	Path        string // relative to the workspace root and to the Mount
	LandingMode string
	BaseRef     string
}

// planTaskMembers resolves a Brief's `repos:` against the Project and returns the
// members to check out, each with its Landing Mode after the Brief's tightening.
func (s *Service) planTaskMembers(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, brief dispatch.Brief, reviewed *store.Task) ([]taskMember, string, error) {
	if !project.IsWorkspace() {
		if len(brief.Repos) > 0 {
			return nil, "", axi.Failure("brief_invalid", "repos: is valid only in a workspace Project", false, "Remove `repos:` from the Brief")
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
	if len(requested) == 0 && brief.Type == "ship" {
		return nil, "", axi.Failure("brief_invalid", "a Ship Brief in a workspace Project must list the members it changes in `repos:`", false, "Add `repos: [<member>, ...]` to the Brief frontmatter; `posse project show` lists members")
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
		member.LandingMode = memberLandingMode(cfg, repo.Name, remote)
		if brief.LandingMode != "" {
			if landingRank[brief.LandingMode] < landingRank[member.LandingMode] {
				return nil, "", axi.Failure("brief_invalid", fmt.Sprintf("landing_mode %s would loosen member %s from %s", brief.LandingMode, name, member.LandingMode), false, "Remove landing_mode or tighten it")
			}
			member.LandingMode = brief.LandingMode
		}
		if member.LandingMode == "no-mistakes" {
			return nil, "", axi.Failure("brief_invalid", "no-mistakes landing is not available for workspace members", false, "Use landing_mode local or pr")
		}
		if member.LandingMode == "pr" && remote == "" {
			return nil, "", axi.Failure("brief_invalid", "member "+name+" has no origin and can only Land locally", false, "Remove landing_mode: pr from the Brief")
		}
		member.BaseRef = memberBaseRef(member.repoTarget, remote != "")
		if reviewed != nil {
			member.BaseRef = reviewed.Branch
		}
		if landingRank[member.LandingMode] > landingRank[taskMode] {
			taskMode = member.LandingMode
		}
		members = append(members, member)
	}
	return members, taskMode, nil
}

func memberBaseRef(target repoTarget, hasOrigin bool) string {
	if hasOrigin {
		return "refs/remotes/origin/" + target.DefaultBranch
	}
	return "refs/heads/" + target.DefaultBranch
}

// acquireWorkspaceMount prepares a workspace Mount: a folder that mirrors the
// workspace root with a fresh copy of the shared files and one warm worktree per
// member. Member worktrees are created on first use and kept between Tasks.
func (s *Service) acquireWorkspaceMount(ctx context.Context, db *store.DB, project store.Project, task store.Task, members []taskMember, home string, cleanMode string, setup []string) (store.Mount, error) {
	base := filepath.Join(home, "remuda", project.Name)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return store.Mount{}, err
	}
	targets, err := workspaceMountTargets(ctx, db, project)
	if err != nil {
		return store.Mount{}, err
	}
	for attempts := 0; attempts < 64; attempts++ {
		mount, err := db.AcquireMount(ctx, project.ID, task.ID, base)
		if err != nil {
			return store.Mount{}, err
		}
		if err := os.MkdirAll(mount.Path, 0o700); err != nil {
			return mount, err
		}
		if err := s.prepareWorkspaceMount(ctx, project, targets, members, mount.Path, cleanMode); err != nil {
			if breakErr := breakMount(ctx, db, project, task, mount, err.Error()); breakErr != nil {
				return mount, errors.Join(err, breakErr)
			}
			var failure *axi.Error
			if errors.As(err, &failure) && failure.Code == "mount_shared_too_large" {
				return mount, err
			}
			continue
		}
		rows := make([]store.TaskRepo, 0, len(members))
		for _, member := range members {
			worktree := filepath.Join(mount.Path, member.Path)
			if _, err := gitOutput(ctx, worktree, "checkout", "-b", task.Branch, member.BaseRef); err != nil {
				ref := "refs/heads/" + task.Branch
				found, refErr := gitOutput(ctx, worktree, "for-each-ref", "--format=%(refname)", ref)
				if refErr == nil && found != "" {
					return mount, branchRefTaken(task.ShortName, member.Name, found)
				}
				return mount, axi.Failure("branch_check_failed", "could not create "+ref+" in repository "+member.Name, true, err.Error())
			}
			rows = append(rows, store.TaskRepo{Repo: member.Name, WorktreePath: worktree, BaseRef: member.BaseRef, LandingMode: member.LandingMode})
		}
		if err := db.CreateTaskRepos(ctx, task.ID, rows); err != nil {
			return mount, err
		}
		if err := db.UpdateTaskMountPath(ctx, task.ID, mount.ID, mount.Path); err != nil {
			return mount, err
		}
		for _, command := range setup {
			if output, err := commandOutput(ctx, mount.Path, command); err != nil {
				return mount, axi.Failure("mount_setup_failed", fmt.Sprintf("setup %q failed: %s", command, truncate(strings.TrimSpace(output), 1200)), false, "Fix the Remuda setup command, then retry the Task")
			}
		}
		if err := writeWorkspaceRootBaseline(ctx, home, project, task, mount.Path, targets); err != nil {
			return mount, fmt.Errorf("capture workspace root at Mount acquisition: %w", err)
		}
		return mount, nil
	}
	return store.Mount{}, fmt.Errorf("no clean Mount could be acquired")
}

// workspaceMountTargets lists every member, including missing ones, so stale
// worktrees of a removed member are still reset and never copied as shared files.
func workspaceMountTargets(ctx context.Context, db *store.DB, project store.Project) ([]taskMember, error) {
	repos, err := db.ProjectRepos(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	targets := make([]taskMember, 0, len(repos))
	for _, repo := range repos {
		targets = append(targets, taskMember{repoTarget: repoTarget{Name: repo.Name, Root: filepath.Join(project.Root, repo.Path), DefaultBranch: repo.DefaultBranch}, Path: repo.Path})
	}
	return targets, nil
}

func (s *Service) prepareWorkspaceMount(ctx context.Context, project store.Project, targets, members []taskMember, path, cleanMode string) error {
	requested := map[string]bool{}
	for _, member := range members {
		requested[member.Name] = true
	}
	reserved := map[string]bool{}
	for _, target := range targets {
		reserved[target.Path] = true
	}
	if err := removeSharedCopies(path, reserved); err != nil {
		return err
	}
	if err := copySharedFiles(project.Root, path, reserved); err != nil {
		return err
	}
	for _, target := range targets {
		worktree := filepath.Join(path, target.Path)
		_, statErr := os.Stat(filepath.Join(worktree, ".git"))
		present := statErr == nil
		if !present && !requested[target.Name] {
			continue
		}
		if _, err := os.Stat(target.Root); err != nil {
			if requested[target.Name] {
				return fmt.Errorf("%s: member checkout is missing at %s", target.Name, target.Root)
			}
			continue
		}
		if !present {
			if err := addMemberWorktree(ctx, target.repoTarget, worktree); err != nil {
				return fmt.Errorf("%s: %w", target.Name, err)
			}
		}
		if err := resetWorktree(ctx, worktree, target.repoTarget, cleanMode, requested[target.Name]); err != nil {
			return fmt.Errorf("%s: %w", target.Name, err)
		}
	}
	return nil
}

func addMemberWorktree(ctx context.Context, target repoTarget, worktree string) error {
	ref, remote := mountDefaultRef(ctx, target)
	if remote {
		if _, err := gitFetch(ctx, target.Root, "origin"); err != nil {
			return err
		}
	}
	_, err := gitOutput(ctx, target.Root, "worktree", "add", "--detach", worktree, ref)
	return err
}

// releaseWorkspaceMount resets every member worktree in the Mount and removes the
// shared file copies, so nothing of the Task survives into the next one.
func releaseWorkspaceMount(ctx context.Context, db *store.DB, project store.Project, mount store.Mount, clean string) error {
	targets, err := workspaceMountTargets(ctx, db, project)
	if err != nil {
		return err
	}
	reserved := map[string]bool{}
	for _, target := range targets {
		reserved[target.Path] = true
		worktree := filepath.Join(mount.Path, target.Path)
		if _, err := os.Stat(filepath.Join(worktree, ".git")); err != nil {
			continue
		}
		if _, err := os.Stat(target.Root); err != nil {
			continue
		}
		if err := resetWorktree(ctx, worktree, target.repoTarget, clean, false); err != nil {
			return fmt.Errorf("%s: %w", target.Name, err)
		}
	}
	return removeSharedCopies(mount.Path, reserved)
}

// removeWorkspaceMount deletes a pruned workspace Mount: each member worktree
// through Git, then the folder.
func (s *Service) removeWorkspaceMount(ctx context.Context, db *store.DB, project store.Project, path string) error {
	targets, err := workspaceMountTargets(ctx, db, project)
	if err != nil {
		return err
	}
	for _, target := range targets {
		worktree := filepath.Join(path, target.Path)
		if _, err := os.Stat(filepath.Join(worktree, ".git")); err != nil {
			continue
		}
		if err := s.removeMount(ctx, target.Root, worktree); err != nil {
			return fmt.Errorf("%s: %w", target.Name, err)
		}
	}
	return os.RemoveAll(path)
}

// workspaceMountClean reports whether every member worktree in a Mount is clean.
func workspaceMountClean(ctx context.Context, path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		worktree := filepath.Join(path, entry.Name())
		if _, err := os.Stat(filepath.Join(worktree, ".git")); !entry.IsDir() || err != nil {
			continue
		}
		if !mountWorktreeClean(ctx, worktree) {
			return false
		}
	}
	return true
}

// removeSharedCopies deletes everything in a workspace Mount except member worktrees.
func removeSharedCopies(mountPath string, reserved map[string]bool) error {
	entries, err := os.ReadDir(mountPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if reserved[entry.Name()] {
			if _, err := os.Stat(filepath.Join(mountPath, entry.Name(), ".git")); err == nil {
				continue
			}
		}
		if err := os.RemoveAll(filepath.Join(mountPath, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// copySharedFiles copies the workspace root into the Mount, except member folders
// and any folder that is a Git repository or worktree (such as a .worktrees folder
// of extra checkouts). A copy, unlike a link, keeps a Worker's edits away from the
// User's real files and cannot dangle when the Mount moves between Tasks.
func copySharedFiles(root, mountPath string, reserved map[string]bool) error {
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
		return axi.Failure("mount_shared_too_large", fmt.Sprintf("the shared workspace files add up to %d MiB, over the %d MiB a Mount copies", total>>20, sharedFilesLimit>>20), false, "Move large files out of "+root+"; the largest are "+strings.Join(largest, ", "))
	}
	for _, item := range entries {
		if err := copyEntry(filepath.Join(root, item.rel), filepath.Join(mountPath, item.rel), item.dir); err != nil {
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

// validateWorkspaceShipSignal checks a workspace Ship Task before it may report
// done: every requested member is clean, at least one has commits, and no other
// member worktree in the Mount was changed.
func validateWorkspaceShipSignal(ctx context.Context, db *store.DB, task store.Task) error {
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
		status, err := gitOutput(ctx, repo.WorktreePath, "status", "--porcelain")
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
		status, err := gitOutput(ctx, worktree, "status", "--porcelain")
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
