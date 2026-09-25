package axi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Field and Object preserve encounter order for deterministic TOON output.
type Field struct {
	Key   string
	Value any
}

type Object []Field

// Row renders a single tabular record inline under its field name.
type Row []Field

type ToonOptions struct {
	Delimiter  rune
	IndentSize int
}

func Encode(value any) (string, error) {
	return EncodeWithOptions(value, ToonOptions{})
}

func EncodeWithOptions(value any, options ToonOptions) (string, error) {
	if options.Delimiter == 0 {
		options.Delimiter = ','
	}
	if options.Delimiter != ',' && options.Delimiter != '\t' && options.Delimiter != '|' {
		return "", fmt.Errorf("unsupported TOON delimiter %q", options.Delimiter)
	}
	if options.IndentSize == 0 {
		options.IndentSize = 2
	}
	if options.IndentSize < 1 || options.IndentSize > 8 {
		return "", fmt.Errorf("indent size must be between 1 and 8")
	}
	v, err := normalize(value)
	if err != nil {
		return "", err
	}
	lines, err := renderRoot(v, options)
	if err != nil {
		return "", err
	}
	return strings.Join(lines, "\n"), nil
}

func renderRoot(value any, options ToonOptions) ([]string, error) {
	switch v := value.(type) {
	case Object:
		return renderObject(v, 0, false, options)
	case []any:
		return renderArrayRoot(v, options)
	default:
		return []string{encodePrimitive(v, options.Delimiter)}, nil
	}
}

func renderObject(object Object, depth int, listItem bool, options ToonOptions) ([]string, error) {
	if len(object) == 0 {
		if listItem {
			return []string{indent(depth, options) + "-"}, nil
		}
		return nil, nil
	}
	lines := make([]string, 0, len(object))
	for i, field := range object {
		prefix := indent(depth, options)
		childDepth := depth
		if listItem && i == 0 {
			prefix += "- "
			childDepth++
		} else if listItem {
			prefix = indent(depth+1, options)
			childDepth++
		}
		fieldLines, err := renderField(field.Key, field.Value, prefix, childDepth, options)
		if err != nil {
			return nil, err
		}
		lines = append(lines, fieldLines...)
	}
	return lines, nil
}

func renderField(key string, value any, prefix string, childDepth int, options ToonOptions) ([]string, error) {
	keyToken := encodeKey(key, options.Delimiter)
	switch v := value.(type) {
	case Object:
		if len(v) == 0 {
			return []string{prefix + keyToken + ":"}, nil
		}
		children, err := renderObject(v, childDepth+1, false, options)
		if err != nil {
			return nil, err
		}
		return append([]string{prefix + keyToken + ":"}, children...), nil
	case Row:
		headers := make([]string, len(v))
		cells := make([]string, len(v))
		for index, field := range v {
			if !isPrimitive(field.Value) {
				return nil, fmt.Errorf("inline TOON row field %q is not primitive", field.Key)
			}
			headers[index] = encodeKey(field.Key, options.Delimiter)
			cells[index] = encodeInlineCell(field.Value, options.Delimiter)
		}
		return []string{prefix + keyToken + "{" + strings.Join(headers, string(options.Delimiter)) + "}: " + strings.Join(cells, string(options.Delimiter))}, nil
	case []any:
		return renderArrayField(keyToken, v, prefix, childDepth, options)
	default:
		return []string{prefix + keyToken + ": " + encodePrimitive(v, options.Delimiter)}, nil
	}
}

func encodeInlineCell(value any, delimiter rune) string {
	if text, ok := value.(string); ok && strings.ContainsAny(text, " \t") && !quoteString(text, delimiter) {
		return quote(text)
	}
	return encodePrimitive(value, delimiter)
}

func renderArrayField(key string, values []any, prefix string, childDepth int, options ToonOptions) ([]string, error) {
	if len(values) == 0 {
		return []string{prefix + key + ": []"}, nil
	}
	if allPrimitive(values) {
		return []string{prefix + key + arrayLength(len(values), options.Delimiter) + ": " + encodePrimitiveList(values, options.Delimiter)}, nil
	}
	if columns, ok := tabularColumns(values); ok {
		header := prefix + key + arrayLength(len(values), options.Delimiter) + "{" + encodeColumnHeaders(columns, options.Delimiter) + "}:"
		rows := renderTableRows(values, columns, childDepth+1, options)
		return append([]string{header}, rows...), nil
	}
	header := prefix + key + arrayLength(len(values), options.Delimiter) + ":"
	items, err := renderArrayItems(values, childDepth+1, options)
	if err != nil {
		return nil, err
	}
	return append([]string{header}, items...), nil
}

func renderArrayRoot(values []any, options ToonOptions) ([]string, error) {
	if len(values) == 0 {
		return []string{"[]"}, nil
	}
	if allPrimitive(values) {
		return []string{arrayLength(len(values), options.Delimiter) + ": " + encodePrimitiveList(values, options.Delimiter)}, nil
	}
	if columns, ok := tabularColumns(values); ok {
		header := arrayLength(len(values), options.Delimiter) + "{" + encodeColumnHeaders(columns, options.Delimiter) + "}:"
		return append([]string{header}, renderTableRows(values, columns, 1, options)...), nil
	}
	header := arrayLength(len(values), options.Delimiter) + ":"
	items, err := renderArrayItems(values, 1, options)
	if err != nil {
		return nil, err
	}
	return append([]string{header}, items...), nil
}

func renderArrayItems(values []any, depth int, options ToonOptions) ([]string, error) {
	var lines []string
	for _, value := range values {
		prefix := indent(depth, options) + "- "
		switch v := value.(type) {
		case Object:
			if len(v) == 0 {
				lines = append(lines, strings.TrimSuffix(prefix, " "))
				continue
			}
			item, err := renderObject(v, depth, true, options)
			if err != nil {
				return nil, err
			}
			lines = append(lines, item...)
		case []any:
			if len(v) == 0 {
				lines = append(lines, prefix+"[0]:")
				continue
			}
			if allPrimitive(v) {
				lines = append(lines, prefix+arrayLength(len(v), options.Delimiter)+": "+encodePrimitiveList(v, options.Delimiter))
				continue
			}
			header := prefix + arrayLength(len(v), options.Delimiter) + ":"
			children, err := renderArrayItems(v, depth+1, options)
			if err != nil {
				return nil, err
			}
			lines = append(lines, header)
			lines = append(lines, children...)
		default:
			lines = append(lines, prefix+encodePrimitive(v, options.Delimiter))
		}
	}
	return lines, nil
}

type tableColumn struct {
	key      string
	children []tableColumn
}

func tabularColumns(values []any) ([]tableColumn, bool) {
	if len(values) == 0 {
		return nil, false
	}
	first, ok := values[0].(Object)
	if !ok || len(first) == 0 {
		return nil, false
	}
	columns := make([]tableColumn, len(first))
	for i, field := range first {
		columns[i].key = field.Key
	}
	for _, value := range values {
		object, ok := value.(Object)
		if !ok || len(object) != len(columns) {
			return nil, false
		}
		for i := range columns {
			column := &columns[i]
			cell, found := objectValue(object, column.key)
			if !found {
				return nil, false
			}
			if isPrimitive(cell) {
				continue
			}
			child, ok := nestedColumnsForKey(values, column.key)
			if !ok {
				return nil, false
			}
			column.children = child
		}
	}
	return columns, true
}

func nestedColumnsForKey(values []any, key string) ([]tableColumn, bool) {
	var objects []any
	for _, value := range values {
		object, ok := value.(Object)
		if !ok {
			return nil, false
		}
		cell, found := objectValue(object, key)
		if !found {
			return nil, false
		}
		child, ok := cell.(Object)
		if !ok || len(child) == 0 {
			return nil, false
		}
		objects = append(objects, child)
	}
	return tabularColumns(objects)
}

func encodeColumnHeaders(columns []tableColumn, delimiter rune) string {
	parts := make([]string, len(columns))
	for i, column := range columns {
		parts[i] = encodeKey(column.key, delimiter)
		if len(column.children) > 0 {
			parts[i] += "{" + encodeColumnHeaders(column.children, delimiter) + "}"
		}
	}
	return strings.Join(parts, string(delimiter))
}

func renderTableRows(values []any, columns []tableColumn, depth int, options ToonOptions) []string {
	rows := make([]string, len(values))
	for i, value := range values {
		object, _ := value.(Object)
		cells := flattenTableRow(object, columns, options.Delimiter)
		rows[i] = indent(depth, options) + strings.Join(cells, string(options.Delimiter))
	}
	return rows
}

func flattenTableRow(object Object, columns []tableColumn, delimiter rune) []string {
	var cells []string
	for _, column := range columns {
		value, _ := objectValue(object, column.key)
		if len(column.children) > 0 {
			child, _ := value.(Object)
			cells = append(cells, flattenTableRow(child, column.children, delimiter)...)
		} else {
			cells = append(cells, encodePrimitive(value, delimiter))
		}
	}
	return cells
}

func objectValue(object Object, key string) (any, bool) {
	for _, field := range object {
		if field.Key == key {
			return field.Value, true
		}
	}
	return nil, false
}

func allPrimitive(values []any) bool {
	for _, value := range values {
		if !isPrimitive(value) {
			return false
		}
	}
	return true
}

func isPrimitive(value any) bool {
	switch value.(type) {
	case nil, string, bool, json.Number, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return true
	default:
		return false
	}
}

func encodePrimitiveList(values []any, delimiter rune) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = encodePrimitive(value, delimiter)
	}
	return strings.Join(parts, string(delimiter))
}

func arrayLength(length int, delimiter rune) string {
	suffix := ""
	if delimiter != ',' {
		suffix = string(delimiter)
	}
	return "[" + strconv.Itoa(length) + suffix + "]"
}

func encodePrimitive(value any, delimiter rune) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		if quoteString(v, delimiter) {
			return quote(v)
		}
		return v
	case bool:
		return strconv.FormatBool(v)
	case json.Number:
		return canonicalNumber(v.String())
	case float32:
		return canonicalNumber(strconv.FormatFloat(float64(v), 'g', -1, 32))
	case float64:
		return canonicalNumber(strconv.FormatFloat(v, 'g', -1, 64))
	default:
		return fmt.Sprint(v)
	}
}

func quoteString(value string, delimiter rune) bool {
	if value == "" || strings.Trim(value, " \t\r\n") != value || value == "true" || value == "false" || value == "null" ||
		value == "-" || strings.HasPrefix(value, "-") || strings.HasPrefix(value, "#") ||
		strings.ContainsAny(value, ":\\\"[]{}") || strings.ContainsRune(value, delimiter) || numericLike(value) {
		return true
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func quoteKey(value string, delimiter rune) bool {
	if value == "" || strings.Trim(value, " \t\r\n") != value || strings.ContainsFunc(value, unicode.IsSpace) || strings.ContainsAny(value, ":\\\"[]{}") ||
		strings.ContainsRune(value, delimiter) || numericLike(value) || strings.HasPrefix(value, "-") || strings.HasPrefix(value, "#") {
		return true
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r > unicode.MaxASCII {
			return true
		}
	}
	return false
}

func encodeKey(value string, delimiter rune) string {
	if quoteKey(value, delimiter) {
		return quote(value)
	}
	return value
}

func quote(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\\':
			out.WriteString("\\\\")
		case '"':
			out.WriteString("\\\"")
		case '\n':
			out.WriteString("\\n")
		case '\r':
			out.WriteString("\\r")
		case '\t':
			out.WriteString("\\t")
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&out, "\\u%04x", r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
	return out.String()
}

func numericLike(value string) bool {
	if value == "" {
		return false
	}
	_, ok := new(big.Rat).SetString(value)
	if ok {
		return true
	}
	_, err := strconv.ParseFloat(value, 64)
	return err == nil
}

func canonicalNumber(value string) string {
	if value == "" || strings.Contains(value, "NaN") || strings.Contains(value, "Inf") {
		return "null"
	}
	negative := strings.HasPrefix(value, "-")
	if strings.HasPrefix(value, "+") || negative {
		value = value[1:]
	}
	parts := strings.SplitN(strings.ToLower(value), "e", 2)
	mantissa := parts[0]
	exponent := 0
	if len(parts) == 2 {
		exponent, _ = strconv.Atoi(parts[1])
	}
	digits := strings.ReplaceAll(mantissa, ".", "")
	dot := strings.IndexByte(mantissa, '.')
	if dot < 0 {
		dot = len(mantissa)
	}
	point := dot + exponent
	if point <= 0 {
		digits = strings.Repeat("0", -point) + digits
		point = 0
	}
	if point >= len(digits) {
		digits += strings.Repeat("0", point-len(digits))
		point = len(digits)
	}
	whole, fraction := digits[:point], digits[point:]
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	fraction = strings.TrimRight(fraction, "0")
	result := whole
	if fraction != "" {
		result += "." + fraction
	}
	if result == "0" {
		negative = false
	}
	if negative {
		return "-" + result
	}
	return result
}

func indent(depth int, options ToonOptions) string {
	return strings.Repeat(" ", depth*options.IndentSize)
}

// emptyJSONValue reports whether encoding/json's omitempty drops the value.
func emptyJSONValue(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return value.Len() == 0
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Interface, reflect.Pointer:
		return value.IsZero()
	}
	return false
}

func normalize(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	switch v := value.(type) {
	case Row:
		out := make(Row, len(v))
		for i, field := range v {
			normalized, err := normalize(field.Value)
			if err != nil {
				return nil, err
			}
			out[i] = Field{Key: field.Key, Value: normalized}
		}
		return out, nil
	case Object:
		out := make(Object, len(v))
		for i, field := range v {
			normalized, err := normalize(field.Value)
			if err != nil {
				return nil, err
			}
			out[i] = Field{Key: field.Key, Value: normalized}
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i := range v {
			normalized, err := normalize(v[i])
			if err != nil {
				return nil, err
			}
			out[i] = normalized
		}
		return out, nil
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make(Object, 0, len(keys))
		for _, key := range keys {
			normalized, err := normalize(v[key])
			if err != nil {
				return nil, err
			}
			out = append(out, Field{Key: key, Value: normalized})
		}
		return out, nil
	case json.RawMessage:
		return ParseOrderedJSON(bytes.NewReader(v))
	}
	if isPrimitive(value) {
		return value, nil
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, nil
		}
		return normalize(rv.Elem().Interface())
	}
	if rv.Kind() == reflect.Struct {
		typ := rv.Type()
		out := make(Object, 0, rv.NumField())
		for i := 0; i < rv.NumField(); i++ {
			fieldType := typ.Field(i)
			if fieldType.PkgPath != "" {
				continue
			}
			name := fieldType.Name
			tagParts := strings.Split(fieldType.Tag.Get("json"), ",")
			if tag := tagParts[0]; tag != "" && tag != "-" {
				name = tag
			}
			if fieldType.Tag.Get("json") == "-" {
				continue
			}
			if slices.Contains(tagParts[1:], "omitempty") && emptyJSONValue(rv.Field(i)) {
				continue
			}
			field, err := normalize(rv.Field(i).Interface())
			if err != nil {
				return nil, err
			}
			out = append(out, Field{Key: name, Value: field})
		}
		return out, nil
	}
	if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		out := make([]any, rv.Len())
		for i := range out {
			normalized, err := normalize(rv.Index(i).Interface())
			if err != nil {
				return nil, err
			}
			out[i] = normalized
		}
		return out, nil
	}
	if rv.Kind() == reflect.Map {
		if rv.IsNil() {
			return nil, nil
		}
		if rv.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("unsupported TOON map key type %s", rv.Type().Key())
		}
		keys := rv.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		out := make(Object, 0, len(keys))
		for _, key := range keys {
			normalized, err := normalize(rv.MapIndex(key).Interface())
			if err != nil {
				return nil, err
			}
			out = append(out, Field{Key: key.String(), Value: normalized})
		}
		return out, nil
	}
	switch rv.Kind() {
	case reflect.String:
		return rv.String(), nil
	case reflect.Bool:
		return rv.Bool(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint(), nil
	case reflect.Float32:
		return float32(rv.Float()), nil
	case reflect.Float64:
		return rv.Float(), nil
	}
	if isPrimitive(value) {
		return value, nil
	}
	return nil, fmt.Errorf("unsupported TOON value %T", value)
}

// ParseOrderedJSON decodes JSON while retaining object key order for fixtures.
func ParseOrderedJSON(reader io.Reader) (any, error) {
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	value, err := parseJSONValue(decoder)
	if err != nil {
		return nil, err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("unexpected trailing JSON token %v", token)
		}
		return nil, err
	}
	return value, nil
}

func parseJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		var object Object
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("expected JSON object key, got %T", keyToken)
			}
			value, err := parseJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			object = append(object, Field{Key: key, Value: value})
		}
		_, err := decoder.Token()
		return object, err
	case '[':
		var values []any
		for decoder.More() {
			value, err := parseJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		_, err := decoder.Token()
		return values, err
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}
