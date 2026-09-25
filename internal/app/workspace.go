package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mattn/go-isatty"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// detectedProject is what `posse up` and `posse project add` find in a folder
// that is not registered yet.
type detectedProject struct {
	Kind          string
	Root          string
	Name          string
	DefaultBranch string // repo Projects only
	Repos         []detectedRepo
	Hint          string
}

type detectedRepo struct {
	Name          string
	Path          string // relative to the workspace root
	DefaultBranch string
	Remote        string // origin host, or "" without an origin
}

// repoTarget is one repository posse works on: the Project itself for a repo
// Project (Name ""), or one member of a workspace Project.
type repoTarget struct {
	Name          string
	Root          string
	DefaultBranch string
}

// currentDir returns the working directory with symlinks resolved, so it compares
// equal to the resolved roots Git reports.
func currentDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(dir); resolveErr == nil {
		dir = resolved
	}
	return filepath.Clean(dir), nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// detectProject finds the Project shape of dir: the enclosing Git repository, or
// a workspace of direct child repositories when dir is not inside one.
func detectProject(ctx context.Context, dir string) (detectedProject, error) {
	if top, err := gitTop(ctx, dir); err == nil {
		branch, err := defaultBranch(ctx, top)
		if err != nil {
			return detectedProject{}, axi.Failure("default_branch_unknown", "could not determine the default branch of "+top, false, err.Error(), "Check out the default branch or run `git remote set-head origin --auto`")
		}
		detected := detectedProject{Kind: store.ProjectKindRepo, Root: top, Name: projectName(filepath.Base(top)), DefaultBranch: branch}
		detected.Repos = []detectedRepo{{Name: detected.Name, Path: ".", DefaultBranch: branch, Remote: originHost(ctx, top)}}
		if parent := filepath.Dir(top); parent != top {
			if siblings, scanErr := scanWorkspace(ctx, parent); scanErr == nil && len(siblings) > 1 {
				if _, parentErr := gitTop(ctx, parent); parentErr != nil {
					detected.Hint = fmt.Sprintf("%s holds %d repositories; run `posse up` there to manage them as one workspace Project", parent, len(siblings))
				}
			}
		}
		return detected, nil
	}
	repos, err := scanWorkspace(ctx, dir)
	if err != nil {
		return detectedProject{}, err
	}
	if len(repos) == 0 {
		return detectedProject{}, axi.Failure("not_in_git_repository", "this folder is not a Git repository and has no Git repositories directly inside it", false, "Run `posse up` inside a repository, or in a folder whose direct subfolders are repositories")
	}
	return detectedProject{Kind: store.ProjectKindWorkspace, Root: dir, Name: projectName(filepath.Base(dir)), Repos: repos}, nil
}

// scanWorkspace lists the direct child folders of root that are the top level of
// their own Git repository. Hidden folders and linked worktrees are skipped: a
// linked worktree is another checkout of a member, not a member of its own.
func scanWorkspace(ctx context.Context, root string) ([]detectedRepo, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	repos := []detectedRepo{}
	names := map[string]string{}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
			continue
		}
		top, err := gitTop(ctx, path)
		if err != nil || top != path {
			continue
		}
		gitDir, gitDirErr := gitOutput(ctx, path, "rev-parse", "--absolute-git-dir")
		commonDir, commonErr := gitOutput(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if gitDirErr != nil || commonErr != nil || filepath.Clean(gitDir) != filepath.Clean(commonDir) {
			continue
		}
		name := projectName(entry.Name())
		if name == "" {
			return nil, axi.Failure("repo_name_invalid", "cannot derive a member name from folder "+entry.Name(), false, "Rename the folder to use letters, digits, - or _")
		}
		if previous, taken := names[name]; taken {
			return nil, axi.Failure("repo_name_conflict", fmt.Sprintf("folders %s and %s both map to member name %s", previous, entry.Name(), name), false, "Rename one of the folders")
		}
		names[name] = entry.Name()
		branch, err := defaultBranch(ctx, path)
		if err != nil {
			return nil, axi.Failure("default_branch_unknown", "could not determine the default branch of member "+name, false, err.Error(), "Check out its default branch or run `git -C "+path+" remote set-head origin --auto`")
		}
		repos = append(repos, detectedRepo{Name: name, Path: entry.Name(), DefaultBranch: branch, Remote: originHost(ctx, path)})
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })
	return repos, nil
}

// originHost returns the host of a repository's origin remote, or "" without one.
func originHost(ctx context.Context, root string) string {
	remote, err := gitOutput(ctx, root, "remote", "get-url", "origin")
	if err != nil || remote == "" {
		return ""
	}
	if strings.Contains(remote, "://") {
		if parsed, parseErr := url.Parse(remote); parseErr == nil && parsed.Hostname() != "" {
			return parsed.Hostname()
		}
		return "local"
	}
	hostPath := remote
	if at := strings.LastIndex(hostPath, "@"); at >= 0 {
		hostPath = hostPath[at+1:]
	}
	if separator := strings.IndexByte(hostPath, ':'); separator > 0 && !strings.ContainsRune(hostPath[:separator], '/') {
		return hostPath[:separator]
	}
	return "local"
}

// memberLandingMode is how one member Lands: its own `repositories.<name>.landing_mode`,
// else `local` when it has no origin to open a pull request against, else the Project's mode.
func memberLandingMode(cfg config.Config, name, remote string) string {
	if repository, ok := cfg.Repositories[name]; ok && repository.LandingMode != "" {
		return repository.LandingMode
	}
	if remote == "" {
		return "local"
	}
	return cfg.Defaults.LandingMode
}

// projectTargets lists the repositories of a Project: the Project itself, or its active members.
func (s *Service) projectTargets(ctx context.Context, db *store.DB, project store.Project) ([]repoTarget, error) {
	if !project.IsWorkspace() {
		return []repoTarget{{Root: project.Root, DefaultBranch: project.DefaultBranch}}, nil
	}
	repos, err := db.ProjectRepos(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	targets := make([]repoTarget, 0, len(repos))
	for _, repo := range repos {
		if repo.Status != store.RepoActive {
			continue
		}
		targets = append(targets, repoTarget{Name: repo.Name, Root: filepath.Join(project.Root, repo.Path), DefaultBranch: repo.DefaultBranch})
	}
	return targets, nil
}

func (s *Service) projectTarget(ctx context.Context, db *store.DB, project store.Project, name string) (repoTarget, error) {
	targets, err := s.projectTargets(ctx, db, project)
	if err != nil {
		return repoTarget{}, err
	}
	for _, target := range targets {
		if target.Name == name {
			return target, nil
		}
	}
	return repoTarget{}, axi.Failure("repo_unknown", fmt.Sprintf("Project %s has no member repository %q", project.Name, name), false, "Run `posse project show` to list members, or `posse project scan` after adding one")
}

// registeredProjectFor resolves the Project that owns dir: a repo Project by its
// Git top level, the nearest workspace Project that contains dir, or the Project
// of the Task whose Mount contains dir.
func registeredProjectFor(ctx context.Context, db *store.DB, dir string) (store.Project, bool, error) {
	if top, err := gitTop(ctx, dir); err == nil {
		project, lookupErr := db.ProjectByRoot(ctx, top)
		if lookupErr == nil {
			return project, true, nil
		}
		if !store.IsNotFound(lookupErr) {
			return store.Project{}, false, lookupErr
		}
	}
	projects, err := db.Projects(ctx)
	if err != nil {
		return store.Project{}, false, err
	}
	var best store.Project
	for _, candidate := range projects {
		if candidate.IsWorkspace() && pathWithin(candidate.Root, dir) && len(candidate.Root) > len(best.Root) {
			best = candidate
		}
	}
	if best.ID != 0 {
		return best, true, nil
	}
	if task, err := taskForPath(ctx, db, dir); err == nil {
		project, err := db.ProjectByID(ctx, task.ProjectID)
		return project, err == nil, err
	}
	return store.Project{}, false, nil
}

// taskForPath finds the live-or-finished Task whose Mount contains dir.
func taskForPath(ctx context.Context, db *store.DB, dir string) (store.Task, error) {
	if top, err := gitTop(ctx, dir); err == nil {
		if task, err := db.TaskByWorktree(ctx, top); err == nil {
			return task, nil
		}
	}
	projects, err := db.Projects(ctx)
	if err != nil {
		return store.Task{}, err
	}
	for _, project := range projects {
		mounts, err := db.Mounts(ctx, project.ID)
		if err != nil {
			return store.Task{}, err
		}
		for _, mount := range mounts {
			if mount.State != "held" || mount.TaskID == 0 || !pathWithin(resolvedPath(mount.Path), dir) {
				continue
			}
			return db.TaskByID(ctx, project.ID, mount.TaskID)
		}
	}
	return store.Task{}, errNoTask
}

var errNoTask = fmt.Errorf("no Task owns this path: %w", store.ErrNotFound)

func resolvedPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

// detectionObject prints what registration would save.
func detectionObject(detected detectedProject, cfg config.Config) axi.Object {
	repos := make([]any, 0, len(detected.Repos))
	for _, repo := range detected.Repos {
		remote := repo.Remote
		if remote == "" {
			remote = "none"
		}
		repos = append(repos, axi.Object{
			{Key: "name", Value: repo.Name}, {Key: "default_branch", Value: repo.DefaultBranch},
			{Key: "remote", Value: remote}, {Key: "landing_mode", Value: memberLandingMode(cfg, repo.Name, repo.Remote)},
		})
	}
	object := axi.Object{
		{Key: "detected", Value: axi.Object{{Key: "project", Value: detected.Name}, {Key: "kind", Value: detected.Kind}, {Key: "root", Value: detected.Root}}},
		{Key: "repos", Value: repos},
	}
	if detected.Hint != "" {
		object = append(object, axi.Field{Key: "hint", Value: detected.Hint})
	}
	return object
}

// registerDetected saves a detected Project.
func registerDetected(ctx context.Context, db *store.DB, detected detectedProject) (store.Project, error) {
	var (
		project store.Project
		err     error
	)
	if detected.Kind == store.ProjectKindWorkspace {
		repos := make([]store.ProjectRepo, 0, len(detected.Repos))
		for _, repo := range detected.Repos {
			repos = append(repos, store.ProjectRepo{Name: repo.Name, Path: repo.Path, DefaultBranch: repo.DefaultBranch})
		}
		project, err = db.CreateWorkspaceProject(ctx, detected.Name, detected.Root, repos)
	} else {
		project, err = db.CreateProject(ctx, detected.Name, detected.Root, detected.DefaultBranch)
	}
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: projects.name") {
			return store.Project{}, axi.Failure("name_taken", fmt.Sprintf("Project name %q is already in use", detected.Name), false, "Retry with `--name <different-name>`")
		}
		return store.Project{}, err
	}
	return project, nil
}

// confirmFunc asks the User one yes/no question in the terminal. interactive is
// false when there is no terminal to ask in.
type confirmFunc func(out io.Writer, question string) (yes bool, interactive bool, err error)

func terminalConfirm(out io.Writer, question string) (bool, bool, error) {
	if !isatty.IsTerminal(os.Stdin.Fd()) {
		return false, false, nil
	}
	fmt.Fprint(out, question+" [Y/n] ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, true, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "" || answer == "y" || answer == "yes", true, nil
}

// confirmRegistration prints the detection and asks once before it is saved.
func (s *Service) confirmRegistration(ctx *axi.Context, detected detectedProject, cfg config.Config, assumeYes bool, retry string) error {
	if err := ctx.PrintWithoutHelp(detectionObject(detected, cfg)); err != nil {
		return err
	}
	if assumeYes {
		return nil
	}
	confirm := s.confirm
	if confirm == nil {
		confirm = terminalConfirm
	}
	question := fmt.Sprintf("Register %s as a %s Project?", detected.Root, detected.Kind)
	yes, interactive, err := confirm(ctx.ErrOut, question)
	if err != nil {
		return err
	}
	if !interactive {
		return axi.Failure("confirmation_required", "registering a new Project needs the User's confirmation", false, "Run `"+retry+" --yes` to register what was detected")
	}
	if !yes {
		return axi.Failure("registration_declined", "the User declined to register this folder", false, "Run `posse up` in the folder you want to manage")
	}
	return nil
}
