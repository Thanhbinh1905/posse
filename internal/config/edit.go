package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/thanhbinh1905/posse/internal/atomicfile"
)

func ReadFile(path string) (map[string]any, []byte, error) {
	contents, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	values := map[string]any{}
	if _, err := toml.Decode(string(contents), &values); err != nil {
		return nil, contents, &InvalidError{File: path, Key: tomlErrorKey(err), Cause: err}
	}
	return values, contents, nil
}

func SetFile(path, key, text string, projectFile bool) (any, error) {
	spec, ok := Lookup(key)
	if !ok {
		return nil, &InvalidError{File: path, Key: key, Reason: "unknown configuration key"}
	}
	value, literal, err := parseSetting(key, spec, text, projectFile)
	if err != nil {
		return nil, &InvalidError{File: path, Key: key, Reason: err.Error()}
	}
	_, original, err := ReadFile(path)
	if err != nil {
		return nil, err
	}
	updated, err := replaceTOMLValue(string(original), key, literal)
	if err != nil {
		return nil, err
	}
	if err := atomicfile.Write(path, []byte(updated), filePerm(path)); err != nil {
		return nil, err
	}
	return value, nil
}

func UnsetFile(path, key string, projectFile bool) error {
	if _, ok := Lookup(key); !ok {
		return &InvalidError{File: path, Key: key, Reason: "unknown configuration key"}
	}
	_, original, err := ReadFile(path)
	if err != nil {
		return err
	}
	updated, found, err := removeTOMLValue(string(original), key)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	return atomicfile.Write(path, []byte(updated), filePerm(path))
}

func parseSetting(key string, spec KeySpec, text string, projectFile bool) (any, string, error) {
	literal := strings.TrimSpace(text)
	if literal == "" {
		return nil, "", fmt.Errorf("value is required")
	}
	if spec.Type == TypeString || spec.Type == TypeDuration {
		if !strings.HasPrefix(literal, "\"") && !strings.HasPrefix(literal, "'") {
			literal = strconv.Quote(literal)
		}
	}
	var document map[string]any
	if _, err := toml.Decode("value = "+literal, &document); err != nil {
		return nil, "", fmt.Errorf("value must be a TOML %s", spec.Type)
	}
	value, found := document["value"]
	if !found || len(document) != 1 {
		return nil, "", fmt.Errorf("value must contain exactly one TOML value")
	}
	if err := ValidateSetting(key, value, projectFile); err != nil {
		return nil, "", err
	}
	encoded, err := encodeSettingValue(spec.Type, value)
	if err != nil {
		return nil, "", err
	}
	return value, encoded, nil
}

func encodeSettingValue(kind ValueType, value any) (string, error) {
	switch kind {
	case TypeString, TypeDuration:
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("value must be %s", kind)
		}
		return strconv.Quote(text), nil
	case TypeInteger:
		integer, ok := integerValue(value)
		if !ok {
			return "", fmt.Errorf("value must be %s", kind)
		}
		return strconv.FormatInt(integer, 10), nil
	case TypeBoolean:
		boolean, ok := value.(bool)
		if !ok {
			return "", fmt.Errorf("value must be %s", kind)
		}
		return strconv.FormatBool(boolean), nil
	case TypeStringArray:
		var values []string
		switch typed := value.(type) {
		case []string:
			values = typed
		case []any:
			for _, item := range typed {
				text, ok := item.(string)
				if !ok {
					return "", fmt.Errorf("value must be %s", kind)
				}
				values = append(values, text)
			}
		default:
			return "", fmt.Errorf("value must be %s", kind)
		}
		encoded := make([]string, len(values))
		for index, text := range values {
			encoded[index] = strconv.Quote(text)
		}
		return "[" + strings.Join(encoded, ", ") + "]", nil
	case TypeTableArray:
		entries, ok := tableEntries(value)
		if !ok {
			return "", fmt.Errorf("value must be %s", kind)
		}
		encoded := make([]string, 0, len(entries))
		for _, entry := range entries {
			fields := []string{}
			for _, key := range []string{"type", "when", "use"} {
				if text, found := entry[key].(string); found {
					fields = append(fields, key+" = "+strconv.Quote(text))
				}
			}
			encoded = append(encoded, "{"+strings.Join(fields, ", ")+"}")
		}
		return "[" + strings.Join(encoded, ", ") + "]", nil
	default:
		return "", fmt.Errorf("unsupported configuration type %s", kind)
	}
}

func tableEntries(value any) ([]map[string]any, bool) {
	switch typed := value.(type) {
	case []map[string]any:
		return typed, true
	case []any:
		entries := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			entry, ok := item.(map[string]any)
			if !ok {
				return nil, false
			}
			entries = append(entries, entry)
		}
		return entries, true
	default:
		return nil, false
	}
}

func replaceTOMLValue(contents, key, literal string) (string, error) {
	parts := strings.Split(key, ".")
	if key == "dispatch" {
		return replaceDispatch(contents, literal)
	}
	section, field := strings.Join(parts[:len(parts)-1], "."), parts[len(parts)-1]
	if match, ok := findAssignment(contents, section, field); ok {
		return contents[:match.valueStart] + literal + contents[match.valueEnd:], nil
	}
	return insertAssignment(contents, section, field, literal), nil
}

func removeTOMLValue(contents, key string) (string, bool, error) {
	if key == "dispatch" {
		updated, found := removeDispatch(contents)
		return updated, found, nil
	}
	parts := strings.Split(key, ".")
	section, field := strings.Join(parts[:len(parts)-1], "."), parts[len(parts)-1]
	match, ok := findAssignment(contents, section, field)
	if !ok {
		return contents, false, nil
	}
	return contents[:match.lineStart] + contents[match.lineEnd:], true, nil
}

type assignmentSpan struct {
	lineStart  int
	lineEnd    int
	valueStart int
	valueEnd   int
}

func findAssignment(contents, wantedSection, wantedField string) (assignmentSpan, bool) {
	section := ""
	for offset := 0; offset < len(contents); {
		lineEnd := strings.IndexByte(contents[offset:], '\n')
		fullEnd := len(contents)
		if lineEnd >= 0 {
			fullEnd = offset + lineEnd + 1
		}
		line := strings.TrimSpace(strings.TrimSuffix(contents[offset:fullEnd], "\n"))
		if next, ok := parseTableHeader(line); ok {
			section = next
		} else if section == wantedSection {
			if equal := strings.IndexByte(line, '='); equal > 0 {
				field := strings.TrimSpace(line[:equal])
				if field == wantedField {
					valueStart := offset + strings.Index(contents[offset:fullEnd], "=") + 1
					for valueStart < fullEnd && (contents[valueStart] == ' ' || contents[valueStart] == '\t') {
						valueStart++
					}
					valueEnd := tomlExpressionEnd(contents, valueStart)
					return assignmentSpan{lineStart: offset, lineEnd: fullEnd, valueStart: valueStart, valueEnd: valueEnd}, true
				}
			}
		}
		if fullEnd == len(contents) {
			break
		}
		offset = fullEnd
	}
	return assignmentSpan{}, false
}

func parseTableHeader(line string) (string, bool) {
	if !strings.HasPrefix(line, "[") {
		return "", false
	}
	start, end := 1, len(line)-1
	if strings.HasPrefix(line, "[[") && strings.HasSuffix(line, "]]") {
		start, end = 2, len(line)-2
	} else if !strings.HasSuffix(line, "]") {
		return "", false
	}
	name := strings.TrimSpace(line[start:end])
	if name == "" {
		return "", false
	}
	return name, true
}

func tomlExpressionEnd(contents string, start int) int {
	depth := 0
	quote := byte(0)
	triple := false
	escaped := false
	last := start
	seen := false
	for i := start; i < len(contents); i++ {
		ch := contents[i]
		if quote != 0 {
			if triple && i+2 < len(contents) && contents[i:i+3] == strings.Repeat(string(quote), 3) && !escaped {
				quote = 0
				triple = false
				i += 2
				last = i + 1
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
			last = i + 1
			continue
		}
		if ch == '\n' {
			if depth == 0 && seen {
				return last
			}
			continue
		}
		if ch == '#' {
			if depth == 0 {
				return last
			}
			for i < len(contents) && contents[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			if i+2 < len(contents) && contents[i:i+3] == strings.Repeat(string(ch), 3) {
				triple = true
				i += 2
			}
			seen = true
			last = i + 1
			continue
		}
		switch ch {
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		}
		if !isTOMLSpace(ch) {
			seen = true
			last = i + 1
		}
	}
	return last
}

func isTOMLSpace(ch byte) bool { return ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' }

func insertAssignment(contents, section, field, literal string) string {
	line := field + " = " + literal + "\n"
	start, end, found := tableRange(contents, section)
	if found {
		if end > start && contents[end-1] != '\n' {
			return contents[:end] + "\n" + line + contents[end:]
		}
		return contents[:end] + line + contents[end:]
	}
	if section == "" {
		return line + contents
	}
	trimmed := strings.TrimRight(contents, "\n")
	separator := "\n"
	if trimmed == "" {
		separator = ""
	} else if trimmed == contents {
		separator = "\n\n"
	} else {
		separator = "\n"
	}
	return trimmed + separator + "[" + section + "]\n" + line
}

func tableRange(contents, wanted string) (int, int, bool) {
	if wanted == "" {
		for offset := 0; offset < len(contents); {
			lineEnd := strings.IndexByte(contents[offset:], '\n')
			if lineEnd < 0 {
				return 0, len(contents), true
			}
			fullEnd := offset + lineEnd + 1
			if _, header := parseTableHeader(strings.TrimSpace(contents[offset:fullEnd])); header {
				return 0, offset, true
			}
			offset = fullEnd
		}
		return 0, len(contents), true
	}
	section := ""
	start, end := -1, len(contents)
	for offset := 0; offset < len(contents); {
		lineEnd := strings.IndexByte(contents[offset:], '\n')
		fullEnd := len(contents)
		if lineEnd >= 0 {
			fullEnd = offset + lineEnd + 1
		}
		if next, ok := parseTableHeader(strings.TrimSpace(contents[offset:fullEnd])); ok {
			if start >= 0 {
				end = offset
				break
			}
			if next == wanted {
				section, start = next, fullEnd
			}
		}
		if fullEnd == len(contents) {
			break
		}
		offset = fullEnd
	}
	if section == wanted {
		return start, end, true
	}
	return 0, 0, false
}

func replaceDispatch(contents, literal string) (string, error) {
	var document map[string]any
	if _, err := toml.Decode("dispatch = "+literal, &document); err != nil {
		return "", fmt.Errorf("dispatch must be an array of tables")
	}
	entries := []map[string]any{}
	switch value := document["dispatch"].(type) {
	case []map[string]any:
		entries = value
	case []any:
		for _, item := range value {
			if table, ok := item.(map[string]any); ok {
				entries = append(entries, table)
			}
		}
	}
	without, _ := removeDispatch(contents)
	var output strings.Builder
	output.WriteString(without)
	if output.Len() > 0 && !strings.HasSuffix(without, "\n\n") {
		if !strings.HasSuffix(without, "\n") {
			output.WriteByte('\n')
		}
		output.WriteByte('\n')
	}
	for _, entry := range entries {
		if fallback, ok := entry["default"].(map[string]any); ok {
			if use, ok := fallback["use"].(string); ok {
				fmt.Fprintf(&output, "[dispatch.default]\nuse = %s\n\n", strconv.Quote(use))
			}
			delete(entry, "default")
		}
		if len(entry) == 0 {
			continue
		}
		output.WriteString("[[dispatch]]\n")
		for _, key := range []string{"type", "when", "use"} {
			if value, ok := entry[key].(string); ok {
				fmt.Fprintf(&output, "%s = %s\n", key, strconv.Quote(value))
			}
		}
		output.WriteByte('\n')
	}
	return output.String(), nil
}

func removeDispatch(contents string) (string, bool) {
	var output strings.Builder
	removed, inDispatch := false, false
	for offset := 0; offset < len(contents); {
		lineEnd := strings.IndexByte(contents[offset:], '\n')
		fullEnd := len(contents)
		if lineEnd >= 0 {
			fullEnd = offset + lineEnd + 1
		}
		line := strings.TrimSpace(strings.TrimSuffix(contents[offset:fullEnd], "\n"))
		if section, ok := parseTableHeader(line); ok {
			inDispatch = section == "dispatch" || strings.HasPrefix(section, "dispatch.")
			if inDispatch {
				removed = true
			} else {
				output.WriteString(contents[offset:fullEnd])
			}
		} else if !inDispatch {
			output.WriteString(contents[offset:fullEnd])
		} else {
			removed = true
		}
		if fullEnd == len(contents) {
			break
		}
		offset = fullEnd
	}
	return output.String(), removed
}

func ConfigPath(home, project string) string {
	if project != "" {
		return filepath.Join(home, "projects", project, "config.toml")
	}
	return filepath.Join(home, "config.toml")
}

func filePerm(path string) os.FileMode {
	info, err := os.Stat(path)
	if err == nil {
		return info.Mode().Perm()
	}
	return 0o600
}
