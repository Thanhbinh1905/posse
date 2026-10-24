package app

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
)

type taskDiffSource struct {
	member string
	root   string
	base   string
}

// produceTaskDiff is the shared diff renderer for Task-oriented views such as
// diff, board and inbox.
func produceTaskDiff(ctx context.Context, sources []taskDiffSource, stat bool) (string, error) {
	var output strings.Builder
	for i, source := range sources {
		if i > 0 {
			output.WriteByte('\n')
		}
		if source.member != "" {
			fmt.Fprintf(&output, "Member: %s\n", source.member)
		}
		patch, err := taskWorktreeDiff(ctx, source, stat)
		if err != nil {
			return "", err
		}
		output.WriteString(patch)
		if patch != "" && !strings.HasSuffix(patch, "\n") {
			output.WriteByte('\n')
		}
	}
	return output.String(), nil
}

func taskWorktreeDiff(ctx context.Context, source taskDiffSource, stat bool) (string, error) {
	index, err := os.CreateTemp("", "posse-diff-index-")
	if err != nil {
		return "", fmt.Errorf("create temporary Git index: %w", err)
	}
	indexPath := index.Name()
	if err := index.Close(); err != nil {
		_ = os.Remove(indexPath)
		return "", fmt.Errorf("close temporary Git index: %w", err)
	}
	defer os.Remove(indexPath)
	env := []string{"GIT_INDEX_FILE=" + indexPath}
	if _, err := commandStdoutArgsWithEnv(ctx, source.root, env, "git", "read-tree", "HEAD"); err != nil {
		return "", fmt.Errorf("initialize temporary Git index: %w", err)
	}
	if _, err := commandStdoutArgsWithEnv(ctx, source.root, env, "git", "add", "-N", "--", "."); err != nil {
		return "", fmt.Errorf("register untracked files in temporary Git index: %w", err)
	}
	args := []string{"diff", "--no-ext-diff"}
	if stat {
		args = append(args, "--stat")
	}
	if source.base != "" {
		args = append(args, "--merge-base", source.base)
	}
	args = append(args, "--")
	output, err := commandStdoutArgsWithEnv(ctx, source.root, env, "git", args...)
	if err != nil {
		return "", fmt.Errorf("git diff: %w", err)
	}
	return output, nil
}

func (s *Service) diff(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("diff", args, map[string]flagSpec{"stat": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("diff requires one Task id", "Run `posse list` to list Tasks")
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if err := s.requireLead(ctx.Context, db, project); err != nil {
		return err
	}
	task, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	sources := []taskDiffSource{}
	if project.IsWorkspace() {
		repos, err := db.TaskRepos(ctx.Context, task.ID)
		if err != nil {
			return err
		}
		for _, repo := range repos {
			if repo.WorktreePath == "" {
				continue
			}
			sources = append(sources, taskDiffSource{member: repo.Repo, root: repo.WorktreePath, base: repo.BaseRef})
		}
	} else {
		if task.WorktreePath == "" {
			return axi.Failure("task_worktree_missing", "Task has no worktree to diff", false)
		}
		sources = append(sources, taskDiffSource{root: task.WorktreePath, base: task.BaseRef})
	}
	result, err := produceTaskDiff(ctx.Context, sources, parsed.Bool("stat"))
	if err != nil {
		return axi.Failure("task_diff_failed", "cannot produce Task diff: "+err.Error(), false)
	}
	_, err = fmt.Fprint(ctx.Out, result)
	return err
}
