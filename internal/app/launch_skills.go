package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	setupassets "github.com/thanhbinh1905/posse/internal/setup"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

type posseLaunchSkills struct {
	ClaudePluginDir   string
	PiArgs            []string
	CodexInstructions string
}

// freezePosseLaunchSkills writes the platform skills plus the role's
// configured workflow skills into a content-addressed snapshot under
// POSSE_HOME. Pi receives these frozen skills plus unrelated User skills from
// its global filesystem discovery roots.
func freezePosseLaunchSkills(home, project, projectRoot string, cfg config.Config, role string) (posseLaunchSkills, error) {
	selected, err := roleSkillInputs(cfg, role)
	if err != nil {
		return posseLaunchSkills{}, err
	}
	return freezePosseLaunchSkillInputs(home, project, projectRoot, selected)
}

// roleSkillInputs reads the platform skills and the role's [skills] entries,
// each followed by the asset files it ships beside its SKILL.md.
func roleSkillInputs(cfg config.Config, role string) ([]taskplan.Input, error) {
	var sources []config.SkillSource
	for _, name := range setupassets.PlatformSkillNames() {
		sources = append(sources, config.SkillSource{Name: name})
	}
	configured, err := cfg.RoleSkills(role)
	if err != nil {
		return nil, axi.Failure("config_invalid", "skills."+role+": "+err.Error(), false)
	}
	var inputs []taskplan.Input
	for _, source := range append(sources, configured...) {
		var data []byte
		var assets []setupassets.SkillAsset
		if source.Path == "" {
			data, err = setupassets.Skill(source.Name)
			if err == nil {
				assets, err = setupassets.SkillAssets(source.Name)
			}
		} else {
			data, err = os.ReadFile(filepath.Join(source.Path, "SKILL.md"))
			if err == nil {
				assets, err = setupassets.ReadSkillAssets(os.DirFS(source.Path))
			}
		}
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, taskplan.Select("skill", source.Name, data))
		for _, asset := range assets {
			inputs = append(inputs, taskplan.Select("skill-asset", source.Name+"/"+asset.Path, asset.Data))
		}
	}
	return inputs, nil
}

func freezePosseLaunchSkillInputs(home, project, projectRoot string, selected []taskplan.Input) (posseLaunchSkills, error) {
	type skillFile struct {
		name string
		data []byte
	}
	var files, assets []skillFile
	var digestInput bytes.Buffer
	for _, input := range selected {
		switch input.Role {
		case "skill":
			if !config.IsSkillName(input.Source) {
				return posseLaunchSkills{}, fmt.Errorf("selected skill name %q cannot name a skill directory", input.Source)
			}
			files = append(files, skillFile{name: input.Source, data: input.Bytes})
		case "skill-asset":
			if !validSkillAssetSource(input.Source) {
				return posseLaunchSkills{}, fmt.Errorf("selected skill asset %q is not a relative path inside a skill directory", input.Source)
			}
			assets = append(assets, skillFile{name: input.Source, data: input.Bytes})
		default:
			continue
		}
		// Skill names have no slash and asset sources do, so entries stay distinct.
		fmt.Fprintf(&digestInput, "%s\x00%d\x00", input.Source, len(input.Bytes))
		digestInput.Write(input.Bytes)
	}
	if len(files) == 0 {
		return posseLaunchSkills{}, fmt.Errorf("admitted plan selects no Posse skills")
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
	for _, asset := range assets {
		if err := writeFrozenSkill(root, filepath.Join(root, "skills", filepath.FromSlash(asset.name)), asset.data); err != nil {
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

	var skillNames []string
	for _, file := range files {
		skillNames = append(skillNames, file.name)
	}
	codexInstructions := "Read the frozen Posse skill file " + filepath.Join(root, "skills", "{name}", "SKILL.md") + " when relevant, where {name} is one of: " + strings.Join(skillNames, ", ") + "."

	// Preserve native discovery unless legacy Posse copies would shadow the frozen files.
	userSkillPaths, hasLegacyPosseSkills := collectPiSkillPaths()
	var piArgs []string
	if hasLegacyPosseSkills {
		piArgs = append(piArgs, "--no-skills")
		for _, path := range userSkillPaths {
			piArgs = append(piArgs, "--skill", path)
		}
	}
	piArgs = append(piArgs, "--skill", filepath.Join(root, "skills"))
	return posseLaunchSkills{
		ClaudePluginDir:   root,
		PiArgs:            piArgs,
		CodexInstructions: codexInstructions,
	}, nil
}

func validSkillAssetSource(source string) bool {
	name, path, found := strings.Cut(source, "/")
	return found && config.IsSkillName(name) && fs.ValidPath(path) && path != "SKILL.md"
}

func collectPiSkillPaths() ([]string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		current, userErr := user.Current()
		if userErr != nil || current.HomeDir == "" {
			return nil, false
		}
		home = current.HomeDir
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return nil, false
	}
	agentDir := os.Getenv("PI_CODING_AGENT_DIR")
	if agentDir == "" {
		agentDir = filepath.Join(home, ".pi", "agent")
	} else if agentDir == "~" {
		agentDir = home
	} else if strings.HasPrefix(agentDir, "~"+string(os.PathSeparator)) {
		agentDir = filepath.Join(home, strings.TrimPrefix(agentDir, "~"+string(os.PathSeparator)))
	}
	if !filepath.IsAbs(agentDir) {
		if absolute, absErr := filepath.Abs(agentDir); absErr == nil {
			agentDir = absolute
		}
	}

	roots := []string{
		filepath.Join(agentDir, "skills"),
		filepath.Join(home, ".agents", "skills"),
	}

	var paths []string
	var hasLegacyPosseSkills bool
	seenRoots := make(map[string]bool)
	seenFiles := make(map[string]bool)
	for _, root := range roots {
		root = filepath.Clean(root)
		if seenRoots[root] {
			continue
		}
		seenRoots[root] = true
		collectPiSkillsFromDir(root, true, seenFiles, &paths, &hasLegacyPosseSkills)
	}
	return paths, hasLegacyPosseSkills
}

func collectPiSkillsFromDir(dir string, includeRootFiles bool, seenFiles map[string]bool, paths *[]string, hasLegacyPosseSkills *bool) {
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return
	}
	if seenFiles[resolvedDir] {
		return
	}
	seenFiles[resolvedDir] = true
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	declaredSkill := filepath.Join(dir, "SKILL.md")
	if info, statErr := os.Stat(declaredSkill); statErr == nil && info.Mode().IsRegular() {
		appendPiSkillPath(declaredSkill, seenFiles, paths, hasLegacyPosseSkills)
		return
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || entry.Name() == "node_modules" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, statErr := os.Stat(path)
		if statErr != nil {
			continue
		}
		if info.IsDir() {
			collectPiSkillsFromDir(path, false, seenFiles, paths, hasLegacyPosseSkills)
		} else if includeRootFiles && info.Mode().IsRegular() && filepath.Ext(entry.Name()) == ".md" {
			appendPiSkillPath(path, seenFiles, paths, hasLegacyPosseSkills)
		}
	}
}

func appendPiSkillPath(path string, seenFiles map[string]bool, paths *[]string, hasLegacyPosseSkills *bool) {
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil || seenFiles[resolvedPath] {
		return
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return
	}
	name, hasDescription := piSkillIdentity(contents, path)
	if !hasDescription {
		return
	}
	if setupassets.IsSkill(name) {
		*hasLegacyPosseSkills = true
		return
	}
	seenFiles[resolvedPath] = true
	*paths = append(*paths, path)
}

func piSkillIdentity(contents []byte, path string) (string, bool) {
	name := filepath.Base(filepath.Dir(path))
	lines := strings.Split(string(contents), "\n")
	if len(lines) == 0 || strings.TrimSpace(strings.TrimSuffix(lines[0], "\r")) != "---" {
		return name, false
	}
	var hasDescription bool
	var closedFrontmatter bool
	for _, rawLine := range lines[1:] {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		if line == "---" {
			closedFrontmatter = true
			break
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "name":
			if value = strings.TrimSpace(value); value != "" {
				name = strings.Trim(value, "\"'")
			}
		case "description":
			description := strings.Trim(strings.TrimSpace(value), "\"'")
			hasDescription = description != ""
		}
	}
	return name, hasDescription && closedFrontmatter
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
	return writeFrozenSkillWith(root, path, contents, nil)
}

func writeFrozenSkillWith(root, path string, contents []byte, writeContents func(*os.File, []byte) error) error {
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
	file, err := os.CreateTemp(filepath.Dir(path), ".posse-skill-*")
	if err != nil {
		return err
	}
	temporaryPath := file.Name()
	defer os.Remove(temporaryPath)
	if writeContents != nil {
		err = writeContents(file, contents)
	} else {
		_, err = file.Write(contents)
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return nil
}

func appendPosseSkillLaunchArgs(kind string, args []string, skills posseLaunchSkills) ([]string, error) {
	switch kind {
	case "claude":
		return append(args, "--plugin-dir", skills.ClaudePluginDir), nil
	case "codex":
		return mergeCodexDeveloperInstructions(args, skills.CodexInstructions)
	case "pi":
		return append(args, skills.PiArgs...), nil
	default:
		return args, nil
	}
}

func mergeCodexDeveloperInstructions(args []string, injected string) ([]string, error) {
	instructions, err := codexConfigDeveloperInstructions()
	if err != nil {
		return nil, err
	}
	return mergeCodexDeveloperInstructionsWithBase(args, injected, instructions)
}

func mergeCodexDeveloperInstructionsWithBase(args []string, injected, instructions string) ([]string, error) {
	var instructionParts []string
	if instructions != "" {
		instructionParts = append(instructionParts, instructions)
	}
	var preserved []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		configValue, attached, configOption := codexConfigOption(arg)
		if !configOption {
			preserved = append(preserved, arg)
			continue
		}
		if !attached {
			if index+1 >= len(args) {
				preserved = append(preserved, arg)
				continue
			}
			configValue = args[index+1]
		}
		value, developerInstruction, parseErr := codexDeveloperInstructionOverride(configValue)
		if parseErr != nil {
			return nil, parseErr
		}
		if developerInstruction {
			if value != "" {
				instructionParts = append(instructionParts, value)
			}
			if !attached {
				index++
			}
			continue
		}
		if attached {
			preserved = append(preserved, arg)
		} else {
			preserved = append(preserved, arg, configValue)
			index++
		}
	}
	if injected != "" {
		instructionParts = append(instructionParts, injected)
	}
	argument, err := codexSkillConfigArgument(strings.Join(instructionParts, "\n\n"))
	if err != nil {
		return nil, err
	}
	return append(preserved, "-c", argument), nil
}

func codexConfigDeveloperInstructions() (string, error) {
	configHome := os.Getenv("CODEX_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		configHome = filepath.Join(home, ".codex")
	}
	path := filepath.Join(configHome, "config.toml")
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read Codex config %s: %w", path, err)
	}
	var config map[string]any
	if _, err := toml.Decode(string(contents), &config); err != nil {
		return "", fmt.Errorf("decode Codex config %s: %w", path, err)
	}
	value, exists := config["developer_instructions"]
	if !exists {
		return "", nil
	}
	instructions, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("codex config %s has non-string developer_instructions", path)
	}
	return instructions, nil
}

func codexConfigOption(argument string) (value string, attached, found bool) {
	for _, option := range []string{"--config=", "--configuration="} {
		if value, found := strings.CutPrefix(argument, option); found {
			return value, true, true
		}
	}
	if argument == "-c" || argument == "--config" || argument == "--configuration" {
		return "", false, true
	}
	if strings.HasPrefix(argument, "-c") && len(argument) > 2 {
		return strings.TrimPrefix(strings.TrimPrefix(argument, "-c"), "="), true, true
	}
	return "", false, false
}

func codexDeveloperInstructionOverride(value string) (string, bool, error) {
	key, raw, found := strings.Cut(strings.TrimSpace(value), "=")
	if !found || strings.TrimSpace(key) != "developer_instructions" {
		return "", false, nil
	}
	var decoded map[string]any
	if _, err := toml.Decode("value = "+strings.TrimSpace(raw), &decoded); err != nil {
		// Codex treats values that are not valid TOML as raw strings.
		return strings.TrimSpace(raw), true, nil
	}
	instructions, ok := decoded["value"].(string)
	if !ok {
		return "", true, fmt.Errorf("codex developer_instructions override must be a string")
	}
	return instructions, true, nil
}

func codexSkillConfigArgument(instructions string) (string, error) {
	encoded, err := json.Marshal(instructions)
	if err != nil {
		return "", err
	}
	return "developer_instructions=" + string(encoded), nil
}
