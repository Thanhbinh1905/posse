package config

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

type vocabularyTextPatch struct {
	start       int
	end         int
	replacement string
}

type tomlKeySegment struct {
	value string
	start int
	end   int
}

// rewriteVocabularyText edits only migrated TOML tokens, then checks the
// decoded result against the schema migration before it can be written.
func rewriteVocabularyText(contents string, expected map[string]any) (string, error) {
	patches := []vocabularyTextPatch{}
	section := []string{}
	for offset := 0; offset < len(contents); {
		lineEnd := strings.IndexByte(contents[offset:], '\n')
		fullEnd := len(contents)
		if lineEnd >= 0 {
			fullEnd = offset + lineEnd
		}
		nextOffset := fullEnd
		if _, nameStart, nameEnd, ok := tableHeaderSpans(contents, offset, fullEnd); ok {
			section = tomlKeyPath(contents[nameStart:nameEnd])
			if target := ReplacementKey(strings.Join(section, ".")); target != "" {
				targetParts := strings.Split(target, ".")
				segment := changedPathSegment(section, targetParts)
				if segment >= 0 && segment < len(section) {
					spans := tomlKeySpans(contents, nameStart, nameEnd)
					if segment < len(spans) {
						patches = append(patches, vocabularyTextPatch{start: spans[segment].start, end: spans[segment].end, replacement: targetParts[segment]})
					}
				}
				section = targetParts
			}
		} else {
			equal := findTOMLCharacter(contents, offset, fullEnd, '=')
			if equal >= 0 {
				keyStart, keyEnd := trimTOMLRange(contents, offset, equal)
				segments := tomlKeySpans(contents, keyStart, keyEnd)
				keyParts := make([]string, 0, len(segments))
				for _, segment := range segments {
					keyParts = append(keyParts, segment.value)
				}
				fullParts := append(append([]string(nil), section...), keyParts...)
				fullPath := strings.Join(fullParts, ".")
				if target := ReplacementKey(fullPath); target != "" {
					targetParts := strings.Split(target, ".")
					segment := changedPathSegment(fullParts, targetParts)
					keySegment := segment - len(section)
					if keySegment >= 0 && keySegment < len(segments) {
						patches = append(patches, vocabularyTextPatch{start: segments[keySegment].start, end: segments[keySegment].end, replacement: targetParts[segment]})
					}
					fullPath = target
				}
				valueStart := equal + 1
				for valueStart < fullEnd && (contents[valueStart] == ' ' || contents[valueStart] == '\t') {
					valueStart++
				}
				valueEnd := tomlExpressionEnd(contents, valueStart)
				if err := rewriteVocabularyValue(contents, valueStart, valueEnd, tomlKeyPath(fullPath), &patches); err != nil {
					return "", err
				}
				nextOffset = valueEnd
			}
		}
		if nextOffset == len(contents) {
			break
		}
		for nextOffset < len(contents) && contents[nextOffset] != '\n' {
			nextOffset++
		}
		if nextOffset < len(contents) {
			nextOffset++
		}
		offset = nextOffset
	}

	sort.Slice(patches, func(i, j int) bool { return patches[i].start > patches[j].start })
	updated := contents
	lastStart := len(contents) + 1
	for _, patch := range patches {
		if patch.start < 0 || patch.end < patch.start || patch.end > len(updated) || patch.end > lastStart {
			return "", fmt.Errorf("overlapping TOML vocabulary migration edits")
		}
		updated = updated[:patch.start] + patch.replacement + updated[patch.end:]
		lastStart = patch.start
	}
	var decoded map[string]any
	if _, err := toml.Decode(updated, &decoded); err != nil {
		return "", err
	}
	if !reflect.DeepEqual(expected, decoded) {
		return "", fmt.Errorf("could not preserve TOML layout while migrating vocabulary")
	}
	return updated, nil
}

func tableHeaderSpans(contents string, start, end int) (string, int, int, bool) {
	for start < end && (contents[start] == ' ' || contents[start] == '\t' || contents[start] == '\r') {
		start++
	}
	openWidth := 0
	if start+1 < end && contents[start:start+2] == "[[" {
		openWidth = 2
	} else if start < end && contents[start] == '[' {
		openWidth = 1
	}
	if openWidth == 0 {
		return "", 0, 0, false
	}
	quote := byte(0)
	escaped := false
	closeStart := -1
	for i := start + openWidth; i < end; i++ {
		ch := contents[i]
		if quote != 0 {
			if quote == '"' && ch == '\\' && !escaped {
				escaped = true
				continue
			}
			if ch == quote && !escaped {
				quote = 0
			}
			escaped = false
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			continue
		}
		if ch == ']' {
			closeStart = i
			break
		}
	}
	if closeStart < 0 || openWidth == 2 && (closeStart+1 >= end || contents[closeStart+1] != ']') {
		return "", 0, 0, false
	}
	closeWidth := openWidth
	suffix := strings.TrimSpace(contents[closeStart+closeWidth : end])
	if suffix != "" && !strings.HasPrefix(suffix, "#") {
		return "", 0, 0, false
	}
	nameStart, nameEnd := start+openWidth, closeStart
	for nameStart < nameEnd && (contents[nameStart] == ' ' || contents[nameStart] == '\t') {
		nameStart++
	}
	for nameEnd > nameStart && (contents[nameEnd-1] == ' ' || contents[nameEnd-1] == '\t') {
		nameEnd--
	}
	if nameStart == nameEnd {
		return "", 0, 0, false
	}
	return contents[nameStart:nameEnd], nameStart, nameEnd, true
}

func tomlKeyPath(raw string) []string {
	segments := tomlKeySpans(raw, 0, len(raw))
	parts := make([]string, 0, len(segments))
	for _, segment := range segments {
		parts = append(parts, segment.value)
	}
	return parts
}

func tomlKeySpans(contents string, start, end int) []tomlKeySegment {
	segments := []tomlKeySegment{}
	partStart := start
	quote := byte(0)
	escaped := false
	for i := start; i <= end; i++ {
		if i == end || contents[i] == '.' && quote == 0 {
			left, right := trimTOMLRange(contents, partStart, i)
			if left < right {
				valueStart, valueEnd := left, right
				if valueEnd-valueStart >= 2 && (contents[valueStart] == '"' && contents[valueEnd-1] == '"' || contents[valueStart] == '\'' && contents[valueEnd-1] == '\'') {
					valueStart++
					valueEnd--
				}
				value := contents[valueStart:valueEnd]
				if left < right && (contents[left] == '"' || contents[left] == '\'') {
					var decoded map[string]any
					if _, err := toml.Decode("value = "+contents[left:right], &decoded); err == nil {
						if text, ok := decoded["value"].(string); ok {
							value = text
						}
					}
				}
				segments = append(segments, tomlKeySegment{value: value, start: valueStart, end: valueEnd})
			}
			partStart = i + 1
			continue
		}
		ch := contents[i]
		if quote != 0 {
			if quote == '"' && ch == '\\' && !escaped {
				escaped = true
				continue
			}
			if ch == quote && !escaped {
				quote = 0
			}
			escaped = false
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
		}
	}
	return segments
}

func changedPathSegment(original, replacement []string) int {
	if len(original) > 0 && len(replacement) > 0 && original[0] != replacement[0] {
		return 0
	}
	if len(original) == 0 || len(replacement) == 0 {
		return -1
	}
	return len(original) - 1
}

func trimTOMLRange(contents string, start, end int) (int, int) {
	for start < end && (contents[start] == ' ' || contents[start] == '\t' || contents[start] == '\r' || contents[start] == '\n') {
		start++
	}
	for end > start && (contents[end-1] == ' ' || contents[end-1] == '\t' || contents[end-1] == '\r' || contents[end-1] == '\n') {
		end--
	}
	return start, end
}

func findTOMLCharacter(contents string, start, end int, wanted byte) int {
	quote := byte(0)
	triple := false
	escaped := false
	for i := start; i < end; i++ {
		ch := contents[i]
		if quote != 0 {
			if triple && i+2 < end && contents[i:i+3] == strings.Repeat(string(quote), 3) && !escaped {
				quote, triple = 0, false
				i += 2
				continue
			}
			if !triple && ch == quote && !escaped {
				quote = 0
			}
			if quote == '"' && ch == '\\' && !escaped {
				escaped = true
			} else {
				escaped = false
			}
			continue
		}
		if ch == '#' {
			return -1
		}
		if ch == wanted {
			return i
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			if i+2 < end && contents[i:i+3] == strings.Repeat(string(ch), 3) {
				triple = true
				i += 2
			}
		}
	}
	return -1
}

func rewriteVocabularyValue(contents string, start, end int, path []string, patches *[]vocabularyTextPatch) error {
	left, right := trimTOMLRange(contents, start, end)
	if left >= right {
		return nil
	}
	switch contents[left] {
	case '{':
		return rewriteVocabularyInlineTable(contents, left, right, path, patches)
	case '[':
		return rewriteVocabularyArray(contents, left, right, path, patches)
	}
	var decoded map[string]any
	if _, err := toml.Decode("value = "+contents[left:right], &decoded); err != nil {
		return err
	}
	value, ok := decoded["value"].(string)
	if !ok {
		return nil
	}
	fullPath := strings.Join(path, ".")
	updated := migrateVocabularyString(fullPath, value)
	if isReviewPolicyPath(fullPath) && value == "local" {
		updated = "quick"
	}
	if updated != value {
		*patches = append(*patches, vocabularyTextPatch{start: left, end: right, replacement: preserveTOMLString(contents[left:right], updated)})
	}
	return nil
}

func rewriteVocabularyInlineTable(contents string, start, end int, path []string, patches *[]vocabularyTextPatch) error {
	cursor := start + 1
	limit := end - 1
	for cursor < limit {
		cursor = skipTOMLTrivia(contents, cursor, limit)
		if cursor < limit && contents[cursor] == ',' {
			cursor++
			continue
		}
		if cursor >= limit {
			break
		}
		equal := findTOMLCharacter(contents, cursor, limit, '=')
		if equal < 0 {
			return fmt.Errorf("invalid inline TOML table during vocabulary migration")
		}
		keyStart, keyEnd := trimTOMLRange(contents, cursor, equal)
		keySegments := tomlKeySpans(contents, keyStart, keyEnd)
		keyParts := make([]string, 0, len(keySegments))
		for _, segment := range keySegments {
			keyParts = append(keyParts, segment.value)
		}
		fullParts := append(append([]string(nil), path...), keyParts...)
		fullPath := strings.Join(fullParts, ".")
		if target := ReplacementKey(fullPath); target != "" {
			targetParts := strings.Split(target, ".")
			segment := changedPathSegment(fullParts, targetParts)
			keySegment := segment - len(path)
			if keySegment >= 0 && keySegment < len(keySegments) {
				*patches = append(*patches, vocabularyTextPatch{start: keySegments[keySegment].start, end: keySegments[keySegment].end, replacement: targetParts[segment]})
			}
			fullPath = target
		}
		valueStart := equal + 1
		for valueStart < limit && isTOMLSpace(contents[valueStart]) {
			valueStart++
		}
		valueEnd := inlineValueEnd(contents, valueStart, limit)
		if err := rewriteVocabularyValue(contents, valueStart, valueEnd, tomlKeyPath(fullPath), patches); err != nil {
			return err
		}
		cursor = skipTOMLTrivia(contents, valueEnd, limit)
		if cursor < limit && contents[cursor] == ',' {
			cursor++
		}
	}
	return nil
}

func rewriteVocabularyArray(contents string, start, end int, path []string, patches *[]vocabularyTextPatch) error {
	cursor := start + 1
	limit := end - 1
	for cursor < limit {
		cursor = skipTOMLTrivia(contents, cursor, limit)
		if cursor < limit && contents[cursor] == ',' {
			cursor++
			continue
		}
		if cursor >= limit {
			break
		}
		valueEnd := inlineValueEnd(contents, cursor, limit)
		if err := rewriteVocabularyValue(contents, cursor, valueEnd, path, patches); err != nil {
			return err
		}
		cursor = skipTOMLTrivia(contents, valueEnd, limit)
		if cursor < limit && contents[cursor] == ',' {
			cursor++
		}
	}
	return nil
}

func skipTOMLTrivia(contents string, start, end int) int {
	for start < end {
		switch contents[start] {
		case ' ', '\t', '\r', '\n':
			start++
		case '#':
			for start < end && contents[start] != '\n' {
				start++
			}
		default:
			return start
		}
	}
	return start
}

func inlineValueEnd(contents string, start, end int) int {
	depth := 0
	quote := byte(0)
	triple := false
	escaped := false
	last, seen := start, false
	for i := start; i < end; i++ {
		ch := contents[i]
		if quote != 0 {
			if triple && i+2 < end && contents[i:i+3] == strings.Repeat(string(quote), 3) && !escaped {
				quote, triple = 0, false
				i += 2
				last, seen = i+1, true
				continue
			}
			if !triple && ch == quote && !escaped {
				quote = 0
			}
			if quote == '"' && ch == '\\' && !escaped {
				escaped = true
			} else {
				escaped = false
			}
			last, seen = i+1, true
			continue
		}
		if ch == ',' && depth == 0 || (ch == '}' || ch == ']') && depth == 0 {
			break
		}
		if ch == '#' && depth == 0 {
			for i < end && contents[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			if i+2 < end && contents[i:i+3] == strings.Repeat(string(ch), 3) {
				triple = true
				i += 2
			}
			last, seen = i+1, true
			continue
		}
		switch ch {
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		}
		if !isTOMLSpace(ch) {
			last, seen = i+1, true
		}
	}
	if !seen {
		return start
	}
	return last
}

func migrateVocabularyString(path, value string) string {
	switch {
	case path == "dispatch.type":
		if replacement := legacyTaskType(value); replacement != "" {
			return replacement
		}
	case strings.HasPrefix(path, "kinds.") && strings.HasSuffix(path, ".notice_delivery") && value == "lookout":
		return "watch"
	case (path == "auto_teardown" || path == "defaults.auto_teardown") && value == "landed":
		return "merged"
	case path == "identity.worker.display_prefix" && value == "rider":
		return "worker"
	}
	return value
}

func preserveTOMLString(literal, value string) string {
	if len(literal) >= 6 && (strings.HasPrefix(literal, "\"\"\"") || strings.HasPrefix(literal, "'''")) {
		delimiter := literal[:3]
		return delimiter + value + delimiter
	}
	if len(literal) >= 2 && (literal[0] == '\'' || literal[0] == '"') {
		return string(literal[0]) + value + string(literal[0])
	}
	return strconv.Quote(value)
}
