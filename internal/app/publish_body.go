package app

import (
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
)

const (
	publishBodyStart = "<!-- posse:publish:start -->"
	publishBodyEnd   = "<!-- posse:publish:end -->"
)

var publishBodySections = []string{
	"## Summary",
	"## Issue Link",
	"## Changes",
	"## Verification",
	"## Proof",
	"## Risk And Rollback",
	"## Documentation",
}

func managedPublishBody(body string) string {
	return publishBodyStart + "\n" + strings.TrimRight(body, "\n") + "\n" + publishBodyEnd
}

// refreshPublishBody replaces only the region Posse owns. Legacy templates have
// no ownership markers, so adopting one requires the Rider's explicit --refresh.
func refreshPublishBody(existing, next string, refreshLegacy bool) (string, error) {
	starts, ends := strings.Count(existing, publishBodyStart), strings.Count(existing, publishBodyEnd)
	if starts == 1 && ends == 1 {
		start := strings.Index(existing, publishBodyStart)
		end := strings.Index(existing, publishBodyEnd)
		if end > start && markerIsWholeLine(existing, start, publishBodyStart) && markerIsWholeLine(existing, end, publishBodyEnd) {
			return existing[:start] + managedPublishBody(next) + existing[end+len(publishBodyEnd):], nil
		}
	}
	if starts != 0 || ends != 0 {
		return "", legacyPublishConflict("description contains incomplete Posse ownership markers")
	}
	if hasLegacyPublishSections(existing) {
		if !refreshLegacy {
			return "", legacyPublishConflict("description uses the unmarked Posse template")
		}
		start, end, ok := legacyPublishBodyRange(existing)
		if !ok {
			return "", legacyPublishConflict("the legacy Posse template was edited and cannot be separated safely from human text")
		}
		return existing[:start] + managedPublishBody(next) + existing[end:], nil
	}
	if existing == "" {
		return managedPublishBody(next), nil
	}
	return "", legacyPublishConflict("description has no Posse ownership markers and is not a recognized legacy template")
}

func legacyPublishBodyRange(body string) (int, int, bool) {
	positions := make([]int, len(publishBodySections))
	previous := -1
	for i, heading := range publishBodySections {
		if strings.Count(body, heading) != 1 {
			return 0, 0, false
		}
		position := strings.Index(body, heading)
		if position <= previous || (position > 0 && body[position-1] != '\n') {
			return 0, 0, false
		}
		positions[i] = position
		previous = position
	}
	documentationEnd := strings.Index(body[positions[len(positions)-1]:], documentationChecklist)
	if documentationEnd < 0 {
		return 0, 0, false
	}
	start := positions[0]
	end := positions[len(positions)-1] + documentationEnd + len(documentationChecklist)
	lines := strings.Split(body[start:end], "\n")
	for i, line := range lines {
		if isMarkdownSectionHeading(line) {
			known := false
			for _, heading := range publishBodySections {
				if strings.TrimSpace(line) == heading {
					known = true
					break
				}
			}
			if !known {
				return 0, 0, false
			}
		}
		if i > 0 && strings.TrimSpace(lines[i-1]) != "" && isSetextHeadingUnderline(line) {
			return 0, 0, false
		}
	}
	return start, end, true
}

func isMarkdownSectionHeading(line string) bool {
	line = strings.TrimSpace(line)
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	return level > 0 && level <= 6 && (level == len(line) || line[level] == ' ' || line[level] == '\t')
}

func isSetextHeadingUnderline(line string) bool {
	line = strings.TrimSpace(line)
	if len(line) < 3 || (line[0] != '-' && line[0] != '=') {
		return false
	}
	for i := 1; i < len(line); i++ {
		if line[i] != line[0] {
			return false
		}
	}
	return true
}

func markerIsWholeLine(value string, start int, marker string) bool {
	end := start + len(marker)
	return (start == 0 || value[start-1] == '\n') && (end == len(value) || value[end] == '\n')
}

func hasLegacyPublishSections(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		for _, heading := range publishBodySections {
			if strings.TrimSpace(line) == heading {
				return true
			}
		}
	}
	return false
}

func legacyPublishConflict(reason string) error {
	help := "Inspect the pull request description, resolve the ownership conflict, then re-run `posse publish`"
	if reason == "description uses the unmarked Posse template" {
		help = "Re-run `posse publish --refresh \"<summary>\"` to adopt the legacy Posse template"
	}
	return axi.Failure(
		"pr_body_conflict",
		"could not safely refresh the existing pull request description: "+reason,
		false,
		help,
	)
}
