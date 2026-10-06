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
	if existing == "" {
		return managedPublishBody(next), nil
	}
	if hasLegacyPublishSections(existing) {
		if !refreshLegacy {
			return "", legacyPublishConflict("description uses the unmarked Posse template")
		}
		return existing + "\n\n" + managedPublishBody(next), nil
	}
	return "", legacyPublishConflict("description has no Posse ownership markers and is not a recognized legacy template")
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
		help = "Re-run `posse publish --refresh \"<summary>\"` to preserve the legacy description and append a managed Posse section"
	}
	return axi.Failure(
		"pr_body_conflict",
		"could not safely refresh the existing pull request description: "+reason,
		false,
		help,
	)
}
