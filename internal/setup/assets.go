package setup

import (
	"embed"
	"fmt"
	"strconv"
	"strings"
)

//go:embed assets/herdr-plugin.toml assets/pi-worker-guard.ts assets/lead-guidance.md skills/*/SKILL.md
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

var skillNames = []string{"posse", "posse-setup", "posse-down", "posse-update", "posse-whats-next", "posse-catch-up", "posse-enlighten-me", "posse-research", "posse-brainstorming", "posse-sharpen", "posse-spec", "posse-arena", "posse-tickets", "posse-triage", "posse-build", "posse-tdd", "posse-deslop", "posse-no-comments", "posse-technical-writing", "posse-unslop", "posse-review", "posse-diagnose", "posse-better-each-day"}

func SkillNames() []string {
	return append([]string(nil), skillNames...)
}

var platformSkillNames = []string{"posse", "posse-setup", "posse-down", "posse-update"}

// PlatformSkillNames are the embedded skills every Posse session loads; the
// rest are workflow skills loaded only through the [skills] config.
func PlatformSkillNames() []string {
	return append([]string(nil), platformSkillNames...)
}

// RecommendedSkills is the embedded workflow skill set for each [skills] role.
func RecommendedSkills() map[string][]string {
	return map[string][]string{
		"lead": {"posse-whats-next", "posse-catch-up", "posse-enlighten-me", "posse-research",
			"posse-brainstorming", "posse-sharpen", "posse-spec", "posse-tickets",
			"posse-build", "posse-arena", "posse-triage", "posse-diagnose",
			"posse-better-each-day", "posse-technical-writing", "posse-unslop"},
		"change":   {"posse-tdd", "posse-deslop", "posse-no-comments", "posse-technical-writing", "posse-unslop"},
		"review":   {"posse-review", "posse-unslop"},
		"research": {"posse-research", "posse-technical-writing", "posse-unslop"},
	}
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
