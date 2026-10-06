package app

import (
	"strconv"
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
	normalized := strings.ReplaceAll(body, "\r\n", "\n")
	positions := make([]int, len(publishBodySections))
	previous := -1
	for i, heading := range publishBodySections {
		if strings.Count(normalized, heading) != 1 {
			return 0, 0, false
		}
		position := strings.Index(normalized, heading)
		if position <= previous || (position > 0 && normalized[position-1] != '\n') {
			return 0, 0, false
		}
		positions[i] = position
		previous = position
	}
	documentationEnd := strings.Index(normalized[positions[len(positions)-1]:], documentationChecklist)
	if documentationEnd < 0 {
		return 0, 0, false
	}
	start := positions[0]
	end := positions[len(positions)-1] + documentationEnd + len(documentationChecklist)
	if !isLegacyPublishTemplate(normalized[start:end]) {
		return 0, 0, false
	}
	return originalOffsetForNormalizedBody(body, start), originalOffsetForNormalizedBody(body, end), true
}

func originalOffsetForNormalizedBody(body string, offset int) int {
	original := 0
	for normalized := 0; normalized < offset; normalized++ {
		if body[original] == '\r' && original+1 < len(body) && body[original+1] == '\n' {
			original++
		}
		original++
	}
	return original
}

func isLegacyPublishTemplate(body string) bool {
	sections := make([]string, len(publishBodySections))
	positions := make([]int, len(publishBodySections))
	for i, heading := range publishBodySections {
		positions[i] = strings.Index(body, heading)
		if positions[i] < 0 || (i > 0 && positions[i] <= positions[i-1]) {
			return false
		}
	}
	for i := range publishBodySections {
		end := len(body)
		if i+1 < len(positions) {
			end = positions[i+1] - 2
		}
		if end < positions[i] {
			return false
		}
		sections[i] = body[positions[i]:end]
	}

	summary, ok := legacySectionValue(sections[0], publishBodySections[0])
	if !ok || summary == "" {
		return false
	}
	if !validLegacyIssueLinks(sections[1]) || !validLegacyChanges(sections[2]) || !validLegacyChecklist(sections[3]) {
		return false
	}
	proof, ok := legacySectionValue(sections[4], publishBodySections[4])
	if !ok || proof == "" {
		return false
	}
	risk, ok := legacySectionValue(sections[5], publishBodySections[5])
	if !ok || risk == "" {
		return false
	}
	return sections[6] == publishBodySections[6]+"\n\n"+documentationChecklist
}

func legacySectionValue(section, heading string) (string, bool) {
	if !strings.HasPrefix(section, heading) {
		return "", false
	}
	value := strings.TrimPrefix(section, heading)
	if value == "" {
		return "", true
	}
	if !strings.HasPrefix(value, "\n\n") {
		return "", false
	}
	value = strings.TrimPrefix(value, "\n\n")
	return value, value == strings.TrimSpace(value)
}

func validLegacyIssueLinks(section string) bool {
	value, ok := legacySectionValue(section, publishBodySections[1])
	if !ok {
		return false
	}
	if value == "" {
		return true
	}
	for _, line := range strings.Split(value, "\n") {
		if !strings.HasPrefix(line, "Closes #") && !strings.HasPrefix(line, "Refs #") {
			return false
		}
		if !positiveIssueNumber(strings.TrimPrefix(strings.TrimPrefix(line, "Closes #"), "Refs #")) {
			return false
		}
	}
	return true
}

func positiveIssueNumber(value string) bool {
	number, err := strconv.Atoi(value)
	return err == nil && number > 0 && strconv.Itoa(number) == value
}

func validLegacyChanges(section string) bool {
	value, ok := legacySectionValue(section, publishBodySections[2])
	if !ok || value == "" {
		return false
	}
	for _, line := range strings.Split(value, "\n") {
		if !strings.HasPrefix(line, "- ") || strings.TrimSpace(strings.TrimPrefix(line, "- ")) == "" {
			return false
		}
	}
	return true
}

func validLegacyChecklist(section string) bool {
	value, ok := legacySectionValue(section, publishBodySections[3])
	if !ok || value == "" {
		return false
	}
	for _, line := range strings.Split(value, "\n") {
		if !strings.HasPrefix(line, "- [ ] ") && !strings.HasPrefix(line, "- [x] ") && !strings.HasPrefix(line, "- [X] ") {
			return false
		}
		if strings.TrimSpace(line[6:]) == "" {
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
