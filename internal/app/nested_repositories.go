package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
)

const (
	nestedRepositoryDepth = 3
	maxNestedRepositories = 64
	maxDiscoveryEntries   = 4096
)

type nestedRepositories struct {
	Repositories      []string
	Truncated         bool
	SkippedUnreadable int
	IgnoreRules       string
}

func (found nestedRepositories) object() axi.Object {
	return axi.Object{
		{Key: "depth_limit", Value: nestedRepositoryDepth},
		{Key: "repositories", Value: found.Repositories},
		{Key: "truncated", Value: found.Truncated},
		{Key: "skipped_unreadable", Value: found.SkippedUnreadable},
		{Key: "ignore_rules", Value: found.IgnoreRules},
	}
}

func discoverNestedRepositories(ctx context.Context, root string) nestedRepositories {
	found := nestedRepositories{Repositories: []string{}, IgnoreRules: "gitignore"}
	ignored, ignoreErr := newIgnoreMatcher(ctx, root)
	if ignoreErr != nil {
		found.IgnoreRules = "unavailable"
		found.Truncated = true
		return found
	}
	defer ignored.close()
	level := []string{"."}
	entries := 0
	for depth := 1; depth <= nestedRepositoryDepth && len(level) > 0; depth++ {
		candidates := []string{}
		for _, parent := range level {
			if entries == maxDiscoveryEntries {
				break
			}
			directory, err := os.Open(filepath.Join(root, parent))
			if err != nil {
				found.SkippedUnreadable++
				continue
			}
			children, err := directory.ReadDir(maxDiscoveryEntries - entries)
			_ = directory.Close()
			if err != nil && err != io.EOF {
				found.SkippedUnreadable++
			}
			entries += len(children)
			if entries == maxDiscoveryEntries {
				found.Truncated = true
			}
			sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
			for _, child := range children {
				if !child.IsDir() || child.Type()&os.ModeSymlink != 0 || strings.HasPrefix(child.Name(), ".") {
					continue
				}
				candidates = append(candidates, filepath.Join(parent, child.Name()))
			}
		}
		candidates, err := ignored.filter(candidates)
		if err != nil {
			found.IgnoreRules = "unavailable"
			found.Truncated = true
			break
		}
		next := []string{}
		for _, candidate := range candidates {
			if _, err := os.Lstat(filepath.Join(root, candidate, ".git")); err != nil {
				next = append(next, candidate)
				continue
			}
			if depth == 1 {
				continue
			}
			if len(found.Repositories) == maxNestedRepositories {
				found.Truncated = true
				continue
			}
			found.Repositories = append(found.Repositories, filepath.ToSlash(candidate))
		}
		if found.Truncated {
			break
		}
		level = next
	}
	sort.Strings(found.Repositories)
	return found
}

// ignoreMatcher applies Git ignore rules to a folder that need not be a
// repository: Git reads the folder as the work tree of an empty repository in
// the temporary directory, so nothing is written to the folder.
type ignoreMatcher struct {
	ctx    context.Context
	root   string
	gitDir string
}

func newIgnoreMatcher(ctx context.Context, root string) (*ignoreMatcher, error) {
	scratch, err := os.MkdirTemp("", "posse-ignore-")
	if err != nil {
		return nil, err
	}
	gitDir := filepath.Join(scratch, "git")
	if err := gitCommand(ctx, "", "init", "--quiet", "--bare", gitDir).Run(); err != nil {
		_ = os.RemoveAll(scratch)
		return nil, err
	}
	return &ignoreMatcher{ctx: ctx, root: root, gitDir: gitDir}, nil
}

func (m *ignoreMatcher) close() { _ = os.RemoveAll(filepath.Dir(m.gitDir)) }

func (m *ignoreMatcher) filter(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return paths, nil
	}
	command := gitCommand(m.ctx, m.gitDir, "--work-tree="+m.root, "-c", "core.bare=false", "check-ignore", "--no-index", "-z", "--stdin")
	command.Dir = m.root
	command.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	var output bytes.Buffer
	command.Stdout = &output
	if err := command.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			return nil, err
		}
	}
	excluded := map[string]bool{}
	for _, path := range strings.Split(output.String(), "\x00") {
		excluded[path] = true
	}
	kept := make([]string, 0, len(paths))
	for _, path := range paths {
		if !excluded[path] {
			kept = append(kept, path)
		}
	}
	return kept, nil
}

func gitCommand(ctx context.Context, gitDir string, args ...string) *exec.Cmd {
	if gitDir != "" {
		args = append([]string{"--git-dir=" + gitDir}, args...)
	}
	return exec.CommandContext(ctx, "git", args...)
}
