package app

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

type pruneItem struct {
	kind   string
	label  string
	path   string
	state  string
	reason string
	bytes  int64
	seq    int
	sha    string
	mount  store.Mount
	files  []string
}

func taskScratchPruneItems(ctx context.Context, db *store.DB, home string, project store.Project, tasks []store.Task) ([]pruneItem, error) {
	path, err := taskScratchPath(home, project, store.Task{Seq: 1})
	if err != nil {
		return nil, err
	}
	root := filepath.Dir(path)
	scratchRoot := filepath.Dir(root)
	for _, directory := range []string{scratchRoot, root} {
		info, err := os.Lstat(directory)
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refuse to inspect unsafe Task scratch root %s", directory)
		}
	}
	known := make(map[int]store.Task, len(tasks))
	for _, task := range tasks {
		known[task.Seq] = task
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	items := []pruneItem{}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(entry.Name(), "t") {
			continue
		}
		seq, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), "t"))
		if err != nil || seq < 1 || "t"+strconv.Itoa(seq) != entry.Name() {
			continue
		}
		path := filepath.Join(root, entry.Name())
		state, reason := "orphan", "Task no longer exists"
		if task, found := known[seq]; found {
			state = string(task.State)
			if !terminalScratchState(task.State) {
				continue
			}
			if _, intentErr := db.IntentByTask(ctx, task.ID); intentErr == nil {
				continue
			} else if !store.IsNotFound(intentErr) {
				return nil, intentErr
			}
			mount, mountErr := db.MountByTask(ctx, task.ID)
			if mountErr == nil && mount.State != "idle" && mount.State != "broken" {
				continue
			}
			if mountErr != nil && !store.IsNotFound(mountErr) {
				return nil, mountErr
			}
			reason = "terminal Task scratch"
		}
		bytes, err := pathBytes(path)
		if err != nil {
			return nil, err
		}
		items = append(items, pruneItem{kind: "scratch", label: entry.Name(), path: path, state: state, reason: reason, bytes: bytes, seq: seq})
	}
	return items, nil
}

func terminalScratchState(state store.State) bool {
	switch state {
	case store.StateLanded, store.StateTornDown, store.StateFailed, store.StateLost:
		return true
	default:
		return false
	}
}

func expiredTaskArtifactItems(home string, project store.Project, tasks []store.Task, retention time.Duration, now time.Time) ([]pruneItem, error) {
	items := []pruneItem{}
	cutoff := now.Add(-retention).UnixMilli()
	for _, task := range tasks {
		if task.State != store.StateTornDown || task.UpdatedAt > cutoff {
			continue
		}
		root, exists, err := safeTaskArtifactRoot(home, project, task.Seq)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		files, bytes, err := taskArtifactFiles(root)
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			continue
		}
		items = append(items, pruneItem{kind: "artifacts", label: taskIDString(task.Seq), path: root, state: string(task.State), reason: "Task artifacts exceeded retention", bytes: bytes, seq: task.Seq, files: files})
	}
	return items, nil
}

func safeTaskArtifactRoot(home string, project store.Project, seq int) (string, bool, error) {
	if project.Name == "" || project.Name == "." || project.Name == ".." || filepath.Base(project.Name) != project.Name || strings.ContainsAny(project.Name, `/\\`) {
		return "", false, fmt.Errorf("unsafe Project name %q for Task artifacts", project.Name)
	}
	if seq < 1 {
		return "", false, fmt.Errorf("invalid Task sequence %d for artifacts", seq)
	}
	homeRoot, err := filepath.Abs(home)
	if err != nil {
		return "", false, err
	}
	homeRoot, err = filepath.EvalSymlinks(homeRoot)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	root := filepath.Join(homeRoot, "projects", project.Name, "tasks", taskIDString(seq))
	for _, directory := range []string{
		filepath.Join(homeRoot, "projects"),
		filepath.Join(homeRoot, "projects", project.Name),
		filepath.Join(homeRoot, "projects", project.Name, "tasks"),
		root,
	} {
		info, err := os.Lstat(directory)
		if os.IsNotExist(err) {
			return root, false, nil
		}
		if err != nil {
			return "", false, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", false, fmt.Errorf("refuse to inspect Task artifacts beneath unsafe directory %s", directory)
		}
	}
	return root, true, nil
}

func taskArtifactFiles(root string) ([]string, int64, error) {
	preserve := map[string]bool{"task.toml": true, "brief.md": true, "launch.md": true, "relaunch.md": true, "findings.toon": true}
	files := []string{}
	var bytes int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if preserve[relative] {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		files = append(files, relative)
		bytes += info.Size()
		return nil
	})
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	return files, bytes, err
}

func removeTaskArtifacts(home string, project store.Project, seq int, files []string) error {
	root, exists, err := safeTaskArtifactRoot(home, project, seq)
	if err != nil || !exists {
		return err
	}
	return removeTaskArtifactFiles(root, files)
}

func removeTaskArtifactFiles(root string, files []string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	rootInfo, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse to remove artifacts beneath non-directory Task data %s", root)
	}
	for _, relative := range files {
		if filepath.IsAbs(relative) || filepath.Clean(relative) != relative || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe Task artifact path %q", relative)
		}
		path := filepath.Join(root, relative)
		for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
			directoryRelative, err := filepath.Rel(root, directory)
			if err != nil || directoryRelative == ".." || strings.HasPrefix(directoryRelative, ".."+string(os.PathSeparator)) {
				return fmt.Errorf("task artifact parent escapes Task data: %s", directory)
			}
			info, err := os.Lstat(directory)
			if err != nil {
				return err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refuse to remove artifacts through unsafe directory %s", directory)
			}
			if info.Mode().Perm()&0o700 != 0o700 {
				if err := os.Chmod(directory, info.Mode().Perm()|0o700); err != nil {
					return err
				}
			}
			if directory == root {
				break
			}
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("refuse to remove non-regular Task artifact %s", path)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		for directory := filepath.Dir(path); directory != root && pathInside(directory, root); directory = filepath.Dir(directory) {
			if err := os.Remove(directory); err != nil {
				break
			}
		}
	}
	return nil
}

func landedOrDiscardedBranchItems(ctx context.Context, db *store.DB, home string, project store.Project, tasks []store.Task) ([]pruneItem, error) {
	items := []pruneItem{}
	for _, task := range tasks {
		if task.Branch == "" || !strings.HasPrefix(task.Branch, "posse/") || task.State != store.StateLanded && task.State != store.StateTornDown {
			continue
		}
		approved, err := db.LatestApprovalBranchSHA(ctx, task.ID, "discard")
		if store.IsNotFound(err) {
			approved = ""
		} else if err != nil {
			return nil, err
		}
		if project.IsWorkspace() {
			workspaceItems, err := workspaceBranchPruneItems(ctx, db, home, project, task, approved)
			if err != nil {
				return nil, err
			}
			items = append(items, workspaceItems...)
			continue
		}
		branchRef := "refs/heads/" + task.Branch
		sha, err := gitOutput(ctx, project.Root, "rev-parse", "--verify", branchRef)
		if err != nil {
			if isMissingGitRef(err) {
				continue
			}
			return nil, err
		}
		eligible, reason := false, ""
		if approved != "" && sha == approved {
			baseRef := task.BaseRef
			if baseRef == "" {
				baseRef = project.DefaultBranch
			}
			captured, err := discardTipBundleExists(ctx, home, project, task, project.Name, project.Root, sha, baseRef)
			if err != nil {
				return nil, err
			}
			if captured {
				eligible, reason = true, "approved discard tip is captured"
			}
		} else if _, err := gitOutput(ctx, project.Root, "merge-base", "--is-ancestor", branchRef, "refs/heads/"+project.DefaultBranch); err == nil {
			eligible, reason = true, "Task branch is landed"
		} else if task.LandingMode == "pr" {
			observation, observationErr := db.LatestPRObservation(ctx, task.ID)
			if observationErr != nil && !store.IsNotFound(observationErr) {
				return nil, observationErr
			}
			if observationErr == nil && observation.State == "MERGED" && sha == observation.HeadSHA {
				eligible, reason = true, "merged PR head is captured"
			}
		}
		if !eligible {
			continue
		}
		bytes, err := gitReachableBytes(ctx, project.Root, branchRef, "refs/heads/"+project.DefaultBranch)
		if err != nil {
			return nil, err
		}
		items = append(items, pruneItem{kind: "branch", label: task.Branch, path: project.Root, state: string(task.State), reason: reason, bytes: bytes, seq: task.Seq, sha: sha})
	}
	return items, nil
}

func workspaceBranchPruneItems(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task, approved string) ([]pruneItem, error) {
	repos, err := db.TaskRepos(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	projectRepos, err := db.ProjectRepos(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	approvedTips := map[string]string{}
	for _, pair := range strings.Split(approved, ",") {
		if repository, sha, ok := strings.Cut(pair, "="); ok {
			approvedTips[repository] = sha
		}
	}
	items := []pruneItem{}
	for _, repo := range repos {
		root := filepath.Join(project.Root, repo.Repo)
		var defaultBranch string
		found := false
		for _, projectRepo := range projectRepos {
			if projectRepo.Name == repo.Repo {
				root = filepath.Join(project.Root, projectRepo.Path)
				defaultBranch = projectRepo.DefaultBranch
				found = true
				break
			}
		}
		if !found {
			continue
		}
		branchRef := "refs/heads/" + task.Branch
		branchBase := repo.BaseRef
		if branchBase == "" {
			branchBase = defaultBranch
		}
		sha, err := gitOutput(ctx, root, "rev-parse", "--verify", branchRef)
		if err != nil {
			if isMissingGitRef(err) {
				continue
			}
			return nil, err
		}
		eligible, reason := false, ""
		if approvedSHA := approvedTips[repo.Repo]; approvedSHA != "" && sha == approvedSHA {
			captured, err := discardTipBundleExists(ctx, home, project, task, repo.Repo, root, sha, branchBase)
			if err != nil {
				return nil, err
			}
			if !captured {
				continue
			}
			eligible, reason = true, "approved discard tip is captured"
		} else if repo.State == store.TaskRepoLanded && repo.LandedRef != "" && repo.GatedSHA == sha {
			if _, err := gitOutput(ctx, root, "merge-base", "--is-ancestor", sha, repo.LandedRef); err == nil || repo.LandingMode == "pr" {
				eligible, reason = true, "Task branch is landed"
			}
		}
		if !eligible {
			continue
		}
		bytes, err := gitReachableBytes(ctx, root, branchRef, branchBase)
		if err != nil {
			return nil, err
		}
		items = append(items, pruneItem{kind: "branch", label: task.Branch, path: root, state: string(task.State), reason: reason, bytes: bytes, seq: task.Seq, sha: sha})
	}
	return items, nil
}

func discardTipBundleExists(ctx context.Context, home string, project store.Project, task store.Task, repository, root, sha, baseRef string) (bool, error) {
	if baseRef != "" {
		if base, err := gitOutput(ctx, root, "merge-base", sha, baseRef); err == nil && base == sha {
			return true, nil
		}
	}
	homeRoot, err := filepath.Abs(home)
	if err != nil {
		return false, err
	}
	homeRoot, err = filepath.EvalSymlinks(homeRoot)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	artifactRoot := filepath.Join(homeRoot, "projects", project.Name, "tasks", taskIDString(task.Seq), "discard")
	relative, err := filepath.Rel(homeRoot, artifactRoot)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return false, fmt.Errorf("discard artifact path escapes Posse home: %s", artifactRoot)
	}
	for directory := artifactRoot; ; directory = filepath.Dir(directory) {
		info, err := os.Lstat(directory)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, nil
		}
		if directory == homeRoot {
			break
		}
	}
	bundle := filepath.Join(artifactRoot, discardTipName(repository, sha))
	info, err := os.Lstat(bundle)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, nil
	}
	if _, err := gitOutput(ctx, root, "bundle", "verify", bundle); err != nil {
		return false, nil
	}
	heads, err := gitOutput(ctx, root, "bundle", "list-heads", bundle)
	if err != nil {
		return false, nil
	}
	return bundleHasCommit(heads, sha), nil
}

func gitReachableBytes(ctx context.Context, root, ref, base string) (int64, error) {
	command := exec.CommandContext(ctx, "git", "-C", root, "rev-list", "--objects", ref, "--not", base)
	output, err := command.Output()
	if err != nil {
		return 0, err
	}
	ids := []string{}
	seen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		id := strings.Fields(scanner.Text())
		if len(id) > 0 && !seen[id[0]] {
			seen[id[0]] = true
			ids = append(ids, id[0])
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	check := exec.CommandContext(ctx, "git", "-C", root, "cat-file", "--batch-check=%(objectsize)")
	check.Stdin = strings.NewReader(strings.Join(ids, "\n") + "\n")
	output, err = check.Output()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		size, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64)
		if err == nil {
			total += size
		}
	}
	return total, nil
}

func staleOwnedWorktreeItems(ctx context.Context, project store.Project, home string, mounts []store.Mount) ([]pruneItem, error) {
	listing, err := gitOutput(ctx, project.Root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	managed := filepath.Join(home, "remuda", project.Name)
	registered := map[string]bool{}
	for _, block := range strings.Split(listing, "\n\n") {
		var path string
		prunable := false
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "worktree ") {
				path = strings.TrimPrefix(line, "worktree ")
			}
			if strings.HasPrefix(line, "prunable ") {
				prunable = true
			}
		}
		if !prunable || path == "" || !managedMountPath(managed, path) || !safeManagedMountParent(home, project.Name) {
			continue
		}
		if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
			continue
		}
		registered[filepath.Clean(path)] = true
	}
	items := []pruneItem{}
	for path := range registered {
		var knownMount store.Mount
		for _, mount := range mounts {
			if filepath.Clean(mount.Path) == path {
				knownMount = mount
				break
			}
		}
		if knownMount.ID != 0 && (knownMount.State == "held" || knownMount.State == "releasing" || knownMount.State == "pruning" || knownMount.TaskID != 0) {
			continue
		}
		label := filepath.Base(path)
		items = append(items, pruneItem{kind: "worktree-registration", label: label, path: path, state: "stale", reason: "missing Posse-owned Mount checkout", mount: knownMount})
	}
	return items, nil
}

func safeManagedMountPath(home, project, path string) bool {
	root := filepath.Join(home, "remuda", project)
	if !managedMountPath(root, path) || !safeManagedMountParent(home, project) {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func safeManagedMountParent(home, project string) bool {
	for _, directory := range []string{filepath.Join(home, "remuda"), filepath.Join(home, "remuda", project)} {
		info, err := os.Lstat(directory)
		if os.IsNotExist(err) {
			return true
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

func managedMountPath(root, path string) bool {
	root, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || filepath.Dir(relative) != "." {
		return false
	}
	name := filepath.Base(relative)
	if !strings.HasPrefix(name, "mount-") {
		return false
	}
	_, err = strconv.Atoi(strings.TrimPrefix(name, "mount-"))
	return err == nil
}

func removeStaleWorktreeRegistration(ctx context.Context, root, path string) error {
	common, err := gitOutput(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	common = strings.TrimSpace(common)
	adminRoot := filepath.Join(common, "worktrees")
	entries, err := os.ReadDir(adminRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	want, err := filepath.Abs(filepath.Join(path, ".git"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		admin := filepath.Join(adminRoot, entry.Name())
		gitdir := filepath.Join(admin, "gitdir")
		info, err := os.Lstat(gitdir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		contents, err := os.ReadFile(gitdir)
		if err != nil {
			return err
		}
		registered := strings.TrimSpace(string(contents))
		if !filepath.IsAbs(registered) {
			registered = filepath.Join(admin, registered)
		}
		if filepath.Clean(registered) != want {
			continue
		}
		return os.RemoveAll(admin)
	}
	return nil
}

func pathBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	return total, err
}
