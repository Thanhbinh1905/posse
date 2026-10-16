//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

type legacyWorkspaceRootBaseline struct {
	Version int                                 `json:"version"`
	Files   map[string]legacyWorkspaceFileState `json:"files"`
	Members map[string]string                   `json:"members,omitempty"`
}

type legacyWorkspaceFileState struct {
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256,omitempty"`
}

// prepareLegacyWorkspaceScout converts a newly admitted fixture into the plan
// shape written before selected workspace scope existed, then restores the
// shared-root copy and acquisition baseline that legacy Tasks relied on.
func prepareLegacyWorkspaceScout(t *testing.T, db *store.DB, project store.Project, workspace string, env []string, home string, task store.Task, includeUnrequestedWorker bool) error {
	t.Helper()
	plan, err := task.AdmittedPlan()
	if err != nil {
		return fmt.Errorf("read admitted plan: %w", err)
	}
	if plan == nil {
		return fmt.Errorf("Task has no admitted plan")
	}
	plan.Execution.RootContext = ""
	planJSON, err := plan.Encode()
	if err != nil {
		return fmt.Errorf("encode legacy plan: %w", err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE tasks SET plan_json=? WHERE id=?`, planJSON, task.ID); err != nil {
		return fmt.Errorf("store legacy Task plan: %w", err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=? AND revision=?`, planJSON, task.ID, plan.Revision); err != nil {
		return fmt.Errorf("store legacy Task plan revision: %w", err)
	}

	repos, err := db.ProjectRepos(context.Background(), project.ID)
	if err != nil {
		return err
	}
	members := make(map[string]bool, len(repos))
	for _, repo := range repos {
		members[repo.Path] = true
	}
	for _, entry := range mustReadDir(t, workspace) {
		if entry.IsDir() || !entry.Type().IsRegular() {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(workspace, entry.Name()))
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(task.WorktreePath, entry.Name()), contents, info.Mode().Perm()); err != nil {
			return err
		}
	}
	if includeUnrequestedWorker {
		worker := filepath.Join(task.WorktreePath, "worker")
		if _, err := os.Stat(filepath.Join(worker, ".git")); os.IsNotExist(err) {
			gitTest(t, env, filepath.Join(workspace, "worker"), "worktree", "add", "--detach", worker, "HEAD")
		} else if err != nil {
			return err
		}
	}

	baseline := legacyWorkspaceRootBaseline{Version: 1, Files: map[string]legacyWorkspaceFileState{}, Members: map[string]string{}}
	err = filepath.WalkDir(task.WorktreePath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(task.WorktreePath, path)
		if err != nil || relative == "." {
			return err
		}
		if entry.IsDir() {
			if members[relative] || entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() || relative == "brief.md" || relative == "launch.md" || relative == "report.md" {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		hash := sha256.Sum256(contents)
		baseline.Files[relative] = legacyWorkspaceFileState{Mode: uint32(info.Mode() & (fs.ModeType | 0o777)), SHA256: hex.EncodeToString(hash[:])}
		return nil
	})
	if err != nil {
		return err
	}
	for _, repo := range repos {
		worktree := filepath.Join(task.WorktreePath, repo.Path)
		if _, err := os.Stat(filepath.Join(worktree, ".git")); os.IsNotExist(err) {
			baseline.Members[repo.Path] = ""
			continue
		} else if err != nil {
			return err
		}
		baseline.Members[repo.Path] = strings.TrimSpace(gitTest(t, env, worktree, "rev-parse", "--verify", "HEAD"))
	}
	data, err := json.Marshal(baseline)
	if err != nil {
		return err
	}
	baselinePath := filepath.Join(home, "posse", "projects", project.Name, ".workspace-root-baselines", fmt.Sprintf("t%d.json", task.Seq))
	if err := os.MkdirAll(filepath.Dir(baselinePath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(baselinePath, data, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(home, "scout-ready"), []byte("ready\n"), 0o600)
}

func mustReadDir(t *testing.T, path string) []fs.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
