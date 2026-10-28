package setup

import (
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

//go:embed assets/herdr-plugin.toml assets/pi-worker-guard.ts assets/lead-guidance.md skills
var files embed.FS

func PluginManifest(binary, version string) ([]byte, error) {
	template, err := files.ReadFile("assets/herdr-plugin.toml")
	if err != nil {
		return nil, err
	}
	contents := strings.ReplaceAll(string(template), "{{POSSE_BINARY}}", strconv.Quote(binary))
	contents = strings.ReplaceAll(contents, "{{VERSION}}", strconv.Quote(version))
	if strings.Contains(contents, "{{") {
		return nil, fmt.Errorf("plugin template has unresolved placeholders")
	}
	return []byte(contents), nil
}

// PiWorkerGuard is the pi extension that runs `posse _guard` before every bash
// tool call.
func PiWorkerGuard(binary string) ([]byte, error) {
	template, err := files.ReadFile("assets/pi-worker-guard.ts")
	if err != nil {
		return nil, err
	}
	contents := strings.ReplaceAll(string(template), "{{POSSE_BINARY}}", strconv.Quote(binary))
	if strings.Contains(contents, "{{") {
		return nil, fmt.Errorf("pi guard template has unresolved placeholders")
	}
	return []byte(contents), nil
}

func LeadGuidance() ([]byte, error) {
	return files.ReadFile("assets/lead-guidance.md")
}

var skillNames = []string{"posse", "posse-setup", "posse-down", "posse-update", "posse-whats-next", "posse-catch-up", "posse-enlighten-me", "posse-research", "posse-brainstorming", "posse-sharpen", "posse-spec", "posse-arena", "posse-tickets", "posse-triage", "posse-build", "posse-board", "posse-tdd", "posse-deslop", "posse-no-comments", "posse-technical-writing", "posse-unslop", "posse-review", "posse-diagnose", "posse-better-each-day"}

func SkillNames() []string {
	return append([]string(nil), skillNames...)
}

var platformSkillNames = []string{"posse", "posse-setup", "posse-down", "posse-update", "posse-better-each-day"}

// PlatformSkillNames are the embedded skills every Posse session loads; the
// rest are workflow skills loaded only through the [skills] config.
func PlatformSkillNames() []string {
	return append([]string(nil), platformSkillNames...)
}

// IntakeSkill is a Lead skill the User's plain words trigger.
type IntakeSkill struct {
	Name    string
	Trigger string
}

var intakeSkills = []IntakeSkill{
	{"posse-whats-next", "what's next"},
	{"posse-catch-up", "catch up"},
	{"posse-enlighten-me", "explain"},
	{"posse-research", "research"},
	{"posse-brainstorming", "brainstorm"},
	{"posse-sharpen", "lock an idea"},
	{"posse-spec", "write a spec"},
	{"posse-arena", "compare designs for a large one-way door"},
	{"posse-tickets", "break a spec into tickets"},
	{"posse-triage", "triage open issues"},
	{"posse-build", "build approved tickets"},
	{"posse-board", "open the board"},
	{"posse-diagnose", "something broken"},
	{"posse-better-each-day", "what should improve"},
}

// IntakeSkills lists the embedded Intake skills in routing order.
func IntakeSkills() []IntakeSkill {
	return append([]IntakeSkill(nil), intakeSkills...)
}

// RecommendedSkills is the embedded workflow skill set for each [skills] role.
func RecommendedSkills() map[string][]string {
	return map[string][]string{
		"lead": {"posse-whats-next", "posse-catch-up", "posse-enlighten-me", "posse-research",
			"posse-brainstorming", "posse-sharpen", "posse-spec", "posse-tickets",
			"posse-build", "posse-board", "posse-arena", "posse-triage", "posse-diagnose",
			"posse-technical-writing", "posse-unslop"},
		"change":   {"posse-tdd", "posse-deslop", "posse-no-comments", "posse-technical-writing", "posse-unslop"},
		"review":   {"posse-review", "posse-unslop"},
		"research": {"posse-research", "posse-technical-writing", "posse-unslop"},
	}
}

// SkillSummary is the first sentence of an embedded skill's description.
func SkillSummary(name string) string {
	contents, err := Skill(name)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(contents), "\n") {
		value, found := strings.CutPrefix(line, "description:")
		if !found {
			continue
		}
		description, err := strconv.Unquote(strings.TrimSpace(value))
		if err != nil {
			description = strings.Trim(strings.TrimSpace(value), `"'`)
		}
		sentence, _, _ := strings.Cut(description, ". ")
		return strings.TrimSuffix(sentence, ".")
	}
	return ""
}

func IsSkill(name string) bool {
	for _, skill := range skillNames {
		if name == skill {
			return true
		}
	}
	return false
}

func Skill(name string) ([]byte, error) {
	if !IsSkill(name) {
		return nil, fmt.Errorf("unknown embedded skill %q", name)
	}
	return files.ReadFile("skills/" + name + "/SKILL.md")
}

// SkillAsset is a file a skill ships beside its SKILL.md; Path is relative to
// the skill directory and slash-separated.
type SkillAsset struct {
	Path string
	Data []byte
}

func SkillAssets(name string) ([]SkillAsset, error) {
	if !IsSkill(name) {
		return nil, fmt.Errorf("unknown embedded skill %q", name)
	}
	directory, err := fs.Sub(files, "skills/"+name)
	if err != nil {
		return nil, err
	}
	return ReadSkillAssets(directory)
}

func ReadSkillAssets(directory fs.FS) ([]SkillAsset, error) {
	var assets []SkillAsset
	err := fs.WalkDir(directory, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != "." && strings.HasPrefix(entry.Name(), ".") {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || path == "SKILL.md" {
			return nil
		}
		data, err := fs.ReadFile(directory, path)
		if err != nil {
			return err
		}
		assets = append(assets, SkillAsset{Path: path, Data: data})
		return nil
	})
	return assets, err
}
