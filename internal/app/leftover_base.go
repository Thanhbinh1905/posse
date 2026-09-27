package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// leftoverBase creates an unreferenced commit on the current default branch.
// The saved snapshot itself can contain commits already squash-merged, so only
// its tree difference from the merged head belongs in the new Task.
func leftoverBase(ctx context.Context, root, snapshotRef, mergedRef, defaultRef string) (string, error) {
	base, err := gitOutput(ctx, root, "rev-parse", "--verify", defaultRef+"^{commit}")
	if err != nil {
		return "", err
	}
	merged, err := gitOutput(ctx, root, "rev-parse", "--verify", mergedRef+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("merged head is not available locally: %w", err)
	}
	diff := exec.CommandContext(ctx, "git", "-C", root, "diff", "--binary", "--full-index", merged, snapshotRef)
	patch, err := diff.Output()
	if err != nil {
		return "", fmt.Errorf("read Leftover diff: %w", err)
	}
	if len(patch) == 0 {
		return base, nil
	}
	index, err := os.CreateTemp("", "posse-leftover-index-*")
	if err != nil {
		return "", err
	}
	path := index.Name()
	_ = index.Close()
	_ = os.Remove(path)
	defer os.Remove(path)
	git := func(input []byte, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+path)
		if input != nil {
			cmd.Stdin = strings.NewReader(string(input))
		}
		output, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(output)), err)
		}
		return strings.TrimSpace(string(output)), nil
	}
	if _, err := git(nil, "read-tree", base); err != nil {
		return "", err
	}
	if _, err := git(patch, "apply", "--cached", "--3way", "-"); err != nil {
		return "", fmt.Errorf("leftover conflicts with the current default branch: %w", err)
	}
	tree, err := git(nil, "write-tree")
	if err != nil {
		return "", err
	}
	return git(nil, "commit-tree", tree, "-p", base, "-m", "Rebase approved Leftover onto current default branch")
}
