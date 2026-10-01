package app

import (
	"fmt"
	"strconv"
	"strings"
)

type launchArgAnalysis struct {
	models   []string
	sessions []string
	unknown  []string
}

func analyzeLaunchArgs(kind string, args []string) launchArgAnalysis {
	var result launchArgAnalysis
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "resume" || arg == "fork" {
			value := ""
			if index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") {
				value = args[index+1]
				index++
			}
			result.sessions = append(result.sessions, value)
			continue
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			result.unknown = append(result.unknown, "positional argument")
			continue
		}
		if strings.HasPrefix(arg, "--") {
			name, value, attached := strings.Cut(arg, "=")
			index += analyzeLongLaunchArg(kind, name, value, attached, args, index, &result)
			continue
		}
		analyzeShortLaunchArg(kind, arg, args, &index, &result)
	}
	return result
}

// analyzeLongLaunchArg returns the number of following argv entries consumed.
// Identity-affecting options are recognized even when malformed so they fail
// closed rather than being ignored.
func analyzeLongLaunchArg(kind, name, attachedValue string, attached bool, args []string, index int, result *launchArgAnalysis) int {
	lower := strings.ToLower(name)
	if isLongModelOption(lower) {
		value, consumed, ok := optionValue(args, index, attachedValue, attached)
		if !ok {
			value = "<unknown>"
		}
		result.models = append(result.models, value)
		return consumed
	}
	if isLongSessionOption(kind, lower) {
		if lower == "--continue" || lower == "--fork-session" || lower == "--fork" {
			result.sessions = append(result.sessions, "")
			return 0
		}
		value, consumed, ok := optionValue(args, index, attachedValue, attached)
		if !ok {
			value = ""
		}
		result.sessions = append(result.sessions, value)
		return consumed
	}
	if strings.HasPrefix(lower, "--session") || strings.HasPrefix(lower, "--resume") || strings.HasPrefix(lower, "--thread") || strings.HasPrefix(lower, "--conversation") {
		result.sessions = append(result.sessions, "<unclassified>")
		result.unknown = append(result.unknown, name)
		return 0
	}
	if kind == "codex" && (lower == "--config" || lower == "--configuration") {
		value, consumed, ok := optionValue(args, index, attachedValue, attached)
		if !ok || !classifyCodexConfig(value, result) {
			result.unknown = append(result.unknown, name)
		}
		return consumed
	}
	if arity, known := longBenignOption(kind, lower); known {
		if arity == 0 {
			if attached {
				result.unknown = append(result.unknown, name)
			}
			return 0
		}
		_, consumed, ok := optionValue(args, index, attachedValue, attached)
		if !ok {
			result.unknown = append(result.unknown, name)
		}
		return consumed
	}
	if strings.HasPrefix(lower, "--model-") || strings.HasPrefix(lower, "--model_") {
		result.models = append(result.models, "<unclassified>")
		result.unknown = append(result.unknown, name)
		return 0
	}
	result.unknown = append(result.unknown, name)
	return 0
}

func analyzeShortLaunchArg(kind, arg string, args []string, index *int, result *launchArgAnalysis) {
	cluster := arg[1:]
	if cluster == "" {
		return
	}
	for offset := 0; offset < len(cluster); offset++ {
		flag := cluster[offset]
		rest := cluster[offset+1:]
		switch kind {
		case "claude":
			switch flag {
			case 'p', 'v', 'h':
				continue
			case 'c':
				result.sessions = append(result.sessions, "")
				continue
			case 'r':
				value, consumed, ok := shortOptionValue(args, *index, rest)
				if !ok {
					value = ""
				}
				result.sessions = append(result.sessions, value)
				*index += consumed
				return
			}
		case "codex":
			switch flag {
			case 'm':
				value, consumed, ok := shortOptionValue(args, *index, rest)
				if !ok {
					value = "<unknown>"
				}
				result.models = append(result.models, value)
				*index += consumed
				return
			case 'c':
				value, consumed, ok := shortOptionValue(args, *index, rest)
				if !ok || !classifyCodexConfig(value, result) {
					result.unknown = append(result.unknown, "-c")
				}
				*index += consumed
				return
			case 'p':
				_, consumed, ok := shortOptionValue(args, *index, rest)
				result.models = append(result.models, "<profile>")
				if !ok {
					result.unknown = append(result.unknown, "-p")
				}
				*index += consumed
				return
			case 's', 'a', 'i', 'C':
				_, consumed, ok := shortOptionValue(args, *index, rest)
				if !ok {
					result.unknown = append(result.unknown, "-"+string(flag))
				}
				*index += consumed
				return
			case 'h', 'V':
				continue
			}
		case "pi":
			switch flag {
			case 'c':
				result.sessions = append(result.sessions, "")
				continue
			case 'r':
				value, consumed, ok := shortOptionValue(args, *index, rest)
				if !ok {
					value = ""
				}
				result.sessions = append(result.sessions, value)
				*index += consumed
				return
			case 'm':
				value, consumed, ok := shortOptionValue(args, *index, rest)
				if !ok {
					value = "<unknown>"
				}
				result.models = append(result.models, value)
				*index += consumed
				return
			case 't':
				_, consumed, ok := shortOptionValue(args, *index, rest)
				if !ok {
					result.unknown = append(result.unknown, "-t")
				}
				*index += consumed
				return
			case 'h', 'v':
				continue
			}
		case "opencode":
			switch flag {
			case 'c':
				result.sessions = append(result.sessions, "")
				continue
			case 's':
				value, consumed, ok := shortOptionValue(args, *index, rest)
				if !ok {
					value = ""
				}
				result.sessions = append(result.sessions, value)
				*index += consumed
				return
			case 'm':
				value, consumed, ok := shortOptionValue(args, *index, rest)
				if !ok {
					value = "<unknown>"
				}
				result.models = append(result.models, value)
				*index += consumed
				return
			case 'p':
				_, consumed, ok := shortOptionValue(args, *index, rest)
				if !ok {
					result.unknown = append(result.unknown, "-p")
				}
				*index += consumed
				return
			case 'h', 'v':
				continue
			}
		}
		result.unknown = append(result.unknown, "-"+string(flag))
	}
}

func isLongModelOption(option string) bool {
	return option == "--model"
}

func isLongSessionOption(kind, option string) bool {
	switch kind {
	case "claude":
		switch option {
		case "--resume", "--continue", "--session-id", "--fork-session", "--from-pr", "--teleport", "--cloud":
			return true
		}
	case "codex":
		switch option {
		case "--resume", "--fork", "--session", "--session-id", "--thread", "--thread-id":
			return true
		}
	case "pi":
		switch option {
		case "--session", "--session-id", "--continue", "--resume", "--fork", "--session-dir":
			return true
		}
	case "opencode":
		switch option {
		case "--session", "--continue", "--fork":
			return true
		}
	}
	return false
}

// longBenignOption contains only options whose values cannot select a model or
// session. Unlisted options are unknown and make the configured identity fail closed.
func longBenignOption(kind, option string) (int, bool) {
	if option == "--help" || option == "--version" {
		return 0, true
	}
	switch kind {
	case "claude":
		options := map[string]int{"--dangerously-skip-permissions": 0, "--print": 0, "--verbose": 0, "--effort": 1, "--output-format": 1, "--append-system-prompt": 1, "--append-system-prompt-file": 1, "--system-prompt": 1, "--system-prompt-file": 1, "--tools": 1, "--allowed-tools": 1, "--disallowed-tools": 1, "--permission-mode": 1, "--add-dir": 1, "--name": 1, "--debug": 1, "--betas": 1, "--max-budget-usd": 1, "--max-turns": 1, "--json-schema": 1, "--input-format": 1, "--strict-mcp-config": 0}
		if arity, ok := options[option]; ok {
			return arity, true
		}
	case "codex":
		options := map[string]int{"--dangerously-bypass-approvals-and-sandbox": 0, "--oss": 0, "--enable": 1, "--disable": 1, "--strict-config": 0, "--sandbox": 1, "--approve-for-me": 0, "--dangerously-bypass-hook-trust": 0, "--cd": 1, "--worktree": 0, "--add-dir": 1, "--ask-for-approval": 1, "--search": 0, "--no-alt-screen": 0, "--no-daemon": 0, "--local-provider": 1, "--image": 1}
		if arity, ok := options[option]; ok {
			return arity, true
		}
	case "pi":
		options := map[string]int{"--thinking": 1, "--append-system-prompt": 1, "--append-system-prompt-file": 1, "--provider": 1, "--api-key": 1, "--mode": 1, "--no-session": 0, "--continue": 0}
		if arity, ok := options[option]; ok {
			return arity, true
		}
	case "opencode":
		options := map[string]int{"--auto": 0, "--prompt": 1, "--format": 1, "--title": 1, "--file": 1}
		if arity, ok := options[option]; ok {
			return arity, true
		}
	}
	return 0, false
}

func optionValue(args []string, index int, attachedValue string, attached bool) (string, int, bool) {
	if attached {
		return attachedValue, 0, strings.TrimSpace(attachedValue) != ""
	}
	if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" || strings.HasPrefix(args[index+1], "-") {
		return "", 0, false
	}
	return args[index+1], 1, true
}

func shortOptionValue(args []string, index int, remainder string) (string, int, bool) {
	if remainder != "" {
		value := strings.TrimPrefix(remainder, "=")
		return value, 0, strings.TrimSpace(value) != ""
	}
	if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" || strings.HasPrefix(args[index+1], "-") {
		return "", 0, false
	}
	return args[index+1], 1, true
}

func classifyCodexConfig(value string, result *launchArgAnalysis) bool {
	key, raw, ok := strings.Cut(strings.TrimSpace(value), "=")
	if !ok {
		return false
	}
	// Codex configuration keys are case-sensitive. Unknown spellings fail closed.
	key = strings.TrimSpace(key)
	raw = strings.TrimSpace(raw)
	switch key {
	case "model":
		if model, err := unquoteConfigValue(raw); err == nil && model != "" {
			result.models = append(result.models, model)
		} else {
			result.models = append(result.models, "<unknown>")
			return false
		}
		return true
	case "session_id", "session-id", "thread_id", "thread-id", "conversation_id", "conversation-id":
		result.sessions = append(result.sessions, strings.TrimSpace(raw))
		return true
	case "model_reasoning_effort":
		return true
	default:
		return false
	}
}

func unquoteConfigValue(value string) (string, error) {
	if strings.HasPrefix(value, "\"") {
		return strconv.Unquote(value)
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1], nil
	}
	if value == "" || strings.ContainsAny(value, " \t") {
		return "", fmt.Errorf("ambiguous config value")
	}
	return value, nil
}

func configuredModelFromLaunchArgs(profileModel, kind string, args []string) (string, bool) {
	if strings.TrimSpace(profileModel) == "" {
		return profileModel, false
	}
	analysis := analyzeLaunchArgs(kind, args)
	if len(analysis.unknown) != 0 || len(analysis.models) != 1 || analysis.models[0] != profileModel {
		return profileModel, false
	}
	return profileModel, true
}

func reviewSessionOverrideReason(kind string, args []string) string {
	analysis := analyzeLaunchArgs(kind, args)
	if len(analysis.sessions) != 0 {
		return fmt.Sprintf("launch arguments contain %d session selector(s)", len(analysis.sessions))
	}
	if len(analysis.unknown) != 0 {
		return fmt.Sprintf("launch arguments contain unclassified option %q", analysis.unknown[0])
	}
	return ""
}

func reviewResumeArgsRefusal(kind string, args []string, session string) string {
	if session == "" || len(args) == 0 {
		return ""
	}
	rendered := renderArgs(args, "{session}", session)
	analysis := analyzeLaunchArgs(kind, rendered)
	if len(analysis.unknown) != 0 {
		return fmt.Sprintf("configured resume arguments contain unclassified option %q", analysis.unknown[0])
	}
	if len(analysis.models) != 0 {
		return "configured resume arguments can override the Review Task model"
	}
	if len(analysis.sessions) != 1 || analysis.sessions[0] != session {
		return "configured resume arguments do not select only Posse's own Review Task session"
	}
	return ""
}
