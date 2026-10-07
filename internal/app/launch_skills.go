package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	setupassets "github.com/thanhbinh1905/posse/internal/setup"
)

type posseLaunchSkills struct {
	ClaudePluginDir   string
	PiArgs            []string
	CodexInstructions string
}

// freezePosseLaunchSkills writes the embedded skills into a content-addressed
// snapshot under POSSE_HOME. Harnesses receive only this exact snapshot, never
// the User's global skill directories or a Project/workspace path.
func freezePosseLaunchSkills(home, project, projectRoot string) (posseLaunchSkills, error) {
	type skillFile struct {
		name string
		data []byte
	}
	files := make([]skillFile, 0, 2)
	var digestInput bytes.Buffer
	for _, name := range []string{"posse", "posse-setup"} {
		data, err := setupassets.Skill(name)
		if err != nil {
			return posseLaunchSkills{}, err
		}
		files = append(files, skillFile{name: name, data: data})
		fmt.Fprintf(&digestInput, "%s\x00%d\x00", name, len(data))
		digestInput.Write(data)
	}
	digest := sha256.Sum256(digestInput.Bytes())
	homePath, err := filepath.Abs(home)
	if err != nil {
		return posseLaunchSkills{}, err
	}
	if err := os.MkdirAll(homePath, 0o700); err != nil {
		return posseLaunchSkills{}, err
	}
	realHome, err := filepath.EvalSymlinks(homePath)
	if err != nil {
		return posseLaunchSkills{}, err
	}
	root := filepath.Join(realHome, "projects", project, "launch-skills", hex.EncodeToString(digest[:]))
	if !pathWithin(realHome, root) {
		return posseLaunchSkills{}, fmt.Errorf("posse skill snapshot path escapes POSSE_HOME")
	}
	if projectRoot != "" {
		realProject, projectErr := filepath.EvalSymlinks(projectRoot)
		if projectErr != nil {
			return posseLaunchSkills{}, fmt.Errorf("resolve Project path before writing Posse skills: %w", projectErr)
		}
		if pathWithin(realProject, root) {
			return posseLaunchSkills{}, fmt.Errorf("posse skill snapshots must stay outside Project %s", projectRoot)
		}
	}
	if err := ensureLaunchSkillDirectories(realHome, root); err != nil {
		return posseLaunchSkills{}, err
	}
	for _, file := range files {
		if err := writeFrozenSkill(root, filepath.Join(root, "skills", file.name, "SKILL.md"), file.data); err != nil {
			return posseLaunchSkills{}, err
		}
	}
	pluginManifest, err := json.MarshalIndent(map[string]string{
		"name":        "posse-launch-skills",
		"version":     "1.0.0",
		"description": "Posse skills for sessions launched by Posse",
	}, "", "  ")
	if err != nil {
		return posseLaunchSkills{}, err
	}
	pluginManifest = append(pluginManifest, '\n')
	if err := writeFrozenSkill(root, filepath.Join(root, ".claude-plugin", "plugin.json"), pluginManifest); err != nil {
		return posseLaunchSkills{}, err
	}

	var skillPaths []string
	for _, file := range files {
		skillPaths = append(skillPaths, filepath.Join(root, "skills", file.name, "SKILL.md"))
	}
	codexInstructions := "Read these frozen Posse skill files when relevant: " + strings.Join(skillPaths, "; ") + "."

	piArgs := make([]string, 0, len(files)*2)
	for _, file := range files {
		piArgs = append(piArgs, "--skill", filepath.Join(root, "skills", file.name, "SKILL.md"))
	}
	return posseLaunchSkills{
		ClaudePluginDir:   root,
		PiArgs:            piArgs,
		CodexInstructions: codexInstructions,
	}, nil
}

func ensureLaunchSkillDirectories(base, target string) error {
	relative, err := filepath.Rel(base, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("posse skill directory escapes its managed root: %s", target)
	}
	current := base
	for _, component := range strings.Split(relative, string(os.PathSeparator)) {
		if component == "." || component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if mkdirErr := os.Mkdir(current, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
				return mkdirErr
			}
			info, statErr = os.Lstat(current)
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("posse skill directory is not a managed directory: %s", current)
		}
	}
	return nil
}

func writeFrozenSkill(root, path string, contents []byte) error {
	if !pathWithin(root, path) {
		return fmt.Errorf("frozen posse skill path escapes its snapshot: %s", path)
	}
	if err := ensureLaunchSkillDirectories(root, filepath.Dir(path)); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("frozen Posse skill path is not a regular file: %s", path)
		}
		current, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(current, contents) {
			return fmt.Errorf("frozen Posse skill snapshot was modified: %s", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return writeFrozenSkill(root, path, contents)
	}
	if err != nil {
		return err
	}
	_, writeErr := file.Write(contents)
	closeErr := file.Close()
	if writeErr != nil {
		_ = os.Remove(path)
		return writeErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return closeErr
	}
	return nil
}

func posseSkillLaunchArgs(kind string, skills posseLaunchSkills) ([]string, error) {
	switch kind {
	case "claude":
		return []string{"--plugin-dir", skills.ClaudePluginDir}, nil
	case "codex":
		argument, err := codexSkillConfigArgument(skills.CodexInstructions)
		if err != nil {
			return nil, err
		}
		return []string{"-c", argument}, nil
	case "pi":
		return append([]string(nil), skills.PiArgs...), nil
	default:
		return nil, nil
	}
}

func codexSkillConfigArgument(instructions string) (string, error) {
	encoded, err := json.Marshal(instructions)
	if err != nil {
		return "", err
	}
	return "developer_instructions=" + string(encoded), nil
}
