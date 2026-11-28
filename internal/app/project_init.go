package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) projectInit(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("project init", args, map[string]flagSpec{"user-approved": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("project init takes no positional arguments")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := s.requireConfigCaller(ctx.Context, db); err != nil {
		return err
	}
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if !project.IsDirectory() {
		return axi.Failure("project_not_directory", "Git init is supported only for a registered Directory Project", false, "Run `posse project show` to inspect the Project")
	}
	if _, err := s.requireUserApproval(ctx.Context, db, "project init", project.Name, parsed.Flags["user-approved"]); err != nil {
		return err
	}
	root, err := filepath.Abs(project.Root)
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve Directory Project root: %w", err)
	}
	root = filepath.Clean(root)
	if err := validateProjectInitRoot(ctx.Context, root); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(root, ".git")); os.IsNotExist(err) {
		if err := requireGitUnbornTaskSupport(ctx.Context); err != nil {
			return err
		}
		if _, err := commandOutputArgs(ctx.Context, "", "git", "-C", root, "init", "--initial-branch=main"); err != nil {
			return fmt.Errorf("initialize Git in %s: %w", root, err)
		}
	} else if err != nil {
		return fmt.Errorf("inspect Git metadata in %s: %w", root, err)
	} else if detected, detectErr := detectProject(ctx.Context, root); detectErr != nil {
		return detectErr
	} else if detected.Kind != store.ProjectKindRepo || detected.Root != root {
		return axi.Failure("project_init_git_invalid", "the existing Git metadata does not identify this Directory Project as a repository", false, "Inspect the .git entry before retrying")
	} else if defaultBranchUnborn(ctx.Context, repoTarget{Root: detected.Root, DefaultBranch: detected.DefaultBranch}) {
		if err := requireGitUnbornTaskSupport(ctx.Context); err != nil {
			return err
		}
	}
	return s.scanDirectoryProject(ctx, db, home, project)
}

func validateProjectInitRoot(ctx context.Context, root string) error {
	if filepath.Dir(root) == root {
		return axi.Failure("project_init_root", "Git init is refused at the filesystem root", false)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(home); resolveErr == nil {
		home = resolved
	}
	if filepath.Clean(home) == root {
		return axi.Failure("project_init_home", "Git init is refused in the User home directory", false)
	}
	if top, err := gitTop(ctx, root); err == nil {
		if filepath.Clean(top) != root {
			return axi.Failure("project_init_nested", "Git init is refused inside another repository", false)
		}
		return nil
	}
	if repos, err := scanWorkspace(ctx, root); err != nil {
		return err
	} else if len(repos) != 0 {
		return axi.Failure("project_init_workspace", "Git init is refused in a folder that contains repositories; register it as a Workspace", false)
	}
	parent := filepath.Dir(root)
	if repos, err := scanWorkspace(ctx, parent); err != nil {
		return err
	} else if len(repos) != 0 {
		return axi.Failure("project_init_workspace_parent", "Git init is refused because the parent contains repositories and may be a Workspace", false)
	}
	return nil
}

func requireGitUnbornTaskSupport(ctx context.Context) error {
	return requireGitVersion(ctx, 36, "Git 2.36 or newer is required for unborn-branch Task worktrees")
}

func requireGitVersion(ctx context.Context, minimumMinor int, requirement string) error {
	output, err := commandOutputArgs(ctx, "", "git", "--version")
	if err != nil {
		return axi.Failure("git_unavailable", "Git is required to initialize this Project", false, err.Error())
	}
	fields := strings.Fields(string(output))
	if len(fields) < 3 {
		return axi.Failure("git_version_unknown", "could not determine Git's version", false)
	}
	parts := strings.Split(fields[2], ".")
	if len(parts) < 2 {
		return axi.Failure("git_version_unknown", "could not determine Git's version", false)
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil || major < 2 || (major == 2 && minor < minimumMinor) {
		return axi.Failure("git_version_unsupported", requirement, false, "Upgrade Git before continuing")
	}
	return nil
}
