package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thanhbinh1905/posse/internal/store"
)

type reportAttachmentRepository struct {
	path     string
	relative string
	baseRef  string
}

type reportAttachmentSource struct {
	root string
	name string
}

// reportAttachmentGitArgs lists changes without treating an absent base ref as
// an empty pathspec. The HEAD diff includes staged and unstaged changes.
func reportAttachmentGitArgs(baseRef string) [][]string {
	args := make([][]string, 0, 3)
	if baseRef != "" {
		args = append(args, []string{"diff", "--name-only", "-z", baseRef, "HEAD"})
	}
	return append(args,
		[]string{"diff", "--name-only", "-z", "HEAD"},
		[]string{"ls-files", "--others", "--exclude-standard", "-z"},
	)
}

// preserveReportAttachments saves the Rider's non-ignored, unlanded files at
// their worktree-relative paths next to report.md before the Mount is reset.
// Workspace Mounts are enumerated repository by repository. A retry may
// encounter the same saved files but never removes or replaces them.
func preserveReportAttachments(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task) error {
	if task.WorktreePath == "" {
		return nil
	}
	repositories := []reportAttachmentRepository{{path: task.WorktreePath, baseRef: task.BaseRef}}
	if project.IsWorkspace() {
		targets, err := workspaceMountTargets(ctx, db, project)
		if err != nil {
			return err
		}
		taskRepos, err := db.TaskRepos(ctx, task.ID)
		if err != nil {
			return err
		}
		baseRefs := make(map[string]string, len(taskRepos))
		for _, repo := range taskRepos {
			baseRefs[repo.Repo] = repo.BaseRef
		}
		repositories = repositories[:0]
		for _, target := range targets {
			if target.Path == "" || filepath.IsAbs(target.Path) || filepath.Clean(target.Path) != target.Path || target.Path == ".." || strings.HasPrefix(target.Path, ".."+string(os.PathSeparator)) {
				return fmt.Errorf("unsafe workspace Member path %q", target.Path)
			}
			path := filepath.Join(task.WorktreePath, target.Path)
			if _, err := os.Stat(filepath.Join(path, ".git")); os.IsNotExist(err) {
				continue
			} else if err != nil {
				return err
			}
			baseRef, selected := baseRefs[target.Name]
			if !selected {
				baseRef = "HEAD"
			}
			repositories = append(repositories, reportAttachmentRepository{path: path, relative: target.Path, baseRef: baseRef})
		}
	}

	paths := map[string]reportAttachmentSource{}
	for _, repository := range repositories {
		for _, args := range reportAttachmentGitArgs(repository.baseRef) {
			output, err := gitOutputRaw(ctx, repository.path, args...)
			if err != nil {
				return fmt.Errorf("list Report attachments in %s: %w", repository.path, err)
			}
			for _, name := range nulPaths(output) {
				if name == "" || filepath.IsAbs(name) || name == ".git" || strings.HasPrefix(name, ".git"+string(os.PathSeparator)) || filepath.Clean(name) != name || name == ".." || strings.HasPrefix(name, ".."+string(os.PathSeparator)) {
					return fmt.Errorf("unsafe Report attachment path %q", name)
				}
				relative := name
				if repository.relative != "" {
					relative = filepath.Join(repository.relative, name)
				}
				paths[relative] = reportAttachmentSource{root: repository.path, name: name}
			}
		}
	}

	names := make([]string, 0, len(paths))
	for path := range paths {
		names = append(names, path)
	}
	sort.Strings(names)
	destination := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq))
	for _, name := range names {
		sourcePath := paths[name]
		root, err := filepath.EvalSymlinks(sourcePath.root)
		if err != nil {
			return err
		}
		source := filepath.Join(root, sourcePath.name)
		resolved, err := filepath.EvalSymlinks(source)
		if os.IsNotExist(err) {
			continue
		} // A deleted file has no bytes to preserve.
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, resolved)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("report attachment %q escapes Member repository", name)
		}
		info, err := os.Stat(source)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("report attachment %q is not a regular file", name)
		}
		contents, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, name)
		if err := safeAttachmentDestination(destination, target); err != nil {
			return err
		}
		if _, err := os.Lstat(target); err == nil { // Never replace an earlier saved artifact with a different file.
			existing, err := os.ReadFile(target)
			if err != nil {
				return err
			}
			if string(existing) != string(contents) {
				return fmt.Errorf("report attachment %q conflicts with an existing saved file", name)
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := writeFile(target, contents); err != nil {
			return err
		}
	}
	return nil
}

func safeAttachmentDestination(root, path string) error {
	for parent := filepath.Dir(path); parent != root && parent != filepath.Dir(root); parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("report attachment directory %q is not a directory", parent)
		}
	}
	return nil
}

// saveExplicitAttachment includes ignored evidence named by a Rider on its done Signal.
func saveExplicitAttachment(task store.Task, home string, project store.Project, supplied string) error {
	root, err := filepath.Abs(task.WorktreePath)
	if err != nil {
		return err
	}
	path := supplied
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || relative == ".git" || strings.HasPrefix(relative, ".git"+string(os.PathSeparator)) {
		return fmt.Errorf("attachment must stay inside the Rider's Mount")
	}
	destination := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq))
	return filepath.WalkDir(path, func(source string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		name, err := filepath.Rel(root, source)
		if err != nil || name == "report.md" || name == "brief.md" || name == "launch.md" || name == "relaunch.md" || name == "findings.toon" {
			return fmt.Errorf("invalid attachment %q", source)
		}
		target := filepath.Join(destination, name)
		if err := safeAttachmentDestination(destination, target); err != nil {
			return err
		}
		_, err = copyWorktreeFile(task, source, target)
		return err
	})
}

func reportAttachments(home string, project store.Project, task store.Task) ([]string, error) {
	root := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq))
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative != "brief.md" && relative != "launch.md" && relative != "relaunch.md" && relative != "report.md" && relative != "findings.toon" {
			paths = append(paths, filepath.ToSlash(relative))
		}
		return nil
	})
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	return paths, err
}
