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

func preserveOmittedPublishBodyFields(existing, next, token string) string {
	startMarker, endMarker := publishBodyMarker("start", token), publishBodyMarker("end", token)
	if token == "" || strings.Count(existing, startMarker) != 1 || strings.Count(existing, endMarker) != 1 {
		return next
	}
	start, end := strings.Index(existing, startMarker)+len(startMarker), strings.Index(existing, endMarker)
	if end <= start {
		return next
	}
	previous := existing[start:end]
	defaults := map[string]string{
		"Verification":      "- [ ] Not run; reason: not supplied",
		"Proof":             "_No proof supplied._",
		"Risk And Rollback": "Risk: not stated\nRollback: revert this PR",
	}
	for heading, placeholder := range defaults {
		current, found := publishSection(next, heading)
		if !found || current != placeholder {
			continue
		}
		prior, found := publishSection(previous, heading)
		if !found || prior == "" || prior == placeholder {
			continue
		}
		next = replacePublishSection(next, heading, prior)
	}
	return next
}

func publishSection(body, heading string) (string, bool) {
	marker := "## " + heading
	for offset := 0; offset < len(body); {
		end := strings.IndexByte(body[offset:], '\n')
		if end < 0 {
			return "", false
		}
		lineEnd := offset + end
		if body[offset:lineEnd] == marker {
			contentStart := lineEnd + 1
			contentEnd := len(body)
			for pos := contentStart; pos < len(body); {
				nextEnd := strings.IndexByte(body[pos:], '\n')
				if nextEnd < 0 {
					break
				}
				lineEnd := pos + nextEnd
				if publishSectionBoundary(body[pos:lineEnd]) {
					contentEnd = pos
					break
				}
				pos = lineEnd + 1
			}
			return strings.TrimSpace(body[contentStart:contentEnd]), true
		}
		offset = lineEnd + 1
	}
	return "", false
}

func replacePublishSection(body, heading, content string) string {
	marker := "## " + heading
	for offset := 0; offset < len(body); {
		lineEnd := strings.IndexByte(body[offset:], '\n')
		if lineEnd < 0 {
			return body
		}
		lineEnd += offset
		if body[offset:lineEnd] == marker {
			contentStart, contentEnd := lineEnd+1, len(body)
			for pos := contentStart; pos < len(body); {
				nextEnd := strings.IndexByte(body[pos:], '\n')
				if nextEnd < 0 {
					break
				}
				sectionEnd := pos + nextEnd
				if publishSectionBoundary(body[pos:sectionEnd]) {
					contentEnd = pos
					break
				}
				pos = sectionEnd + 1
			}
			return body[:contentStart] + content + "\n\n" + body[contentEnd:]
		}
		offset = lineEnd + 1
	}
	return body
}

func publishSectionBoundary(line string) bool {
	line = strings.TrimSuffix(line, "\r")
	for _, heading := range publishBodySections {
		if line == heading {
			return true
		}
	}
	return line == validationBlockStart || line == validationBlockEnd
}

func markerIsWholeLine(value string, start int, marker string) bool {
	end := start + len(marker)
	lineStart := start == 0 || value[start-1] == '\n'
	lineEnd := end == len(value) || value[end] == '\n' || (value[end] == '\r' && end+1 < len(value) && value[end+1] == '\n')
	return lineStart && lineEnd
}

func hasUnownedTokenizedPublishMarkerPair(body string) bool {
	return len(tokenizedPublishMarkerPairs(body)) > 0
}

// tokenizedPublishMarkerPairs returns, in order, each token that has a start
// marker line followed by its end marker line.
func tokenizedPublishMarkerPairs(body string) []string {
	starts := make(map[string]int)
	var tokens []string
	paired := make(map[string]bool)
	for index, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if token, ok := publishBodyMarkerToken(line, "start"); ok && token != "legacy" {
			starts[token] = index
			continue
		}
		if token, ok := publishBodyMarkerToken(line, "end"); ok && token != "legacy" {
			if start, found := starts[token]; found && start < index && !paired[token] {
				paired[token] = true
				tokens = append(tokens, token)
			}
		}
	}
	return tokens
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
