package app

import (
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
)

// User input is rejected. The recorded 0.14 Lead adapter protocol is the only
// executable exception, and uses the canonical Watch's caller guards.
func addRemovedVocabularyCommands(root *axi.Command) {
	var watchHandler func(*axi.Context, []string) error
	for _, command := range root.Subcommands {
		if command.Name == "watch" {
			watchHandler = command.Handler
		}
	}
	for _, entry := range [][2]string{
		{"ride", "start"}, {"holler", "signal"}, {"unsaddle", "teardown"},
		{"lookout", "watch"}, {"remuda", "pool"}, {"roster", "list"},
		{"land", "merge"}, {"sweep", "pool prune"}, {"playbook", "preferences"},
	} {
		old, replacement := entry[0], entry[1]
		command := &axi.Command{Name: old, Hidden: true, ReplacedBy: replacement}
		if old == "lookout" && watchHandler != nil {
			command.Handler = legacyAdapterWatch(watchHandler)
		}
		root.Subcommands = append(root.Subcommands, command)
	}
}

func legacyAdapterWatch(watch func(*axi.Context, []string) error) func(*axi.Context, []string) error {
	return func(ctx *axi.Context, args []string) error {
		parsed, err := parseArgs("watch", args, map[string]flagSpec{
			"quiet-routine": {boolean: true}, "handoff": {boolean: true}, "destination": {},
			"receipt": {}, "receipt-outcome": {}, "receipt-token": {},
		})
		if err == nil && ctx.JSON && len(parsed.Positionals) == 0 {
			destination := parsed.Flags["destination"]
			adapter, session, _ := strings.Cut(destination, ":")
			handoff := parsed.Bool("handoff") && session != "" && (adapter == "pi" || adapter == "opencode") && parsed.Flags["receipt"] == "" && parsed.Flags["receipt-outcome"] == "" && parsed.Flags["receipt-token"] == ""
			receipt := parsed.Flags["receipt"] != "" && oneOfString(parsed.Flags["receipt-outcome"], "accepted", "rejected", "uncertain") && !parsed.Bool("handoff") && destination == "" && !parsed.Bool("quiet-routine")
			if handoff || receipt {
				return watch(ctx, args)
			}
		}
		return axi.Usage("posse lookout was removed; use posse watch")
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
