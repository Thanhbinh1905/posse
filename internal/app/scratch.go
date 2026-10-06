package app

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/thanhbinh1905/posse/internal/store"
)

func taskScratchPath(home string, project store.Project, task store.Task) (string, error) {
	if project.Name == "" || project.Name == "." || project.Name == ".." || filepath.Base(project.Name) != project.Name || strings.ContainsAny(project.Name, `/\\`) {
		return "", fmt.Errorf("unsafe Project name %q for Task scratch", project.Name)
	}
	if task.Seq < 1 {
		return "", fmt.Errorf("invalid Task sequence %d for scratch", task.Seq)
	}
	root, err := filepath.Abs(home)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return filepath.Join(root, "scratch", project.Name, taskIDString(task.Seq)), nil
}

func ensureTaskScratch(home string, project store.Project, task store.Task) (string, error) {
	path, err := taskScratchPath(home, project, task)
	if err != nil {
		return "", err
	}
	root := filepath.Join(filepath.Dir(filepath.Dir(path)))
	for _, directory := range []string{root, filepath.Dir(path), path} {
		info, err := os.Lstat(directory)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("task scratch path component is not a directory: %s", directory)
			}
			continue
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if err := os.Mkdir(directory, 0o700); err != nil && !os.IsExist(err) {
			return "", err
		}
	}
	return path, nil
}

func removeTaskScratch(home string, project store.Project, task store.Task) error {
	path, err := taskScratchPath(home, project, task)
	if err != nil {
		return err
	}
	root := filepath.Join(filepath.Dir(filepath.Dir(path)))
	for _, directory := range []string{root, filepath.Dir(path), path} {
		info, err := os.Lstat(directory)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refuse to remove non-directory Task scratch component %s", directory)
		}
	}
	if err := filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().Perm()&0o700 != 0o700 {
				return os.Chmod(current, info.Mode().Perm()|0o700)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	_ = os.Remove(filepath.Dir(path))
	_ = os.Remove(root)
	return nil
}

func exportTaskScratch(ctx context.Context, s *Service, paneID, scratch string) error {
	if paneID == "" || scratch == "" {
		return fmt.Errorf("task scratch environment requires a pane and directory")
	}
	quoted := "'" + strings.ReplaceAll(scratch, "'", "'\"'\"'") + "'"
	command := "export TMPDIR=" + quoted + " GOTMPDIR=" + quoted + " TMP=" + quoted + " TEMP=" + quoted
	_, err := s.herdrCall(ctx, "pane.send_input", map[string]any{"pane_id": paneID, "text": command, "keys": []string{"enter"}})
	return err
}
