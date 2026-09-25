package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type jsonSpan struct {
	kind     byte
	start    int
	end      int
	members  []jsonSpanMember
	elements []*jsonSpan
}

type jsonSpanMember struct {
	key      string
	keyStart int
	value    *jsonSpan
}

func parseJSONSpans(data []byte) (*jsonSpan, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	root, err := parseJSONSpan(decoder, data)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("unexpected JSON after root value")
		}
		return nil, err
	}
	return root, nil
}

func parseJSONSpan(decoder *json.Decoder, data []byte) (*jsonSpan, error) {
	start := nextJSONTokenStart(data, int(decoder.InputOffset()))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	node := &jsonSpan{start: start}
	if delimiter, ok := token.(json.Delim); ok {
		node.kind = byte(delimiter)
		switch delimiter {
		case '{':
			for decoder.More() {
				keyStart := nextJSONTokenStart(data, int(decoder.InputOffset()))
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, fmt.Errorf("JSON object key is not a string")
				}
				value, err := parseJSONSpan(decoder, data)
				if err != nil {
					return nil, err
				}
				node.members = append(node.members, jsonSpanMember{key: key, keyStart: keyStart, value: value})
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
		case '[':
			for decoder.More() {
				value, err := parseJSONSpan(decoder, data)
				if err != nil {
					return nil, err
				}
				node.elements = append(node.elements, value)
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unexpected JSON delimiter %q", delimiter)
		}
	}
	node.end = int(decoder.InputOffset())
	return node, nil
}

func nextJSONTokenStart(data []byte, offset int) int {
	for offset < len(data) && isJSONSpace(data[offset]) {
		offset++
	}
	if offset < len(data) && (data[offset] == ':' || data[offset] == ',') {
		offset++
		for offset < len(data) && isJSONSpace(data[offset]) {
			offset++
		}
	}
	return offset
}

func isJSONSpace(value byte) bool {
	return value == ' ' || value == '\n' || value == '\r' || value == '\t'
}

func (node *jsonSpan) member(key string) (*jsonSpan, int) {
	if node == nil || node.kind != '{' {
		return nil, -1
	}
	for index := len(node.members) - 1; index >= 0; index-- {
		if node.members[index].key == key {
			return node.members[index].value, index
		}
	}
	return nil, -1
}

func jsonTrailingSpaceStart(data []byte, start, end int) int {
	for end > start && isJSONSpace(data[end-1]) {
		end--
	}
	return end
}

func insertJSONMember(data []byte, node *jsonSpan, key string, value []byte) []byte {
	position := jsonTrailingSpaceStart(data, node.start+1, node.end-1)
	encodedKey, _ := json.Marshal(key)
	prefix := []byte(nil)
	if len(node.members) > 0 {
		prefix = []byte{','}
	}
	separator := ":"
	if memberIndent, indent, multiline := objectMemberIndent(data, node); multiline {
		newline := jsonNewline(data)
		prefix = append(prefix, []byte(newline+memberIndent)...)
		separator = ": "
		value = bytes.TrimPrefix(formatJSONItem(value, memberIndent, indent, newline), []byte(memberIndent))
	}
	member := make([]byte, 0, len(prefix)+len(encodedKey)+len(separator)+len(value))
	member = append(member, prefix...)
	member = append(member, encodedKey...)
	member = append(member, separator...)
	member = append(member, value...)
	return insertJSONBytes(data, position, member)
}

// objectMemberIndent returns the indent of an object's members and the indent
// unit when the object puts each member and its closing brace on own lines.
func objectMemberIndent(data []byte, node *jsonSpan) (string, string, bool) {
	if len(node.members) == 0 {
		return "", "", false
	}
	memberIndent, ok := lineIndent(data, node.members[0].keyStart)
	if !ok {
		return "", "", false
	}
	closeIndent, ok := lineIndent(data, node.end-1)
	if !ok || !strings.HasPrefix(memberIndent, closeIndent) || len(memberIndent) == len(closeIndent) {
		return "", "", false
	}
	return memberIndent, memberIndent[len(closeIndent):], true
}

func removeJSONMember(data []byte, node *jsonSpan, index int) []byte {
	member := node.members[index]
	start, end := member.keyStart, member.value.end
	if len(node.members) > 1 && index == len(node.members)-1 {
		previous := node.members[index-1]
		if comma := bytes.IndexByte(data[previous.value.end:start], ','); comma >= 0 {
			start = previous.value.end + comma
		}
	} else if len(node.members) > 1 && index == 0 {
		next := node.members[index+1]
		if comma := bytes.IndexByte(data[end:next.keyStart], ','); comma >= 0 {
			end += comma + 1
		}
	}
	return removeJSONBytes(data, start, end)
}

func appendJSONArrayItem(data []byte, node *jsonSpan, item []byte) []byte {
	var position int
	prefix := []byte(nil)
	separator := []byte(nil)
	if len(node.elements) > 0 {
		position = jsonTrailingSpaceStart(data, node.start+1, node.end-1)
		prefix = []byte{','}
		if itemIndent, indent, multiline := arrayItemIndent(data, node); multiline {
			newline := jsonNewline(data)
			item = formatJSONItem(item, itemIndent, indent, newline)
			separator = []byte(newline)
		}
	} else {
		position = node.start + 1
	}
	addition := make([]byte, 0, len(prefix)+len(separator)+len(item))
	addition = append(addition, prefix...)
	addition = append(addition, separator...)
	addition = append(addition, item...)
	return insertJSONBytes(data, position, addition)
}

func arrayItemIndent(data []byte, node *jsonSpan) (string, string, bool) {
	if len(node.elements) == 0 {
		return "", "", false
	}
	item := node.elements[0]
	itemIndent, ok := lineIndent(data, item.start)
	if !ok || item.kind != '{' || len(item.members) == 0 {
		return "", "", false
	}
	memberIndent, ok := lineIndent(data, item.members[0].keyStart)
	if !ok || !strings.HasPrefix(memberIndent, itemIndent) || len(memberIndent) == len(itemIndent) {
		return "", "", false
	}
	return itemIndent, memberIndent[len(itemIndent):], true
}

func lineIndent(data []byte, position int) (string, bool) {
	lineStart := bytes.LastIndexByte(data[:position], '\n') + 1
	indent := data[lineStart:position]
	for _, value := range indent {
		if value != ' ' && value != '\t' {
			return "", false
		}
	}
	return string(indent), true
}

func formatJSONItem(item []byte, itemIndent, indent, newline string) []byte {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, item, "", indent); err != nil {
		return item
	}
	lines := bytes.Split(pretty.Bytes(), []byte{'\n'})
	var formatted bytes.Buffer
	for index, line := range lines {
		if index > 0 {
			formatted.WriteString(newline)
		}
		formatted.WriteString(itemIndent)
		formatted.Write(line)
	}
	return formatted.Bytes()
}

func jsonNewline(data []byte) string {
	if bytes.Contains(data, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

func removeJSONArrayItem(data []byte, node *jsonSpan, index int) []byte {
	item := node.elements[index]
	start, end := item.start, item.end
	if len(node.elements) > 1 && index == len(node.elements)-1 {
		previous := node.elements[index-1]
		if comma := bytes.IndexByte(data[previous.end:start], ','); comma >= 0 {
			start = previous.end + comma
		}
	} else if len(node.elements) > 1 && index == 0 {
		next := node.elements[index+1]
		if comma := bytes.IndexByte(data[end:next.start], ','); comma >= 0 {
			end += comma + 1
		}
	}
	return removeJSONBytes(data, start, end)
}

func insertJSONBytes(data []byte, position int, addition []byte) []byte {
	updated := make([]byte, 0, len(data)+len(addition))
	updated = append(updated, data[:position]...)
	updated = append(updated, addition...)
	updated = append(updated, data[position:]...)
	return updated
}

func removeJSONBytes(data []byte, start, end int) []byte {
	updated := make([]byte, 0, len(data)-(end-start))
	updated = append(updated, data[:start]...)
	updated = append(updated, data[end:]...)
	return updated
}
