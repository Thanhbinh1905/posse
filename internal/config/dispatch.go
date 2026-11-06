package config

import "strings"

func (r Rule) ProfileNames() []string {
	var names []string
	switch value := r.Use.(type) {
	case string:
		names = []string{value}
	case []string:
		names = value
	case []any:
		for _, item := range value {
			name, ok := item.(string)
			if !ok {
				return nil
			}
			names = append(names, name)
		}
	default:
		return nil
	}
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return nil
		}
	}
	return names
}
