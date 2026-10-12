package app

import (
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
)

const (
	legacyPublishBodyStart = "<!-- posse:publish:start -->"
	legacyPublishBodyEnd   = "<!-- posse:publish:end -->"
	publishBodyStartPrefix = "<!-- posse:publish:start:"
	publishBodyEndPrefix   = "<!-- posse:publish:end:"
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

func publishBodyMarker(edge, token string) string {
	prefix := publishBodyStartPrefix
	if edge == "end" {
		prefix = publishBodyEndPrefix
	}
	return prefix + token + " -->"
}

func managedPublishBody(body, token string) string {
	return publishBodyMarker("start", token) + "\n" + strings.TrimRight(body, "\n") + "\n" + publishBodyMarker("end", token)
}

// refreshPublishBody replaces only the span paired with this PR's stored token.
// Unrecognized marker-like text is never ownership evidence; legacy bodies are
// adopted only by appending a new tokenized section.
func refreshPublishBody(existing, next, token string, refreshLegacy bool) (string, error) {
	if token == "" {
		return "", legacyPublishConflict("pull request has no stored Posse ownership token")
	}
	startMarker, endMarker := publishBodyMarker("start", token), publishBodyMarker("end", token)
	starts, ends := strings.Count(existing, startMarker), strings.Count(existing, endMarker)
	if starts != 0 || ends != 0 {
		if starts == 1 && ends == 1 {
			start, end := strings.Index(existing, startMarker), strings.Index(existing, endMarker)
			if end > start && markerIsWholeLine(existing, start, startMarker) && markerIsWholeLine(existing, end, endMarker) {
				return existing[:start] + managedPublishBody(next, token) + existing[end+len(endMarker):], nil
			}
		}
		return "", legacyPublishConflict("stored ownership token has incomplete or duplicate markers")
	}
	if existing == "" {
		return managedPublishBody(next, token), nil
	}
	if hasUnownedTokenizedPublishMarkerPair(existing) {
		return existing + "\n\n" + managedPublishBody(next, token), nil
	}
	if hasLegacyPublishSections(existing) {
		if !refreshLegacy {
			return "", legacyPublishConflict("description uses the unmarked Posse template")
		}
		return existing + "\n\n" + managedPublishBody(next, token), nil
	}
	return "", legacyPublishConflict("description has no Posse ownership markers and is not a recognized legacy template")
}

func markerIsWholeLine(value string, start int, marker string) bool {
	end := start + len(marker)
	lineStart := start == 0 || value[start-1] == '\n'
	lineEnd := end == len(value) || value[end] == '\n' || (value[end] == '\r' && end+1 < len(value) && value[end+1] == '\n')
	return lineStart && lineEnd
}

func hasUnownedTokenizedPublishMarkerPair(body string) bool {
	starts := make(map[string]int)
	for index, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if token, ok := publishBodyMarkerToken(line, "start"); ok && token != "legacy" {
			starts[token] = index
			continue
		}
		if token, ok := publishBodyMarkerToken(line, "end"); ok && token != "legacy" {
			if start, found := starts[token]; found && start < index {
				return true
			}
		}
	}
	return false
}

func publishBodyMarkerToken(line, edge string) (string, bool) {
	if edge == "start" && line == legacyPublishBodyStart || edge == "end" && line == legacyPublishBodyEnd {
		return "legacy", true
	}
	prefix := publishBodyStartPrefix
	if edge == "end" {
		prefix = publishBodyEndPrefix
	}
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, " -->") {
		return "", false
	}
	token := strings.TrimSuffix(strings.TrimPrefix(line, prefix), " -->")
	if token == "" || strings.ContainsAny(token, " <>\r\n") {
		return "", false
	}
	return token, true
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

func missingPRBodyMarkerError() error {
	return axi.Failure(
		"pr_body_marker_missing",
		"the existing PR/MR has a tokenized Posse section, but its ownership token is missing from the store",
		true,
		"Run `posse recover --rebuild` to restore the marker from the Task snapshot, then retry `posse publish`",
		"If the Task snapshot does not contain the token, restore a POSSE_HOME backup before retrying; do not edit the PR/MR markers",
	)
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
