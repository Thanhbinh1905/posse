package app

import "github.com/thanhbinh1905/posse/internal/axi"

// These are error-only tombstones, not aliases. They never invoke an operation.
func addRemovedVocabularyCommands(root *axi.Command) {
	for _, entry := range [][2]string{
		{"ride", "start"}, {"holler", "signal"}, {"unsaddle", "teardown"},
		{"lookout", "watch"}, {"remuda", "pool"}, {"roster", "list"},
		{"land", "merge"}, {"sweep", "pool prune"}, {"playbook", "preferences"},
	} {
		old, replacement := entry[0], entry[1]
		root.Subcommands = append(root.Subcommands, &axi.Command{Name: old, Hidden: true, ReplacedBy: replacement})
	}
}

func removedFlag(command, name string) string {
	switch {
	case command == "merge" && name == "merge":
		return "execute"
	case name == "from-leftover":
		return "from-orphan"
	case name == "stop-lookouts":
		return "stop-watches"
	}
	return ""
}
