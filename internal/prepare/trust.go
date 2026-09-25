package prepare

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/thanhbinh1905/posse/internal/execgroup"
)

var errClaudeStoreChanged = errors.New("config changed during Claude trust registration")

func ClaudeTrust(worktreePath, projectRoot string) error {
	if err := validateLinkedWorktree(worktreePath, projectRoot); err != nil {
		return fmt.Errorf("refuse Claude trust registration: %w", err)
	}
	return registerClaudeTrust(worktreePath, projectRoot)
}

// ClaudeTrustWorkspace trusts a workspace Mount folder like the workspace root it
// mirrors. members maps each member worktree in the Mount to its member checkout.
func ClaudeTrustWorkspace(mountPath, workspaceRoot string, members map[string]string) error {
	for worktree, memberRoot := range members {
		if err := validateLinkedWorktree(worktree, memberRoot); err != nil {
			return fmt.Errorf("refuse Claude trust registration for %s: %w", worktree, err)
		}
	}
	if resolvedMount, err := filepath.EvalSymlinks(mountPath); err != nil {
		return err
	} else if resolvedRoot, err := filepath.EvalSymlinks(workspaceRoot); err != nil {
		return err
	} else if filepath.Clean(resolvedMount) == filepath.Clean(resolvedRoot) {
		return fmt.Errorf("refuse Claude trust registration: the Mount cannot be the workspace root")
	}
	return registerClaudeTrust(mountPath, workspaceRoot)
}

func registerClaudeTrust(worktreePath, projectRoot string) error {
	storePath, err := claudeStorePath()
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 3; attempt++ {
		original, root, mode, err := readClaudeStoreFile(storePath)
		if err != nil {
			return err
		}
		projects := map[string]map[string]json.RawMessage{}
		if raw, exists := root["projects"]; exists {
			if err := json.Unmarshal(raw, &projects); err != nil {
				return fmt.Errorf("decode Claude project trust entries: %w", err)
			}
			if projects == nil {
				projects = map[string]map[string]json.RawMessage{}
			}
		}
		primary := projects[projectRoot]
		if primary == nil {
			primary = map[string]json.RawMessage{}
		}
		worktree := projects[worktreePath]
		if worktree == nil {
			worktree = map[string]json.RawMessage{}
		}
		changed := setJSONIfAbsent(primary, "hasTrustDialogAccepted", "true")
		changed = setJSONIfAbsent(worktree, "hasTrustDialogAccepted", "true") || changed
		if isTrue(primary["hasClaudeMdExternalIncludesApproved"]) {
			for _, key := range []string{"hasClaudeMdExternalIncludesApproved", "hasClaudeMdExternalIncludesWarningShown"} {
				if value, exists := primary[key]; exists {
					changed = setJSONIfAbsent(worktree, key, string(value)) || changed
				}
			}
		}
		if !changed {
			return nil
		}
		projects[projectRoot] = primary
		projects[worktreePath] = worktree
		encodedProjects, err := json.Marshal(projects)
		if err != nil {
			return err
		}
		root["projects"] = encodedProjects
		contents, err := json.MarshalIndent(root, "", "  ")
		if err != nil {
			return err
		}
		if err := atomicWriteIfUnchanged(storePath, append(contents, '\n'), mode, original); err != nil {
			if errors.Is(err, errClaudeStoreChanged) {
				continue
			}
			return err
		}
		_, after, _, err := readClaudeStoreFile(storePath)
		if err != nil {
			return err
		}
		if sameJSONMap(root, after) {
			return nil
		}
	}
	return fmt.Errorf("config changed during Claude trust registration after 3 attempts")
}

func setJSONIfAbsent(values map[string]json.RawMessage, key, value string) bool {
	if _, exists := values[key]; exists {
		return false
	}
	values[key] = json.RawMessage(value)
	return true
}

func CodexTrust(worktreePath string) error {
	configHome := os.Getenv("CODEX_HOME")
	if configHome == "" {
		current, err := user.Current()
		if err != nil {
			return err
		}
		configHome = filepath.Join(current.HomeDir, ".codex")
	}
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		return err
	}
	configPath := filepath.Join(configHome, "config.toml")
	if err := rejectNonregular(configPath); err != nil {
		return err
	}
	contents, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	config := map[string]any{}
	if len(contents) > 0 {
		if _, err := toml.Decode(string(contents), &config); err != nil {
			return fmt.Errorf("decode Codex config: %w", err)
		}
	}
	projects, _ := config["projects"].(map[string]any)
	if _, exists := projects[worktreePath]; exists {
		return nil
	}
	quotedPath, err := json.Marshal(worktreePath)
	if err != nil {
		return err
	}
	var block strings.Builder
	if len(contents) > 0 {
		block.Write(contents)
		if contents[len(contents)-1] != '\n' {
			block.WriteByte('\n')
		}
		if !bytes.HasSuffix(contents, []byte("\n\n")) {
			block.WriteByte('\n')
		}
	}
	fmt.Fprintf(&block, "[projects.%s]\ntrust_level = \"trusted\"\n", quotedPath)
	mode := fs.FileMode(0o600)
	if info, err := os.Stat(configPath); err == nil {
		mode = info.Mode().Perm()
	}
	return atomicWrite(configPath, []byte(block.String()), mode)
}

func validateLinkedWorktree(worktreePath, projectRoot string) error {
	worktreeRoot, err := gitPath(worktreePath, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("worktree is not a Git checkout: %w", err)
	}
	projectTop, err := gitPath(projectRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("project is not a Git checkout: %w", err)
	}
	worktreeRoot, err = filepath.EvalSymlinks(worktreeRoot)
	if err != nil {
		return err
	}
	projectTop, err = filepath.EvalSymlinks(projectTop)
	if err != nil {
		return err
	}
	requestedRoot, err := filepath.EvalSymlinks(worktreePath)
	if err != nil {
		return err
	}
	if filepath.Clean(worktreeRoot) != filepath.Clean(requestedRoot) {
		return fmt.Errorf("worktree path must be its own Git top-level directory")
	}
	if filepath.Clean(worktreeRoot) == filepath.Clean(projectTop) {
		return fmt.Errorf("worktree cannot be the primary Project checkout")
	}
	worktreeCommon, err := gitPath(worktreeRoot, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	projectCommon, err := gitPath(projectTop, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(worktreeCommon) {
		worktreeCommon = filepath.Join(worktreeRoot, worktreeCommon)
	}
	if !filepath.IsAbs(projectCommon) {
		projectCommon = filepath.Join(projectTop, projectCommon)
	}
	worktreeCommon, err = filepath.EvalSymlinks(worktreeCommon)
	if err != nil {
		return err
	}
	projectCommon, err = filepath.EvalSymlinks(projectCommon)
	if err != nil {
		return err
	}
	if filepath.Clean(worktreeCommon) != filepath.Clean(projectCommon) {
		return fmt.Errorf("worktree and Project do not share a Git common directory")
	}
	return nil
}

func gitPath(root string, args ...string) (string, error) {
	commandArgs := append([]string{"-C", root}, args...)
	output, err := execgroup.Command("git", commandArgs...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func claudeStorePath() (string, error) {
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if configDir == "" {
		current, err := user.Current()
		if err != nil {
			return "", err
		}
		configDir = current.HomeDir
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(configDir, ".claude.json"), nil
}

func readClaudeStoreFile(path string) ([]byte, map[string]json.RawMessage, fs.FileMode, error) {
	if err := rejectNonregular(path); err != nil {
		return nil, nil, 0, err
	}
	contents, err := os.ReadFile(path)
	mode := fs.FileMode(0o600)
	if os.IsNotExist(err) {
		contents = nil
	} else if err != nil {
		return nil, nil, 0, err
	} else if info, statErr := os.Stat(path); statErr != nil {
		return nil, nil, 0, statErr
	} else {
		mode = info.Mode().Perm()
	}
	var root map[string]json.RawMessage
	if len(contents) > 0 {
		if err := json.Unmarshal(contents, &root); err != nil {
			return nil, nil, 0, fmt.Errorf("decode Claude config: %w", err)
		}
	}
	if root == nil {
		root = map[string]json.RawMessage{}
	}
	return contents, root, mode, nil
}

func sameJSONMap(left, right map[string]json.RawMessage) bool {
	leftBytes, leftErr := json.Marshal(left)
	rightBytes, rightErr := json.Marshal(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	var leftValue, rightValue any
	if json.Unmarshal(leftBytes, &leftValue) != nil || json.Unmarshal(rightBytes, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func rejectNonregular(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refuse to modify non-regular config file %s", path)
	}
	return nil
}

func atomicWrite(path string, contents []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(directory, ".posse-prepare-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(contents); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func atomicWriteIfUnchanged(path string, contents []byte, mode os.FileMode, original []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(directory, ".posse-prepare-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(contents); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	current, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		current = nil
	} else if err != nil {
		return err
	}
	if !bytes.Equal(current, original) {
		return errClaudeStoreChanged
	}
	return os.Rename(tempPath, path)
}

func isTrue(value json.RawMessage) bool { return string(value) == "true" }
