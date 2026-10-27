package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/atomicfile"
	setupassets "github.com/thanhbinh1905/posse/internal/setup"
)

// Skills lists the workflow skills each role loads on top of the platform
// skills. Each entry is a bundled skill name or a directory holding SKILL.md.
type Skills struct {
	Lead     []string `toml:"lead" json:"lead"`
	Change   []string `toml:"change" json:"change"`
	Review   []string `toml:"review" json:"review"`
	Research []string `toml:"research" json:"research"`
}

var SkillRoles = []string{"lead", "change", "review", "research"}

// SkillSource is one resolved [skills] entry; Path is empty for a bundled skill.
type SkillSource struct {
	Name string
	Path string
}

var skillNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// IsSkillName reports whether name can name a frozen skill directory.
func IsSkillName(name string) bool {
	return skillNamePattern.MatchString(name)
}

func (s Skills) Role(role string) []string {
	switch role {
	case "lead":
		return s.Lead
	case "change":
		return s.Change
	case "review":
		return s.Review
	case "research":
		return s.Research
	}
	return nil
}

// RoleSkills resolves the configured workflow skills of role in order,
// dropping repeated entries and the platform skills that always load.
func (c Config) RoleSkills(role string) ([]SkillSource, error) {
	var sources []SkillSource
	seen := map[string]string{}
	for _, name := range setupassets.PlatformSkillNames() {
		seen[name] = name
	}
	for _, entry := range c.Skills.Role(role) {
		source, err := ResolveSkill(entry)
		if err != nil {
			return nil, err
		}
		if previous, found := seen[source.Name]; found {
			if previous == entry || source.Path == "" {
				continue
			}
			return nil, fmt.Errorf("%q: skill name %s is already selected by %q", entry, source.Name, previous)
		}
		seen[source.Name] = entry
		sources = append(sources, source)
	}
	return sources, nil
}

// ResolveSkill turns a [skills] entry into a bundled skill or an absolute
// skill directory named by its SKILL.md frontmatter.
func ResolveSkill(entry string) (SkillSource, error) {
	if !strings.Contains(entry, "/") && !strings.HasPrefix(entry, "~") {
		if !setupassets.IsSkill(entry) {
			return SkillSource{}, fmt.Errorf("%q is not a bundled skill; use a bundled name or an absolute or ~/ path to a skill directory", entry)
		}
		return SkillSource{Name: entry}, nil
	}
	path := entry
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return SkillSource{}, fmt.Errorf("%q: %w", entry, err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	if !filepath.IsAbs(path) {
		return SkillSource{}, fmt.Errorf("%q must be a bundled skill name or an absolute or ~/ path", entry)
	}
	path = filepath.Clean(path)
	contents, err := os.ReadFile(filepath.Join(path, "SKILL.md"))
	if err != nil {
		return SkillSource{}, fmt.Errorf("%q is not a directory holding a readable SKILL.md: %w", entry, err)
	}
	name := skillFrontmatterName(string(contents))
	if name == "" {
		name = filepath.Base(path)
	}
	if !IsSkillName(name) {
		return SkillSource{}, fmt.Errorf("%q has skill name %q, which cannot name a skill directory", entry, name)
	}
	if setupassets.IsSkill(name) {
		return SkillSource{}, fmt.Errorf("%q is named %s, which is a bundled skill", entry, name)
	}
	return SkillSource{Name: name, Path: path}, nil
}

func skillFrontmatterName(contents string) string {
	lines := strings.Split(strings.ReplaceAll(contents, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			break
		}
		if value, found := strings.CutPrefix(line, "name:"); found {
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return ""
}

func (c Config) skillIssues(file string) []error {
	var issues []error
	for _, role := range SkillRoles {
		if _, err := c.RoleSkills(role); err != nil {
			issues = append(issues, &InvalidError{File: file, Key: "skills." + role, Reason: err.Error()})
		}
	}
	return issues
}

// WriteRecommendedSkills appends the embedded recommended [skills] block to
// the config at path unless the file already has one, keeping its contents.
func WriteRecommendedSkills(path string) (bool, error) {
	values, original, err := ReadFile(path)
	if err != nil {
		return false, err
	}
	if _, found := values["skills"]; found {
		return false, nil
	}
	var block strings.Builder
	if len(original) > 0 {
		if !bytes.HasSuffix(original, []byte("\n")) {
			block.WriteString("\n")
		}
		block.WriteString("\n")
	}
	block.WriteString("# Workflow skills loaded on top of the platform skills, per role.\n[skills]\n")
	recommended := setupassets.RecommendedSkills()
	for _, role := range SkillRoles {
		block.WriteString(role + " = [\n")
		for _, name := range recommended[role] {
			block.WriteString("  " + strconv.Quote(name) + ",\n")
		}
		block.WriteString("]\n")
	}
	if err := atomicfile.Write(path, append(original, block.String()...), filePerm(path)); err != nil {
		return false, err
	}
	return true, nil
}
