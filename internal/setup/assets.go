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

var skillNames = []string{"posse", "posse-setup", "posse-down", "posse-update", "posse-whats-next", "posse-catch-up", "posse-enlighten-me", "posse-research", "posse-brainstorming", "posse-sharpen", "posse-spec", "posse-tickets", "posse-diagnose", "posse-better-each-day"}

func SkillNames() []string {
	return append([]string(nil), skillNames...)
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
