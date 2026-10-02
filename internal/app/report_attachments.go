package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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

type workspaceRootBaseline struct {
	Version int                                 `json:"version"`
	Files   map[string]workspaceRootFingerprint `json:"files"`
}

type workspaceRootFingerprint struct {
	Mode          uint32 `json:"mode"`
	SHA256        string `json:"sha256,omitempty"`
	SymlinkTarget string `json:"symlink_target,omitempty"`
}

const workspaceRootBaselineVersion = 1

func workspaceRootBaselinePath(home string, project store.Project, task store.Task) string {
	return filepath.Join(home, "projects", project.Name, ".workspace-root-baselines", taskIDString(task.Seq)+".json")
}

func writeWorkspaceRootBaseline(home string, project store.Project, task store.Task, mountRoot string, targets []taskMember) error {
	members, err := workspaceMemberPaths(targets)
	if err != nil {
		return err
	}
	files, err := workspaceRootFileStates(mountRoot, members)
	if err != nil {
		return err
	}
	data, err := json.Marshal(workspaceRootBaseline{Version: workspaceRootBaselineVersion, Files: files})
	if err != nil {
		return err
	}
	return writeFile(workspaceRootBaselinePath(home, project, task), data)
}

func readWorkspaceRootBaseline(home string, project store.Project, task store.Task) (workspaceRootBaseline, error) {
	data, err := os.ReadFile(workspaceRootBaselinePath(home, project, task))
	if err != nil {
		return workspaceRootBaseline{}, fmt.Errorf("read workspace root acquisition baseline: %w", err)
	}
	var baseline workspaceRootBaseline
	if err := json.Unmarshal(data, &baseline); err != nil {
		return workspaceRootBaseline{}, fmt.Errorf("decode workspace root acquisition baseline: %w", err)
	}
	if baseline.Version != workspaceRootBaselineVersion || baseline.Files == nil {
		return workspaceRootBaseline{}, fmt.Errorf("unsupported or incomplete workspace root acquisition baseline")
	}
	for path := range baseline.Files {
		if !safeWorkspaceRootPath(path) || isGitMetadataPath(path) || isWorkspaceControlPath(path) {
			return workspaceRootBaseline{}, fmt.Errorf("unsafe path %q in workspace root acquisition baseline", path)
		}
	}
	return baseline, nil
}

func workspaceMemberPaths(targets []taskMember) (map[string]bool, error) {
	members := make(map[string]bool, len(targets))
	for _, target := range targets {
		if !safeWorkspaceRootPath(target.Path) {
			return nil, fmt.Errorf("unsafe workspace Member path %q", target.Path)
		}
		members[target.Path] = true
	}
	return members, nil
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
// Workspace Members are enumerated repository by repository, and shared-root
// files are compared with the prelaunch Mount snapshot. A retry may encounter
// the same saved files but never removes or replaces them.
func preserveReportAttachments(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task) error {
	if task.WorktreePath == "" {
		return nil
	}
	repositories := []reportAttachmentRepository{{path: task.WorktreePath, baseRef: task.BaseRef}}
	workspaceMembers := map[string]bool{}
	if project.IsWorkspace() {
		targets, err := workspaceMountTargets(ctx, db, project)
		if err != nil {
			return err
		}
		workspaceMembers, err = workspaceMemberPaths(targets)
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
	if project.IsWorkspace() {
		baseline, err := readWorkspaceRootBaseline(home, project, task)
		if err != nil {
			return err
		}
		rootAttachments, err := workspaceRootReportAttachments(ctx, task.WorktreePath, workspaceMembers, baseline)
		if err != nil {
			return err
		}
		for name, source := range rootAttachments {
			paths[name] = source
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
			return fmt.Errorf("report attachment %q escapes its source tree", name)
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

// workspaceRootReportAttachments finds new or changed files in a workspace
// Mount relative to the copy captured before the Rider started. Files inside
// nested repositories come from Git's tracked and non-ignored file listing.
func workspaceRootReportAttachments(ctx context.Context, mountRoot string, members map[string]bool, baseline workspaceRootBaseline) (map[string]reportAttachmentSource, error) {
	mountRoot, err := filepath.EvalSymlinks(mountRoot)
	if err != nil {
		return nil, err
	}
	var rootFiles []string
	var repositories []reportAttachmentRepository
	err = filepath.WalkDir(mountRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(mountRoot, path)
		if err != nil || relative == "." {
			return err
		}
		if entry.IsDir() {
			if members[relative] || entry.Name() == ".git" {
				return filepath.SkipDir
			}
			if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
				repositories = append(repositories, reportAttachmentRepository{path: path, relative: relative})
			} else if !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		if isGitMetadataPath(relative) || isWorkspaceControlPath(relative) {
			return nil
		}
		if !safeWorkspaceRootPath(relative) {
			return fmt.Errorf("unsafe workspace-root Report attachment path %q", relative)
		}
		rootFiles = append(rootFiles, relative)
		return nil
	})
	if err != nil {
		return nil, err
	}

	attachments := map[string]reportAttachmentSource{}
	addIfChanged := func(relative, root, name string) error {
		if !safeWorkspaceRootPath(relative) || isGitMetadataPath(relative) || isWorkspaceControlPath(relative) {
			return fmt.Errorf("unsafe workspace-root Report attachment path %q", relative)
		}
		current, err := fingerprintWorkspaceRootFile(filepath.Join(root, name))
		if err != nil {
			return err
		}
		previous, found := baseline.Files[relative]
		if !found || current != previous {
			attachments[relative] = reportAttachmentSource{root: root, name: name}
		}
		return nil
	}
	for _, relative := range rootFiles {
		insideRepository := false
		for _, repository := range repositories {
			if workspaceRelativePathWithin(repository.relative, relative) {
				insideRepository = true
				break
			}
		}
		if insideRepository {
			continue
		}
		if err := addIfChanged(relative, mountRoot, relative); err != nil {
			return nil, err
		}
	}
	for _, repository := range repositories {
		output, err := gitOutputRaw(ctx, repository.path, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
		if err != nil {
			return nil, fmt.Errorf("list workspace-root Report attachments in %s: %w", repository.path, err)
		}
		for _, name := range nulPaths(output) {
			if !safeWorkspaceRootPath(name) || isGitMetadataPath(name) {
				return nil, fmt.Errorf("unsafe nested Git Report attachment path %q", name)
			}
			source := filepath.Join(repository.path, name)
			info, err := os.Lstat(source)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if info.IsDir() {
				continue
			}
			relative := filepath.Join(repository.relative, name)
			if err := addIfChanged(relative, repository.path, name); err != nil {
				return nil, err
			}
		}
	}
	return attachments, nil
}

func workspaceRootFileStates(root string, members map[string]bool) (map[string]workspaceRootFingerprint, error) {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	files := map[string]workspaceRootFingerprint{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == "." {
			return err
		}
		if entry.IsDir() {
			if members[relative] || entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if isGitMetadataPath(relative) || isWorkspaceControlPath(relative) {
			return nil
		}
		if !safeWorkspaceRootPath(relative) {
			return fmt.Errorf("unsafe workspace-root baseline path %q", relative)
		}
		fingerprint, err := fingerprintWorkspaceRootFile(path)
		if err != nil {
			return err
		}
		files[relative] = fingerprint
		return nil
	})
	return files, err
}

func fingerprintWorkspaceRootFile(path string) (workspaceRootFingerprint, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return workspaceRootFingerprint{}, err
	}
	fingerprint := workspaceRootFingerprint{Mode: uint32(info.Mode() & (fs.ModeType | 0o777))}
	if info.Mode().IsRegular() {
		file, err := os.Open(path)
		if err != nil {
			return workspaceRootFingerprint{}, err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return workspaceRootFingerprint{}, copyErr
		}
		if closeErr != nil {
			return workspaceRootFingerprint{}, closeErr
		}
		fingerprint.SHA256 = hex.EncodeToString(hash.Sum(nil))
	} else if info.Mode()&os.ModeSymlink != 0 {
		fingerprint.SymlinkTarget, err = os.Readlink(path)
		if err != nil {
			return workspaceRootFingerprint{}, err
		}
	}
	return fingerprint, nil
}

func safeWorkspaceRootPath(path string) bool {
	return path != "" && path != "." && path != ".." && !filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.HasPrefix(path, ".."+string(os.PathSeparator))
}

func isGitMetadataPath(path string) bool {
	for _, component := range strings.Split(path, string(os.PathSeparator)) {
		if component == ".git" {
			return true
		}
	}
	return false
}

func isWorkspaceControlPath(path string) bool {
	return oneOfString(path, "report.md", "brief.md", "launch.md", "relaunch.md", "findings.toon")
}

func workspaceRelativePathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
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
