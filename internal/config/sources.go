package config

import (
	"encoding/json"
	"strings"
)

// EffectiveSources names the winning layer for each effective leaf. Dispatch
// rules concatenate Project and global layers rather than replacing them.
func (c Config) EffectiveSources() (map[string][]string, error) {
	sources := map[string][]string{}
	var walk func(string, any, string)
	walk = func(prefix string, value any, source string) {
		if table, ok := value.(map[string]any); ok {
			for key, item := range table {
				path := key
				if prefix != "" {
					path = prefix + "." + key
				}
				walk(path, item, source)
			}
			return
		}
		prefix = canonicalSchemaKey(prefix)
		prefix = strings.Replace(prefix, "dispatch_default.", "dispatch.default.", 1)
		if prefix == "dispatch" && source != "built-in default" {
			previous := sources[prefix]
			if len(previous) == 1 && previous[0] == "built-in default" {
				previous = nil
			}
			sources[prefix] = append([]string{source}, previous...)
		} else {
			sources[prefix] = []string{source}
		}
	}
	data, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var values map[string]any
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	walk("", values, "built-in default")
	for _, path := range c.Files {
		values, _, err := ReadFile(path)
		if err != nil {
			return nil, err
		}
		walk("", values, path)
	}
	if c.Autonomy.Yolo {
		sources["autonomy.review"] = sources["autonomy.yolo"]
		sources["autonomy.merge"] = sources["autonomy.yolo"]
	}
	return sources, nil
}
