package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) captureDiscardTips(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task) error {
	captured, err := db.LatestApprovalBranchSHA(ctx, task.ID, "discard")
	if store.IsNotFound(err) || err == nil && captured == "" {
		return nil
	}
	if err != nil {
		return err
	}
	tips := []discardTip{}
	if project.IsWorkspace() {
		taskRepos, err := db.TaskRepos(ctx, task.ID)
		if err != nil {
			return err
		}
		baseRefs := make(map[string]string, len(taskRepos))
		for _, taskRepo := range taskRepos {
			baseRefs[taskRepo.Repo] = taskRepo.BaseRef
		}
		for _, pair := range strings.Split(captured, ",") {
			repository, sha, ok := strings.Cut(pair, "=")
			if !ok || repository == "" || sha == "" {
				return fmt.Errorf("invalid approved workspace discard tip %q", pair)
			}
			target, err := s.projectTarget(ctx, db, project, repository)
			if err != nil {
				return err
			}
			baseRef := baseRefs[repository]
			if baseRef == "" {
				baseRef = target.DefaultBranch
			}
			tips = append(tips, discardTip{repository: repository, root: target.Root, sha: sha, baseRef: baseRef})
		}
	} else {
		baseRef := task.BaseRef
		if baseRef == "" {
			baseRef = project.DefaultBranch
		}
		tips = append(tips, discardTip{repository: project.Name, root: project.Root, sha: captured, baseRef: baseRef})
	}
	needed := tips[:0]
	for _, tip := range tips {
		commit, err := gitOutput(ctx, tip.root, "rev-parse", "--verify", tip.sha+"^{commit}")
		if err != nil {
			return err
		}
		if commit != tip.sha {
			return fmt.Errorf("approved SHA %s resolved to %s", tip.sha, commit)
		}
		tip.sha = commit
		if tip.baseRef != "" {
			if base, baseErr := gitOutput(ctx, tip.root, "merge-base", commit, tip.baseRef); baseErr == nil {
				if base == commit {
					continue
				}
				tip.baseCommit = base
			}
		}
		needed = append(needed, tip)
	}
	if len(needed) == 0 {
		return nil
	}
	artifactDir, err := ensureDiscardArtifactDirectory(home, project, task)
	if err != nil {
		return err
	}
	for _, tip := range needed {
		if err := captureDiscardTip(ctx, tip, artifactDir, task); err != nil {
			return fmt.Errorf("capture approved discard tip for %s: %w", tip.repository, err)
		}
	}
	return nil
}

type discardTip struct {
	repository string
	root       string
	sha        string
	baseRef    string
	baseCommit string
}

func ensureDiscardArtifactDirectory(home string, project store.Project, task store.Task) (string, error) {
	scratchPath, err := taskScratchPath(home, project, task)
	if err != nil {
		return "", err
	}
	homeRoot := filepath.Dir(filepath.Dir(filepath.Dir(scratchPath)))
	root := filepath.Join(homeRoot, "projects", project.Name, "tasks", taskIDString(task.Seq))
	for _, directory := range []string{
		filepath.Join(homeRoot, "projects"),
		filepath.Join(homeRoot, "projects", project.Name),
		filepath.Join(homeRoot, "projects", project.Name, "tasks"),
		root,
		filepath.Join(root, "discard"),
	} {
		info, err := os.Lstat(directory)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("refuse to write discard capture beneath unsafe directory %s", directory)
			}
			continue
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if err := os.Mkdir(directory, 0o700); err != nil && !os.IsExist(err) {
			return "", err
		}
		info, err = os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refuse to write discard capture beneath unsafe directory %s", directory)
		}
	}
	return filepath.Join(root, "discard"), nil
}

func captureDiscardTip(ctx context.Context, tip discardTip, artifactDir string, task store.Task) (returnErr error) {
	commit := tip.sha
	name := discardTipName(tip.repository, commit)
	bundlePath := filepath.Join(artifactDir, name)
	if info, err := os.Lstat(bundlePath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("refuse to replace non-regular discard capture %s", bundlePath)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	repositoryHash := sha256.Sum256([]byte(tip.repository))
	captureRef := fmt.Sprintf("refs/posse/discard-captures/%s/%s", taskIDString(task.Seq), hex.EncodeToString(repositoryHash[:8]))
	previous, refErr := gitOutput(ctx, tip.root, "rev-parse", "--verify", captureRef)
	if refErr != nil && !isMissingGitRef(refErr) {
		return refErr
	}
	if previous == "" {
		if _, err := gitOutput(ctx, tip.root, "update-ref", captureRef, commit, strings.Repeat("0", len(commit))); err != nil {
			return err
		}
	} else if previous != commit {
		if _, err := gitOutput(ctx, tip.root, "update-ref", captureRef, commit, previous); err != nil {
			return err
		}
	}
	defer func() {
		cleanupCtx := context.WithoutCancel(ctx)
		if _, err := gitOutput(cleanupCtx, tip.root, "update-ref", "-d", captureRef, commit); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove temporary discard capture ref: %w", err))
		}
	}()

	temporary, err := os.CreateTemp(artifactDir, ".discard-*.bundle")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		if err := os.Remove(temporaryPath); err != nil && !os.IsNotExist(err) {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove temporary discard bundle: %w", err))
		}
	}()
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Remove(temporaryPath); err != nil {
		return err
	}
	bundleArgs := []string{"bundle", "create", temporaryPath, captureRef}
	if tip.baseCommit != "" {
		bundleArgs = append(bundleArgs, "^"+tip.baseCommit)
	}
	if _, err := gitOutput(ctx, tip.root, bundleArgs...); err != nil {
		return err
	}
	if _, err := gitOutput(ctx, tip.root, "bundle", "verify", temporaryPath); err != nil {
		return err
	}
	heads, err := gitOutput(ctx, tip.root, "bundle", "list-heads", temporaryPath)
	if err != nil {
		return err
	}
	if !bundleHasCommit(heads, commit) {
		return fmt.Errorf("discard bundle does not advertise approved commit %s", commit)
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, bundlePath); err != nil {
		return err
	}
	return nil
}

func discardTipName(repository, commit string) string {
	name := strings.Map(func(char rune) rune {
		if unicode.IsLetter(char) || unicode.IsDigit(char) || char == '-' || char == '_' {
			return char
		}
		return '-'
	}, repository)
	name = strings.Trim(name, "-")
	if name == "" {
		name = "repository"
	}
	hash := sha256.Sum256([]byte(repository))
	return fmt.Sprintf("%s-%s-%s.bundle", name, hex.EncodeToString(hash[:4]), commit[:12])
}

func bundleHasCommit(heads, commit string) bool {
	for _, line := range strings.Split(heads, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == commit {
			return true
		}
	}
	return false
}
